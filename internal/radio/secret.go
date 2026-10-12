package radio

// Секретная волна: живое зеркало УВБ-76 («Жужжалка», 4625 кГц). На шкале её нет: страница сама,
// с шансом 0,13 на визит, прячет точку на левом краю шкалы и, если на неё подкрутили, шлёт
// {"tune": SecretTune}. В списке станций секретной волны нет, лайков, писем и жалоб у неё тоже.
//
// Звук берётся с публичных приёмников KiwiSDR (любительские SDR, открытые для прослушивания):
// список — kiwisdr.com через rx.linkfanel.net, ближние к передатчику первыми. Многие приёмники
// автоматических клиентов не пускают (молча закрывают соединение), у многих — лимит минут на IP в
// сутки, поэтому: подключаемся только пока кто-то слушает, отключаемся через secretIdle после
// последнего слушателя, неответившие приёмники пропускаем час.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	SecretTune   = 4625 // кГц УВБ-76; номер волны для tune
	secretFreqHz = 4625000
	kiwiListTTL  = 6 * time.Hour
	kiwiBadFor   = time.Hour
	kiwiMaxTries = 30 // приёмников за один заход
	kiwiBatch    = 6  // опрашиваем одновременно
	kiwiSilence  = 20 * time.Second
)

var (
	secretIdle      = time.Minute            // столько ждём после последнего слушателя; переменная — для тестов
	secretWarmEvery = 30 * time.Second       // прогрев от одного слушателя — не чаще
	kiwiFirstSound  = 8 * time.Second        // столько ждём первый звук от приёмника
	uvbSite         = [2]float64{57.8, 28.3} // передатчик (Псковская обл.) — ближние приёмники слышат лучше
)

type secretWave struct {
	h       *Hub
	list    string // URL списка KiwiSDR
	st      *station
	mu      sync.Mutex
	running bool
	bad     map[string]time.Time
	cache   []string
	cached  time.Time
	good    string    // последний приёмник, отдавший звук: его пробуем первым
	warmTil time.Time // до этого времени зеркало держим и без слушателей (прогрев)
}

func newSecretWave(h *Hub, list string) *secretWave {
	return &secretWave{h: h, list: list, bad: map[string]time.Time{},
		st: &station{freq: SecretTune, subs: map[*listener]struct{}{}, reported: map[string]bool{},
			letterFrom: map[uint64]string{}, blocked: map[string]bool{}}}
}

// secretChance — шанс спрятать секретную волну на этом визите: до SecretSureUntil (проверка) — 1.
func (h *Hub) secretChance(now time.Time) float64 {
	if now.Before(h.opt.SecretSureUntil) {
		return 1
	}
	if h.opt.SecretChance > 0 {
		return h.opt.SecretChance
	}
	return 0.13
}

// stationFor — станция по номеру настройки: обычная частота или секретная волна.
// Вызывать под h.mu.
func (h *Hub) stationFor(f int) *station {
	if f == SecretTune && h.secret != nil {
		return h.secret.st
	}
	return h.st[f]
}

// warm — страница, где выпала секретная волна, просит подготовить звук заранее: приёмник ищется
// сейчас, а не когда докрутят до точки (поиск — секунды). Держим secretIdle.
func (w *secretWave) warm() {
	w.mu.Lock()
	w.warmTil = time.Now().Add(secretIdle)
	w.mu.Unlock()
	w.h.secretEvents.Inc("warm")
	w.wake()
}

// wake — на секретную волну настроился слушатель: зеркало запускается, если ещё не идёт.
func (w *secretWave) wake() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		return
	}
	w.running = true
	go w.run()
}

func (w *secretWave) listeners() int {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return len(w.st.subs)
}

