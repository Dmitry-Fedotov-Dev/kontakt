package signal

// Входящий звонок. Трубка, лежащая на рычаге, слушает свою линию — WebSocket /api/ring?line=<комната>
// (комната — та же, что в её ссылке /?call=<комната>: ссылка и есть номер телефона). Звонит она в двух
// случаях:
//
//   - call — друг открыл ссылку и ждёт в комнате один: «ответить» соединяет с ним, «положить»
//     отказывает (его нога кончается с причиной declined);
//   - notify — оповещение POST /api/ring {"line","from","text"}: так звонят программы (cmd/kontakt-ring,
//     хуки Claude Code). Разговора нет, на дисплее — от кого и текст.
//
// Сообщения слушателю (JSON по тексту WebSocket):
//
//	{"ring":"call","id":"…"}                          — звонит друг
//	{"ring":"notify","id":"…","from":"…","text":"…"}  — оповещение
//	{"ring":"stop","id":"…","reason":"answered|gone|declined|timeout"}
//
// От слушателя: {"decline":"<id>"} — отказ; {"answer":"<id>"} — оповещение прочитано (у остальных
// устройств линии звонок смолкает). Звонок, начавшийся до подключения слушателя, приходит сразу.
//
// Приватность: ни линия, ни текст в журнал не пишутся; метрики — только счётчики по виду звонка.

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"kontakt/internal/identity"
	"kontakt/internal/netutil"
	"kontakt/internal/sip"
)

const (
	RingCall   = "call"
	RingNotify = "notify"

	ReasonDeclined = "declined" // вызываемый нажал «положить», пока у него звонило
	ReasonRinging  = "ringing"  // INFO ждущему в комнате: у хозяина линии звонит трубка

	maxRingListeners = 8  // устройств на одной линии (телефон, ПК, вкладки)
	maxRingPerID     = 4  // линий, которые слушает одна кука
	maxRingText      = 80 // знаков в тексте оповещения
	maxRingFrom      = 24
)

// RingOptions — тайминги звонка (полями экземпляра: тесты ставят короткие).
type RingOptions struct {
	CallFor    time.Duration // сколько звенит друг, ждущий в комнате (дальше — «пропущенный»)
	NotifyFor  time.Duration // сколько звенит оповещение
	NotifyGap  time.Duration // не чаще одного оповещения на линию за этот срок
	NotifyHour int           // и не больше стольких за час
}

func DefaultRingOptions() RingOptions {
	return RingOptions{CallFor: 60 * time.Second, NotifyFor: 30 * time.Second, NotifyGap: 2 * time.Second, NotifyHour: 120}
}

type ringListener struct {
	id   string // кука
	out  chan []byte
	once sync.Once
}

func (l *ringListener) send(b []byte) {
	select {
	case l.out <- b:
	default: // медленный слушатель: звонок важнее очереди, старое не копим
	}
}

type ringing struct {
	id, kind, from, text string
	caller               string // кука звонящего (call): у него самого не звеним
	leg                  *Leg
	timer                *time.Timer
}

func (r *ringing) msg() []byte {
	m := map[string]string{"ring": r.kind, "id": r.id}
	if r.kind == RingNotify {
		m["from"], m["text"] = r.from, r.text
	}
	b, _ := json.Marshal(m)
	return b
}

type ringHub struct {
	mu      sync.Mutex
	lines   map[string]map[*ringListener]bool
	perID   map[string]int
	active  map[string]*ringing
	notifed map[string][]time.Time // линия → времена оповещений за последний час
}

func newRingHub() *ringHub {
	return &ringHub{lines: map[string]map[*ringListener]bool{}, perID: map[string]int{},
		active: map[string]*ringing{}, notifed: map[string][]time.Time{}}
}

// listeners — сколько устройств слушают линию, не считая куки except.
func (h *ringHub) listeners(line, except string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for l := range h.lines[line] {
		if l.id != except {
			n++
		}
	}
	return n
}

