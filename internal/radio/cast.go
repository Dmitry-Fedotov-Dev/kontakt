package radio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Вещание с сервера: «ведущий» без браузера внутри процесса радио. Файлы станции по кругу; ffmpeg
// декодирует их в G.711 μ-law 8 кГц. Каждые 20 мс caster берёт кадр трека и кадр микрофона
// модератора (страница /stations/, через SSH-туннель), сводит их по фейдерам и шлёт в собственный
// /ws/host радио по localhost. Через сокет, а не напрямую в хаб: станция с сервера проходит тот же
// допуск, модерацию, названия треков и логотипы, что и ведущий из браузера. Готовый эфир уходит и
// на монитор страницы. У слушателя такая станция звучит ровнее всех: между ведущим и радио нет
// интернета (INC-001).

const (
	castFrame   = 20 * time.Millisecond
	castBanCode = 4003
	micPrebuf   = 5  // кадров (100 мс) микрофона копим перед звуком: туннель привозит их рывками
	micMax      = 25 // больше 0,5 с в запасе — старое выкидываем, иначе голос отстаёт всё сильнее
)

var errCastBanned = errors.New("станция снята модерацией")

// castSource — откуда caster берёт эфир: список файлов по порядку и где он остановился.
type castSource interface {
	castFiles() []string // полные пути, в порядке эфира
	castPos() int        // номер файла, с которого продолжать
	castSetPos(i int)    // дальше играть файл i
	castTitle(t string)  // сейчас играет
}

// castGains — фейдеры станции: громкость трека, микрофона и общая (0…1,5).
type castGains struct{ music, mic, air atomic.Uint32 }

func (g *castGains) set(music, mic, air float64) {
	clamp := func(v float64) uint32 { return math.Float32bits(float32(math.Max(0, math.Min(1.5, v)))) }
	g.music.Store(clamp(music))
	g.mic.Store(clamp(mic))
	g.air.Store(clamp(air))
}

func (g *castGains) get() (music, mic, air float64) {
	return float64(math.Float32frombits(g.music.Load())), float64(math.Float32frombits(g.mic.Load())),
		float64(math.Float32frombits(g.air.Load()))
}

type castCmd struct {
	op string // next, prev, jump, pause, play
	i  int
}

type caster struct {
	freq   int
	name   string
	url    string // ws://…/ws/host
	id     string // кука ведущего
	ffmpeg string
	src    castSource

	gains  castGains
	ctl    chan castCmd
	mic    chan []byte // кадры микрофона модератора
	micOn  atomic.Bool
	paused atomic.Bool
	index  atomic.Int64 // какой файл играет
	onAir  atomic.Bool

	infoMu  sync.Mutex
	info    castInfo
	hostOut chan []byte // текстовые сообщения радио от студии (действия с письмами)

	monMu sync.Mutex
	mon   map[chan []byte]struct{}

	conn   *websocket.Conn
	closed chan struct{}
	banned atomic.Bool

	dec      *castDecoder
	title    string // последнее название, отправленное радио
	micReady bool   // запас микрофона набран
}

func newCaster(freq int, name, wsURL, id, ffmpeg string, src castSource) *caster {
	c := &caster{freq: freq, name: name, url: wsURL, id: id, ffmpeg: ffmpeg, src: src,
		ctl: make(chan castCmd, 16), mic: make(chan []byte, micMax+5), mon: map[chan []byte]struct{}{},
		hostOut: make(chan []byte, 32)}
	c.gains.set(1, 1, 1)
	return c
}

// run — эфир до отмены ctx или бана (errCastBanned).
func (c *caster) run(ctx context.Context) error {
	defer c.hangUp()
	defer c.closeDec()
	next := time.Now()
	for ctx.Err() == nil {
		c.commands()
		music, haveMusic := c.musicFrame(ctx)
		micF := c.micFrame()
		if !haveMusic && micF == nil && !c.micOn.Load() {
			// нечего играть: файлов нет, микрофон выключен — волну не занимаем
			c.hangUp()
			c.setTitle("")
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			next = time.Now()
			continue
		}
		out := c.mixFrame(music, micF)
		if d := time.Until(next); d > 0 { // по часам, а не по тикам: задержка не копится в дрейф
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		} else if d < -time.Second {
			next = time.Now() // долго стояли (переподключение) — отсчёт заново
		}
		next = next.Add(castFrame)
		if err := c.send(ctx, out); err != nil {
			return err
		}
		c.monitor(out)
	}
	return ctx.Err()
}

