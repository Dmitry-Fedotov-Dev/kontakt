// radiocast — постоянная станция Открытого радио, которая вещает с самого сервера: папка с
// аудиофайлами по кругу, без браузера и без интернета между ведущим и радио. У слушателя такая
// станция звучит ровнее всех: провалы чаще всего рождаются в сети ведущего (INC-001).
//
//	radiocast -f 934 -name "9¾" -dir /opt/kontakt/stations/934 -id-file /opt/kontakt/data/cast-934.id
//
// Файлы декодирует ffmpeg (G.711 μ-law, 8 кГц, моно); кадры по 160 байт уходят в /ws/host радио
// строго по часам — 50 в секунду. Название трека — имя файла без расширения. Связь оборвалась —
// переподключается и продолжает с того же места; станцию сняли модерацией (4003) — выходит с кодом
// 3, и systemd больше её не поднимает (RestartPreventExitStatus=3). Папка пуста — ждёт файлов, не
// занимая волну.
//
// Ведущий опознаётся кукой kontakt_id, как браузер: её значение хранится в -id-file и не меняется,
// поэтому за станцией держатся модерация и закреплённый логотип.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"kontakt/internal/radio"
)

const (
	frameDur  = 20 * time.Millisecond
	exitBan   = 3 // станцию сняли модерацией: не перезапускаться
	closeBan  = 4003
	emptyWait = 30 * time.Second
)

var audioExt = map[string]bool{".mp3": true, ".ogg": true, ".opus": true, ".m4a": true, ".aac": true, ".flac": true, ".wav": true}

func main() {
	freq := flag.String("f", "", "частота: 934 или 93.4")
	name := flag.String("name", "", "название станции (до 24 знаков)")
	dir := flag.String("dir", "", "папка с аудиофайлами; играются по кругу в порядке имён")
	server := flag.String("url", "ws://127.0.0.1:8083/ws/host", "сокет ведущего радио")
	idFile := flag.String("id-file", "", "файл с куки ведущего (создаётся сам)")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "путь к ffmpeg")
	flag.Parse()
	f, ok := radio.ParseFreq(*freq)
	if !ok || *dir == "" || *idFile == "" {
		fmt.Fprintln(os.Stderr, "нужны -f, -dir и -id-file")
		os.Exit(2)
	}
	id, err := loadID(*idFile)
	if err != nil {
		log.Fatalf("куки ведущего: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &caster{freq: f, name: strings.TrimSpace(*name), dir: *dir, server: *server, id: id, ffmpeg: *ffmpeg}
	if err := c.run(ctx); errors.Is(err, errBanned) {
		log.Print("станцию сняли с эфира модерацией — остановлено")
		os.Exit(exitBan)
	} else if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

// loadID — постоянная кука ведущего: 32 шестнадцатеричных знака, как у браузера.
func loadID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) == 32 {
			return s, nil
		}
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	s := hex.EncodeToString(buf)
	return s, os.WriteFile(path, []byte(s+"\n"), 0o600)
}

var errBanned = errors.New("станция снята модерацией")

type caster struct {
	freq       int
	name, dir  string
	server, id string
	ffmpeg     string
	conn       *websocket.Conn
	closed     chan struct{} // закрывается, когда читающая горутина увидела конец соединения
	banned     atomic.Bool   // радио закрыло эфир кодом 4003
	frames     uint64
}

func (c *caster) run(ctx context.Context) error {
	for ctx.Err() == nil {
		files := listAudio(c.dir)
		if len(files) == 0 {
			c.hangUp()
			log.Printf("в %s нет аудиофайлов — жду", c.dir)
			select {
			case <-ctx.Done():
			case <-time.After(emptyWait):
			}
			continue
		}
		for _, file := range files {
			if err := c.play(ctx, file); err != nil {
				if errors.Is(err, errBanned) || ctx.Err() != nil {
					c.hangUp()
					return err
				}
				log.Printf("%s: %v", filepath.Base(file), err)
			}
		}
	}
	c.hangUp()
	return ctx.Err()
}

// listAudio — аудиофайлы папки в порядке имён (без подпапок).
func listAudio(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && audioExt[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// play — один файл: ffmpeg декодирует, кадры уходят по часам. Обрыв связи посреди файла —
// переподключение и продолжение с того же места.
func (c *caster) play(ctx context.Context, file string) error {
	cmd := exec.CommandContext(ctx, c.ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", file, "-ac", "1", "-ar", "8000", "-f", "mulaw", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	title := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if err := c.ensure(ctx); err != nil {
		return err
	}
	c.sendTitle(title)
	r := bufio.NewReaderSize(out, 64<<10)
	frame := make([]byte, radio.FrameBytes)
	next := time.Now()
	for {
		n, err := io.ReadFull(r, frame)
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if w := cmd.Wait(); w != nil && stderr.Len() > 0 {
					return fmt.Errorf("ffmpeg: %s", strings.TrimSpace(stderr.String()))
				}
				return nil
			}
			return err
		}
		for i := n; i < len(frame); i++ { // хвост файла — тишиной до целого кадра
			frame[i] = 0xff
		}
		// по часам, а не по тикам: задержка одного кадра не копится в дрейф
		if d := time.Until(next); d > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		} else if d < -time.Second { // долго стояли (переподключение) — начинаем отсчёт заново
			next = time.Now()
		}
		next = next.Add(frameDur)
		for {
			if err := c.ensure(ctx); err != nil {
				return err
			}
			if c.write(websocket.BinaryMessage, frame) == nil {
				c.frames++
				break
			}
			c.hangUp() // запись не прошла — переподключаемся и шлём тот же кадр
			c.sendTitleOnReconnect(title)
		}
	}
}

func (c *caster) sendTitle(t string) {
	b, _ := json.Marshal(map[string]string{"title": t})
	if c.write(websocket.TextMessage, b) != nil {
		c.hangUp()
	}
}

// sendTitleOnReconnect — после переподключения радио не помнит трек: шлём заново.
func (c *caster) sendTitleOnReconnect(t string) {
	if c.conn != nil {
		c.sendTitle(t)
	}
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

// ensure — есть живое соединение с радио; нет — подключается, с паузой между попытками.
func (c *caster) ensure(ctx context.Context) error {
	if c.banned.Load() {
		return errBanned
	}
	if c.conn != nil {
		select {
		case <-c.closed:
			c.hangUp()
			if c.banned.Load() {
				return errBanned
			}
		default:
			return nil
		}
	}
	wait := time.Second
	for {
		err := c.dial(ctx)
		if err == nil {
			return nil
		}
		log.Printf("радио: %v — повтор через %v", err, wait)
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
	h := http.Header{"Cookie": {"kontakt_id=" + c.id}}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, c.server+"?"+q.Encode(), h)
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			resp.Body.Close()
			// 409 — волна занята (ведущий из браузера), 503 — нет мест: ждём и пробуем снова
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return err
	}
	c.conn, c.closed = conn, make(chan struct{})
	go c.read(conn, c.closed)
	log.Printf("в эфире на %s: %q", radio.FormatFreq(c.freq), c.name)
	return nil
}

// read — читает и выбрасывает сообщения радио (слушатели, лайки, письма), иначе не дойдут
// управляющие кадры; видит закрытие и код 4003 (бан).
func (c *caster) read(conn *websocket.Conn, closed chan struct{}) {
	defer close(closed)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) && ce.Code == closeBan {
				c.banned.Store(true)
			}
			return
		}
	}
}

func (c *caster) hangUp() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}
