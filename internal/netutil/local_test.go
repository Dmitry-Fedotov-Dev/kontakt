package netutil

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalOnly(t *testing.T) {
	h := LocalOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, tc := range []struct {
		method, host, path, origin string
		want                       int
	}{
		{"POST", "127.0.0.1:8091", "/admin/unban", "", 200},                        // curl, бот, радио
		{"POST", "localhost:8091", "/admin/ban", "http://localhost:8091", 200},     // своя страница
		{"POST", "localhost:8091", "/admin/unban", "https://evil.example", 403},    // CSRF с чужой вкладки
		{"POST", "localhost:8091", "/mod/report", "null", 403},                     // песочница, file://
		{"POST", "localhost:8091", "/admin/ban", "http://localhost:9999", 403},     // чужой порт на localhost
		{"GET", "evil.example:8091", "/admin/journal", "", 403},                    // DNS rebinding
		{"GET", "host.docker.internal:8091", "/metrics", "", 200},                  // Prometheus в Docker Desktop
		{"GET", "evil.example:8091", "/metrics", "https://evil.example:8091", 200}, // DNS rebinding прочтёт метрики — в них только счётчики
		{"POST", "evil.example:8091", "/metrics", "", 403},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s%s, Origin %q: %d, ждали %d", tc.method, tc.host, tc.path, tc.origin, w.Code, tc.want)
		}
	}
}