// commands — нажатия на странице: следующий, предыдущий, этот трек, пауза.
func (c *caster) commands() {
	for {
		select {
		case cmd := <-c.ctl:
			n := len(c.src.castFiles())
			i := int(c.index.Load())
			switch cmd.op {
			case "pause":
				c.paused.Store(true)
				continue
			case "play":
				c.paused.Store(false)
				continue
			case "next":
				i++
			case "prev":
				if c.dec == nil || c.dec.frames <= 150 { // сыграл больше 3 с — «назад» начинает трек заново
					i--
				}
			case "jump":
				i = cmd.i
			}
			if n > 0 {
				i = ((i % n) + n) % n
			}
			c.closeDec()
			c.src.castSetPos(i)
			c.paused.Store(false)
		default:
			return
		}
	}
}

// musicFrame — следующий кадр трека; на паузе — тишина. false — играть нечего (файлов нет).
func (c *caster) musicFrame(ctx context.Context) ([]byte, bool) {
	if c.paused.Load() {
		return nil, true
	}
	for tries := 0; ; tries++ {
		files := c.src.castFiles()
		if len(files) == 0 {
			c.closeDec()
			return nil, false
		}
		if tries > len(files) { // ни один файл не читается — тишина, но волну держим
			return nil, true
		}
		if c.dec == nil {
			i := c.src.castPos() % len(files)
			c.index.Store(int64(i))
			d, err := startDecoder(ctx, c.ffmpeg, files[i])
			if err != nil {
				log.Printf("станция %s: %s: %v", FormatFreq(c.freq), filepath.Base(files[i]), err)
				c.src.castSetPos(i + 1)
				continue
			}
			c.dec = d
			c.setTitle(trackTitle(files[i]))
		}
		if f, err := c.dec.frame(); err == nil {
			return f, true
		} else if !errors.Is(err, io.EOF) {
			log.Printf("станция %s: %s: %v", FormatFreq(c.freq), c.title, err)
		}
		i := int(c.index.Load())
		c.closeDec()
		c.src.castSetPos(i + 1) // трек доигран
	}
}

// micFrame — кадр микрофона, если модератор говорит; nil — нет.
func (c *caster) micFrame() []byte {
	if !c.micReady {
		if len(c.mic) < micPrebuf {
			return nil
		}
		c.micReady = true
	}
	for len(c.mic) > micMax { // отстали — догоняем, выкидывая старое
		<-c.mic
	}
	select {
	case f := <-c.mic:
		return f
	default:
		c.micReady = false // кончился запас — копим заново
		return nil
	}
}

// mixFrame — кадр в эфир: трек и микрофон по фейдерам. Без микрофона и с фейдерами на 1 — байты
// трека как есть, без перекодирования.
func (c *caster) mixFrame(music, mic []byte) []byte {
	gm, gv, ga := c.gains.get()
	out := make([]byte, FrameBytes)
	if mic == nil && gm == 1 && ga == 1 && music != nil {
		copy(out, music)
		return out
	}
	for i := range out {
		var v float64
		if music != nil {
			v += ulawToLinear[music[i]] * gm
		}
		if mic != nil && i < len(mic) {
			v += ulawToLinear[mic[i]] * gv
		}
		out[i] = linearToUlaw(v * ga)
	}
	return out
}

// send — кадр в эфир; соединения нет — подключается (волна занята — ждёт), после подключения
// заново шлёт название трека.
func (c *caster) send(ctx context.Context, frame []byte) error {
	for {
		fresh := c.conn == nil
		if err := c.ensure(ctx); err != nil {
			return err
		}
		if fresh && c.title != "" {
			c.writeTitle(c.title)
		}
		for pending := true; pending; { // действия студии с письмами
			select {
			case b := <-c.hostOut:
				c.write(websocket.TextMessage, b)
			default:
				pending = false
			}
		}
		if c.write(websocket.BinaryMessage, frame) == nil {
			c.onAir.Store(true)
			return nil
		}
		c.hangUp()
	}
}

