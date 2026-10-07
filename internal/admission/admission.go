// Package admission — допуск к конечным ресурсам (места приёмников радио, частоты, трубки рулетки):
// чтобы анонимный поток — наплыв людей или ботов — не занял всё, и свои проходили во время наплыва.
//
// Каждый ресурс — отдельные ворота (Gate, переборка): атака на один не трогает другие. У ворот:
//   - жёсткий предел Max, проверка и занятие места — под одной блокировкой (одновременный штурм
//     не проскочит);
//   - резерв Reserve: последние места — только своим (уровень ≥ Known: см. Level). Аноним получает
//     отказ, когда занято Max−Reserve;
//   - пределы на источник: мест на одну куку (PerID) и на один IP (PerIP);
//   - владельцу (Owner) — ещё OwnerExtra мест сверх Max: войти, даже когда всё занято.
//
// Установленные сессии не вытесняются никогда: ворота решают только про новые подключения.
//
// IP — только в памяти и только как хеш с солью, которая живёт до перезапуска процесса: ни в
// журнал, ни в метрики, ни на диск он не попадает (правило проекта: IP не пишем).
package admission

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Level — насколько мы знаем человека.
type Level int

const (
	Anon    Level = iota // новая кука
	Known                // модерация засчитала ему сессии (moderation: Trusted) и он не забанен
	Trusted              // назначен вручную в админке (например, ведущие станции)
	Owner                // владелец: сверх предела на OwnerExtra
)

// Reason — исход допуска (метка метрики).
type Reason string

const (
	Public  Reason = "public"  // пустили в общие места
	Reserve Reason = "reserve" // пустили в резерв (свой)
	Extra   Reason = "owner"   // владелец сверх предела
	PerID   Reason = "per_id"  // у куки уже PerID мест
	PerIP   Reason = "per_ip"  // у IP уже PerIP мест
	RateIP  Reason = "rate_ip" // с IP слишком часто новые подключения
	Full    Reason = "full"    // всё занято, а он не свой
)

// Results — все исходы, для объявления счётчиков с известными метками.
var Results = []string{string(Public), string(Reserve), string(Extra), string(PerID), string(PerIP), string(RateIP), string(Full)}

// Gate — ворота одного ресурса.
type Gate struct {
	Max, Reserve, OwnerExtra int
	PerID, PerIP             int // 0 — без предела

	mu       sync.Mutex
	used     int
	reserved int // из used — заняты своими сверх общих мест
	byID     map[string]int
	byIP     map[string]int
}

// Ticket — занятое место; Release отпускает его ровно один раз.
type Ticket struct {
	g        *Gate
	id, ip   string
	reserved bool
	once     sync.Once
}

func (t *Ticket) Release() {
	if t == nil {
		return
	}
	t.once.Do(func() { t.g.release(t) })
}

// Src — кто подключается: кука и источник (хеш IP; "" — не ограничивать по IP).
type Src struct {
	ID string
	IP string
}

// Acquire занимает место. level вызывается, только когда общие места кончились (ему можно ходить
// в базу модерации), и без блокировки ворот.
func (g *Gate) Acquire(s Src, level func() Level) (*Ticket, Reason) {
	g.mu.Lock()
	if g.byID == nil {
		g.byID, g.byIP = map[string]int{}, map[string]int{}
	}
	if g.PerID > 0 && s.ID != "" && g.byID[s.ID] >= g.PerID {
		g.mu.Unlock()
		return nil, PerID
	}
	if g.PerIP > 0 && s.IP != "" && g.byIP[s.IP] >= g.PerIP {
		g.mu.Unlock()
		return nil, PerIP
	}
	if g.used < g.Max-g.Reserve {
		t := g.takeLocked(s, false)
		g.mu.Unlock()
		return t, Public
	}
	g.mu.Unlock()

	lv := Anon
	if level != nil {
		lv = level()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.PerID > 0 && s.ID != "" && g.byID[s.ID] >= g.PerID: // пока ходили за уровнем, могли занять
		return nil, PerID
	case g.PerIP > 0 && s.IP != "" && g.byIP[s.IP] >= g.PerIP:
		return nil, PerIP
	case g.used < g.Max-g.Reserve:
		return g.takeLocked(s, false), Public
	case lv >= Known && g.used < g.Max:
		return g.takeLocked(s, true), Reserve
	case lv >= Owner && g.used < g.Max+g.OwnerExtra:
		return g.takeLocked(s, true), Extra
	}
	return nil, Full
}

