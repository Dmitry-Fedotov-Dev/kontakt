package radio

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// streamHub — радио, у которого вместо ffmpeg — тестовый бинарник: он отдаёт μ-law как есть
// (TestMain, «-i pipe:0»), так видно, что именно ушло в кодер.
func streamHub(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	t.Setenv("RADIO_FAKE_FFMPEG", "1")
	h := NewHub(Options{})
	h.ffmpeg, _ = os.Executable()
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return h, srv
}

// readUntil читает поток, пока в нём не появится want.
func readUntil(t *testing.T, r io.Reader, want []byte, what string) {
	t.Helper()
	got := make(chan bool, 1)
	go func() {
		buf := make([]byte, 4096)
		var seen []byte
		for {
			n, err := r.Read(buf)
			seen = append(seen, buf[:n]...)
			if bytes.Contains(seen, want) {
				got <- true
				return
			}
			if err != nil {
				got <- false
				return
			}
		}
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("поток оборвался: нет %s", what)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("за 3 с нет %s", what)
	}
}

// Фоновый приёмник: звук станции идёт в кодер, станция видит слушателя.
func TestStreamCarriesStation(t *testing.T) {
	h, srv := streamHub(t)
	host, _, err := dial(t, srv, "/ws/host?f=99.6&name=K")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })

	r, err := http.Get(srv.URL + "/stream/99.6")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if ct := r.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type: %s", ct)
	}
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Listeners == 1 })
	go func() {
		for i := 0; i < 20; i++ {
			host.WriteMessage(websocket.BinaryMessage, frame(0x80))
			time.Sleep(20 * time.Millisecond)
		}
	}()
	readUntil(t, r.Body, frame(0x80), "звука станции")
	r.Body.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Listeners == 0 })
}

// Станции нет — поток не стоит: в кодер идёт тишина, иначе браузер встанет «за нехваткой данных».
func TestStreamSilenceWithoutStation(t *testing.T) {
	_, srv := streamHub(t)
	r, err := http.Get(srv.URL + "/stream/88.8")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	readUntil(t, r.Body, bytes.Repeat([]byte{ulawSilent}, 1000), "тишины")
	if r, _ := http.Get(srv.URL + "/stream/200.0"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("частота вне шкалы: %d", r.StatusCode)
	}
}

// Без ffmpeg фонового приёма нет — честный отказ, страница остаётся на Web Audio.
func TestStreamWithoutEncoder(t *testing.T) {
	h := NewHub(Options{})
	h.ffmpeg = ""
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	r, err := http.Get(srv.URL + "/stream/99.6")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("без ffmpeg: %d", r.StatusCode)
	}
}
