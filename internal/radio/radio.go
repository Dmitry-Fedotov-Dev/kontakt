// Package radio — «Открытое радио»: любой может занять свободную частоту и вещать,
// любой может крутить ручку и слушать.
//
// Сервер звук НЕ кодирует и не смешивает: ведущий присылает уже готовый G.711 μ-law
// (8 кГц, 160 байт = 20 мс, итого 64 кбит/с), сервер раздаёт эти байты как есть всем,
// кто настроен на его частоту. Поэтому станция стоит копейки по процессору: одна
// копия кадра на слушателя, ни одного преобразования.
//
// Протокол (WebSocket):
//
//	/ws/host?f=1017&name=…   ведущий; двоичные сообщения — кадры μ-law, текстовые —
//	                         {"title":"…"} (что сейчас в эфире)
//	/ws/listen               слушатель; шлёт {"tune":1017} (0 — между станциями),
//	                         получает двоичные кадры станции и раз в секунду, если
//	                         что-то изменилось, текст {"stations":[…]}
//
// Частота хранится в десятых долях МГц: 1017 = 101.7 МГц, диапазон 87.5–108.0.
// Сервер не пишет в журнал ни адресов, ни содержимого эфира.
package radio

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"kontakt/internal/metrics"
)

const (
	MinFreq = 875
	MaxFreq = 1080

	FrameBytes = 160  // 20 мс μ-law при 8 кГц
	maxMessage = 2048 // больше в одном сообщении ведущему не нужно никогда
	// Ведущему разрешено 64 кбит/с с запасом на неровную отправку из браузера.
	bytesPerSec = 8000
	burstBytes  = 8000 // секунда запаса: вкладка браузера, бывает, замирает
	listenerBuf = 25   // 0,5 с кадров на слушателя; медленный теряет кадры, а не тормозит станцию
	nameRunes   = 24
	titleRunes  = 64
)

//go:embed static
var staticFS embed.FS

type Options struct {
	MaxStations  int
	MaxListeners int
	// PrivateMetrics убирает /metrics со страницы радио: тогда метрики отдаёт только
	// MetricsHandler на отдельном адресе (флаг -admin).
	PrivateMetrics bool
}

// MetricsHandler — /metrics для отдельного служебного адреса.
func (h *Hub) MetricsHandler() http.Handler { return h.reg.Handler() }

type Hub struct {
	opt     Options
	mu      sync.Mutex
	st      map[int]*station
	ls      map[*listener]struct{}
	version atomic.Uint64

	framesIn  *metrics.Counter
	framesOut *metrics.Counter
	bytesIn   *metrics.Counter
	bytesOut  *metrics.Counter
	dropped   *metrics.CounterVec
	rejected  *metrics.CounterVec
	hosts     *metrics.Counter
	sessions  *metrics.Counter
	tunes     *metrics.Counter
	shares    *metrics.CounterVec
	ogRenders *metrics.Counter
	onAir     *metrics.Histogram
	reg       *metrics.Registry
	upgrader  websocket.Upgrader
	og        ogCache
}

type station struct {
	freq        int
	name, title string
	since       time.Time
	subs        map[*listener]struct{}
}

type listener struct {
	tuned int
	out   chan []byte
}

// Station — то, что видят слушатели.
type Station struct {
	Freq      int    `json:"f"`
	Name      string `json:"n"`
	Title     string `json:"t,omitempty"`
	Listeners int    `json:"l"`
	OnAirSec  int64  `json:"s"`
}

