package radio

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Вместо ffmpeg тесты запускают сам тестовый бинарник: RADIO_FAKE_FFMPEG=1 — «декодер», который
// печатает fakeFrames кадров, заполненных первым байтом имени файла (по нему тест узнаёт трек),
// а с «-f null» (проверка при загрузке) — молча соглашается, если файл не начинается с «bad».
const fakeFrames = 30

func TestMain(m *testing.M) {
	if os.Getenv("RADIO_FAKE_FFMPEG") == "1" {
		in := ""
		for i, a := range os.Args {
			if a == "-i" && i+1 < len(os.Args) {
				in = os.Args[i+1]
			}
		}
		b, _ := os.ReadFile(in)
		if strings.HasPrefix(string(b), "bad") {
			os.Stderr.WriteString("Invalid data found when processing input")
			os.Exit(1)
		}
		if os.Args[len(os.Args)-2] == "null" {
			os.Exit(0)
		}
		os.Stdout.Write(bytes.Repeat([]byte{filepath.Base(in)[0]}, fakeFrames*FrameBytes))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func castsForTest(t *testing.T) (*Hub, *httptest.Server, *Casts, http.Handler) {
	t.Helper()
	t.Setenv("RADIO_FAKE_FFMPEG", "1")
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	c, err := h.OpenCasts(t.TempDir(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/host", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	c.ffmpeg = self
	t.Cleanup(func() {
		for f := range c.st {
			c.stop(f)
		}
	})
	return h, srv, c, LocalOnly(c.Handler())
}

func call(t *testing.T, hd http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://localhost:8093"+path, bytes.NewReader(body))
	w := httptest.NewRecorder()
	hd.ServeHTTP(w, r)
	return w
}

// Станция с сервера через страницу: создать, загрузить файлы, включить — она в эфире под своим
// названием, слушатель получает кадры по часам, треки идут в заданном порядке.
func TestCastsOnAir(t *testing.T) {
	h, srv, _, api := castsForTest(t)
	if w := call(t, api, "POST", "/stations/api/station", []byte(`{"f":"93.4","name":"9¾"}`)); w.Code != 200 {
		t.Fatalf("создать: %d %s", w.Code, w.Body)
	}
	for _, n := range []string{"a-first.mp3", "b-second.mp3"} {
		if w := call(t, api, "POST", "/stations/api/upload?f=934&name="+n, []byte("audio")); w.Code != 200 {
			t.Fatalf("загрузка %s: %d %s", n, w.Code, w.Body)
		}
	}
	if w := call(t, api, "POST", "/stations/api/upload?f=934&name=bad.mp3", []byte("bad data")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("битый файл принят: %d %s", w.Code, w.Body)
	}
	if w := call(t, api, "POST", "/stations/api/upload?f=934&name=notes.txt", []byte("x")); w.Code != http.StatusBadRequest {
		t.Fatalf("не аудио принято: %d", w.Code)
	}
	// порядок: сначала b, потом a
	call(t, api, "POST", "/stations/api/order?f=934", []byte(`["b-second.mp3","a-first.mp3"]`))
	if w := call(t, api, "POST", "/stations/api/on?f=934", nil); w.Code != 200 {
		t.Fatalf("в эфир: %d %s", w.Code, w.Body)
	}
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Freq == 934 && s[0].Name == "9¾" })

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
	var order []byte
	start := time.Now()
	for len(order) < 2 {
		select {
		case b, ok := <-msgs:
			if !ok {
				t.Fatal("слушателя отключили")
			}
			for _, x := range b {
				if x != 0xff && (len(order) == 0 || order[len(order)-1] != x) {
					order = append(order, x)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("кадры не дошли, пришло %q", order)
		}
	}
	if string(order) != "ba" {
		t.Fatalf("порядок треков %q, ждали «ba»", order)
	}
	if d := time.Since(start); d < fakeFrames*castFrame/2 {
		t.Fatalf("трек из %d кадров ушёл за %v — не по часам", fakeFrames, d)
	}

	var list struct {
		Stations []castJSON `json:"stations"`
	}
	json.Unmarshal(call(t, api, "GET", "/stations/api", nil).Body.Bytes(), &list)
	if len(list.Stations) != 1 || !list.Stations[0].OnAir || len(list.Stations[0].Files) != 2 {
		t.Fatalf("список: %+v", list.Stations)
	}
	if w := call(t, api, "DELETE", "/stations/api/station?f=934", nil); w.Code != http.StatusConflict {
		t.Fatalf("удалили станцию в эфире: %d", w.Code)
	}
	call(t, api, "POST", "/stations/api/off?f=934", nil)
	waitStations(t, h, func(s []Station) bool { return len(s) == 0 })
}

// Админ-порт отвечает только localhost и только своей странице.
func TestLocalOnly(t *testing.T) {
	_, _, _, api := castsForTest(t)
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"localhost:8093", "", 200},
		{"127.0.0.1:8093", "http://127.0.0.1:8093", 200},
		{"evil.example:8093", "", 403},                   // DNS rebinding
		{"localhost:8093", "https://evil.example", 403},  // чужая страница
		{"localhost:8093", "http://localhost:9999", 403}, // чужой порт на localhost
	} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/stations/api", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("Host %s, Origin %q: %d, ждали %d", tc.host, tc.origin, w.Code, tc.want)
		}
	}
}

// Бан станции модерацией (код 4003) — caster останавливается и не переподключается.
func TestCasterStopsOnBan(t *testing.T) {
	up := websocket.Upgrader{}
	dials := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, err := up.Upgrade(w, r, nil); err == nil {
			dials++
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(castBanCode, "banned"), time.Now().Add(time.Second))
			conn.Close()
		}
	}))
	defer srv.Close()
	_, _, c, api := castsForTest(t)
	c.url = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/host"
	call(t, api, "POST", "/stations/api/station", []byte(`{"f":"93.4","name":"X"}`))
	call(t, api, "POST", "/stations/api/upload?f=934&name=a.mp3", []byte("audio"))
	call(t, api, "POST", "/stations/api/on?f=934", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		s := c.st[934]
		stopped := !s.On && s.cancel == nil
		c.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("после бана станция не остановилась")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dials > 2 {
		t.Fatalf("после бана переподключался: %d раз", dials)
	}
}

func TestCleanFileName(t *testing.T) {
	for in, want := range map[string]string{
		"song.mp3":         "song.mp3",
		"../../etc/x.mp3":  "x.mp3",
		`C:\music\a b.MP3`: "a b.MP3",
		".hidden.mp3":      "",
		"station.json":     "",
		"notes.txt":        "",
		"a:b?.flac":        "a_b_.flac",
	} {
		got, ok := cleanFileName(in)
		if !ok {
			got = ""
		}
		if got != want {
			t.Errorf("%q → %q, ждали %q", in, got, want)
		}
	}
}