// broadcastLocked — всем слушателям линии, кроме куки звонящего.
func (h *ringHub) broadcastLocked(line string, r *ringing, b []byte) {
	for l := range h.lines[line] {
		if r == nil || l.id != r.caller {
			l.send(b)
		}
	}
}

// start ставит звонок на линию (прежний смолкает) и через d снимает его сам.
func (h *ringHub) start(line string, r *ringing, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.active[line]; old != nil {
		h.stopLocked(line, old, "gone")
	}
	h.active[line] = r
	r.timer = time.AfterFunc(d, func() { h.stop(line, r.id, "timeout") })
	h.broadcastLocked(line, r, r.msg())
}

// stop снимает звонок id с линии ("" — любой текущий); возвращает снятый.
func (h *ringHub) stop(line, id, reason string) *ringing {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.active[line]
	if r == nil || (id != "" && r.id != id) {
		return nil
	}
	h.stopLocked(line, r, reason)
	return r
}

func (h *ringHub) stopLocked(line string, r *ringing, reason string) {
	delete(h.active, line)
	if r.timer != nil {
		r.timer.Stop()
	}
	b, _ := json.Marshal(map[string]string{"ring": "stop", "id": r.id, "reason": reason})
	h.broadcastLocked(line, r, b)
}

// stopLeg — нога ушла из комнаты или соединилась: если звонили из-за неё, звонок смолкает.
func (h *ringHub) stopLeg(l *Leg, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r := h.active[l.room]; r != nil && r.leg == l {
		h.stopLocked(l.room, r, reason)
	}
}

// allowNotify — предел оповещений на линию: не чаще NotifyGap и не больше NotifyHour за час.
func (h *ringHub) allowNotify(line string, o RingOptions, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := h.notifed[line]
	keep := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	if len(keep) > 0 && now.Sub(keep[len(keep)-1]) < o.NotifyGap || len(keep) >= o.NotifyHour {
		h.notifed[line] = keep
		return false
	}
	h.notifed[line] = append(keep, now)
	return true
}

func (h *ringHub) add(line string, l *ringListener) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines[line]) >= maxRingListeners || h.perID[l.id] >= maxRingPerID {
		return false
	}
	if h.lines[line] == nil {
		h.lines[line] = map[*ringListener]bool{}
	}
	h.lines[line][l] = true
	h.perID[l.id]++
	if r := h.active[line]; r != nil && r.caller != l.id {
		l.send(r.msg()) // звонок уже идёт — трубка, открытая посреди него, звонит сразу
	}
	return true
}

func (h *ringHub) remove(line string, l *ringListener) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.lines[line], l)
	if len(h.lines[line]) == 0 {
		delete(h.lines, line)
	}
	if h.perID[l.id] <= 1 {
		delete(h.perID, l.id)
	} else {
		h.perID[l.id]--
	}
}

func (h *ringHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, ls := range h.lines {
		n += len(ls)
	}
	return n
}

// ---------- связь с комнатами ----------

// ringRoom — нога одна в комнате: звоним хозяину линии (если его трубка слушает). Ждущему —
// INFO «ringing», трубка покажет «звоним» вместо ссылки.
func (s *Server) ringRoom(l *Leg) {
	if l.room == "" || s.ring.listeners(l.room, l.identity) == 0 {
		return
	}
	l.mu.Lock()
	alone := l.state == legQueued && l.peer == nil
	l.mu.Unlock()
	if !alone {
		return
	}
	s.m.rings.Inc(RingCall)
	s.ring.start(l.room, &ringing{id: sip.RandHex(6), kind: RingCall, caller: l.identity, leg: l}, s.Ring.CallFor)
	l.mu.Lock()
	alone = l.state == legQueued && l.peer == nil
	l.mu.Unlock()
	if !alone { // соединился или ушёл, пока ставили звонок, — stopLeg мог пройти раньше start
		s.ring.stopLeg(l, "answered")
		return
	}
	s.notify(l, ReasonRinging)
}

