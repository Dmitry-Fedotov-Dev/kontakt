// Package webapp — входная точка «Контакта»: пиксельная трубка, вечная кука,
// прокси /sip и /api → signal, /media → media, /radio/ → Открытое радио.
package webapp

import (
	"bytes"
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
	invite := invitePage()

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
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && validRoom(r.URL.Query().Get("call")) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(invite)
			return
		}
		files.ServeHTTP(w, r)
	}))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/media") {
			identity.Ensure(w, r)
		}
		if !strings.HasPrefix(r.URL.Path, RadioPrefix+"/") { // радио ставит эти заголовки само
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=()")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'") // трубка во фрейме чужого сайта — тот же кликджекинг с микрофоном, что у радио
			w.Header().Set("X-Frame-Options", "DENY")
		}
		mux.ServeHTTP(w, r)
	})
}

// Приглашение по ссылке /?call=<комната> — та же трубка, но превью в мессенджерах (их боты JS не
// выполняют, читают заголовки страницы) зовёт к разговору, а не в рулетку.
const (
	inviteTitle = "Вам звонят"
	inviteDesc  = "Абонент на линии приглашает вас к разговору. Откройте ссылку — и трубка соединит вас один на один."
)

func invitePage() []byte {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		panic(err)
	}
	head := []byte(`<title>` + inviteTitle + `</title>
<meta name="description" content="` + inviteDesc + `">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Контакт">
<meta property="og:title" content="` + inviteTitle + `">
<meta property="og:description" content="` + inviteDesc + `">`)
	i, j := bytes.Index(page, []byte("<title>")), bytes.Index(page, []byte(`<meta name="theme-color"`))
	if i < 0 || j < i {
		panic("webapp: в index.html не найдены <title> и описание")
	}
	out := append(append([]byte{}, page[:i]...), head...)
	return append(append(out, '\n'), page[j:]...)
}

// validRoom — имя комнаты как у signal: 8–32 знака a-z0-9.
func validRoom(r string) bool {
	if len(r) < 8 || len(r) > 32 {
		return false
	}
	for _, c := range r {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