// run — пока есть слушатели (и ещё secretIdle после последнего): звук с приёмника, обрыв — следующий.
func (w *secretWave) run() {
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()
	idleSince := time.Time{}
	stop := func() bool {
		w.mu.Lock()
		warm := time.Now().Before(w.warmTil)
		w.mu.Unlock()
		if warm || w.listeners() > 0 {
			idleSince = time.Time{}
			return false
		}
		if idleSince.IsZero() {
			idleSince = time.Now()
		}
		return time.Since(idleSince) >= secretIdle
	}
	for !stop() {
		k := w.find(stop)
		if k == nil {
			w.h.secretEvents.Inc("no_receiver")
			for t := time.Now(); time.Since(t) < 20*time.Second && !stop(); { // передышка, чтобы не долбить список
				time.Sleep(500 * time.Millisecond)
			}
			continue
		}
		w.h.secretEvents.Inc("connected")
		log.Printf("секретная волна: звук с приёмника %s", k.host) // адрес приёмника, не слушателя
		w.mu.Lock()
		w.good = k.host
		w.mu.Unlock()
		k.stream(w, stop)
		k.close()
	}
}

// find — первый приёмник, отдавший звук: прошлый удачный — сразу, остальные — пачками по
// kiwiBatch одновременно (многие молча не пускают — по одному искали бы минутами).
func (w *secretWave) find(stop func() bool) *kiwiConn {
	hosts := w.candidates()
	w.mu.Lock()
	good := w.good
	w.mu.Unlock()
	if good != "" {
		rest := []string{good}
		for _, h := range hosts {
			if h != good {
				rest = append(rest, h)
			}
		}
		hosts = rest
	}
	if len(hosts) > kiwiMaxTries {
		hosts = hosts[:kiwiMaxTries]
	}
	for i := 0; i < len(hosts) && !stop(); i += kiwiBatch {
		batch := hosts[i:min(i+kiwiBatch, len(hosts))]
		res := make(chan *kiwiConn, len(batch))
		for _, h := range batch {
			go func(h string) {
				k, err := dialKiwi(h)
				if err != nil {
					w.markBad(h)
					res <- nil
					return
				}
				res <- k
			}(h)
		}
		var win *kiwiConn
		for range batch {
			if k := <-res; k != nil {
				if win == nil {
					win = k
				} else {
					k.close() // опоздавшие — сразу отпускаем: у приёмника мало мест
				}
			}
		}
		if win != nil {
			return win
		}
	}
	return nil
}

// kiwiConn — соединение со звуком KiwiSDR, уже настроенное на 4625 кГц и отдавшее первый звук.
type kiwiConn struct {
	host  string
	c     *websocket.Conn
	send  func(string)
	rs    *resampler
	first []byte // отсчёты первого пакета
	done  chan struct{}
	once  sync.Once
}

func (k *kiwiConn) close() {
	k.once.Do(func() { close(k.done); k.c.Close() })
}

// dialKiwi — подключиться, настроиться и дождаться первого звука (kiwiFirstSound).
func dialKiwi(host string) (*kiwiConn, error) {
	c, _, err := (&websocket.Dialer{HandshakeTimeout: 8 * time.Second}).Dial(
		fmt.Sprintf("ws://%s/%d/SND", host, time.Now().Unix()), nil)
	if err != nil {
		return nil, err
	}
	k := &kiwiConn{host: host, c: c, rs: newResampler(12000), done: make(chan struct{})}
	var wmu sync.Mutex
	k.send = func(s string) {
		wmu.Lock()
		defer wmu.Unlock()
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		c.WriteMessage(websocket.TextMessage, []byte(s))
	}
	k.send("SET auth t=kiwi p=")
	k.send("SET ident_user=openline.one")
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-k.done:
				return
			case <-t.C:
				k.send("SET keepalive")
			}
		}
	}()
	deadline := time.Now().Add(kiwiFirstSound)
	for time.Now().Before(deadline) {
		c.SetReadDeadline(deadline)
		snd, err := k.read()
		if err != nil {
			k.close()
			return nil, err
		}
		if snd != nil && loud(snd) { // нули — у приёмника звук выключен или нет антенны
			k.first = snd
			return k, nil
		}
	}
	k.close()
	return nil, errors.New("нет звука")
}

