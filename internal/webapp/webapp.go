// Package webapp — входная точка «Контакта»: пиксельная трубка, вечная кука,
// прокси /sip и /api → signal, /media → media, /radio/ → Открытое радио.
package webapp

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"kontakt/internal/identity"
)

//go:embed static
var staticFS embed.FS

// Upstreams — куда web проксирует. Radio == nil — радио на этом домене нет.
type Upstreams struct {
	Signal, Media, Radio *url.URL
}

// RadioPrefix — радио под тем же доменом, что и рулетка: одна кука kontakt_id, одна модерация.
const RadioPrefix = "/radio"

func Handler(up Upstreams) http.Handler {
	quiet := log.New(discard{}, "", 0) // прокси не пишет в журнал ничего об абонентах
	proxy := func(u *url.URL) *httputil.ReverseProxy {
		p := httputil.NewSingleHostReverseProxy(u)
		p.ErrorLog = quiet
		return p
	}
	sigProxy := proxy(up.Signal)
	mediaProxy := proxy(up.Media)

	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(static))

	mux := http.NewServeMux()
	mux.Handle("/sip", sigProxy)
	mux.Handle("/api/", sigProxy)
	mux.Handle("/media", mediaProxy)
	if up.Radio != nil {
		rp := proxy(up.Radio)
		director := rp.Director
		rp.Director = func(r *http.Request) {
			director(r)
			// По нему радио строит абсолютные ссылки (og:url, og:image) с /radio.
			r.Header.Set("X-Forwarded-Prefix", RadioPrefix)
		}
		mux.Handle(RadioPrefix+"/", http.StripPrefix(RadioPrefix, rp))
		mux.Handle("/r/", rp) // короткие ссылки на волну — от корня домена, короче на «/radio»
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("/", files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/media") {
			identity.Ensure(w, r)
		}
		if !strings.HasPrefix(r.URL.Path, RadioPrefix+"/") { // радио ставит эти заголовки само
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=()")
			w.Header().Set("X-Content-Type-Options", "nosniff")
		}
		mux.ServeHTTP(w, r)
	})
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
