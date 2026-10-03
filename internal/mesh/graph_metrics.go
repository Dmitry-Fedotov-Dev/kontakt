package mesh

import (
	"math"
	"time"

	"kontakt/internal/metrics"
)

// Цвета узлов и рёбер для Grafana Node Graph (поле color принимает имя цвета).
var (
	healthColor  = map[Health]string{Healthy: "green", Degraded: "orange", Offline: "red"}
	qualityColor = map[string]string{"ok": "green", "warning": "orange", "critical": "red",
		"unmeasured": "gray", "unconfirmed": "gray", "control": "blue", "lost": "red"}
)

func (g GraphNode) title() string {
	if g.Name != "" {
		return g.Name
	}
	if len(g.ID) > 8 {
		return string(g.ID[:8])
	}
	return string(g.ID)
}

// Корзины вместо точных чисел: метка, меняющаяся на каждом сборе, рождала бы новый ряд
// в Prometheus каждые несколько секунд.
func lossBucket(m LinkMetrics) string {
	switch {
	case m.Samples == 0:
		return "—"
	case m.Loss == 0:
		return "0%"
	case m.Loss < 0.01:
		return "<1%"
	case m.Loss < 0.05:
		return "1–5%"
	}
	return "≥5%"
}

func jitterBucket(m LinkMetrics) string {
	switch {
	case m.Samples == 0:
		return "—"
	case m.Jitter < 5*time.Millisecond:
		return "<5 мс"
	case m.Jitter < 20*time.Millisecond:
		return "5–20 мс"
	case m.Jitter < 50*time.Millisecond:
		return "20–50 мс"
	}
	return "≥50 мс"
}

// graphMetrics — граф системы в формате, который панель Grafana Node Graph читает
// напрямую из табличного запроса Prometheus: метки называются как поля панели.
func (n *Node) graphMetrics(r *metrics.Registry) {
	obs := string(n.ID)
	r.GaugeSet("kontakt_mesh_observer_info", "Этот узел как наблюдатель графа (переменная в Grafana): роль и имя.", func() []metrics.Sample {
		title := n.cfg.Name
		if title == "" {
			title = string(n.ID[:8])
		}
		// роль первой: в списке Grafana (по алфавиту) Master'ы окажутся сверху
		return []metrics.Sample{{Labels: [][2]string{{"observer", obs}, {"title", string(n.cfg.Role) + " · " + title}}, Value: 1}}
	})
	r.GaugeSet("kontakt_mesh_graph_node", "Узел графа системы (Grafana Node Graph); значение — активные звонки.", func() []metrics.Sample {
		nodes, _ := n.Graph()
		out := make([]metrics.Sample, 0, len(nodes))
		for _, g := range nodes {
			color := healthColor[g.Health]
			if g.Role == RoleMaster {
				color = "blue"
			}
			out = append(out, metrics.Sample{Labels: [][2]string{
				{"observer", obs}, {"id", string(g.ID)}, {"title", g.title()},
				{"subtitle", string(g.Role) + " · " + string(g.Health)}, {"color", color},
				{"detail__role", string(g.Role)}, {"detail__health", string(g.Health)}, {"detail__addr", g.Addr},
			}, Value: float64(g.ActiveCalls)})
		}
		return out
	})
	r.GaugeSet("kontakt_mesh_graph_edge", "Ребро графа системы; значение — RTT, мс (у рёбер control plane — NaN).", func() []metrics.Sample {
		nodes, edges := n.Graph()
		names := map[NodeID]string{}
		for _, g := range nodes {
			names[g.ID] = g.title()
		}
		out := make([]metrics.Sample, 0, len(edges))
		for _, e := range edges {
			kind, dash, v := string(e.Kind), "", e.Metrics.RTT.Seconds()*1000
			if e.Control {
				kind, dash, v = "control", "6,6", math.NaN()
			} else if !e.Confirmed || e.Metrics.Samples == 0 {
				dash = "2,4"
			}
			if kind == "" {
				kind = "direct"
			}
			out = append(out, metrics.Sample{Labels: [][2]string{
				{"observer", obs}, {"id", string(e.A) + "-" + string(e.B)}, {"source", string(e.A)}, {"target", string(e.B)},
				{"color", qualityColor[e.Quality]}, {"strokeDasharray", dash},
				{"detail__from", names[e.A]}, {"detail__to", names[e.B]},
				{"detail__kind", kind}, {"detail__quality", e.Quality},
				{"detail__loss", lossBucket(e.Metrics)}, {"detail__jitter", jitterBucket(e.Metrics)},
			}, Value: v})
		}
		return out
	})
	r.GaugeSet("kontakt_mesh_graph_node_stat", "Числа по узлам графа для таблицы: calls_free, utilization, cpu, memory_mb, seen_ago_s.", func() []metrics.Sample {
		nodes, _ := n.Graph()
		var out []metrics.Sample
		for _, g := range nodes {
			for _, st := range []struct {
				k string
				v float64
			}{{"active_calls", float64(g.ActiveCalls)}, {"calls_free", callsFree(g)}, {"utilization", nanIfUnknown(g.Utilization)},
				{"cpu", nanIfUnknown(g.CPU)}, {"memory_mb", g.MemoryMB}, {"seen_ago_s", g.SeenAgo.Seconds()}} {
				out = append(out, metrics.Sample{Labels: [][2]string{{"observer", obs}, {"id", string(g.ID)}, {"title", g.title()},
					{"role", string(g.Role)}, {"stat", st.k}}, Value: st.v})
			}
		}
		return out
	})
}

// callsFree — NaN, если ёмкость канала неизвестна: «0 свободно» читалось бы как «узел
// полон», хотя посчитать просто не из чего.
func callsFree(g GraphNode) float64 {
	if g.Utilization < 0 {
		return math.NaN()
	}
	return float64(g.CallsFree)
}