func (c *caster) setTitle(t string) {
	if t == c.title {
		return
	}
	c.title = t
	c.src.castTitle(t)
	if c.conn != nil && t != "" {
		c.writeTitle(t)
	}
}

func (c *caster) writeTitle(t string) {
	b, _ := json.Marshal(map[string]string{"title": t})
	if c.write(websocket.TextMessage, b) != nil {
		c.hangUp()
	}
}

// monitor — готовый эфир подписчикам страницы (не успевают — кадр теряется у них, не в эфире).
func (c *caster) monitor(frame []byte) {
	c.monMu.Lock()
	defer c.monMu.Unlock()
	for ch := range c.mon {
		select {
		case ch <- frame:
		default:
		}
	}
}

func (c *caster) subscribe() (chan []byte, func()) {
	ch := make(chan []byte, 50)
	c.monMu.Lock()
	c.mon[ch] = struct{}{}
	c.monMu.Unlock()
	return ch, func() {
		c.monMu.Lock()
		delete(c.mon, ch)
		c.monMu.Unlock()
	}
}

// pushMic — кадр с микрофона страницы.
func (c *caster) pushMic(f []byte) {
	select {
	case c.mic <- f:
	default: // переполнено — кадр теряется, micFrame догонит
	}
}

func (c *caster) closeDec() {
	if c.dec != nil {
		c.dec.close()
		c.dec = nil
	}
}

// trackTitle — название трека в эфире: имя файла без расширения.
func trackTitle(file string) string {
	b := filepath.Base(file)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

func (c *caster) write(typ int, b []byte) error {
	if c.conn == nil {
		return errors.New("нет соединения")
	}
	select {
	case <-c.closed:
		return errors.New("соединение закрыто")
	default:
	}
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(typ, b)
}

// ensure — есть живое соединение с радио; нет — подключается, с растущей паузой между попытками
// (волна занята ведущим из браузера — 409 — ждём, пока освободится).
func (c *caster) ensure(ctx context.Context) error {
	if c.conn != nil {
		select {
		case <-c.closed:
			c.hangUp()
		default:
			return nil
		}
	}
	if c.banned.Load() {
		return errCastBanned
	}
	wait := time.Second
	for {
		err := c.dial(ctx)
		if err == nil {
			return nil
		}
		log.Printf("станция %s: %v — повтор через %v", FormatFreq(c.freq), err, wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func (c *caster) dial(ctx context.Context) error {
	q := url.Values{"f": {fmt.Sprint(c.freq)}, "name": {c.name}}
	hdr := http.Header{"Cookie": {"kontakt_id=" + c.id}}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, c.url+"?"+q.Encode(), hdr)
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			resp.Body.Close()
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return err
	}
	c.conn, c.closed = conn, make(chan struct{})
	c.infoMu.Lock()
	c.info.Likes, c.info.Card = 0, false // у нового эфира свой счёт лайков (как у ведущего в браузере)
	c.infoMu.Unlock()
	go c.read(conn, c.closed)
	return nil
}

// read — сообщения радио ведущему: слушатели, лайки, письма, итог жалобы, жёлтая карточка — для
// студии; видит закрытие и код бана.
func (c *caster) read(conn *websocket.Conn, closed chan struct{}) {
	defer close(closed)
	for {
		typ, b, err := conn.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) && ce.Code == castBanCode {
				c.banned.Store(true)
			}
			return
		}
		if typ == websocket.TextMessage {
			c.hostMessage(b)
		}
	}
}

// castLetter — письмо слушателя ведущему (только в памяти, как у радио).
type castLetter struct {
	ID      uint64    `json:"id"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Blocked bool      `json:"blocked"` // письма от этого слушателя больше не принимаются
	Report  bool      `json:"report"`  // на письмо пожаловались
}

// castInfo — что видит ведущий: слушатели, лайки за эфир, письма, итог последней жалобы.
type castInfo struct {
	Listeners int          `json:"listeners"`
	Likes     int          `json:"likes"`
	Card      bool         `json:"card"` // жёлтая карточка станции
	Letters   []castLetter `json:"letters"`
	Report    string       `json:"lastReport,omitempty"`
}

func (c *caster) hostMessage(b []byte) {
	var m struct {
		Listeners *int `json:"listeners"`
		Total     *int `json:"total"`
		Letter    *struct {
			ID   uint64 `json:"id"`
			Text string `json:"text"`
		} `json:"letter"`
		LetterReport string `json:"letterReport"`
		Card         string `json:"card"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	c.infoMu.Lock()
	defer c.infoMu.Unlock()
	if m.Listeners != nil {
		c.info.Listeners = *m.Listeners
	}
	if m.Total != nil {
		c.info.Likes = *m.Total
	}
	if m.Letter != nil && m.Letter.Text != "" {
		c.info.Letters = append([]castLetter{{ID: m.Letter.ID, Text: m.Letter.Text, At: time.Now()}}, c.info.Letters...)
		if len(c.info.Letters) > letterMemory {
			c.info.Letters = c.info.Letters[:letterMemory]
		}
	}
	if m.LetterReport != "" {
		c.info.Report = m.LetterReport
	}
	if m.Card == "yellow" {
		c.info.Card = true
	}
}