// read — одно сообщение: настройки отвечаем сами, звук — отсчёты (int16 BE), иначе nil.
func (k *kiwiConn) read() ([]byte, error) {
	_, b, err := k.c.ReadMessage()
	if err != nil {
		return nil, err
	}
	if len(b) < 4 {
		return nil, nil
	}
	switch string(b[:3]) {
	case "MSG":
		for _, kv := range strings.Fields(string(b[4:])) {
			key, v, _ := strings.Cut(kv, "=")
			switch key {
			case "audio_rate":
				k.send("SET AR OK in=" + v + " out=44100")
			case "sample_rate":
				if r, err := strconv.ParseFloat(v, 64); err == nil && r > 1000 {
					k.rs = newResampler(r)
				}
				k.send("SET squelch=0 max=0")
				k.send("SET genattn=0")
				k.send("SET gen=0 mix=-1")
				k.send(fmt.Sprintf("SET mod=am low_cut=-4000 high_cut=4000 freq=%.3f", float64(secretFreqHz)/1000))
				k.send("SET agc=1 hang=0 thresh=-100 slope=6 decay=1000 manGain=50")
				k.send("SET compression=0")
				k.send("SET keepalive")
			case "too_busy", "badp", "down", "redirect": // badp=0 — «пароль не нужен», это не отказ
				if v != "" && v != "0" {
					return nil, errors.New(key)
				}
			}
		}
	case "SND":
		if body := b[3:]; len(body) > 7 {
			return body[7:], nil
		}
	}
	return nil, nil
}

// loud — в отсчётах (int16 BE) есть сигнал, а не нули.
func loud(be []byte) bool {
	for i := 0; i+1 < len(be); i += 2 {
		if v := int16(binary.BigEndian.Uint16(be[i:])); v > 64 || v < -64 {
			return true
		}
	}
	return false
}

// stream — звук приёмника в эфир секретной волны, пока stop() не скажет хватит, связь не оборвётся
// или приёмник не замолчит нулями дольше kiwiSilence (тогда — другой).
func (k *kiwiConn) stream(w *secretWave, stop func() bool) {
	pend := k.rs.push(k.first, nil)
	heard := time.Now()
	for {
		if time.Since(heard) > kiwiSilence {
			w.markBad(k.host)
			return
		}
		for len(pend) >= FrameBytes {
			fr := make([]byte, FrameBytes)
			copy(fr, pend[:FrameBytes])
			pend = pend[FrameBytes:]
			w.h.broadcast(w.st, fr)
		}
		if stop() {
			return
		}
		k.c.SetReadDeadline(time.Now().Add(10 * time.Second))
		snd, err := k.read()
		if err != nil {
			return
		}
		if snd != nil {
			if loud(snd) {
				heard = time.Now()
			}
			pend = k.rs.push(snd, pend)
		}
	}
}

func (w *secretWave) markBad(host string) {
	w.mu.Lock()
	w.bad[host] = time.Now()
	w.mu.Unlock()
}

