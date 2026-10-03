package mesh

import (
	"sync"
	"time"
)

// LinkMetrics — качество ребра по замерам (§12).
type LinkMetrics struct {
	RTT     time.Duration `json:"rtt"`     // сглаженное (EWMA 1/8, как SRTT в TCP)
	Jitter  time.Duration `json:"jitter"`  // RFC 3550: EWMA 1/16 от |ΔRTT|
	Loss    float64       `json:"loss"`    // доля потерянных проб в окне, 0..1
	Uptime  time.Duration `json:"uptime"`  // сколько живёт связь
	Samples int           `json:"samples"` // сколько проб в окне: 0 — качество неизвестно
}

// Prober ведёт замеры одного ребра. Пробы отправляет вызывающий (Next), ответы
// приходят в Pong, не дождавшиеся ответа к Expire считаются потерянными.
type Prober struct {
	Window  int           // сколько последних проб учитывается в потерях
	Timeout time.Duration // сколько ждать ответа

	mu      sync.Mutex
	next    uint64
	pending map[uint64]time.Time
	results []bool // кольцо исходов проб: true — ответ пришёл
	pos     int
	filled  int
	srtt    time.Duration
	jitter  time.Duration
	lastRTT time.Duration
	haveRTT bool
	upSince time.Time
	now     func() time.Time
}

func NewProber(window int, timeout time.Duration) *Prober {
	if window <= 0 {
		window = 20
	}
	return &Prober{Window: window, Timeout: orDefault(timeout, 2*time.Second), pending: map[uint64]time.Time{},
		results: make([]bool, window), upSince: time.Now(), now: time.Now}
}

// Next — номер новой пробы; её время отправки запоминается.
func (p *Prober) Next() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireLocked()
	p.next++
	p.pending[p.next] = p.now()
	return p.next
}

// Pong — пришёл ответ на пробу n. Ответ на уже списанную или чужую пробу не
// учитывается: опоздавший ответ — это потеря, а не хороший RTT.
func (p *Prober) Pong(n uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireLocked() // сначала списать просроченные: иначе опоздавший ответ стал бы RTT
	sent, ok := p.pending[n]
	if !ok {
		return
	}
	delete(p.pending, n)
	rtt := p.now().Sub(sent)
	if !p.haveRTT {
		p.srtt, p.haveRTT = rtt, true
	} else {
		p.srtt += (rtt - p.srtt) / 8
		d := rtt - p.lastRTT
		if d < 0 {
			d = -d
		}
		p.jitter += (d - p.jitter) / 16
	}
	p.lastRTT = rtt
	p.record(true)
}

func (p *Prober) expireLocked() {
	now := p.now()
	for n, sent := range p.pending {
		if now.Sub(sent) > p.Timeout {
			delete(p.pending, n)
			p.record(false)
		}
	}
}

func (p *Prober) record(ok bool) {
	p.results[p.pos] = ok
	p.pos = (p.pos + 1) % len(p.results)
	if p.filled < len(p.results) {
		p.filled++
	}
}

// Metrics — текущие замеры.
func (p *Prober) Metrics() LinkMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireLocked()
	m := LinkMetrics{RTT: p.srtt, Jitter: p.jitter, Uptime: p.now().Sub(p.upSince), Samples: p.filled}
	if p.filled > 0 {
		lost := 0
		for i := 0; i < p.filled; i++ {
			if !p.results[i] {
				lost++
			}
		}
		m.Loss = float64(lost) / float64(p.filled)
	}
	return m
}
