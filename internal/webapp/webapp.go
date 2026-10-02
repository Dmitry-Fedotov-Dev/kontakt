// Package webapp — входная точка «Контакта»: пиксельная трубка, вечная кука,
// прокси /sip и /api → signal, /media → media.
package webapp

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"kontakt/internal/identity"
)

//go:embed static
var staticFS embed.FS

func Handler(signalURL, mediaURL *url.URL) http.Handler {
	quiet := log.New(discard{}, "", 0) // прокси не пишет в журнал ничего об абонентах
	sigProxy := httputil.NewSingleHostReverseProxy(signalURL)
	sigProxy.ErrorLog = quiet
	mediaProxy := httputil.NewSingleHostReverseProxy(mediaURL)
	mediaProxy.ErrorLog = quiet

	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(static))

	mux := http.NewServeMux()
	mux.Handle("/sip", sigProxy)
	mux.Handle("/api/", sigProxy)
	mux.Handle("/media", mediaProxy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("/", files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if identity.FromRequest(r) == "" && !strings.HasPrefix(r.URL.Path, "/media") {
			id := identity.New()
			http.SetCookie(w, &http.Cookie{
				Name: identity.CookieName, Value: id, Path: "/",
				MaxAge: 10 * 365 * 24 * 3600, Expires: time.Now().AddDate(10, 0, 0),
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
				Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
			})
			r.AddCookie(&http.Cookie{Name: identity.CookieName, Value: id}) // чтобы и этот запрос уже был «с кукой»
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=()")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
