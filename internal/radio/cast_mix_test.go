package radio

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// μ-law туда и обратно — тот же байт (у нуля два кода: 0x7f и 0xff — оба тишина).
func TestUlawRoundTrip(t *testing.T) {
	for i := 0; i < 256; i++ {
		b := byte(i)
		got := linearToUlaw(ulawToLinear[b])
		if got != b && !(ulawToLinear[b] == 0 && ulawToLinear[got] == 0) {
			t.Fatalf("0x%02x → %v → 0x%02x", b, ulawToLinear[b], got)
		}
	}
}

// Сведение: без голоса и с фейдерами на 1 — трек байт в байт; голос подмешивается; фейдер голоса 0
// — его не слышно; фейдер эфира 0 — тишина.
func TestCastMixFrame(t *testing.T) {
	c := newCaster(934, "X", "", "", "", nil)
	music := bytes.Repeat([]byte{0x9a}, FrameBytes)
	voice := bytes.Repeat([]byte{0x20}, FrameBytes) // громкий отрицательный отсчёт
	if out := c.mixFrame(music, nil); !bytes.Equal(out, music) {
		t.Fatal("трек без голоса изменился")
	}
	mixed := c.mixFrame(music, voice)
	want := ulawToLinear[0x9a] + ulawToLinear[0x20]
	if d := ulawToLinear[mixed[0]] - want; d > 0.02 || d < -0.02 {
		t.Fatalf("сведение %v, ждали %v", ulawToLinear[mixed[0]], want)
	}
	c.gains.set(1, 0, 1)
	if out := c.mixFrame(music, voice); ulawToLinear[out[0]] != ulawToLinear[0x9a] {
		t.Fatal("голос слышно при фейдере 0")
	}
	c.gains.set(1, 1, 0)
	if out := c.mixFrame(music, voice); ulawToLinear[out[0]] != 0 {
		t.Fatal("эфир не заглушён фейдером")
	}
}

// listenFreq — слушатель на частоте: двоичные сообщения в канал.
func listenFreq(t *testing.T, srvURL string, f int) chan []byte {
	t.Helper()
	l, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srvURL, "http")+"/ws/listen", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	l.WriteJSON(map[string]int{"tune": f})
	ch := make(chan []byte, 512)
	go func() {
		for {
			typ, b, err := l.ReadMessage()
			if err != nil {
				close(ch)
				return
			}
			if typ == websocket.BinaryMessage {
				ch <- b
			}
		}
	}()
	return ch
}

// waitByte — ждёт в эфире байт want (кадр трека или голоса).
func waitByte(t *testing.T, ch chan []byte, want func(byte) bool, what string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				t.Fatalf("%s: слушателя отключили", what)
			}
			for _, x := range b {
				if want(x) {
					return
				}
			}
		case <-deadline:
			t.Fatalf("%s: не дождались", what)
		}
	}
}

// Очередь со страницы: «следующий» переключает трек сразу, «пауза» даёт тишину, «играть» —
// возвращает трек.
func TestCastControls(t *testing.T) {
	h, srv, _, api := castsForTest(t)
	call(t, api, "POST", "/stations/api/station", []byte(`{"f":"93.4","name":"X"}`))
	for _, n := range []string{"a.mp3", "b.mp3"} {
		call(t, api, "POST", "/stations/api/upload?f=934&name="+n, []byte("audio"))
	}
	call(t, api, "POST", "/stations/api/on?f=934", nil)
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	ch := listenFreq(t, srv.URL, 934)
	waitByte(t, ch, func(x byte) bool { return x == 'a' }, "первый трек")
	if w := call(t, api, "POST", "/stations/api/ctl?f=934&op=next", nil); w.Code != 200 {
		t.Fatalf("next: %d %s", w.Code, w.Body)
	}
	waitByte(t, ch, func(x byte) bool { return x == 'b' }, "следующий трек")
	call(t, api, "POST", "/stations/api/ctl?f=934&op=pause", nil)
	waitByte(t, ch, func(x byte) bool { return x == 0xff }, "тишина на паузе")
	call(t, api, "POST", "/stations/api/ctl?f=934&op=play", nil)
	waitByte(t, ch, func(x byte) bool { return x == 'a' || x == 'b' }, "трек после паузы")
}

// Станция без файлов выходит в эфир с микрофона страницы, голос доходит до слушателя.
func TestCastMicOnly(t *testing.T) {
	h, srv, c, api := castsForTest(t)
	admin := httptest.NewServer(api)
	defer admin.Close()
	call(t, api, "POST", "/stations/api/station", []byte(`{"f":"93.4","name":"Голос"}`))
	call(t, api, "POST", "/stations/api/on?f=934", nil)
	// без файлов и микрофона станция волну не занимает
	time.Sleep(300 * time.Millisecond)
	if len(h.Stations()) != 0 {
		t.Fatal("пустая станция заняла волну")
	}
	mic, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(admin.URL, "http")+"/stations/api/mic?f=934", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mic.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() { // голос: кадр 0x20 каждые 20 мс, как страница
		tk := time.NewTicker(castFrame)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				mic.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{0x20}, FrameBytes))
			}
		}
	}()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Name == "Голос" })
	ch := listenFreq(t, srv.URL, 934)
	waitByte(t, ch, func(x byte) bool { return x == 0x20 }, "голос в эфире")
	c.mu.Lock()
	micOn := c.st[934].cast != nil && c.st[934].cast.micOn.Load()
	c.mu.Unlock()
	if !micOn {
		t.Fatal("страница не видит, что микрофон в эфире")
	}
}
