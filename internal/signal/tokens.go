package signal

import (
	"sync"
	"time"
)

// Жетоны таксофона. Человек (кука) начинает с полного запаса; жетон уходит, когда он кладёт
// трубку посреди разговора, и возвращается по одному раз в refill, не больше запаса. Хранятся
// только неполные кошельки и только в памяти: перезапуск станции дарит всем полный запас.
type tokenBank struct {
	mu sync.Mutex
	m  map[string]*wallet
}

type wallet struct {
	n    int
	next time.Time // когда придёт следующий жетон
}

// settle — начислить пришедшие жетоны; полный кошелёк забывается (полный — это «нет записи»).
func (b *tokenBank) settle(id string, max int, refill time.Duration, now time.Time) (n int, next time.Time) {
	w := b.m[id]
	if w == nil {
		return max, time.Time{}
	}
	for w.n < max && !now.Before(w.next) {
		w.n++
		w.next = w.next.Add(refill)
	}
	if w.n >= max {
		delete(b.m, id)
		return max, time.Time{}
	}
	return w.n, w.next
}

// State — сколько жетонов и через сколько придёт следующий (0 — запас полон).
func (b *tokenBank) State(id string, max int, refill time.Duration) (int, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	n, next := b.settle(id, max, refill, now)
	if next.IsZero() {
		return n, 0
	}
	return n, next.Sub(now)
}

// Spend — опустить жетон; false — их нет.
func (b *tokenBank) Spend(id string, max int, refill time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	n, next := b.settle(id, max, refill, now)
	if n == 0 {
		return false
	}
	if b.m == nil {
		b.m = map[string]*wallet{}
	}
	if next.IsZero() {
		next = now.Add(refill) // отсчёт пошёл с первого потраченного
	}
	b.m[id] = &wallet{n: n - 1, next: next}
	return true
}

// tokenRules — запас и время возврата из горячего конфига; ok = false — жетоны выключены или
// звонят не из веб-трубки (SIP-телефоны стенда и xk6-sip не считаются).
func (s *Server) tokenRules(tr Transport) (max int, refill time.Duration, ok bool) {
	c := s.Cfg.Get()
	if _, web := tr.(*wsTransport); !web || c.Tokens <= 0 {
		return 0, 0, false
	}
	return c.Tokens, time.Duration(c.TokenRefillSec) * time.Second, true
}

// Tokens — жетоны человека; для выключенных жетонов — (-1, 0).
func (s *Server) Tokens(tr Transport, id string) (int, time.Duration) {
	max, refill, ok := s.tokenRules(tr)
	if !ok {
		return -1, 0
	}
	return s.tokens.State(id, max, refill)
}

func (s *Server) spendToken(l *Leg) {
	if max, refill, ok := s.tokenRules(l.tr); ok && s.tokens.Spend(l.identity, max, refill) {
		s.m.tokens.Inc()
	}
}
