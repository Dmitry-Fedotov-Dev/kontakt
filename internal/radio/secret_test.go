package radio

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeKiwi — KiwiSDR: после «SET auth» шлёт audio_rate и sample_rate, после настройки на 4625 кГц —
// тон 1 кГц пачками по 512 отсчётов; open — сколько соединений сейчас открыто.
func fakeKiwi(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	var open atomic.Int32
	var tuned atomic.Value
	tuned.Store("")
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/SND") {
			http.NotFound(w, r)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		open.Add(1)
		defer open.Add(-1)
		defer c.Close()
		msg := func(s string) { c.WriteMessage(websocket.BinaryMessage, append([]byte("MSG "), s...)) }
		ready := make(chan struct{})
		go func() {
			for {
				_, b, err := c.ReadMessage()
				if err != nil {
					return
				}
				s := string(b)
				if strings.HasPrefix(s, "SET auth") {
					msg("badp=0") // настоящие приёмники так говорят «пароль не нужен»
					msg("audio_rate=12000")
					msg("sample_rate=12000.000")
				}
				if strings.HasPrefix(s, "SET mod=") {
					tuned.Store(s)
					select {
					case <-ready:
					default:
						close(ready)
					}
				}
			}
		}()
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			return
		}
		n := 0
		for seq := uint32(0); ; seq++ {
			b := make([]byte, 3+7+1024)
			copy(b, "SND")
			binary.LittleEndian.PutUint32(b[4:], seq)
			binary.BigEndian.PutUint16(b[8:], 1000)
			for i := 0; i < 512; i++ {
				v := int16(12000 * math.Sin(2*math.Pi*1000*float64(n)/12000))
				binary.BigEndian.PutUint16(b[10+2*i:], uint16(v))
				n++
			}
			if c.WriteMessage(websocket.BinaryMessage, b) != nil {
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &open, &tuned
}

func kiwiListServer(t *testing.T, kiwiURL string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `// list
var kiwisdr_com =
[
	{"url":"%s", "gps":"(59.4, 24.7)", "users":"0", "users_max":"4", "bands":"0-30000000", "offline":"no"},
	{"url":"http://busy.example:8073", "gps":"(57.9, 28.3)", "users":"4", "users_max":"4", "bands":"0-30000000"},
	{"url":"http://mw-only.example:8073", "gps":"(57.8, 28.3)", "users":"0", "users_max":"4", "bands":"530000-1720000"},
];
`, kiwiURL)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Список приёмников: занятые и не слышащие 4625 кГц отброшены, адрес — host:port.
func TestKiwiList(t *testing.T) {
	list := kiwiListServer(t, "http://near.example:8073")
	hosts, err := fetchKiwiList(list.URL)
	if err != nil || len(hosts) != 1 || hosts[0] != "near.example:8073" {
		t.Fatalf("%v %v", hosts, err)
	}
}

// Секретная волна: в списке станций её нет, страница знает о ней по метатегу; настройка на 4625
// будит зеркало — звук с KiwiSDR доходит слушателю; ушёл последний — зеркало отключается.
func TestSecretWave(t *testing.T) {
	kiwi, open, tuned := fakeKiwi(t)
	list := kiwiListServer(t, kiwi.URL)
	h := NewHub(Options{SecretList: list.URL, SecretIdle: 300 * time.Millisecond})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	page, _ := http.Get(srv.URL + "/")
	b, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(b), `<meta name="secret-wave" content="0.13">`) {
		t.Fatal("нет метатега secret-wave")
	}

	l, _, err := dial(t, srv, "/ws/listen")
	if err != nil {
		t.Fatal(err)
	}
	l.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"tune":%d}`, SecretTune)))
	audio := readAudio(t, l)
	if len(audio) == 0 {
		t.Fatal("звука нет")
	}
	if s, _ := tuned.Load().(string); !strings.Contains(s, "freq=4625.000") || !strings.Contains(s, "mod=am") {
		t.Fatalf("приёмник настроен не на 4625 AM: %q", s)
	}
	if len(h.Stations()) != 0 {
		t.Fatal("секретная волна попала в список станций")
	}
	l.Close()
	for end := time.Now().Add(5 * time.Second); open.Load() > 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("слушателей нет, а зеркало держит приёмник")
		}
	}

	// прогрев: страница просит подготовить звук — приёмник подключается без слушателей на волне
	w, _, err := dial(t, srv, "/ws/listen")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.WriteMessage(websocket.TextMessage, []byte(`{"warm":true}`))
	for end := time.Now().Add(5 * time.Second); open.Load() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("прогрев не подключил приёмник")
		}
	}
	for end := time.Now().Add(5 * time.Second); open.Load() > 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("прогрев держит приёмник дольше SecretIdle")
		}
	}

	// без -secret 4625 — просто «между станциями»
	h2 := NewHub(Options{})
	if h2.secret != nil {
		t.Fatal("секретная волна без списка приёмников")
	}
}

// 12 кГц → 8 кГц: отсчётов вдвое-втрое меньше, тон 1 кГц остаётся 1 кГц.
func TestResampler(t *testing.T) {
	r := newResampler(12000)
	be := make([]byte, 2*12000)
	for i := 0; i < 12000; i++ {
		binary.BigEndian.PutUint16(be[2*i:], uint16(int16(16000*math.Sin(2*math.Pi*1000*float64(i)/12000))))
	}
	out := r.push(be, nil)
	if len(out) < 7900 || len(out) > 8000 {
		t.Fatalf("отсчётов на секунду: %d", len(out))
	}
	cross := 0
	prev := ulawToLinear[out[100]]
	for _, c := range out[101:] {
		v := ulawToLinear[c]
		if (v < 0) != (prev < 0) {
			cross++
		}
		prev = v
	}
	if f := float64(cross) / 2 * 8000 / float64(len(out)-101); f < 950 || f > 1050 {
		t.Fatalf("частота после пересчёта %.0f Гц", f)
	}
}
