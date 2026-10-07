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
//	                         {"title":"…"} (что сейчас в эфире), {"logo":"<base64 PNG 64×64>"|""}
//	                         (логотип станции, logos.go; ответ {"logo":хеш}); получает {"listeners":N},
//	                         когда число слушателей изменилось (не чаще раза в 2 с)
//	/ws/listen               слушатель; шлёт {"tune":1017} (0 — между станциями),
//	                         получает двоичные кадры станции, при подключении и когда станция
//	                         вышла в эфир или ушла — {"stations":[{f,n,t,g}…]} (g — хеш логотипа,
//	                         картинка — /logo/{g}.png), при смене трека —
//	                         только {"title":{"f":1017,"t":"…"}}. Числа слушателей в списке нет:
//	                         иначе каждый поворот ручки рассылал бы список всем;
//	                         {"report":true} — жалоба на станцию, на которой стоит,
//	                         ответ {"report":"yellow"|"banned"|…}; {"like":true} — лайк ей же
//	                         (не чаще likeEvery от слушателя, своей станции — нет), ведущий
//	                         получает пачкой {"likes":N,"total":T} не чаще раза в likeFlush
//
// Модерация (Options.Mod) общая с рулеткой: человек — кука kontakt_id, бан эфира — зона air.
// Забаненный в эфир не выходит (сокет закрывается с кодом 4003), на жёлтую карточку ведущий
// получает {"card":"yellow"}. Слушать может любой.
//
// Частота хранится в десятых долях МГц: 1017 = 101.7 МГц, диапазон 87.5–108.0.
// Сервер не пишет в журнал ни адресов, ни содержимого эфира.
package radio

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"log"

	"kontakt/internal/identity"
	"kontakt/internal/metrics"
	"kontakt/internal/moderation"
)

const (
	MinFreq = 875
	MaxFreq = 1080

	FrameBytes = 160  // 20 мс μ-law при 8 кГц
	maxMessage = 2048 // больше в одном сообщении не нужно никогда — кроме логотипа ведущего
	// maxHostText — текст от ведущего: логотип в base64 (PNG до logoMaxBytes) с запасом на JSON
	maxHostText = logoMaxBytes*2*4/3 + 512
	// Ведущему разрешено 64 кбит/с + 10 %: браузер считает кадры по часам звуковой карты, а они
	// расходятся с часами сервера на проценты (на стенде страница слала 50,4–51,2 кадра/с). Без
	// запаса ведро всё время пусто и режет каждый лишний кадр. Подушка 3 с: после рывка сети
	// браузер досылает накопленное (до 2 с) пачкой, и её лучше донести — запас слушателя поглотит.
	// С 5 % и 2 с подушка после пачки восполнялась ~48 с, и следующая пачка резалась (секунда звука).
	bytesPerSec = 8800
	burstBytes  = 24000
	// 3 с кадров на слушателя: замерла связь слушателя (мобильный интернет) — звук дойдёт позже,
	// а не потеряется (было 0,5 с — на стенде терялось по 0,2–0,5 с каждые несколько минут).
	// Медленный всё равно теряет кадры сверх этого, а не тормозит станцию. 150 × 160 Б × 500 = 12 МБ.
	listenerBuf = 150
	// звук слушателю — пачками: batchFrames кадров (100 мс) в одном сообщении, см. writeListener
	batchFrames = 5
	nameRunes   = 24
	titleRunes  = 64

	CloseBanned    = 4003 // код закрытия сокета ведущего: эфир для него закрыт
	reportsPerHour = 10   // жалоб от одного человека в час, больше — rate_limited
	// Лайки: от слушателя не чаще likeEvery (больше — молча не считаются), ведущему — пачкой
	// раз в likeFlush: сотня слушателей, жмущих сердце, — 4 коротких сообщения в секунду, а не сотни.
	likeEvery  = 300 * time.Millisecond
	likeFlush  = 250 * time.Millisecond
	banRecheck = 30 * time.Second
)

//go:embed static
var staticFS embed.FS

