package radio

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dial(t *testing.T, srv *httptest.Server, path string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, nil)
}

// readAudio ждёт первый двоичный кадр, пропуская списки станций.
func readAudio(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		typ, b, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("кадр не пришёл: %v", err)
		}
		if typ == websocket.BinaryMessage {
			return b
		}
	}
}

func waitStations(t *testing.T, h *Hub, cond func([]Station) bool) []Station {
	t.Helper()
	for i := 0; i < 300; i++ {
		if s := h.Stations(); cond(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("не дождались: %+v", h.Stations())
	return nil
}

func frame(b byte) []byte { return bytes.Repeat([]byte{b}, FrameBytes) }

func TestParseFreq(t *testing.T) {
	for in, want := range map[string]int{"101.7": 1017, "1017": 1017, "87.5": 875, "108": 0, "108.0": 1080, "1081": 0, "x": 0, "87.4": 0} {
		got, ok := ParseFreq(in)
		if (want == 0) == ok || (ok && got != want) {
			t.Errorf("ParseFreq(%q) = %d,%v; ждали %d", in, got, ok, want)
		}
	}
}

// Слушатель на частоте ведущего получает его кадры байт в байт, на соседней — ничего.
func TestRelayToTunedListenerOnly(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	on, _, err := dial(t, srv, "/ws/listen")
	if err != nil {
		t.Fatal(err)
	}
	defer on.Close()
	off, _, _ := dial(t, srv, "/ws/listen")
	defer off.Close()
	on.WriteMessage(websocket.TextMessage, []byte(`{"tune":1017}`)) // настроен ДО выхода станции в эфир
	off.WriteMessage(websocket.TextMessage, []byte(`{"tune":1018}`))

	host, _, err := dial(t, srv, "/ws/host?f=101.7&name=%D0%9F%D0%B8%D1%80%D0%B0%D1%82%0A")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	st := waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Listeners == 1 })
	if st[0].Name != "Пират" || st[0].Freq != 1017 {
		t.Fatalf("станция %+v: имя должно быть очищено от перевода строки", st[0])
	}

	host.WriteMessage(websocket.BinaryMessage, frame(0x55))
	if got := readAudio(t, on); !bytes.Equal(got, frame(0x55)) {
		t.Fatalf("кадр искажён: %d байт", len(got))
	}
	off.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		typ, _, err := off.ReadMessage()
		if err != nil {
			break // таймаут — ничего не пришло, так и надо
		}
		if typ == websocket.BinaryMessage {
			t.Fatal("соседняя частота слышит чужой эфир")
		}
	}

	host.WriteMessage(websocket.TextMessage, []byte(`{"title":"track\u0007 one"}`))
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Title == "track one" })
}

func TestBusyFrequencyRejected(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	a, _, err := dial(t, srv, "/ws/host?f=1000")
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := dial(t, srv, "/ws/host?f=100.0")
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("вторая станция на занятой частоте: err=%v resp=%v", err, resp)
	}
	_, resp, err = dial(t, srv, "/ws/host?f=120.0")
	if err == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("частота вне диапазона принята: %v", err)
	}
	a.Close() // ведущий ушёл — частота освободилась
	waitStations(t, h, func(s []Station) bool { return len(s) == 0 })
	b, _, err := dial(t, srv, "/ws/host?f=1000")
	if err != nil {
		t.Fatalf("освободившаяся частота не занимается: %v", err)
	}
	b.Close()
}

func TestStationLimit(t *testing.T) {
	h := NewHub(Options{MaxStations: 1})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	a, _, _ := dial(t, srv, "/ws/host?f=900")
	defer a.Close()
	_, resp, err := dial(t, srv, "/ws/host?f=901")
	if err == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("лимит станций не сработал: %v", err)
	}
}

// Больше 64 кбит/с в эфир не уходит: присланное сверх ведра токенов отбрасывается.
func TestHostRateLimited(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=950")
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	for i := 0; i < 200; i++ { // 4 с эфира разом
		host.WriteMessage(websocket.BinaryMessage, frame(1))
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.framesIn.Value()+h.dropped.Value() < 200 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	in := h.framesIn.Value()
	if in > burstBytes/FrameBytes+10 || in < burstBytes/FrameBytes {
		t.Fatalf("принято %d кадров, ждали около %d", in, burstBytes/FrameBytes)
	}
}

// Список станций приходит приёмнику сам, без запроса.
func TestListenerGetsStationList(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=1053&name=X")
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	l, _, _ := dial(t, srv, "/ws/listen")
	defer l.Close()
	l.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, b, err := l.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var m struct{ Stations []Station }
	if json.Unmarshal(b, &m) != nil || len(m.Stations) != 1 || m.Stations[0].Freq != 1053 {
		t.Fatalf("список: %s", b)
	}
}

func TestPageServed(t *testing.T) {
	srv := httptest.NewServer(NewHub(Options{}).Handler())
	defer srv.Close()
	r, err := http.Get(srv.URL + "/")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("страница: %v %v", err, r)
	}
}