// snapshot — копия castInfo для страницы.
func (c *caster) snapshot() castInfo {
	c.infoMu.Lock()
	defer c.infoMu.Unlock()
	in := c.info
	in.Letters = append([]castLetter(nil), c.info.Letters...)
	return in
}

// letterAction — «не принимать письма от него» (block) или «пожаловаться» (report): уходит радио
// через основной цикл станции — писать в сокет может только он.
func (c *caster) letterAction(id uint64, report bool) bool {
	c.infoMu.Lock()
	found := false
	for i := range c.info.Letters {
		if c.info.Letters[i].ID == id {
			found = true
			c.info.Letters[i].Blocked = true
			c.info.Letters[i].Report = c.info.Letters[i].Report || report
		}
	}
	c.infoMu.Unlock()
	if !found {
		return false
	}
	key := "block"
	if report {
		key = "reportLetter"
	}
	b, _ := json.Marshal(map[string]uint64{key: id})
	select {
	case c.hostOut <- b:
		return true
	default:
		return false
	}
}

func (c *caster) hangUp() {
	c.onAir.Store(false)
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// castDecoder — ffmpeg, который отдаёт файл кадрами μ-law по 160 байт.
type castDecoder struct {
	cmd    *exec.Cmd
	r      *bufio.Reader
	stderr *strings.Builder
	buf    []byte
	frames int
}

func startDecoder(ctx context.Context, ffmpeg, file string) (*castDecoder, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", file, "-ac", "1", "-ar", "8000", "-f", "mulaw", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	d := &castDecoder{cmd: cmd, r: bufio.NewReaderSize(out, 64<<10), stderr: &strings.Builder{}, buf: make([]byte, FrameBytes)}
	cmd.Stderr = d.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	return d, nil
}

// frame — следующий кадр; io.EOF — файл кончился.
func (d *castDecoder) frame() ([]byte, error) {
	n, err := io.ReadFull(d.r, d.buf)
	if n == 0 {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if d.cmd.Wait() != nil && d.stderr.Len() > 0 {
				return nil, fmt.Errorf("ffmpeg: %s", strings.TrimSpace(d.stderr.String()))
			}
			return nil, io.EOF
		}
		return nil, err
	}
	for i := n; i < len(d.buf); i++ { // хвост файла — тишиной до целого кадра
		d.buf[i] = 0xff
	}
	d.frames++
	return d.buf, nil
}

func (d *castDecoder) close() {
	d.cmd.Process.Kill()
	d.cmd.Wait()
}

// G.711 μ-law: байт ↔ отсчёт −1…1 (как в странице радио).
var ulawToLinear = func() (t [256]float64) {
	for i := range t {
		u := ^byte(i)
		sign, exp, mant := u&0x80, (u>>4)&7, int(u&0x0f)
		s := (((mant << 3) + 0x84) << exp) - 0x84
		if sign != 0 {
			s = -s
		}
		t[i] = float64(s) / 32768
	}
	return
}()

func linearToUlaw(v float64) byte {
	s := int(math.Max(-1, math.Min(1, v)) * 32767)
	var sign byte
	if s < 0 {
		sign, s = 0x80, -s
	}
	if s > 32635 {
		s = 32635
	}
	s += 0x84
	exp := byte(7)
	for m := 0x4000; s&m == 0 && exp > 0; m >>= 1 {
		exp--
	}
	mant := byte(s>>(exp+3)) & 0x0f
	return ^(sign | exp<<4 | mant)
}