func NewHub(opt Options) *Hub {
	if opt.MaxStations <= 0 {
		opt.MaxStations = 50
	}
	if opt.MaxListeners <= 0 {
		opt.MaxListeners = 500
	}
	h := &Hub{opt: opt, st: map[int]*station{}, ls: map[*listener]struct{}{}, reg: metrics.NewRegistry()}
	h.upgrader = websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	// Метрики — только счётчики и частоты: ни адресов, ни названий станций и треков (их
	// задают люди, и в метках они раздули бы число рядов без предела).
	h.framesIn = h.reg.Counter("kontakt_radio_frames_in_total", "Кадры μ-law (20 мс), принятые от ведущих")
	h.framesOut = h.reg.Counter("kontakt_radio_frames_out_total", "Кадры, поставленные в отправку слушателям")
	h.bytesIn = h.reg.Counter("kontakt_radio_bytes_in_total", "Байты эфира от ведущих")
	h.bytesOut = h.reg.Counter("kontakt_radio_bytes_out_total", "Байты эфира, поставленные в отправку слушателям")
	h.dropped = h.reg.CounterVec("kontakt_radio_frames_dropped_total",
		"Потерянные кадры: slow_listener — слушатель не успевает принимать, host_rate — ведущий шлёт больше 64 кбит/с",
		"reason", "slow_listener", "host_rate")
	h.rejected = h.reg.CounterVec("kontakt_radio_rejected_total",
		"Отказы: busy — волна занята, bad_freq — вне диапазона, full — нет места для станции, listeners_full — для приёмника",
		"reason", "busy", "bad_freq", "full", "listeners_full")
	h.hosts = h.reg.Counter("kontakt_radio_host_sessions_total", "Выходы станций в эфир")
	h.sessions = h.reg.Counter("kontakt_radio_listener_sessions_total", "Подключения приёмников")
	h.tunes = h.reg.Counter("kontakt_radio_tunes_total", "Перенастройки приёмников (ручка поймала или потеряла станцию)")
	h.shares = h.reg.CounterVec("kontakt_radio_share_views_total",
		"Открытия ссылок на волну: page — страница (люди и мессенджеры), image — картинка превью", "kind", "page", "image")
	h.ogRenders = h.reg.Counter("kontakt_radio_og_renders_total", "Нарисованные картинки превью (промахи кеша)")
	h.onAir = h.reg.Histogram("kontakt_radio_on_air_seconds", "Сколько станция пробыла в эфире",
		[]float64{10, 60, 300, 900, 1800, 3600, 7200, 14400, 43200})
	start := float64(time.Now().Unix())
	h.reg.Gauge("kontakt_radio_start_time_seconds", "Когда запущен сервер радио (видно перезапуски и обновления)",
		func() float64 { return start })
	h.reg.Gauge("kontakt_radio_listeners_tuned", "Приёмники, настроенные на станцию в эфире (остальные слушают шорох)", func() float64 {
		h.mu.Lock()
		defer h.mu.Unlock()
		n := 0
		for _, s := range h.st {
			n += len(s.subs)
		}
		return float64(n)
	})
	h.reg.GaugeVec("kontakt_radio_station_listeners", "Слушатели по волнам (только частота: имя станции задаёт человек)", "freq",
		func() map[string]float64 {
			h.mu.Lock()
			defer h.mu.Unlock()
			m := make(map[string]float64, len(h.st))
			for f, s := range h.st {
				m[FormatFreq(f)] = float64(len(s.subs))
			}
			return m
		})
	h.reg.Gauge("kontakt_radio_stations", "Станции в эфире", func() float64 {
		h.mu.Lock()
		defer h.mu.Unlock()
		return float64(len(h.st))
	})
	h.reg.Gauge("kontakt_radio_listeners", "Подключённые приёмники", func() float64 {
		h.mu.Lock()
		defer h.mu.Unlock()
		return float64(len(h.ls))
	})
	return h
}

