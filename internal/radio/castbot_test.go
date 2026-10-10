package radio

import (
	"encoding/base64"
	"encoding/json"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeTelegram — Bot API для бота вещателей: обновления подкладывает тест, отправленное копится.
type fakeTelegram struct {
	srv  *httptest.Server
	ups  chan map[string]any
	mu   sync.Mutex
	sent []tgSent
	seq  int64
}

type tgSent struct {
	Chat int64
	Text string
	Data []string // callback_data кнопок
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{ups: make(chan map[string]any, 16)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var res any = true
		switch method {
		case "getMe":
			res = map[string]any{"id": 1, "username": "cast_bot"}
		case "getUpdates":
			var out []map[string]any
			select {
			case u := <-f.ups:
				f.mu.Lock()
				f.seq++
				u["update_id"] = f.seq
				f.mu.Unlock()
				out = append(out, u)
			case <-time.After(100 * time.Millisecond):
			}
			res = out
		case "sendMessage":
			s := tgSent{Chat: int64(p["chat_id"].(float64)), Text: p["text"].(string)}
			if rm, ok := p["reply_markup"].(map[string]any); ok {
				for _, row := range rm["inline_keyboard"].([]any) {
					for _, b := range row.([]any) {
						s.Data = append(s.Data, b.(map[string]any)["callback_data"].(string))
					}
				}
			}
			f.mu.Lock()
			f.sent = append(f.sent, s)
			f.mu.Unlock()
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": res})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// waitSent ждёт сообщение в чат chat с текстом, содержащим sub.
func (f *fakeTelegram) waitSent(t *testing.T, chat int64, sub string) tgSent {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		for _, s := range f.sent {
			if s.Chat == chat && strings.Contains(s.Text, sub) {
				f.mu.Unlock()
				return s
			}
		}
		f.mu.Unlock()
	}
	t.Fatalf("бот не написал в %d «%s»", chat, sub)
	return tgSent{}
}

func (f *fakeTelegram) message(from int64, text string) {
	f.ups <- map[string]any{"message": map[string]any{"chat": map[string]any{"id": from}, "text": text,
		"from": map[string]any{"id": from, "first_name": "Вещатель", "username": "caster"}}}
}

func (f *fakeTelegram) press(chat int64, data string) {
	f.ups <- map[string]any{"callback_query": map[string]any{"id": "cb", "data": data,
		"message": map[string]any{"message_id": 7, "text": "запрос", "chat": map[string]any{"id": chat}}}}
}

const botOwnerID, botUserID = 100, 555

func cookieHdr(id string) http.Header { return http.Header{"Cookie": {"kontakt_id=" + id}} }

func logoAccessOK(t *testing.T, srv *httptest.Server, id string) bool {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/logo-access", nil)
	req.Header = cookieHdr(id)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var m struct{ OK bool }
	json.NewDecoder(r.Body).Decode(&m)
	return m.OK
}

// logoLink — ссылка в бота для браузера id (ждёт, пока бот узнает своё имя).
func logoLink(t *testing.T, srv *httptest.Server, id string) string {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/logo-access", nil)
		req.Header = cookieHdr(id)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode == http.StatusOK {
			var m struct{ Link string }
			json.Unmarshal(b, &m)
			return m.Link
		}
	}
	t.Fatal("нет ссылки в бота")
	return ""
}

func waitText(t *testing.T, ch texts, sub string) {
	t.Helper()
	for end := time.After(3 * time.Second); ; {
		select {
		case s := <-ch:
			if strings.Contains(s, sub) {
				return
			}
		case <-end:
			t.Fatalf("ведущему не пришло %s", sub)
		}
	}
}

// Логотип по доступу из бота: без доступа — отказ; ссылка → «Старт» → владелец «Выдать» → ведущий в
// эфире узнаёт сразу и ставит логотип; тот же аккаунт из другого браузера — доступ без владельца;
// «Отозвать» снимает логотип с эфира; доступ переживает перезапуск.
func TestLogoAccessBot(t *testing.T) {
	tg := newFakeTelegram(t)
	dir := t.TempDir()
	h := NewHub(Options{LogosDir: dir, BotToken: "T", BotOwner: botOwnerID, BotAPI: tg.srv.URL})
	defer h.Close()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	page, _ := http.Get(srv.URL + "/")
	b, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(b), `<meta name="host-logos" content="bot">`) {
		t.Fatal("в странице нет метатега host-logos=bot")
	}

	id := strings.Repeat("ab", 16)
	if logoAccessOK(t, srv, id) {
		t.Fatal("доступ до выдачи")
	}
	host, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/host?f=1012&name=K", cookieHdr(id))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ht := readTexts(host)
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	logo := "data:image/png;base64," + base64.StdEncoding.EncodeToString(testLogo(t, 64, color.White))
	host.WriteJSON(map[string]string{"logo": logo})
	waitText(t, ht, `"logoAccess":false`)
	if h.Stations()[0].Logo != "" {
		t.Fatal("логотип без доступа")
	}

	link := logoLink(t, srv, id)
	if !strings.HasPrefix(link, "https://t.me/cast_bot?start=logo_") {
		t.Fatalf("ссылка: %s", link)
	}
	if again := logoLink(t, srv, id); again != link {
		t.Fatal("повторное нажатие — новый код, а не тот же")
	}
	code := strings.TrimPrefix(link, "https://t.me/cast_bot?start=logo_")
	tg.message(botUserID, "/start logo_"+code)
	tg.waitSent(t, botUserID, "Запрос отправлен")
	card := tg.waitSent(t, botOwnerID, "просит доступ")
	if len(card.Data) != 2 || card.Data[0] != "g:"+code {
		t.Fatalf("кнопки владельцу: %v", card.Data)
	}
	tg.press(botUserID, "g:"+code) // кнопка не из чата владельца — ничего
	tg.press(botOwnerID, "g:"+code)
	waitText(t, ht, `"logoAccess":true`)
	tg.waitSent(t, botUserID, "выдан")
	if !logoAccessOK(t, srv, id) {
		t.Fatal("после выдачи доступа нет")
	}
	host.WriteJSON(map[string]string{"logo": logo})
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Logo != "" })

	// тот же аккаунт из другого браузера — сразу, без владельца
	id2 := strings.Repeat("cd", 16)
	code2 := strings.TrimPrefix(logoLink(t, srv, id2), "https://t.me/cast_bot?start=logo_")
	tg.message(botUserID, "/start logo_"+code2)
	tg.waitSent(t, botUserID, "Логотип открыт")
	if !logoAccessOK(t, srv, id2) {
		t.Fatal("второй браузер того же аккаунта без доступа")
	}

	// перезапуск: доступ на диске
	if a := openLogoAccess(dir); !a.Allowed(id) || !a.Allowed(id2) {
		t.Fatal("доступ не сохранился")
	}

	// отзыв: логотип ведущего снят с эфира
	tg.message(botOwnerID, "/list")
	list := tg.waitSent(t, botOwnerID, "Доступ к логотипу")
	if len(list.Data) != 1 || list.Data[0] != "r:555" {
		t.Fatalf("/list: %+v", list)
	}
	tg.press(botOwnerID, "r:555")
	waitText(t, ht, `"logoAccess":false`)
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Logo == "" })
	if logoAccessOK(t, srv, id) || logoAccessOK(t, srv, id2) {
		t.Fatal("доступ после отзыва")
	}

	// чужая или устаревшая ссылка
	tg.message(777, "/start logo_0000")
	tg.waitSent(t, 777, "Ссылка устарела")
	tg.message(777, "привет")
	tg.waitSent(t, 777, "бот вещателей")
}

// Без бота и без -host-logos ссылки нет, метатега нет; с -host-logos доступ у всех.
func TestLogoAccessModes(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	r, _ := http.Post(srv.URL+"/api/logo-access", "", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("без бота: %d", r.StatusCode)
	}
	if logoAccessOK(t, srv, strings.Repeat("ab", 16)) {
		t.Fatal("без бота — доступ")
	}
	h2 := NewHub(Options{HostLogos: true, BotToken: "T", BotOwner: 1, BotAPI: "http://127.0.0.1:1"})
	defer h2.Close()
	if h2.bot != nil {
		t.Fatal("с -host-logos бот не нужен")
	}
	srv2 := httptest.NewServer(h2.Handler())
	defer srv2.Close()
	if !logoAccessOK(t, srv2, "") {
		t.Fatal("-host-logos: доступ у всех")
	}
}
