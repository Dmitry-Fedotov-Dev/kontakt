package e2e

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"kontakt/internal/identity"
	"kontakt/internal/radio"
)

// TestLive — проверка настоящей станции по её публичному адресу (туннель или домен):
//
//	KONTAKT_URL=https://….trycloudflare.com go test ./e2e -run TestLive -v
//
// Страницы трубки и радио отвечают, ведущий радио выходит в эфир на 87.7, слушатель получает его
// кадры байт в байт. Без KONTAKT_URL тест пропускается.
//
// Звонков здесь нет: звонковые тесты — только xk6-sip (CLAUDE.md), а он звонит по SIP/UDP,
// который через туннель не идёт. Звонок через публичный адрес — T-1, людьми в браузерах.
func TestLive(t *testing.T) {
	base := strings.TrimSuffix(os.Getenv("KONTAKT_URL"), "/")
	if base == "" {
		t.Skip("KONTAKT_URL не задан")
	}

	for _, path := range []string{"/", "/healthz", "/terms.html", "/radio/", "/radio/terms.html", "/radio/w/87.7?n=LIVE"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %s", path, resp.Status)
		}
	}

	// радио: ведущий на 87.7, слушатель получает его кадры
	wsBase := "ws" + strings.TrimPrefix(base, "http") + "/radio"
	hdr := func() http.Header { return http.Header{"Cookie": {identity.CookieName + "=" + identity.New()}} }
	host, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws/host?f=877&name=LIVE-TEST", hdr())
	if err != nil {
		t.Fatalf("эфир: %v (волна 87.7 занята?)", err)
	}
	defer host.Close()
	l, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws/listen", hdr())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.WriteJSON(map[string]int{"tune": 877})
	frame := bytes.Repeat([]byte{0x5a}, radio.FrameBytes)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				host.WriteMessage(websocket.BinaryMessage, frame)
			}
		}
	}()
	l.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, got, err := l.ReadMessage()
		if err != nil {
			t.Fatalf("слушатель не получил эфир: %v", err)
		}
		if typ == websocket.BinaryMessage {
			if !bytes.Equal(got, frame) {
				t.Fatalf("кадр искажён: %d байт", len(got))
			}
			t.Log("радио: кадр ведущего дошёл до слушателя байт в байт")
			return
		}
	}
}
