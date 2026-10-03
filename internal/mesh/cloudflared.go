package mesh

import (
	"bufio"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CloudflaredMetrics — что берём из /metrics cloudflared (§19). Ёмкость туннеля из
// числа Мбит/с не выводится: смотрим на реальные сессии и соединения.
var CloudflaredMetrics = []string{
	"cloudflared_tcp_total_sessions",
	"cloudflared_tcp_active_sessions",
	"cloudflared_udp_total_sessions",
	"cloudflared_udp_active_sessions",
	"cloudflared_tunnel_ha_connections",
	"cloudflared_tunnel_total_requests",
	"cloudflared_tunnel_request_errors",
	"cloudflared_tunnel_concurrent_requests_per_tunnel",
}

// Cloudflared периодически читает /metrics cloudflared. Нет cloudflared — пустая карта,
// и узел работает как обычно: туннель — вариант развёртывания, не требование (§2).
type Cloudflared struct {
	URL string

	mu   sync.Mutex
	vals map[string]float64
	ok   bool
}

// Poll читает метрики один раз; значения с разными метками суммируются.
func (c *Cloudflared) Poll() {
	cl := http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(c.URL)
	if err != nil {
		c.set(nil, false)
		return
	}
	defer resp.Body.Close()
	want := map[string]bool{}
	for _, m := range CloudflaredMetrics {
		want[m] = true
	}
	vals := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i > 0 {
			name = line[:i]
		}
		if !want[name] {
			continue
		}
		// значение — после меток; у строки без меток «}» нет, и брать надо то, что
		// после имени (иначе в значение попадало имя, и метрика терялась молча)
		rest := line[len(name):]
		if i := strings.LastIndex(rest, "}"); i >= 0 {
			rest = rest[i+1:]
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			vals[name] += v
		}
	}
	c.set(vals, resp.StatusCode == http.StatusOK)
}

func (c *Cloudflared) set(v map[string]float64, ok bool) {
	c.mu.Lock()
	c.vals, c.ok = v, ok
	c.mu.Unlock()
}

// Values — последние прочитанные значения; ok=false — cloudflared не ответил.
func (c *Cloudflared) Values() (map[string]float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]float64, len(c.vals))
	for k, v := range c.vals {
		out[k] = v
	}
	return out, c.ok
}
