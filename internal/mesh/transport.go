package mesh

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// PathKind — тип сетевого пути ребра (§46): у них разные свойства, и маршрутизация
// их различает. Cloudflare — только один из вариантов, не основа (§2).
type PathKind string

const (
	PathDirect     PathKind = "direct"
	PathWebSocket  PathKind = "websocket"
	PathCloudflare PathKind = "cloudflare"
	PathWebRTC     PathKind = "webrtc"
)

// Conn — соединение с соседом: сообщения целиком, по одному (§16). Ядро mesh не
// знает, TCP под ним, WebSocket, туннель или DataChannel.
type Conn interface {
	Send(data []byte) error
	Receive() ([]byte, error)
	Close() error
	Kind() PathKind
	RemoteAddr() string
}

// Transport устанавливает соединения одного вида.
type Transport interface {
	Dial(addr string) (Conn, error)
	Kind() PathKind
}

// MaxFrame — предел одного сообщения. Управляющие сообщения малы, RTP — ещё меньше;
// предел защищает узел от «пришли мне 4 ГБ».
const MaxFrame = 64 << 10

var ErrFrameTooBig = errors.New("сообщение больше MaxFrame")

// ---------- TCP ----------

// TCPTransport — прямое TCP-соединение: длина (4 байта) + сообщение.
type TCPTransport struct{ Timeout time.Duration }

func (TCPTransport) Kind() PathKind { return PathDirect }

func (t TCPTransport) Dial(addr string) (Conn, error) {
	d := net.Dialer{Timeout: orDefault(t.Timeout, 5*time.Second)}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return newTCPConn(c), nil
}

type tcpConn struct {
	c  net.Conn
	r  *bufio.Reader
	wm sync.Mutex
}

func newTCPConn(c net.Conn) *tcpConn { return &tcpConn{c: c, r: bufio.NewReader(c)} }

func (c *tcpConn) Send(data []byte) error {
	if len(data) > MaxFrame {
		return ErrFrameTooBig
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(data)))
	c.wm.Lock()
	defer c.wm.Unlock()
	c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.c.Write(h[:]); err != nil {
		return err
	}
	_, err := c.c.Write(data)
	return err
}

func (c *tcpConn) Receive() ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n > MaxFrame {
		return nil, ErrFrameTooBig
	}
	b := make([]byte, n)
	_, err := io.ReadFull(c.r, b)
	return b, err
}

func (c *tcpConn) Close() error       { return c.c.Close() }
func (c *tcpConn) Kind() PathKind     { return PathDirect }
func (c *tcpConn) RemoteAddr() string { return c.c.RemoteAddr().String() }

// ListenTCP принимает соединения и отдаёт их в accept.
func ListenTCP(addr string, accept func(Conn)) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go accept(newTCPConn(c))
		}
	}()
	return l, nil
}

// ---------- WebSocket ----------

// WSTransport — WebSocket (ws:// или wss://). Тот же транспорт работает и через
// Cloudflare Tunnel: тогда Kind = cloudflare, протокол mesh не меняется (§2, §17).
type WSTransport struct {
	Via     PathKind // PathWebSocket или PathCloudflare
	Timeout time.Duration
}

func (t WSTransport) Kind() PathKind {
	if t.Via == "" {
		return PathWebSocket
	}
	return t.Via
}

func (t WSTransport) Dial(addr string) (Conn, error) {
	if !strings.HasPrefix(addr, "ws://") && !strings.HasPrefix(addr, "wss://") {
		addr = "ws://" + addr + "/mesh"
	}
	d := websocket.Dialer{HandshakeTimeout: orDefault(t.Timeout, 5*time.Second), Subprotocols: []string{"kontakt-mesh"}}
	c, _, err := d.Dial(addr, nil)
	if err != nil {
		return nil, fmt.Errorf("ws %s: %w", addr, err)
	}
	c.SetReadLimit(MaxFrame)
	return &wsConn{c: c, kind: t.Kind()}, nil
}

type wsConn struct {
	c    *websocket.Conn
	kind PathKind
	wm   sync.Mutex
}

func (c *wsConn) Send(data []byte) error {
	if len(data) > MaxFrame {
		return ErrFrameTooBig
	}
	c.wm.Lock()
	defer c.wm.Unlock()
	c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.c.WriteMessage(websocket.BinaryMessage, data)
}

func (c *wsConn) Receive() ([]byte, error) {
	_, b, err := c.c.ReadMessage()
	return b, err
}

func (c *wsConn) Close() error       { return c.c.Close() }
func (c *wsConn) Kind() PathKind     { return c.kind }
func (c *wsConn) RemoteAddr() string { return c.c.RemoteAddr().String() }

// WSHandler — входящие mesh-соединения по WebSocket (повесить на /mesh).
func WSHandler(accept func(Conn)) http.Handler {
	up := websocket.Upgrader{Subprotocols: []string{"kontakt-mesh"}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(MaxFrame)
		go accept(&wsConn{c: c, kind: PathWebSocket})
	})
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
