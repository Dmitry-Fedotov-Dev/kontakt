package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"kontakt/internal/radio"
)

// Вместо ffmpeg тест запускает сам себя: RADIOCAST_FAKE_FFMPEG=1 — «декодер», который печатает
// fakeFrames кадров, заполненных первым байтом имени файла (по нему тест узнаёт трек).
const fakeFrames = 30

func TestMain(m *testing.M) {
	if os.Getenv("RADIOCAST_FAKE_FFMPEG") == "1" {
		in := ""
		for i, a := range os.Args {
			if a == "-i" && i+1 < len(os.Args) {
				in = os.Args[i+1]
			}
		}
		b := []byte(strings.Repeat(string(filepath.Base(in)[0]), fakeFrames*radio.FrameBytes))
		os.Stdout.Write(b)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Станция с сервера: выходит на волну под своим названием, шлёт кадры по часам (50 в секунду)
// и названия треков, файлы играются по порядку имён.
func TestCasterOnAir(t *testing.T) {
	h := radio.NewHub(radio.Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	dir := t.TempDir()
	for _, n := range []string{"b-second.mp3", "a-first.mp3", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	t.Setenv("RADIOCAST_FAKE_FFMPEG", "1")
	self, _ := os.Executable()
	c := &caster{freq: 934, name: "9¾", dir: dir, id: strings.Repeat("ab", 16), ffmpeg: self,
		server: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/host"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if st := h.Stations(); len(st) == 1 && st[0].Freq == 934 && st[0].Name == "9¾" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("станция не вышла в эфир: %+v", h.Stations())
		}
		time.Sleep(20 * time.Millisecond)
	}

	l, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/listen", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.WriteJSON(map[string]int{"tune": 934})
	msgs := make(chan []byte, 256)
	go func() {
		for {
			typ, b, err := l.ReadMessage()
			if err != nil {
				close(msgs)
				return
			}
			if typ == websocket.BinaryMessage {
				msgs <- b
			}
		}
	}()
	// Ждём смены трека: после кадров «a» должны прийти кадры «b», и между ними —
	// примерно fakeFrames × 20 мс (кадры уходят по часам, а не разом).
	var firstA, firstB time.Time
	for firstB.IsZero() {
		select {
		case b, ok := <-msgs:
			if !ok {
				t.Fatal("слушателя отключили")
			}
			for _, x := range b {
				switch {
				case x == 'a' && firstA.IsZero():
					firstA = time.Now()
				case x == 'b' && firstB.IsZero():
					firstB = time.Now()
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("кадры станции не дошли до слушателя")
		}
	}
	if !firstA.IsZero() {
		if d := firstB.Sub(firstA); d < fakeFrames*frameDur/3 {
			t.Fatalf("трек из %d кадров ушёл за %v — кадры не по часам", fakeFrames, d)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("не остановился по отмене")
	}
}

// Бан станции модерацией (код закрытия 4003) — caster выходит с errBanned и не переподключается.
func TestCasterStopsOnBan(t *testing.T) {
	up := websocket.Upgrader{}
	dials := 0
	srv := httptest.NewServer(httpHandler(func(conn *websocket.Conn) {
		dials++
		conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(closeBan, "banned"), time.Now().Add(time.Second))
		conn.Close()
	}, &up))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.mp3"), []byte("x"), 0o644)
	t.Setenv("RADIOCAST_FAKE_FFMPEG", "1")
	self, _ := os.Executable()
	c := &caster{freq: 934, name: "X", dir: dir, id: strings.Repeat("cd", 16), ffmpeg: self,
		server: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/host"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.run(ctx); err != errBanned {
		t.Fatalf("ждали errBanned, получили %v", err)
	}
	if dials > 2 {
		t.Fatalf("после бана переподключался: %d раз", dials)
	}
}

func TestLoadIDStable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "id")
	a, err := loadID(p)
	if err != nil || len(a) != 32 {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := loadID(p); b != a {
		t.Fatalf("кука сменилась: %s → %s", a, b)
	}
}

// httpHandler — сокет, который отдаёт каждое соединение fn.
func httpHandler(fn func(*websocket.Conn), up *websocket.Upgrader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, err := up.Upgrade(w, r, nil); err == nil {
			fn(conn)
		}
	})
}
