package signal

import (
	"time"

	"kontakt/internal/metrics"
)

// stationMetrics — метрики станции (отдаются на админ-порту: /metrics).
type stationMetrics struct {
	reg       *metrics.Registry
	calls     *metrics.CounterVec
	hangups   *metrics.CounterVec
	pairs     *metrics.Counter
	reports   *metrics.CounterVec
	queueWait *metrics.Histogram
}

// Metrics — реестр метрик станции.
func (s *Server) Metrics() *metrics.Registry { return s.m.reg }

func (s *Server) initMetrics() {
	r := metrics.NewRegistry()
	s.m = &stationMetrics{
		reg: r,
		calls: r.CounterVec("kontakt_signal_calls_total",
			"Новые звонки (INVITE) по исходу: accepted — трубку сняли, остальное — причина отказа.", "result",
			"accepted", ReasonBanned, "maintenance", ReasonMediaError, "no-identity", "only-pcma-pcmu"),
		hangups: r.CounterVec("kontakt_signal_hangups_total",
			"Ноги, покинувшие станцию, по причине: user — сам положил трубку, иначе причина станции.", "reason",
			"user", ReasonBanned, ReasonYellow, ReasonMediaError),
		pairs: r.Counter("kontakt_pairs_total", "Соединённые пары."),
		reports: r.CounterVec("kontakt_signal_reports_total",
			"Жалобы по исходу: yellow, banned, noted — от новичка (в зачёт не пошла), остальное — не принята.", "result",
			"yellow", "banned", "noted", "already", "no_recent_call", "rate_limited"),
		queueWait: r.Histogram("kontakt_signal_queue_wait_seconds",
			"Сколько абонент провёл в очереди до соединения.",
			[]float64{0.25, 0.5, 1, 2, 3, 5, 10, 20, 30, 60, 120, 300}),
	}
	r.GaugeVec("kontakt_signal_legs", "Ноги по состоянию: setup, queued (в очереди, слышит гудки/шум), talking.", "state",
		func() map[string]float64 {
			out := map[string]float64{"setup": 0, "queued": 0, "talking": 0}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, l := range s.legs {
				l.mu.Lock()
				switch l.state {
				case legQueued:
					out["queued"]++
				case legTalking:
					out["talking"]++
				case legEnded:
				default:
					out["setup"]++
				}
				l.mu.Unlock()
			}
			return out
		})
	r.Gauge("kontakt_queue_length", "Длина очереди: сколько абонентов ждут пару.", func() float64 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return float64(len(s.queue))
	})
}

func (s *Server) observeWait(d time.Duration) { s.m.queueWait.Observe(d.Seconds()) }
