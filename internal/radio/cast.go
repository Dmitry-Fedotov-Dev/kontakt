package radio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Вещание с сервера: «ведущий» без браузера внутри процесса радио. Файлы станции по кругу; ffmpeg
// декодирует их в G.711 μ-law 8 кГц, кадры по 160 байт уходят в собственный /ws/host радио по
// localhost строго по часам — 50 в секунду. Через сокет, а не напрямую в хаб: станция с сервера
// проходит тот же допуск, модерацию, названия треков и логотипы, что и ведущий из браузера.
// У слушателя такая станция звучит ровнее всех: между ведущим и радио нет интернета (INC-001).

const (
	castFrame     = 20 * time.Millisecond
	castEmptyWait = 30 * time.Second
	castBanCode   = 4003
)

var errCastBanned = errors.New("станция снята модерацией")

// castSource — откуда caster берёт эфир: список файлов по порядку и где он остановился.
type castSource interface {
	castFiles() []string // полные пути, в порядке эфира
	castPos() int        // номер файла, с которого продолжать
	castPlayed(i int)    // файл i доигран
	castTitle(t string)  // сейчас играет
}

type caster struct {
	freq   int
	name   string
	url    string // ws://…/ws/host
	id     string // кука ведущего
	ffmpeg string
	src    castSource

	conn   *websocket.Conn
	closed chan struct{}
	banned atomic.Bool
}

// run — эфир до отмены ctx или бана (errCastBanned).
func (c *caster) run(ctx context.Context) error {
	defer c.hangUp()
	for ctx.Err() == nil {
		files := c.src.castFiles()
		if len(files) == 0 {
			c.hangUp() // пустая станция волну не занимает
			c.src.castTitle("")
			select {
			case <-ctx.Done():
			case <-time.After(castEmptyWait):
			}
			continue
		}
		i := c.src.castPos() % len(files)
		if err := c.play(ctx, files[i]); err != nil {
			if errors.Is(err, errCastBanned) || ctx.Err() != nil {
				return err
			}
			log.Printf("станция %s: %s: %v", FormatFreq(c.freq), filepath.Base(files[i]), err)
			select { // битый файл не должен крутиться в цикле без пауз
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		c.src.castPlayed(i)
	}
	return ctx.Err()
}

// play — один файл. Обрыв связи посреди файла — переподключение и продолжение с того же места.
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
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	title := trackTitle(file)
	c.src.castTitle(title)
	if err := c.ensure(ctx); err != nil {
		return err
	}
	c.sendTitle(title)
	r := bufio.NewReaderSize(out, 64<<10)
	frame := make([]byte, FrameBytes)
	next := time.Now()
	for {
		n, err := io.ReadFull(r, frame)
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if cmd.Wait() != nil && stderr.Len() > 0 {
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
		} else if d < -time.Second { // долго стояли (переподключение) — отсчёт заново
			next = time.Now()
		}
		next = next.Add(castFrame)
		for {
			if err := c.ensure(ctx); err != nil {
				return err
			}
			if c.write(websocket.BinaryMessage, frame) == nil {
				break
			}
			c.hangUp() // не ушло — переподключаемся, шлём тот же кадр и заново название трека
			if err := c.ensure(ctx); err != nil {
				return err
			}
			c.sendTitle(title)
		}
	}
}

// trackTitle — название трека в эфире: имя файла без расширения.
func trackTitle(file string) string {
	b := filepath.Base(file)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

func (c *caster) sendTitle(t string) {
	b, _ := json.Marshal(map[string]string{"title": t})
	if c.write(websocket.TextMessage, b) != nil {
		c.hangUp()
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
	go c.read(conn, c.closed)
	return nil
}

// read — читает и выбрасывает сообщения радио (слушатели, лайки, письма), иначе не дойдут
// управляющие кадры; видит закрытие и код бана.
func (c *caster) read(conn *websocket.Conn, closed chan struct{}) {
	defer close(closed)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) && ce.Code == castBanCode {
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
