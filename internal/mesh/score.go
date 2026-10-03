package mesh

import "time"

// Health — состояние узла (§10).
type Health string

const (
	Healthy  Health = "HEALTHY"
	Degraded Health = "DEGRADED"
	Offline  Health = "OFFLINE"
)

// Thresholds — пороги качества (§25). Начальные значения — плановые, их
// проверяют нагрузкой; все настраиваются.
type Thresholds struct {
	BandwidthWarning, BandwidthCritical float64       // загрузка канала, 0..1
	LossWarning, LossCritical           float64       // потери, 0..1
	JitterWarning, JitterCritical       time.Duration //
	LatencyWarning, LatencyCritical     time.Duration // RTT ребра
	MinSamples                          int           // меньше проб — качество ребра ещё неизвестно
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		BandwidthWarning: 0.7, BandwidthCritical: 0.9,
		LossWarning: 0.01, LossCritical: 0.05,
		JitterWarning: 20 * time.Millisecond, JitterCritical: 50 * time.Millisecond,
		LatencyWarning: 150 * time.Millisecond, LatencyCritical: 300 * time.Millisecond,
		MinSamples: 3,
	}
}

// Weights — веса RouteScore (§13). Меньше счёт — лучше путь.
type Weights struct {
	Latency     float64 // за мс RTT
	Loss        float64 // за процент потерь
	Jitter      float64 // за мс джиттера
	Hop         float64 // за каждый узел-ретранслятор
	Utilization float64 // за процент загрузки канала узла
	Degraded    float64 // штраф узлу или ребру в DEGRADED
}

func DefaultWeights() Weights {
	// Процент потерь весит как 25 мс RTT: для голоса потери заметнее задержки, и ребро
	// с 4% потерь (100 очков) хуже ребра с лишними 90 мс RTT.
	return Weights{Latency: 1, Loss: 25, Jitter: 2, Hop: 10, Utilization: 1, Degraded: 100}
}

// LinkCost — цена ребра; ok=false — ребро непригодно (выше критических порогов).
func LinkCost(m LinkMetrics, w Weights, th Thresholds) (cost float64, ok bool) {
	if m.Samples < th.MinSamples {
		// Неизмеренное ребро не запрещаем, но и лучшим не считаем: штраф как у
		// деградировавшего, пока не наберётся проб.
		return w.Hop + w.Degraded, true
	}
	if m.Loss >= th.LossCritical || m.Jitter >= th.JitterCritical || m.RTT >= th.LatencyCritical {
		return 0, false
	}
	cost = w.Latency*ms(m.RTT) + w.Loss*m.Loss*100 + w.Jitter*ms(m.Jitter)
	if m.Loss >= th.LossWarning || m.Jitter >= th.JitterWarning || m.RTT >= th.LatencyWarning {
		cost += w.Degraded
	}
	return cost, true
}

// NodeCost — цена провести звонок через узел; ok=false — узел не берёт (OFFLINE или
// канал выше критического порога).
func NodeCost(health Health, utilization float64, w Weights, th Thresholds) (cost float64, ok bool) {
	switch {
	case health == Offline:
		return 0, false
	case utilization >= th.BandwidthCritical:
		return 0, false
	}
	cost = w.Hop
	if utilization > 0 {
		cost += w.Utilization * utilization * 100
	}
	if health == Degraded || utilization >= th.BandwidthWarning {
		cost += w.Degraded
	}
	return cost, true
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
