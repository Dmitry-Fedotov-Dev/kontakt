package radio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	for i := 0; i < 300; i++ { // 6 с эфира разом
		host.WriteMessage(websocket.BinaryMessage, frame(1))
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.framesIn.Value()+h.dropped.Value("host_rate") < 300 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	in := h.framesIn.Value()
	if in > burstBytes/FrameBytes+10 || in < burstBytes/FrameBytes {
		t.Fatalf("принято %d кадров, ждали около %d", in, burstBytes/FrameBytes)
	}
	if d := h.dropped.Value("host_rate"); d+in != 300 {
		t.Fatalf("отброшено сверх 64 кбит/с: %d, принято %d — в сумме должно быть 300", d, in)
	}
}

// Часы звуковой карты ведущего спешат на 2,4 % (51,2 кадра/с вместо 50 — так было на стенде):
// сервер это пропускает целиком, ничего не отбрасывая.
func TestHostClockDriftNotDropped(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=950")
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	tick := time.NewTicker(time.Second / 512 * 10) // 51,2 кадра/с
	defer tick.Stop()
	for i := 0; i < 256; i++ { // 5 с эфира
		<-tick.C
		host.WriteMessage(websocket.BinaryMessage, frame(1))
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.framesIn.Value()+h.dropped.Value("host_rate") < 256 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d := h.dropped.Value("host_rate"); d != 0 {
		t.Fatalf("отброшено %d кадров ведущего, у которого часы спешат на 2,4 %%", d)
	}
}

// Метрики радио: что видит Prometheus после эфира, слушателя, отказа и ссылки на волну.
func TestRadioMetrics(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	l, _, _ := dial(t, srv, "/ws/listen")
	defer l.Close()
	l.WriteMessage(websocket.TextMessage, []byte(`{"tune":1017}`))
	host, _, _ := dial(t, srv, "/ws/host?f=101.7")
	dial(t, srv, "/ws/host?f=101.7") // занято
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Listeners == 1 })
	for i := 0; i < 5; i++ {
		host.WriteMessage(websocket.BinaryMessage, frame(9))
	}
	readAudio(t, l)
	http.Get(srv.URL + "/w/101.7?t=x")
	http.Get(srv.URL + "/og/1017.png?t=x")
	http.Get(srv.URL + "/og/1017.png?t=x") // второй раз — из кеша
	for i := 0; h.framesOut.Value() < 5 && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := http.Get(srv.URL + "/metrics")
	b, _ := io.ReadAll(r.Body)
	m := string(b)
	for _, want := range []string{
		"kontakt_radio_stations 1",
		"kontakt_radio_listeners 1",
		"kontakt_radio_listeners_tuned 1",
		`kontakt_radio_station_listeners{freq="101.7"} 1`,
		"kontakt_radio_frames_in_total 5",
		"kontakt_radio_frames_out_total 5",
		"kontakt_radio_bytes_in_total 800",
		`kontakt_radio_rejected_total{reason="busy"} 1`,
		`kontakt_radio_frames_dropped_total{reason="slow_listener"} 0`,
		"kontakt_radio_host_sessions_total 1",
		`kontakt_radio_share_views_total{kind="page"} 1`,
		`kontakt_radio_share_views_total{kind="image"} 2`,
		"kontakt_radio_og_renders_total 1",
	} {
		if !strings.Contains(m, want+"\n") {
			t.Errorf("нет %q", want)
		}
	}
	host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 0 })
	for i := 0; h.onAir.Count() == 0 && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if h.onAir.Count() != 1 {
		t.Fatal("длительность эфира не записана после ухода станции")
	}
}

// -admin: метрики уходят со страницы радио (и из туннеля) на отдельный адрес.
func TestPrivateMetrics(t *testing.T) {
	h := NewHub(Options{PrivateMetrics: true})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	r, _ := http.Get(srv.URL + "/metrics")
	if r.StatusCode == 200 {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "kontakt_radio") {
			t.Fatal("метрики видны на публичном адресе")
		}
	}
	w := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), "kontakt_radio_stations") {
		t.Fatal("служебный адрес не отдаёт метрики")
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

// texts читает сокет в своей горутине (после тайм-аута чтения gorilla/websocket соединение уже
// не читает) и отдаёт текстовые сообщения; collect — всё, что пришло за d.
type texts chan string

func readTexts(c *websocket.Conn) texts {
	ch := make(texts, 64)
	go func() {
		for {
			typ, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			if typ == websocket.TextMessage {
				ch <- string(b)
			}
		}
	}()
	return ch
}

func (ch texts) collect(d time.Duration) []string {
	var out []string
	end := time.After(d)
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		case <-end:
			return out
		}
	}
}

// Поворот ручки и подключения не рассылают список станций всем: раньше каждое такое событие
// стоило 6,9 КБ × все приёмники. Смена трека — короткое сообщение, а не весь список.
func TestListNotResentOnTune(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=1053&name=X")
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	l, _, _ := dial(t, srv, "/ws/listen")
	defer l.Close()
	lt := readTexts(l)
	if got := lt.collect(1500 * time.Millisecond); len(got) != 1 {
		t.Fatalf("при подключении ждали один список: %q", got)
	}

	for i := 0; i < 10; i++ { // другие приёмники приходят и крутят ручку
		o, _, _ := dial(t, srv, "/ws/listen")
		o.WriteJSON(map[string]int{"tune": 1053})
		o.WriteJSON(map[string]int{"tune": 900 + i})
		defer o.Close()
	}
	if got := lt.collect(2500 * time.Millisecond); len(got) != 0 {
		t.Fatalf("список разослан из-за чужих приёмников: %q", got)
	}

	host.WriteJSON(map[string]string{"title": "песня"})
	got := lt.collect(2500 * time.Millisecond)
	if len(got) != 1 || got[0] != `{"title":{"f":1053,"t":"песня"}}` {
		t.Fatalf("после смены трека ждали короткое сообщение о треке: %q", got)
	}

	// новый приёмник получает полный список уже с новым треком и без числа слушателей
	n, _, _ := dial(t, srv, "/ws/listen")
	defer n.Close()
	first := readTexts(n).collect(time.Second)
	if len(first) != 1 || !strings.Contains(first[0], `"t":"песня"`) || strings.Contains(first[0], `"l":`) {
		t.Fatalf("список новому приёмнику: %q", first)
	}
}

func TestHostGetsListenerCount(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=1053&name=X")
	defer host.Close()
	ht := readTexts(host)
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	for i := 0; i < 2; i++ {
		l, _, _ := dial(t, srv, "/ws/listen")
		defer l.Close()
		l.WriteJSON(map[string]int{"tune": 1053})
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range ht.collect(time.Second) {
			if m == `{"listeners":2}` {
				return
			}
		}
	}
	t.Fatal("ведущий не узнал, что его слушают двое")
}

// Список кодируется один раз на версию: байты в метрике = размер списка × число приёмников.
func TestListBytesMetric(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=1053&name=X")
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	var size int
	for i := 0; i < 5; i++ {
		l, _, _ := dial(t, srv, "/ws/listen")
		defer l.Close()
		got := readTexts(l).collect(300 * time.Millisecond)
		if len(got) != 1 {
			t.Fatalf("приёмник %d: %q", i, got)
		}
		size = len(got[0])
	}
	want := fmt.Sprintf("kontakt_radio_list_bytes_total %d", 5*size)
	rec := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("ждали %q", want)
	}
}
