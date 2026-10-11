package webapp

import (
	"net/http/httptest"
	"net/url"
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