// candidates — приёмники по удалённости от передатчика, без занятых и недавно не ответивших.
func (w *secretWave) candidates() []string {
	w.mu.Lock()
	fresh := time.Since(w.cached) < kiwiListTTL && len(w.cache) > 0
	w.mu.Unlock()
	if !fresh {
		if l, err := fetchKiwiList(w.list); err == nil && len(l) > 0 {
			w.mu.Lock()
			w.cache, w.cached = l, time.Now()
			w.mu.Unlock()
		} else if err != nil {
			log.Printf("секретная волна: список приёмников: %v", err)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, h := range w.cache {
		if t, ok := w.bad[h]; ok && time.Since(t) < kiwiBadFor {
			continue
		}
		out = append(out, h)
	}
	return out
}

var kiwiNum = regexp.MustCompile(`-?\d+(\.\d+)?`)

// fetchKiwiList — список KiwiSDR (JS-массив с kiwisdr.com/public): host:port свободных приёмников,
// слышащих 4625 кГц, ближние к передатчику первыми.
func fetchKiwiList(url string) ([]string, error) {
	cl := &http.Client{Timeout: 20 * time.Second}
	r, err := cl.Get(url)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	s := string(b)
	i, j := strings.Index(s, "["), strings.LastIndex(s, "]")
	if i < 0 || j < i {
		return nil, fmt.Errorf("не список")
	}
	s = regexp.MustCompile(`,\s*([\]}])`).ReplaceAllString(s[i:j+1], "$1")
	var raw []map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	type cand struct {
		host string
		d    float64
	}
	var cs []cand
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	for _, e := range raw {
		if str(e, "offline") == "yes" {
			continue
		}
		users, _ := strconv.Atoi(str(e, "users"))
		umax, _ := strconv.Atoi(str(e, "users_max"))
		if umax > 0 && users >= umax {
			continue
		}
		if lo, hi, ok := strings.Cut(str(e, "bands"), "-"); ok {
			l, _ := strconv.ParseFloat(lo, 64)
			hh, _ := strconv.ParseFloat(hi, 64)
			if secretFreqHz < l || secretFreqHz > hh {
				continue
			}
		}
		g := kiwiNum.FindAllString(str(e, "gps"), 2)
		if len(g) < 2 {
			continue
		}
		la, _ := strconv.ParseFloat(g[0], 64)
		lo, _ := strconv.ParseFloat(g[1], 64)
		host := strings.TrimSuffix(strings.TrimPrefix(str(e, "url"), "http://"), "/")
		if host == "" || strings.Contains(host, "/") || strings.HasPrefix(str(e, "url"), "https://") {
			continue
		}
		if !strings.Contains(host, ":") {
			host += ":80"
		}
		cs = append(cs, cand{host, greatCircle(uvbSite[0], uvbSite[1], la, lo)})
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].d < cs[b].d })
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.host)
	}
	return out, nil
}

func greatCircle(la1, lo1, la2, lo2 float64) float64 {
	r := math.Pi / 180
	return 6371 * math.Acos(math.Min(1, math.Sin(la1*r)*math.Sin(la2*r)+math.Cos(la1*r)*math.Cos(la2*r)*math.Cos((lo1-lo2)*r)))
}

// resampler — 16 бит big-endian с приёмника (~12 кГц) → μ-law 8 кГц: фильтр нижних частот
// (окно Хэмминга, срез 3,6 кГц) и линейная интерполяция. Для гудков и голоса по КВ хватает.
type resampler struct {
	step float64 // отсчётов входа на отсчёт выхода
	pos  float64
	taps []float64
	hist []float64
	lp   []float64 // отфильтрованный вход, ещё не съеденный
}

func newResampler(in float64) *resampler {
	const n = 31
	fc := 3600 / in
	taps := make([]float64, n)
	sum := 0.0
	for i := range taps {
		x := float64(i - n/2)
		v := 2 * fc
		if x != 0 {
			v = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		v *= 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/(n-1))
		taps[i] = v
		sum += v
	}
	for i := range taps {
		taps[i] /= sum
	}
	return &resampler{step: in / 8000, taps: taps, hist: make([]float64, n)}
}

// push — новые отсчёты (int16 BE) → μ-law байты в out.
func (r *resampler) push(be []byte, out []byte) []byte {
	for i := 0; i+1 < len(be); i += 2 {
		v := float64(int16(binary.BigEndian.Uint16(be[i:]))) / 32768
		copy(r.hist, r.hist[1:])
		r.hist[len(r.hist)-1] = v
		acc := 0.0
		for k, t := range r.taps {
			acc += t * r.hist[k]
		}
		r.lp = append(r.lp, acc)
	}
	for r.pos+1 < float64(len(r.lp)) {
		i := int(r.pos)
		f := r.pos - float64(i)
		v := r.lp[i]*(1-f) + r.lp[i+1]*f
		out = append(out, linearToUlaw(v*1.6))
		r.pos += r.step
	}
	drop := int(r.pos)
	r.lp = append(r.lp[:0], r.lp[drop:]...)
	r.pos -= float64(drop)
	return out
}