// Handler — всё радио: страница, два WebSocket, список станций, метрики.
func (h *Hub) Handler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/host", h.serveHost)
	mux.HandleFunc("/ws/listen", h.serveListen)
	mux.HandleFunc("/api/stations", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.Stations())
	})
	if !h.opt.PrivateMetrics {
		mux.Handle("/metrics", h.reg.Handler())
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /{$}", h.serveIndex)
	mux.HandleFunc("GET /w/{freq}", h.serveShare)
	mux.HandleFunc("GET /og/{freq}", h.serveOG)
	mux.Handle("/", http.FileServer(http.FS(static)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=()")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func (h *Hub) Stations() []Station {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Station, 0, len(h.st))
	now := time.Now()
	for _, s := range h.st {
		out = append(out, Station{s.freq, s.name, s.title, len(s.subs), int64(now.Sub(s.since).Seconds())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Freq < out[j].Freq })
	return out
}

// ParseFreq понимает и «1017», и «101.7».
func ParseFreq(s string) (int, bool) {
	s = strings.TrimSpace(s)
	var f int
	if strings.Contains(s, ".") {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		f = int(v*10 + 0.5)
	} else {
		v, err := strconv.Atoi(s)
		if err != nil {
			return 0, false
		}
		f = v
	}
	return f, f >= MinFreq && f <= MaxFreq
}

func FormatFreq(f int) string { return strconv.Itoa(f/10) + "." + strconv.Itoa(f%10) }

// clean оставляет печатаемое и обрезает по числу символов: имя и название трека
// показываются всем, управляющие символы там не нужны никому.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if !unicode.IsPrint(r) {
			continue
		}
		if n == max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

func (h *Hub) changed() { h.version.Add(1) }

// ---------- ведущий ----------

func (h *Hub) serveHost(w http.ResponseWriter, r *http.Request) {
	f, ok := ParseFreq(r.URL.Query().Get("f"))
	if !ok {
		h.rejected.Inc("bad_freq")
		http.Error(w, "частота вне диапазона 87.5–108.0", http.StatusBadRequest)
		return
	}
	name := clean(r.URL.Query().Get("name"), nameRunes)
	if name == "" {
		name = "RADIO " + FormatFreq(f)
	}
	h.mu.Lock()
	switch {
	case h.st[f] != nil:
		h.mu.Unlock()
		h.rejected.Inc("busy")
		http.Error(w, "частота занята", http.StatusConflict)
		return
	case len(h.st) >= h.opt.MaxStations:
		h.mu.Unlock()
		h.rejected.Inc("full")
		http.Error(w, "в эфире нет места", http.StatusServiceUnavailable)
		return
	}
	s := &station{freq: f, name: name, since: time.Now(), subs: map[*listener]struct{}{}}
	h.st[f] = s // занимаем до апгрейда: двое не получат одну частоту
	h.mu.Unlock()

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.removeStation(s)
		return
	}
	defer conn.Close()
	defer h.removeStation(s)
	h.hosts.Inc()
	defer func() { h.onAir.Observe(time.Since(s.since).Seconds()) }()

	h.mu.Lock()
	for l := range h.ls { // кто уже стоял на этой частоте — сразу слышит
		if l.tuned == f {
			s.subs[l] = struct{}{}
		}
	}
	h.mu.Unlock()
	h.changed()

	conn.SetReadLimit(maxMessage)
	tokens, last := float64(burstBytes), time.Now()
	for {
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage {
			var m struct {
				Title *string `json:"title"`
			}
			if json.Unmarshal(data, &m) == nil && m.Title != nil {
				h.mu.Lock()
				s.title = clean(*m.Title, titleRunes)
				h.mu.Unlock()
				h.changed()
			}
			continue
		}
		// Ведро токенов: больше 64 кбит/с в эфир не уходит, что бы ни прислал браузер.
		now := time.Now()
		tokens += now.Sub(last).Seconds() * bytesPerSec
		last = now
		if tokens > burstBytes {
			tokens = burstBytes
		}
		if float64(len(data)) > tokens {
			h.dropped.Inc("host_rate")
			continue
		}
		tokens -= float64(len(data))
		h.framesIn.Add(uint64((len(data) + FrameBytes - 1) / FrameBytes))
		h.bytesIn.Add(uint64(len(data)))
		h.broadcast(s, data)
	}
}

func (h *Hub) broadcast(s *station, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for l := range s.subs {
		select {
		case l.out <- data: // data не меняется после отправки: одна копия на всех
			h.framesOut.Inc()
			h.bytesOut.Add(uint64(len(data)))
		default:
			h.dropped.Inc("slow_listener")
		}
	}
}

func (h *Hub) removeStation(s *station) {
	h.mu.Lock()
	if h.st[s.freq] == s {
		delete(h.st, s.freq)
	}
	h.mu.Unlock()
	h.changed()
}

// ---------- слушатель ----------

func (h *Hub) serveListen(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if len(h.ls) >= h.opt.MaxListeners {
		h.mu.Unlock()
		h.rejected.Inc("listeners_full")
		http.Error(w, "приёмников слишком много", http.StatusServiceUnavailable)
		return
	}
	h.mu.Unlock()
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	l := &listener{out: make(chan []byte, listenerBuf)}
	h.mu.Lock()
	h.ls[l] = struct{}{}
	h.sessions.Inc()
	h.mu.Unlock()
	h.changed()

	done := make(chan struct{})
	go h.writeListener(conn, l, done)

	conn.SetReadLimit(maxMessage)
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var m struct {
			Tune *int `json:"tune"`
		}
		if json.Unmarshal(data, &m) == nil && m.Tune != nil {
			h.tune(l, *m.Tune)
		}
	}
	h.mu.Lock()
	if s := h.st[l.tuned]; s != nil {
		delete(s.subs, l)
	}
	delete(h.ls, l)
	h.mu.Unlock()
	h.changed()
	close(done)
	conn.Close()
}

func (h *Hub) tune(l *listener, f int) {
	if f < MinFreq || f > MaxFreq {
		f = 0
	}
	h.mu.Lock()
	if l.tuned == f {
		h.mu.Unlock()
		return
	}
	if s := h.st[l.tuned]; s != nil {
		delete(s.subs, l)
	}
	l.tuned = f
	h.tunes.Inc()
	if s := h.st[f]; s != nil {
		s.subs[l] = struct{}{}
	}
	h.mu.Unlock()
	h.changed()
}

func (h *Hub) writeListener(conn *websocket.Conn, l *listener, done chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var seen uint64 = ^uint64(0)
	send := func(typ int, b []byte) bool {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteMessage(typ, b) == nil
	}
	sendList := func() bool {
		v := h.version.Load()
		if v == seen {
			return true
		}
		seen = v
		b, _ := json.Marshal(map[string]any{"stations": h.Stations()})
		return send(websocket.TextMessage, b)
	}
	if !sendList() {
		conn.Close()
		return
	}
	for {
		select {
		case <-done:
			return
		case b := <-l.out:
			if !send(websocket.BinaryMessage, b) {
				conn.Close() // разбудит читающий цикл, тот всё уберёт
				return
			}
		case <-tick.C:
			if !sendList() {
				conn.Close()
				return
			}
		}
	}
}
