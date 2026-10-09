package radio

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// Постоянные станции с сервера: модератор загружает файлы и включает эфир со страницы /stations/
// на админ-порту радио (только localhost — снаружи через SSH-туннель). Каждая станция — папка
// <dir>/<частота>/ с файлами и station.json (название, порядок, включена ли, где остановилась,
// постоянная кука ведущего). Вещает caster (cast.go) внутри процесса радио. Ведущим из браузера
// эта возможность не открыта: за загруженное с сервера отвечает сервис.

const (
	castMaxFile = 500 << 20 // один файл
	castMeta    = "station.json"
)

var castExt = map[string]bool{".mp3": true, ".ogg": true, ".opus": true, ".m4a": true, ".aac": true, ".flac": true, ".wav": true}

//go:embed stations.html
var castPage []byte

// castStation — то, что лежит в station.json.
type castStation struct {
	F     int      `json:"f"`
	Name  string   `json:"name"`
	On    bool     `json:"on"`
	Order []string `json:"order"` // имена файлов в порядке эфира
	Pos   int      `json:"pos"`   // с какого файла продолжать после перезапуска
	ID    string   `json:"id"`    // кука ведущего: за ней держатся модерация и логотип
	Mix   *castMix `json:"mix,omitempty"`

	cast   *caster // идёт эфир — его caster
	now    string  // сейчас играет
	errMsg string  // почему остановилась (бан)
	cancel context.CancelFunc
	done   chan struct{}
}

// castMix — фейдеры станции, сохраняются между перезапусками.
type castMix struct {
	Music float64 `json:"music"`
	Mic   float64 `json:"mic"`
	Air   float64 `json:"air"`
}

func (s *castStation) mix() castMix {
	if s.Mix == nil {
		return castMix{1, 1, 1}
	}
	return *s.Mix
}

// Casts — постоянные станции с сервера.
type Casts struct {
	hub    *Hub
	dir    string
	quota  int64
	url    string
	ffmpeg string

	mu sync.Mutex
	st map[int]*castStation
}

// OpenCasts поднимает станции из dir и включает те, что были в эфире. wsURL — сокет ведущего этого
// же радио (ws://127.0.0.1:8083/ws/host), quota — сколько байт файлов можно хранить всего.
func (h *Hub) OpenCasts(dir, wsURL string, quota int64) (*Casts, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		log.Printf("станции с сервера: нет ffmpeg — эфир не включится, пока его не поставят")
		ff = "ffmpeg"
	}
	c := &Casts{hub: h, dir: dir, quota: quota, url: wsURL, ffmpeg: ff, st: map[int]*castStation{}}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		f, ok := ParseFreq(e.Name())
		if !ok || !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), castMeta))
		var s castStation
		if err != nil || json.Unmarshal(b, &s) != nil {
			continue
		}
		s.F = f
		c.st[f] = &s
		if s.On {
			c.startLocked(&s)
		}
	}
	return c, nil
}

func (c *Casts) stationDir(f int) string { return filepath.Join(c.dir, fmt.Sprint(f)) }

func (c *Casts) saveLocked(s *castStation) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	p := filepath.Join(c.stationDir(s.F), castMeta)
	if err := os.WriteFile(p+".tmp", b, 0o640); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// startLocked — эфир станции (под c.mu).
func (c *Casts) startLocked(s *castStation) {
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done, s.errMsg = cancel, make(chan struct{}), ""
	cs := newCaster(s.F, s.Name, c.url, s.ID, c.ffmpeg, &castSrc{c: c, f: s.F})
	m := s.mix()
	cs.gains.set(m.Music, m.Mic, m.Air)
	s.cast = cs
	go func(s *castStation, done chan struct{}) {
		defer close(done)
		err := cs.run(ctx)
		c.mu.Lock()
		defer c.mu.Unlock()
		if errors.Is(err, errCastBanned) {
			s.On, s.errMsg = false, "снята модерацией"
			c.saveLocked(s)
			log.Printf("станция %s снята модерацией — эфир с сервера остановлен", FormatFreq(s.F))
		}
		s.cancel, s.now, s.cast = nil, "", nil
	}(s, s.done)
}