// Notify — оповещение на линию; возвращает, сколько устройств его услышат (0 — никто не слушает).
// ok == false — предел частоты.
func (s *Server) Notify(line, from, text string) (listeners int, ok bool) {
	if !s.ring.allowNotify(line, s.Ring, time.Now()) {
		return 0, false
	}
	n := s.ring.listeners(line, "")
	if n > 0 {
		s.m.rings.Inc(RingNotify)
		s.ring.start(line, &ringing{id: sip.RandHex(6), kind: RingNotify, from: from, text: text}, s.Ring.NotifyFor)
	}
	return n, true
}

// clean — одна строка без управляющих знаков, не длиннее max рун.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// ---------- HTTP ----------

var ringUpgrader = websocket.Upgrader{CheckOrigin: netutil.SameSite}

// ServeRing — GET (WebSocket): трубка слушает линию; POST: оповещение.
func (s *Server) ServeRing(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.serveNotify(w, r)
		return
	}
	line := strings.ToLower(r.URL.Query().Get("line"))
	if !validRoom(line) {
		http.Error(w, "line: 8–32 знака a-z0-9", http.StatusBadRequest)
		return
	}
	id := identity.FromRequest(r)
	if id == "" {
		http.Error(w, "нет куки — откройте страницу заново", http.StatusUnauthorized)
		return
	}
	conn, err := ringUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	l := &ringListener{id: id, out: make(chan []byte, 8)}
	if !s.ring.add(line, l) {
		conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4029, "too many"), time.Now().Add(time.Second))
		conn.Close()
		return
	}
	done := make(chan struct{})
	defer func() {
		s.ring.remove(line, l)
		close(done)
		conn.Close()
	}()
	go func() { // писатель: звонки и пинг раз в 25 с (прокси и мобильные сети рвут молчащие сокеты)
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-done:
				return
			case b := <-l.out:
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if conn.WriteMessage(websocket.TextMessage, b) != nil {
					conn.Close()
					return
				}
			case <-ping.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	conn.SetReadLimit(1024)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m struct{ Decline, Answer string }
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch {
		case m.Decline != "":
			if rg := s.ring.stop(line, m.Decline, "declined"); rg != nil && rg.leg != nil {
				s.End(rg.leg, ReasonDeclined)
			}
		case m.Answer != "":
			s.ring.stop(line, m.Answer, "answered")
		}
	}
}

type notifyReq struct {
	Line string `json:"line"`
	From string `json:"from"`
	Text string `json:"text"`
}

// serveNotify — POST /api/ring: JSON {"line","from","text"} или форма с теми же полями. Линия может
// быть и целой ссылкой https://…/?call=<линия>. Ответ: {"listeners": n} — сколько устройств звонят.
func (s *Server) serveNotify(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req notifyReq
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "JSON: {\"line\",\"from\",\"text\"}", http.StatusBadRequest)
			return
		}
	} else {
		req = notifyReq{Line: r.FormValue("line"), From: r.FormValue("from"), Text: r.FormValue("text")}
	}
	line := LineOf(req.Line)
	if !validRoom(line) {
		http.Error(w, "line: 8–32 знака a-z0-9 или ссылка /?call=…", http.StatusBadRequest)
		return
	}
	from := clean(req.From, maxRingFrom)
	if from == "" {
		from = "Kontakt"
	}
	n, ok := s.Notify(line, from, clean(req.Text, maxRingText))
	if !ok {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "слишком часто", http.StatusTooManyRequests)
		return
	}
	writeJSON(w, map[string]int{"listeners": n})
}

// LineOf — линия из имени комнаты или из ссылки на звонок (…?call=<линия>).
func LineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "call="); i >= 0 {
		s = s[i+len("call="):]
		if j := strings.IndexAny(s, "&#"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.ToLower(s)
}
