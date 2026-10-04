package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"kontakt/internal/moderation"
)

// fakeTelegram записывает вызовы Bot API.
type fakeTelegram struct {
	mu    sync.Mutex
	calls []map[string]any // с полем "_method"
}

func (f *fakeTelegram) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/botTOKEN/") {
			t.Errorf("запрос без токена: %s", r.URL.Path)
		}
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		p["_method"] = strings.TrimPrefix(r.URL.Path, "/botTOKEN/")
		f.mu.Lock()
		f.calls = append(f.calls, p)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeTelegram) texts(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c["_method"] == method {
			out = append(out, c["text"].(string))
		}
	}
	return out
}

func newService(t *testing.T) (*Service, *fakeTelegram) {
	tg := &fakeTelegram{}
	return &Service{Bot: NewBot(tg.server(t).URL, "TOKEN", 42)}, tg
}

func TestAlertsFiringAndResolved(t *testing.T) {
	s, tg := newService(t)
	var mu sync.Mutex
	body := `{"data":{"alerts":[{"state":"firing","labels":{"alertname":"RadioDown","severity":"critical"},"annotations":{"summary":"Радио не отвечает (r)"}},
		{"state":"pending","labels":{"alertname":"StationDown"},"annotations":{}}]}}`
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if body == "" {
			http.Error(w, "down", 500)
			return
		}
		w.Write([]byte(body))
	}))
	defer prom.Close()
	s.Prometheus = prom.URL

	s.CheckAlerts()
	s.CheckAlerts() // та же тревога — второй раз не пишется
	if got := tg.texts("sendMessage"); len(got) != 1 || !strings.Contains(got[0], "RadioDown") || !strings.Contains(got[0], "Радио не отвечает") {
		t.Fatalf("загорелась: %q", got)
	}
	mu.Lock()
	body = `{"data":{"alerts":[]}}`
	mu.Unlock()
	s.CheckAlerts()
	if got := tg.texts("sendMessage"); len(got) != 2 || !strings.Contains(got[1], "Прошло") {
		t.Fatalf("прошла: %q", got)
	}
	mu.Lock()
	body = ""
	mu.Unlock()
	for i := 0; i < 5; i++ {
		s.CheckAlerts()
	}
	if got := tg.texts("sendMessage"); len(got) != 3 || !strings.Contains(got[2], "Prometheus не отвечает") {
		t.Fatalf("Prometheus лёг: %q", got)
	}
}

func TestEventAndUnbanButton(t *testing.T) {
	s, tg := newService(t)
	var adminCalls []string
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminCalls = append(adminCalls, r.Method+" "+r.URL.RequestURI())
		w.Write([]byte("ok\n"))
	}))
	defer admin.Close()
	s.Admin = admin.URL
	key := strings.Repeat("ab", 16)

	ev := httptest.NewServer(s.EventHandler())
	defer ev.Close()
	PostEntry(ev.URL)(moderation.Entry{Action: "ban", Zone: moderation.Air, Key: key, By: "auto"})
	tg.mu.Lock()
	msg := tg.calls[0]
	tg.mu.Unlock()
	if !strings.Contains(msg["text"].(string), "Бан") || !strings.Contains(msg["text"].(string), key) {
		t.Fatalf("событие: %v", msg)
	}
	data := msg["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)["callback_data"].(string)

	var u Update
	json.Unmarshal([]byte(`{"update_id":1,"callback_query":{"id":"c1","data":"`+data+`","message":{"message_id":7,"text":"бан","chat":{"id":42}}}}`), &u)
	s.Handle(u)
	if len(adminCalls) != 1 || adminCalls[0] != "POST /admin/unban?key="+key+"&zone=air" {
		t.Fatalf("кнопка разбана: %v", adminCalls)
	}
	if e := tg.texts("editMessageText"); len(e) != 1 || !strings.Contains(e[0], "снято") {
		t.Fatalf("сообщение не отмечено: %v", e)
	}
}

// Чужой чат не управляет модерацией, даже зная ключ.
func TestForeignChatIgnored(t *testing.T) {
	s, tg := newService(t)
	called := false
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer admin.Close()
	s.Admin = admin.URL
	var u Update
	json.Unmarshal([]byte(`{"update_id":1,"message":{"chat":{"id":666},"text":"/ban `+strings.Repeat("ab", 16)+` all"}}`), &u)
	s.Handle(u)
	if called || len(tg.texts("sendMessage")) != 0 {
		t.Fatal("ответил чужому чату")
	}
}

func TestCommands(t *testing.T) {
	s, _ := newService(t)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/stats":
			w.Write([]byte(`{"signal":{"online":3,"waiting":1,"talking":2,"total_calls":9},"moderation":{"banned":{"air":1},"yellow":{"calls":2}}}`))
		case "/admin/journal":
			w.Write([]byte(`[{"day":"2026-10-04","action":"yellow","zone":"calls","key":"k1","by":"auto"}]`))
		default:
			w.Write([]byte("ok"))
		}
	}))
	defer admin.Close()
	s.Admin = admin.URL
	s.init()
	for cmd, want := range map[string]string{
		"/stats":         "на линии 3",
		"/journal 5":     "yellow · звонки · auto",
		"/ban short all": "32 шестнадцатеричных",
		"/unban " + strings.Repeat("0", 32) + " nowhere": "зона",
		"/ban " + strings.Repeat("0", 32) + " all":       "готово",
		"/start": "/journal",
	} {
		if got := s.command(cmd); !strings.Contains(got, want) {
			t.Errorf("%s: %q, ждали %q", cmd, got, want)
		}
	}
}