// stop — снять станцию с эфира и дождаться, пока caster отпустит волну.
func (c *Casts) stop(f int) {
	c.mu.Lock()
	s := c.st[f]
	var done chan struct{}
	if s != nil && s.cancel != nil {
		s.cancel()
		done = s.done
	}
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

// castSrc — источник эфира одной станции: файлы и позиция из Casts.
type castSrc struct {
	c *Casts
	f int
}

func (p *castSrc) castFiles() []string {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	s := p.c.st[p.f]
	if s == nil {
		return nil
	}
	var out []string
	for _, n := range p.c.orderLocked(s) {
		out = append(out, filepath.Join(p.c.stationDir(p.f), n))
	}
	return out
}

func (p *castSrc) castPos() int {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if s := p.c.st[p.f]; s != nil {
		return s.Pos
	}
	return 0
}

func (p *castSrc) castSetPos(i int) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if s := p.c.st[p.f]; s != nil {
		if n := len(p.c.orderLocked(s)); n > 0 {
			i = ((i % n) + n) % n
		}
		s.Pos = i
		p.c.saveLocked(s)
	}
}

func (p *castSrc) castTitle(t string) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if s := p.c.st[p.f]; s != nil {
		s.now = t
	}
}

// orderLocked — файлы станции в порядке эфира: сначала по Order, новые — в конец по имени,
// исчезнувшие — выпадают.
func (c *Casts) orderLocked(s *castStation) []string {
	ents, _ := os.ReadDir(c.stationDir(s.F))
	have := map[string]bool{}
	var rest []string
	for _, e := range ents {
		if n := e.Name(); !e.IsDir() && castExt[strings.ToLower(filepath.Ext(n))] {
			have[n] = true
			rest = append(rest, n)
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range s.Order {
		if have[n] && !seen[n] {
			out, seen[n] = append(out, n), true
		}
	}
	sort.Strings(rest)
	for _, n := range rest {
		if !seen[n] {
			out = append(out, n)
		}
	}
	return out
}

func (c *Casts) usedBytes() int64 {
	var n int64
	filepath.WalkDir(c.dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// cleanFileName — имя загружаемого файла: только имя, без путей и служебных символов.
func cleanFileName(s string) (string, bool) {
	s = path.Base(strings.ReplaceAll(s, "\\", "/")) // path, а не filepath: «C:» — не диск, а символ
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" || strings.HasPrefix(s, ".") || utf8.RuneCountInString(s) > 200 || s == castMeta {
		return "", false
	}
	return s, castExt[strings.ToLower(filepath.Ext(s))]
}

func newCastID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// LocalOnly — защита админ-порта: он слушает только localhost, но браузер модератора с открытым
// SSH-туннелем могла бы попросить о запросе любая открытая страница. Host — только localhost
// (иначе DNS rebinding), запрос с Origin — только со своей страницы (иначе CSRF). curl и Prometheus
// Origin не шлют — им можно.
func LocalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "[::1]" {
			http.Error(w, "только через localhost", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "чужая страница", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Handler — страница /stations/ и её API (вешать на админ-порт под LocalOnly).
//
//	GET    /stations/                         страница
//	GET    /stations/api                      станции, файлы, место
//	POST   /stations/api/station   {f,name}    создать станцию или переименовать
//	DELETE /stations/api/station?f=934         удалить (только снятую с эфира) вместе с файлами
//	POST   /stations/api/upload?f=934&name=…   тело — файл
//	DELETE /stations/api/file?f=934&name=…
//	POST   /stations/api/order?f=934 [имена]
//	POST   /stations/api/on?f=934 · /off?f=934
func (c *Casts) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stations/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(castPage)
	})
	mux.HandleFunc("GET /logo/{file}", c.hub.serveLogo) // логотипы станций — для самой страницы
	mux.HandleFunc("GET /stations/api", c.apiList)
	mux.HandleFunc("POST /stations/api/station", c.apiStation)
	mux.HandleFunc("DELETE /stations/api/station", c.apiDelete)
	mux.HandleFunc("POST /stations/api/upload", c.apiUpload)
	mux.HandleFunc("DELETE /stations/api/file", c.apiDeleteFile)
	mux.HandleFunc("POST /stations/api/order", c.apiOrder)
	mux.HandleFunc("POST /stations/api/ctl", c.apiCtl)
	mux.HandleFunc("POST /stations/api/mix", c.apiMix)
	mux.HandleFunc("GET /stations/api/letters", c.apiLetters)
	mux.HandleFunc("POST /stations/api/letter", c.apiLetter)
	mux.HandleFunc("GET /stations/api/mic", c.apiMic)
	mux.HandleFunc("GET /stations/api/monitor", c.apiMonitor)
	mux.HandleFunc("POST /stations/api/on", func(w http.ResponseWriter, r *http.Request) { c.apiOnOff(w, r, true) })
	mux.HandleFunc("POST /stations/api/off", func(w http.ResponseWriter, r *http.Request) { c.apiOnOff(w, r, false) })
	return mux
}

type castFileJSON struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type castJSON struct {
	F      int            `json:"f"`
	Freq   string         `json:"freq"`
	Name   string         `json:"name"`
	On     bool           `json:"on"`
	OnAir  bool           `json:"onAir"`
	Busy   bool           `json:"busy"` // волна занята другим ведущим
	Now    string         `json:"now"`
	Error  string         `json:"error,omitempty"`
	Logo   string         `json:"logo,omitempty"`
	Files  []castFileJSON `json:"files"`
	Index  int            `json:"index"`  // какой файл играет
	Paused bool           `json:"paused"` // музыка на паузе
	MicOn  bool           `json:"micOn"`  // модератор в эфире с микрофона
	Mix    castMix        `json:"mix"`

	Listeners int  `json:"listeners"` // в эфире: сколько слушают
	Likes     int  `json:"likes"`     // лайков за этот эфир
	Letters   int  `json:"letters"`   // писем в памяти
	Card      bool `json:"card"`      // жёлтая карточка
}

func (c *Casts) apiList(w http.ResponseWriter, _ *http.Request) {
	c.hub.mu.Lock()
	live := map[int]*station{}
	for f, s := range c.hub.st {
		live[f] = s
	}
	c.hub.mu.Unlock()
	c.mu.Lock()
	var out []castJSON
	for _, s := range c.st {
		j := castJSON{F: s.F, Freq: FormatFreq(s.F), Name: s.Name, On: s.On, Now: s.now, Error: s.errMsg, Files: []castFileJSON{},
			Index: s.Pos, Mix: s.mix()}
		if cs := s.cast; cs != nil {
			j.Index, j.Paused, j.MicOn = int(cs.index.Load()), cs.paused.Load(), cs.micOn.Load()
			in := cs.snapshot()
			if cs.onAir.Load() {
				j.Listeners, j.Likes, j.Card = in.Listeners, in.Likes, in.Card
			}
			j.Letters = len(in.Letters)
		}
		if ls := live[s.F]; ls != nil {
			c.hub.mu.Lock()
			j.OnAir, j.Busy, j.Logo = ls.host == s.ID, ls.host != s.ID, ls.logo
			c.hub.mu.Unlock()
		}
		for _, n := range c.orderLocked(s) {
			if fi, err := os.Stat(filepath.Join(c.stationDir(s.F), n)); err == nil {
				j.Files = append(j.Files, castFileJSON{n, fi.Size()})
			}
		}
		out = append(out, j)
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, k int) bool { return out[i].F < out[k].F })
	_, ffErr := exec.LookPath(c.ffmpeg)
	writeJSON(w, map[string]any{"stations": out, "used": c.usedBytes(), "quota": c.quota, "ffmpeg": ffErr == nil})
}

func (c *Casts) station(w http.ResponseWriter, r *http.Request) *castStation {
	f, ok := ParseFreq(r.URL.Query().Get("f"))
	c.mu.Lock()
	s := c.st[f]
	c.mu.Unlock()
	if !ok || s == nil {
		http.Error(w, "нет такой станции", http.StatusNotFound)
		return nil
	}
	return s
}

func (c *Casts) apiStation(w http.ResponseWriter, r *http.Request) {
	var req struct{ F, Name string }
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
		http.Error(w, "нужен JSON {f, name}", http.StatusBadRequest)
		return
	}
	f, ok := ParseFreq(req.F)
	name := clean(req.Name, nameRunes)
	if !ok || name == "" {
		http.Error(w, "частота 87.5–108.0 и название до 24 знаков", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	s := c.st[f]
	restart := false
	if s == nil {
		if err := os.MkdirAll(c.stationDir(f), 0o750); err != nil {
			c.mu.Unlock()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s = &castStation{F: f, ID: newCastID()}
		c.st[f] = s
	} else {
		restart = s.cancel != nil && s.Name != name // название уходит радио при подключении
	}
	s.Name = name
	err := c.saveLocked(s)
	c.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if restart {
		c.stop(f)
		c.mu.Lock()
		if s.On {
			c.startLocked(s)
		}
		c.mu.Unlock()
	}
	writeJSON(w, map[string]int{"f": f})
}

func (c *Casts) apiDelete(w http.ResponseWriter, r *http.Request) {
	s := c.station(w, r)
	if s == nil {
		return
	}
	c.mu.Lock()
	if s.On || s.cancel != nil {
		c.mu.Unlock()
		http.Error(w, "сначала снимите станцию с эфира", http.StatusConflict)
		return
	}
	delete(c.st, s.F)
	c.mu.Unlock()
	os.RemoveAll(c.stationDir(s.F))
	writeJSON(w, map[string]bool{"ok": true})
}

func (c *Casts) apiUpload(w http.ResponseWriter, r *http.Request) {
	s := c.station(w, r)
	if s == nil {
		return
	}
	name, ok := cleanFileName(r.URL.Query().Get("name"))
	if !ok {
		http.Error(w, "нужен аудиофайл: mp3, ogg, opus, m4a, aac, flac, wav", http.StatusBadRequest)
		return
	}
	if r.ContentLength > castMaxFile {
		http.Error(w, "файл больше 500 МБ", http.StatusRequestEntityTooLarge)
		return
	}
	if c.usedBytes()+max(r.ContentLength, 0) > c.quota {
		http.Error(w, "место для станций кончилось", http.StatusInsufficientStorage)
		return
	}
	dst := filepath.Join(c.stationDir(s.F), name)
	tmp := filepath.Join(c.stationDir(s.F), ".upload-"+newCastID())
	f, err := os.Create(tmp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, castMaxFile))
	f.Close()
	if err != nil || n == 0 {
		os.Remove(tmp)
		http.Error(w, "файл не дошёл целиком", http.StatusBadRequest)
		return
	}
	if c.usedBytes() > c.quota {
		os.Remove(tmp)
		http.Error(w, "место для станций кончилось", http.StatusInsufficientStorage)
		return
	}
	// ffmpeg должен прочитать файл — иначе в эфире была бы тишина и ошибки по кругу
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, c.ffmpeg, "-v", "error", "-nostdin", "-i", tmp, "-t", "2", "-f", "null", "-").CombinedOutput(); err != nil {
		os.Remove(tmp)
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		http.Error(w, "ffmpeg не читает файл: "+msg, http.StatusUnprocessableEntity)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"name": name, "size": n})
}