type Options struct {
	MaxStations  int
	MaxListeners int
	// PrivateMetrics убирает /metrics со страницы радио: тогда метрики отдаёт только
	// MetricsHandler на отдельном адресе (флаг -admin).
	PrivateMetrics bool
	// Mod — модерация; nil — без неё (и без куки): эфир открыт всем, жаловаться некуда.
	Mod moderation.Moderator
	// ReportAfter — сколько надо простоять на станции, чтобы на неё пожаловаться (не на
	// ходу ручкой); CountAfter — прослушивание или эфир такой длины засчитывается как сессия.
	ReportAfter, CountAfter time.Duration
	// LinksPath — где хранить короткие ссылки /r/…; пусто — только в памяти.
	LinksPath string
	// DirectPort — порт прямого HTTPS для сокетов эфира (direct.go); 0 — его нет.
	DirectPort int
	// LogosDir — где хранить логотипы станций (logos.go); пусто — только в памяти.
	LogosDir string
	// HostLogos — ведущий ставит логотип сам ({"logo":…} и ячейка на вкладке «Вещать»). Выключено —
	// логотипы ставит только модератор через админ-порт (AdminHandler).
	HostLogos bool
}

// MetricsHandler — /metrics для отдельного служебного адреса.
func (h *Hub) MetricsHandler() http.Handler { return h.reg.Handler() }

type Hub struct {
	opt     Options
	mu      sync.Mutex
	st      map[int]*station
	ls      map[*listener]struct{}
	version atomic.Uint64 // меняется — полный список всем приёмникам
	content atomic.Uint64 // меняется и при смене трека — только ключ кеша полного списка

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
	reports   *metrics.CounterVec
	kicked    *metrics.Counter
	likes     *metrics.Counter

	letterTimes   map[string]time.Time // под mu: когда человек последний раз писал ведущему
	letterSeq     uint64               // под mu: номер последнего письма
	lettersSent   *metrics.CounterVec
	letterReports *metrics.CounterVec
	ogRenders     *metrics.Counter
	onAir         *metrics.Histogram
	reg           *metrics.Registry
	upgrader      websocket.Upgrader
	og            ogCache

	reportTimes map[string][]time.Time // под mu: кто сколько жаловался за час

	links     *shortLinks
	shortRate *bucket

	listMu    sync.Mutex // кеш списка станций для сокета: один JSON на версию, а не на слушателя
	listVer   uint64     // content, для которого собран listJSON
	listJSON  []byte
	listBytes *metrics.Counter

	logos       *logoStore
	logoChanges *metrics.CounterVec
	waves       *waveMemo // последнее название и логотип каждой волны (waves.go)
}

type station struct {
	freq        int
	name, title string
	logo        string // под Hub.mu: хеш логотипа, "" — нет
	since       time.Time
	subs        map[*listener]struct{}

	host     string          // кука ведущего ("" без модерации)
	conn     *websocket.Conn // чтобы снять с эфира
	wmu      sync.Mutex      // текст ведущему пишут жалобы из разных горутин
	reported map[string]bool // под Hub.mu: кто уже жаловался на этот эфир
	likes    int             // под Hub.mu: лайков ещё не отправлено ведущему
	likesAll int             // под Hub.mu: всего за этот эфир

	letterFrom map[uint64]string // под Hub.mu: номер письма → кто отправил (ведущему не отдаётся)
	blocked    map[string]bool   // под Hub.mu: от кого ведущий писем не принимает
}

type listener struct {
	id    string
	tuned int
	out   chan []byte
	ctl   chan []byte // текстовые ответы (жалоба) — пишет только writeListener

	heard      *station // станция, которую слышит, и с какого момента (под Hub.mu)
	heardSince time.Time
	counted    bool // прослушивание уже засчитано
	lastLike   time.Time
	stale      bool // под Hub.mu: не влезло сообщение о треке — при следующем тике полный список
}