func (g *Gate) takeLocked(s Src, reserved bool) *Ticket {
	g.used++
	if reserved {
		g.reserved++
	}
	if s.ID != "" {
		g.byID[s.ID]++
	}
	if s.IP != "" {
		g.byIP[s.IP]++
	}
	return &Ticket{g: g, id: s.ID, ip: s.IP, reserved: reserved}
}

func (g *Gate) release(t *Ticket) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used--
	if t.reserved {
		g.reserved--
	}
	dec := func(m map[string]int, k string) {
		if k == "" {
			return
		}
		if m[k] <= 1 {
			delete(m, k)
			return
		}
		m[k]--
	}
	dec(g.byID, t.id)
	dec(g.byIP, t.ip)
}

// Usage — занято всего и из них в резерве (для метрик).
func (g *Gate) Usage() (used, reserved int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used, g.reserved
}

// Rate — ведро новых подключений на ключ (хеш IP): не больше rate в секунду с запасом burst.
type Rate struct {
	PerSec, Burst float64

	mu    sync.Mutex
	m     map[string]*rateBucket
	swept time.Time
}

type rateBucket struct {
	tokens float64
	last   time.Time
}

// Allow — можно ли ещё одно подключение с этого ключа ("" — всегда можно).
func (r *Rate) Allow(key string) bool {
	if key == "" {
		return true
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*rateBucket{}
	}
	if now.Sub(r.swept) > time.Minute { // полные вёдра больше не нужны: память не растёт без предела
		for k, b := range r.m {
			if b.tokens+now.Sub(b.last).Seconds()*r.PerSec >= r.Burst {
				delete(r.m, k)
			}
		}
		r.swept = now
	}
	b := r.m[key]
	if b == nil {
		b = &rateBucket{tokens: r.Burst, last: now}
		r.m[key] = b
	}
	b.tokens = min(r.Burst, b.tokens+now.Sub(b.last).Seconds()*r.PerSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Sources превращает запрос в источник: IP берётся из X-Forwarded-For, только если соединение
// пришло с localhost (это Caddy или web); последний адрес в цепочке дописал наш прокси, раньше
// стоящие мог подставить кто угодно. Адреса из Exempt (localhost, сам сервер для нагрузочных
// замеров) по IP не ограничиваются.
type Sources struct {
	Exempt []*net.IPNet
	salt   []byte
	once   sync.Once
}

// ParseExempt — "127.0.0.0/8,::1/128,152.53.204.216" → сети.
func ParseExempt(list string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if strings.Contains(s, ":") {
				s += "/128"
			} else {
				s += "/32"
			}
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// DefaultExempt — localhost: локальный стенд, e2e и сам сервер.
var DefaultExempt = ParseExempt("127.0.0.0/8,::1")

// IP — хеш адреса клиента ("" — не ограничивать: адреса нет или он в Exempt).
func (s *Sources) IP(r *http.Request) string {
	s.once.Do(func() {
		s.salt = make([]byte, 16)
		rand.Read(s.salt)
	})
	ip := hostIP(r.RemoteAddr)
	if ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if p := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); p != nil {
				ip = p
			}
		}
	}
	if ip == nil {
		return ""
	}
	for _, n := range s.Exempt {
		if n.Contains(ip) {
			return ""
		}
	}
	sum := sha256.Sum256(append(append([]byte{}, s.salt...), ip...))
	return hex.EncodeToString(sum[:8])
}

func hostIP(addr string) net.IP {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	return net.ParseIP(h)
}