func (c *Casts) apiDeleteFile(w http.ResponseWriter, r *http.Request) {
	s := c.station(w, r)
	if s == nil {
		return
	}
	name, ok := cleanFileName(r.URL.Query().Get("name"))
	if !ok {
		http.Error(w, "нет такого файла", http.StatusBadRequest)
		return
	}
	if err := os.Remove(filepath.Join(c.stationDir(s.F), name)); err != nil {
		http.Error(w, "нет такого файла", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (c *Casts) apiOrder(w http.ResponseWriter, r *http.Request) {
	s := c.station(w, r)
	if s == nil {
		return
	}
	var order []string
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&order) != nil {
		http.Error(w, "нужен JSON-список имён", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	s.Order = order
	err := c.saveLocked(s)
	c.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (c *Casts) apiOnOff(w http.ResponseWriter, r *http.Request, on bool) {
	s := c.station(w, r)
	if s == nil {
		return
	}
	if !on {
		c.mu.Lock()
		s.On = false
		c.saveLocked(s)
		c.mu.Unlock()
		c.stop(s.F)
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	c.mu.Lock()
	s.On = true // без файлов станция выходит в эфир, когда включат микрофон
	c.saveLocked(s)
	c.startLocked(s)
	c.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// live — caster станции в эфире; нет — ответ 409.
func (c *Casts) live(w http.ResponseWriter, r *http.Request) (*castStation, *caster) {
	s := c.station(w, r)
	if s == nil {
		return nil, nil
	}
	c.mu.Lock()
	cs := s.cast
	c.mu.Unlock()
	if cs == nil {
		http.Error(w, "станция не в эфире — нажмите «В эфир»", http.StatusConflict)
		return nil, nil
	}
	return s, cs
}

// apiCtl — очередь: next, prev, pause, play, jump (&i=номер).
func (c *Casts) apiCtl(w http.ResponseWriter, r *http.Request) {
	_, cs := c.live(w, r)
	if cs == nil {
		return
	}
	op := r.URL.Query().Get("op")
	var i int
	fmt.Sscan(r.URL.Query().Get("i"), &i)
	switch op {
	case "next", "prev", "pause", "play", "jump":
	default:
		http.Error(w, "op: next, prev, pause, play, jump", http.StatusBadRequest)
		return
	}
	select {
	case cs.ctl <- castCmd{op: op, i: i}:
	default:
		http.Error(w, "станция занята, повторите", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// apiMix — фейдеры станции {music, mic, air}, 0…1,5; сохраняются.
func (c *Casts) apiMix(w http.ResponseWriter, r *http.Request) {
	s := c.station(w, r)
	if s == nil {
		return
	}
	var m castMix
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&m) != nil {
		http.Error(w, "нужен JSON {music, mic, air}", http.StatusBadRequest)
		return
	}
	cl := func(v float64) float64 { return max(0, min(1.5, v)) }
	m = castMix{cl(m.Music), cl(m.Mic), cl(m.Air)}
	c.mu.Lock()
	s.Mix = &m
	if s.cast != nil {
		s.cast.gains.set(m.Music, m.Mic, m.Air)
	}
	c.saveLocked(s)
	c.mu.Unlock()
	writeJSON(w, m)
}

// Сокеты страницы. Origin проверил LocalOnly — здесь только апгрейд.
var castUpgrader = websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096,
	CheckOrigin: func(*http.Request) bool { return true }}

// apiMic — микрофон модератора: двоичные сообщения — кадры μ-law по 160 байт (можно пачкой).
func (c *Casts) apiMic(w http.ResponseWriter, r *http.Request) {
	_, cs := c.live(w, r)
	if cs == nil {
		return
	}
	conn, err := castUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	cs.micOn.Store(true)
	defer cs.micOn.Store(false)
	conn.SetReadLimit(64 << 10)
	for {
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		typ, b, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ != websocket.BinaryMessage {
			continue
		}
		for len(b) >= FrameBytes {
			f := make([]byte, FrameBytes)
			copy(f, b)
			cs.pushMic(f)
			b = b[FrameBytes:]
		}
	}
}

// apiMonitor — готовый эфир станции на страницу: пачками по 5 кадров.
func (c *Casts) apiMonitor(w http.ResponseWriter, r *http.Request) {
	_, cs := c.live(w, r)
	if cs == nil {
		return
	}
	conn, err := castUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ch, unsub := cs.subscribe()
	defer unsub()
	gone := make(chan struct{})
	go func() { // читаем, чтобы увидеть закрытие страницей
		defer close(gone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	var pend []byte
	for {
		select {
		case <-gone:
			return
		case f := <-ch:
			pend = append(pend, f...)
			if len(pend) >= batchFrames*FrameBytes {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if conn.WriteMessage(websocket.BinaryMessage, pend) != nil {
					return
				}
				pend = nil
			}
		case <-time.After(2 * time.Second): // станция замолчала — держим соединение
		}
	}
}

// apiLetters — что видит ведущий: слушатели, лайки, письма (новые сверху), итог последней жалобы.
func (c *Casts) apiLetters(w http.ResponseWriter, r *http.Request) {
	_, cs := c.live(w, r)
	if cs == nil {
		return
	}
	in := cs.snapshot()
	if !cs.onAir.Load() {
		in.Listeners = 0
	}
	if in.Letters == nil {
		in.Letters = []castLetter{}
	}
	writeJSON(w, in)
}

// apiLetter — действие с письмом: op=block (не принимать письма от отправителя) или op=report
// (пожаловаться: жалоба идёт в модерацию, как от ведущего в браузере).
func (c *Casts) apiLetter(w http.ResponseWriter, r *http.Request) {
	_, cs := c.live(w, r)
	if cs == nil {
		return
	}
	var id uint64
	fmt.Sscan(r.URL.Query().Get("id"), &id)
	op := r.URL.Query().Get("op")
	if id == 0 || (op != "block" && op != "report") {
		http.Error(w, "нужны id и op=block|report", http.StatusBadRequest)
		return
	}
	if !cs.letterAction(id, op == "report") {
		http.Error(w, "нет такого письма или станция занята", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
