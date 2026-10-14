package webapp

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Трубка не встраивается в чужие страницы: кликджекинг с микрофоном.
func TestNoFraming(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	h := Handler(Upstreams{Signal: u, Media: u})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("заголовки против фрейма: %v", w.Header())
	}
}

// Ссылка на звонок другу: превью в мессенджерах зовёт к разговору; без комнаты — обычная трубка.
func TestInvitePreview(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	h := Handler(Upstreams{Signal: u, Media: u})
	get := func(path string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Body.String()
	}
	inv := get("/?call=5tapr1r3cwgy")
	if !strings.Contains(inv, `<meta property="og:title" content="`+inviteTitle+`">`) || strings.Contains(inv, `content="Позвоните другу по ссылке`) ||
		strings.Count(inv, "<title>") != 1 || !strings.Contains(inv, `id="link"`) {
		t.Fatalf("превью приглашения:\n%.900s", inv)
	}
	for _, p := range []string{"/", "/?call=../x"} {
		if b := get(p); strings.Contains(b, inviteTitle) || !strings.Contains(b, "<title>Контакт</title>") {
			t.Fatalf("%s — не обычная трубка", p)
		}
	}
}

// Шумодав трубки: wasm RNNoise отдаётся с типом application/wasm (иначе браузер его не соберёт) и с лицензией.
func TestRNNoiseServed(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	h := Handler(Upstreams{Signal: u, Media: u})
	for path, ctype := range map[string]string{"/rnnoise.wasm": "application/wasm", "/rnnoise-LICENSE.txt": "text/plain"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), ctype) || w.Body.Len() < 1000 {
			t.Fatalf("%s: %d %q %d байт", path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
}