// Station — то, что видят слушатели.
type Station struct {
	Freq      int    `json:"f"`
	Name      string `json:"n"`
	Title     string `json:"t,omitempty"`
	Logo      string `json:"g,omitempty"`
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
	if opt.ReportAfter <= 0 {
		opt.ReportAfter = 10 * time.Second
	}
	if opt.CountAfter <= 0 {
		opt.CountAfter = 5 * time.Minute
	}
	h := &Hub{opt: opt, st: map[int]*station{}, ls: map[*listener]struct{}{}, reg: metrics.NewRegistry(),
		reportTimes: map[string][]time.Time{}, letterTimes: map[string]time.Time{}}
	h.upgrader = websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024, CheckOrigin: sameSite}
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
		"Отказы: busy — волна занята, bad_freq — вне диапазона, full — нет места для станции, listeners_full — для приёмника, "+
			"banned — эфир закрыт модерацией, no_identity — нет куки",
		"reason", "busy", "bad_freq", "full", "listeners_full", "banned", "no_identity")
	h.reports = h.reg.CounterVec("kontakt_radio_reports_total",
		"Жалобы на станции по исходу: yellow, banned, noted — от новичка (в зачёт не пошла), остальное — не принята",
		"result", "yellow", "banned", "noted", "already", "no_station", "listen_more", "self", "rate_limited", "no_identity", "error")
	h.kicked = h.reg.Counter("kontakt_radio_hosts_banned_total", "Станции, снятые с эфира баном")
	h.likes = h.reg.Counter("kontakt_radio_likes_total", "Лайки станциям от слушателей (принятые)")
	h.lettersSent = h.reg.CounterVec("kontakt_radio_letters_total",
		"Письма ведущему по исходу: sent — передано, too_fast — чаще раза в 30 с, banned, no_station, self, empty",
		"result", "sent", "too_fast", "banned", "no_station", "self", "empty")
	h.letterReports = h.reg.CounterVec("kontakt_radio_letter_reports_total",
		"Жалобы ведущих на письма по исходу модерации", "result", "yellow", "banned", "noted", "already", "rate_limited", "error")
	h.listBytes = h.reg.Counter("kontakt_radio_list_bytes_total",
		"Байты списков станций, отправленные приёмникам (служебный трафик сверх звука)")
	h.hosts = h.reg.Counter("kontakt_radio_host_sessions_total", "Выходы станций в эфир")
	h.sessions = h.reg.Counter("kontakt_radio_listener_sessions_total", "Подключения приёмников")
	h.tunes = h.reg.Counter("kontakt_radio_tunes_total", "Перенастройки приёмников (ручка поймала или потеряла станцию)")
	h.shares = h.reg.CounterVec("kontakt_radio_share_views_total",
		"Открытия ссылок на волну: page — страница (люди и мессенджеры), short — по короткой ссылке /r/, image — картинка превью",
		"kind", "page", "short", "image")
	var err error
	if h.links, err = openShortLinks(opt.LinksPath, 50000); err != nil {
		log.Printf("короткие ссылки: %v — начинаю с пустого списка", err)
	}
	if h.logos, err = openLogos(opt.LogosDir, 5000); err != nil {
		log.Printf("логотипы: %v — храню только в памяти", err)
		h.logos.dir = ""
	}
	h.waves = openWaves(h.logos.dir)
	h.logoChanges = h.reg.CounterVec("kontakt_radio_logo_changes_total",
		"Логотипы станций от ведущих: set — поставлен, removed — убран, bad — не PNG 64×64", "result", "set", "removed", "bad")
	h.shortRate = &bucket{rate: 20, burst: 40, tokens: 40, last: time.Now()} // защита диска от потока POST
	h.reg.Gauge("kontakt_radio_short_links", "Сохранённые короткие ссылки на волну", func() float64 { return float64(h.links.Len()) })
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
	// Те же сокеты под /radio/: Caddy и туннель отдают их радио напрямую, мимо web. Через web
	// каждый кадр шёл ещё через один прокси: на 500 слушателях это 0,48 ядра и ~90 КБ памяти
	// на соединение (замер 05.10.2026 на проде, LOAD_REPORT.md).
	mux.HandleFunc("/radio/ws/host", h.serveHost)
	mux.HandleFunc("/radio/ws/listen", h.serveListen)
	mux.HandleFunc("/api/stations", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.Stations())
	})
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"moderation": h.opt.Mod != nil, "banned": false, "cards": 0}
		if h.opt.Mod != nil {
			st := h.opt.Mod.Status(identity.FromRequest(r), moderation.Air)
			out["banned"], out["cards"] = st.Banned, st.Cards
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(out)
	})
	if !h.opt.PrivateMetrics {
		mux.Handle("/metrics", h.reg.Handler())
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /{$}", h.serveIndex)
	mux.HandleFunc("GET /w/{freq}", h.serveShare)
	mux.HandleFunc("GET /w/{freq}/{code}", h.serveShareCode)
	mux.HandleFunc("GET /r/{code}", h.serveShort)
	mux.HandleFunc("POST /api/short", h.serveShortNew)
	mux.HandleFunc("GET /og/{freq}", h.serveOG)
	mux.HandleFunc("GET /logo/{file}", h.serveLogo)
	mux.Handle("/", http.FileServer(http.FS(static)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.opt.Mod != nil { // за web кука уже есть; радио, запущенное отдельно, ставит её само
			identity.Ensure(w, r)
		}
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
		out = append(out, Station{s.freq, s.name, s.title, s.logo, len(s.subs), int64(now.Sub(s.since).Seconds())})
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

// changed — изменился список станций (вышла в эфир, ушла, сменила трек). Число слушателей сюда
// не относится: его получает только ведущий (hostListeners).
func (h *Hub) changed() {
	h.version.Add(1)
	h.content.Add(1)
}

// titleChanged — ведущий сменил трек: всем приёмникам короткое {"title":…} вместо полного списка
// (трек меняется чаще всего: 50 станций — раз в несколько секунд). Кому не влезло в очередь,
// получит полный список на следующем тике.
func (h *Hub) titleChanged(s *station, title string) {
	b, _ := json.Marshal(map[string]any{"title": map[string]any{"f": s.freq, "t": title}})
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.title == title {
		return
	}
	s.title = title
	h.content.Add(1) // в кеше полного списка — старый трек
	if h.st[s.freq] != s {
		return
	}
	for l := range h.ls {
		select {
		case l.ctl <- b:
			h.listBytes.Add(uint64(len(b)))
		default:
			l.stale = true
		}
	}
}

// listMessage — {"stations":[…]} текущей версии; кодируется один раз на версию, все приёмники
// получают одни и те же байты. Замер до этого: 500 приёмников × 50 станций — 27 Мбит на каждое
// изменение, а менялось от любого поворота ручки.
func (h *Hub) listMessage() (uint64, []byte) {
	h.listMu.Lock()
	defer h.listMu.Unlock()
	v, c := h.version.Load(), h.content.Load()
	if h.listJSON != nil && h.listVer == c {
		return v, h.listJSON
	}
	type wire struct {
		Freq  int    `json:"f"`
		Name  string `json:"n"`
		Title string `json:"t,omitempty"`
		Logo  string `json:"g,omitempty"`
	}
	st := h.Stations()
	out := make([]wire, len(st))
	for i, x := range st {
		out[i] = wire{x.Freq, x.Name, x.Title, x.Logo}
	}
	h.listJSON, _ = json.Marshal(map[string]any{"stations": out})
	h.listVer = c
	return v, h.listJSON
}

// hostListeners шлёт ведущему число его слушателей, когда оно изменилось, и лайки пачкой.
func (h *Hub) hostListeners(s *station, done <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	lt := time.NewTicker(likeFlush)
	defer lt.Stop()
	last := -1
	for {
		select {
		case <-done:
			return
		case <-lt.C:
			h.mu.Lock()
			n, all := s.likes, s.likesAll
			s.likes = 0
			h.mu.Unlock()
			if n > 0 {
				s.sayHost(map[string]int{"likes": n, "total": all})
			}
		case <-t.C:
			h.mu.Lock()
			n := len(s.subs)
			h.mu.Unlock()
			if n != last {
				last = n
				s.sayHost(map[string]int{"listeners": n})
			}
		}
	}
}

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
	host := identity.FromRequest(r)
	if h.opt.Mod != nil {
		if host == "" {
			h.rejected.Inc("no_identity")
			http.Error(w, "нет куки kontakt_id: откройте страницу радио", http.StatusForbidden)
			return
		}
		if h.opt.Mod.Status(host, moderation.Air).Banned {
			h.rejected.Inc("banned")
			// Код ответа на апгрейд браузер не показывает — отказываем кодом закрытия сокета.
			if conn, err := h.upgrader.Upgrade(w, r, nil); err == nil {
				closeBanned(conn)
			}
			return
		}
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
	s := &station{freq: f, name: name, since: time.Now(), subs: map[*listener]struct{}{}, host: host, reported: map[string]bool{},
		letterFrom: map[uint64]string{}, blocked: map[string]bool{}}
	if pin := h.logos.Pinned(host); pin != "" { // логотип, закреплённый модератором; свой — заменит
		if _, ok := h.logos.Get(pin); ok {
			s.logo = pin
		}
	}
	h.st[f] = s // занимаем до апгрейда: двое не получат одну частоту
	h.mu.Unlock()

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.removeStation(s)
		return
	}
	s.wmu.Lock()
	s.conn = conn
	s.wmu.Unlock()
	defer conn.Close()
	hostDone := make(chan struct{})
	defer close(hostDone)
	go h.hostListeners(s, hostDone)
	defer h.removeStation(s)
	h.hosts.Inc()
	defer func() { h.onAir.Observe(time.Since(s.since).Seconds()) }()

	h.mu.Lock()
	for l := range h.ls { // кто уже стоял на этой частоте — сразу слышит
		if l.tuned == f {
			s.subs[l] = struct{}{}
			l.heard, l.heardSince = s, time.Now()
		}
	}
	logo := s.logo
	h.mu.Unlock()
	h.changed()
	h.waves.Seen(f, name, logo)

	conn.SetReadLimit(maxHostText)
	tokens, last := float64(burstBytes), time.Now()
	counted, checked := false, time.Now()
	for {
		if h.opt.Mod != nil {
			if !counted && time.Since(s.since) >= h.opt.CountAfter {
				counted = true
				go h.opt.Mod.Seen(host)
			}
			// Бан из админки — эфир снимается при следующей проверке.
			if time.Since(checked) >= banRecheck {
				checked = time.Now()
				go func() {
					if h.opt.Mod.Status(host, moderation.Air).Banned {
						h.kick(s)
					}
				}()
			}
		}
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage {
			var m struct {
				Title *string `json:"title"`
				Logo  *string `json:"logo"`
			}
			if json.Unmarshal(data, &m) == nil && m.Title != nil {
				h.titleChanged(s, clean(*m.Title, titleRunes))
			}
			if m.Logo != nil {
				if h.opt.HostLogos {
					h.setLogo(s, *m.Logo)
				}
				continue
			}
			h.hostLetterAction(s, data)
			continue
		}
		if len(data) > maxMessage { // звук — кадрами по 160 байт; длинное двоичное не раздаём
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

// kick снимает станцию с эфира: ведущий получает код 4003 и больше не переподключается.
func (h *Hub) kick(s *station) {
	h.kicked.Inc()
	h.mu.Lock()
	logo := s.logo
	h.mu.Unlock()
	if logo != "" { // логотип снятой баном станции не должен жить в превью ссылок
		h.logos.Delete(logo)
	}
	h.waves.Forget(s.freq)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.conn != nil {
		closeBanned(s.conn)
	}
}

func closeBanned(c *websocket.Conn) {
	c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(CloseBanned, "banned"), time.Now().Add(time.Second))
	c.Close()
}

// sayHost — текст ведущему (карточка). Ведущему пишет только этот метод, под wmu.
func (s *station) sayHost(v any) {
	b, _ := json.Marshal(v)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.conn == nil {
		return
	}
	s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	s.conn.WriteMessage(websocket.TextMessage, b)
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
	l := &listener{id: identity.FromRequest(r), out: make(chan []byte, listenerBuf), ctl: make(chan []byte, 16)}
	h.mu.Lock()
	h.ls[l] = struct{}{}
	h.sessions.Inc()
	h.mu.Unlock()

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
			Tune   *int   `json:"tune"`
			Report bool   `json:"report"`
			Like   bool   `json:"like"`
			Letter string `json:"letter"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.Tune != nil {
			h.tune(l, *m.Tune)
		}
		if m.Like {
			h.like(l)
		}
		if m.Letter != "" {
			res, wait := h.letter(l, m.Letter)
			b, _ := json.Marshal(map[string]any{"letter": res, "wait": wait})
			select {
			case l.ctl <- b:
			default:
			}
		}
		if m.Report {
			b, _ := json.Marshal(map[string]string{"report": h.report(l)})
			select {
			case l.ctl <- b:
			default: // слушатель засыпал жалобами — ответы теряются, жалобы считаются
			}
		}
	}
	h.mu.Lock()
	if s := h.st[l.tuned]; s != nil {
		delete(s.subs, l)
	}
	delete(h.ls, l)
	h.mu.Unlock()
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
	l.heard = nil
	if s := h.st[f]; s != nil {
		s.subs[l] = struct{}{}
		l.heard, l.heardSince = s, time.Now()
	}
	h.mu.Unlock()
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
		if h.version.Load() == seen {
			return true
		}
		v, b := h.listMessage()
		seen = v
		h.listBytes.Add(uint64(len(b)))
		return send(websocket.TextMessage, b)
	}
	if !sendList() {
		conn.Close()
		return
	}
	// Кадры — пачками по batchFrames (не дольше batchWait с первого): каждое сообщение
	// WebSocket стоит процессора на шифрование, заголовки и системные вызовы — у сервера, у
	// Caddy и у телефона слушателя, а 160 байт звука обрастали ~40 % заголовков. Пачка из 5 —
	// впятеро меньше сообщений при той же полосе; +100 мс задержки буфер слушателя (400 мс) не видит.
	var pend []byte
	batch := time.NewTimer(time.Hour)
	batch.Stop()
	defer batch.Stop()
	var batchC <-chan time.Time
	flush := func() bool {
		if len(pend) == 0 {
			return true
		}
		ok := send(websocket.BinaryMessage, pend)
		pend, batchC = nil, nil
		batch.Stop()
		return ok
	}
	for {
		select {
		case <-done:
			return
		case b := <-l.out:
			if pend == nil {
				pend = make([]byte, 0, FrameBytes*batchFrames)
				batch.Reset(batchWait)
				batchC = batch.C
			}
			pend = append(pend, b...)
			if len(pend) >= FrameBytes*batchFrames && !flush() {
				conn.Close() // разбудит читающий цикл, тот всё уберёт
				return
			}
		case <-batchC:
			if !flush() {
				conn.Close()
				return
			}
		case b := <-l.ctl:
			if !send(websocket.TextMessage, b) {
				conn.Close()
				return
			}
		case <-tick.C:
			h.countListen(l)
			h.mu.Lock()
			if l.stale {
				l.stale, seen = false, ^uint64(0)
			}
			h.mu.Unlock()
			if !sendList() {
				conn.Close()
				return
			}
		}
	}
}

// ---------- модерация ----------

// countListen засчитывает прослушивание одной станции дольше CountAfter — раз за подключение.
func (h *Hub) countListen(l *listener) {
	if h.opt.Mod == nil || l.id == "" {
		return
	}
	h.mu.Lock()
	ok := !l.counted && l.heard != nil && h.st[l.heard.freq] == l.heard && time.Since(l.heardSince) >= h.opt.CountAfter
	if ok {
		l.counted = true
	}
	h.mu.Unlock()
	if ok {
		h.opt.Mod.Seen(l.id)
	}
}

// like — лайк станции, которую слушатель сейчас слышит. Не чаще likeEvery; свою станцию (та же
// кука) лайкнуть нельзя. Ответа нет: сердце слушатель рисует сам, ведущему уходит пачкой.
func (h *Hub) like(l *listener) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, now := l.heard, time.Now()
	if s == nil || h.st[s.freq] != s || (l.id != "" && s.host == l.id) || now.Sub(l.lastLike) < likeEvery {
		return
	}
	l.lastLike = now
	s.likes++
	s.likesAll++
	h.likes.Inc()
}

// report — слушатель жалуется на станцию, которую слышит не меньше ReportAfter.
func (h *Hub) report(l *listener) (res string) {
	defer func() { h.reports.Inc(res) }()
	if h.opt.Mod == nil || l.id == "" {
		return "no_identity"
	}
	h.mu.Lock()
	s := l.heard
	now := time.Now()
	recent := h.reportTimes[l.id][:0]
	for _, t := range h.reportTimes[l.id] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	switch {
	case s == nil || h.st[s.freq] != s:
		res = "no_station"
	case s.host == l.id:
		res = "self"
	case s.reported[l.id]:
		res = "already"
	case now.Sub(l.heardSince) < h.opt.ReportAfter:
		res = "listen_more"
	case len(recent) >= reportsPerHour:
		res = "rate_limited"
	default:
		s.reported[l.id] = true
		recent = append(recent, now)
	}
	if len(recent) == 0 {
		delete(h.reportTimes, l.id)
	} else {
		h.reportTimes[l.id] = recent
	}
	h.mu.Unlock()
	if res != "" {
		return res
	}

	res, err := h.opt.Mod.Report(s.host, l.id, moderation.Air)
	if err != nil && res == "" {
		h.mu.Lock()
		delete(s.reported, l.id) // жалоба не дошла — можно повторить
		h.mu.Unlock()
		return "error"
	}
	switch res {
	case moderation.ResultBanned:
		h.kick(s)
	case moderation.ResultYellow:
		s.sayHost(map[string]string{"card": "yellow"})
	}
	return res
}

// bucket — ведро токенов: не больше rate событий в секунду с запасом burst.
type bucket struct {
	mu                  sync.Mutex
	rate, burst, tokens float64
	last                time.Time
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// batchWait — пачка уходит не позже, даже неполная (станция замолчала, медленный ведущий);
// переменная — для тестов.
var batchWait = 100 * time.Millisecond

// sameSite — сокет открывает страница с того же домена; порт может отличаться: страница —
// с 443 (Caddy), прямой эфир — с DirectPort. Чужие сайты сокет с кукой посетителя не откроют.
func sameSite(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // не браузер (приложения, генератор нагрузки)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(u.Hostname(), host)
}
