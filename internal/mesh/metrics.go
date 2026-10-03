package mesh

import (
	"math"

	"kontakt/internal/metrics"
)

// nanIfUnknown: неизвестное отдаём как NaN, а не как 0 или -1 — ноль на графике
// читается как «свободно», а это неправда.
func nanIfUnknown(v float64) float64 {
	if v < 0 {
		return math.NaN()
	}
	return v
}

// MetricsRegistry — метрики узла (§38) в формате Prometheus.
func (n *Node) MetricsRegistry(cf *Cloudflared) *metrics.Registry {
	r := metrics.NewRegistry()
	r.GaugeVec("kontakt_mesh_peers", "Соседи по ступени доверия.", "state", func() map[string]float64 {
		out := map[string]float64{"CONNECTED": 0}
		for id := range n.Peers() {
			out[n.Trust.State(id).String()]++
		}
		return out
	})
	link := func(f func(LinkMetrics) float64) func() map[string]float64 {
		return func() map[string]float64 {
			out := map[string]float64{}
			for id, m := range n.Peers() {
				if m.Samples > 0 {
					out[string(id)] = f(m)
				}
			}
			return out
		}
	}
	r.GaugeVec("kontakt_rtt_seconds", "RTT ребра к соседу (сглаженный).", "peer", link(func(m LinkMetrics) float64 { return m.RTT.Seconds() }))
	r.GaugeVec("kontakt_link_loss", "Потери проб на ребре к соседу, 0..1.", "peer", link(func(m LinkMetrics) float64 { return m.Loss }))
	r.GaugeVec("kontakt_link_jitter_seconds", "Джиттер ребра к соседу (RFC 3550).", "peer", link(func(m LinkMetrics) float64 { return m.Jitter.Seconds() }))
	r.CounterFunc("kontakt_route_failovers_total", "Отказы основного маршрута с заменой.", func() float64 { return float64(n.RouteFailovers()) })
	r.Gauge("kontakt_active_calls", "Активные звонки на узле.", func() float64 { _, cc := n.Capacity(); return float64(cc.ActiveCalls) })
	r.Gauge("kontakt_available_capacity", "Сколько ещё звонков узел примет по замерам (§24); NaN — ёмкость канала неизвестна.", func() float64 {
		nc, cc := n.Capacity()
		if nc.CapacityMbps <= 0 {
			return math.NaN()
		}
		return float64(cc.Calls)
	})
	r.Gauge("kontakt_call_bandwidth_bps", "Полоса одного звонка: по замеру, пока звонков нет — плановая.", func() float64 { _, cc := n.Capacity(); return cc.CallKbps * 1000 })
	r.Gauge("kontakt_call_bandwidth_measured", "1 — полоса звонка из замера, 0 — плановое допущение.", func() float64 {
		if _, cc := n.Capacity(); cc.Measured {
			return 1
		}
		return 0
	})
	r.Gauge("kontakt_bandwidth_bps", "Трафик узла по факту, бит/с.", func() float64 { nc, _ := n.Capacity(); return nc.ObservedMbps * 1e6 })
	r.Gauge("kontakt_bandwidth_utilization", "Загрузка канала узла, 0..1; NaN — ёмкость канала неизвестна.", func() float64 { nc, _ := n.Capacity(); return nanIfUnknown(nc.Utilization) })
	r.Gauge("kontakt_cpu", "CPU процесса, ядер; NaN — неизвестно на этой ОС.", func() float64 { c, _ := processUsage(); return nanIfUnknown(c) })
	r.Gauge("kontakt_memory_bytes", "Память процесса (Go runtime), байт.", func() float64 { _, m := processUsage(); return m * (1 << 20) })
	r.Gauge("kontakt_mesh_master_up", "1 — есть связь с Master'ом (данные от неё не зависят).", func() float64 {
		if n.MasterUp() {
			return 1
		}
		return 0
	})
	r.Gauge("kontakt_mesh_topology_nodes", "Узлы в ограниченной топологии вокруг этого.", func() float64 { return float64(len(n.Topo.Nodes())) })
	n.graphMetrics(r)
	if n.Registry != nil {
		r.GaugeVec("kontakt_registry_nodes", "Узлы в реестре Master'а по состоянию.", "health", func() map[string]float64 {
			out := map[string]float64{string(Healthy): 0, string(Degraded): 0, string(Offline): 0}
			for _, e := range n.Registry.Snapshot() {
				out[string(e.Health)]++
			}
			return out
		})
	}
	if cf != nil {
		r.GaugeVec("kontakt_cloudflared", "Метрики cloudflared (§19): сессии и соединения туннеля.", "metric", func() map[string]float64 {
			v, _ := cf.Values()
			return v
		})
		r.Gauge("kontakt_cloudflared_up", "1 — /metrics cloudflared ответил.", func() float64 {
			if _, ok := cf.Values(); ok {
				return 1
			}
			return 0
		})
	}
	return r
}
