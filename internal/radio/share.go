package radio

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Ссылка на волну: /w/101.7?n=<станция>&t=<что играло, когда копировали ссылку>.
// Страница та же, что и главная, только с метатегами Open Graph: мессенджер, получив ссылку,
// сам приходит за ними и показывает карточку с картинкой /og/1017.png?n=…&t=….
//
// Что играло, сервер не хранит: истории эфира нет вовсе. Название трека едет в самой ссылке —
// поэтому карточка показывает ровно ту песню, что звучала в момент копирования.
//
// Язык карточки — язык того, кто поделился: l=en в ссылке (нет — русский, как у старых ссылок).
// Главная страница радио — по Accept-Language. Сама страница потом говорит на языке открывшего.

// ogText — тексты карточки на двух языках.
type ogText struct {
	Site, Desc, Live, Silent, Played, Freq string
}

var ogTexts = map[string]ogText{
	"ru": {
		Site:   "Открытое радио",
		Desc:   "Займи свою волну и вещай: музыка и микрофон. Или крути ручку и слушай, что в эфире.",
		Live:   "Открой и докрути ручку — эфир идёт прямо сейчас.",
		Silent: "Сейчас станция молчит — загляни позже или покрути ручку.",
		Played: "♪ Играло: %s. ",
		Freq:   " МГц",
	},
	"en": {
		Site:   "Open Radio",
		Desc:   "Take your own frequency and broadcast music and voice. Or turn the knob and listen to what's on air.",
		Live:   "Open it and tune in — it's on air right now.",
		Silent: "The station is silent now — come back later or turn the knob.",
		Played: "♪ Was playing: %s. ",
		Freq:   " FM",
	},
}

// langParam — язык ссылки: только en или ru, остальное — ru.
func langParam(v string) string {
	if v == "en" {
		return "en"
	}
	return "ru"
}

// acceptLang — язык по заголовку браузера: русский для ru, uk, be, kk, ky (там русский понимают
// лучше английского), иначе английский. Без заголовка (так ходят мессенджеры) — русский.
func acceptLang(r *http.Request) string {
	h := r.Header.Get("Accept-Language")
	if strings.TrimSpace(h) == "" {
		return "ru"
	}
	for _, part := range strings.Split(h, ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		if tag == "" || tag == "*" {
			continue
		}
		switch strings.SplitN(tag, "-", 2)[0] {
		case "ru", "uk", "be", "kk", "ky":
			return "ru"
		}
		return "en"
	}
	return "ru"
}

const ogPlaceholder = "<!--OG-->"

var indexHTML = func() string {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil || !strings.Contains(string(b), ogPlaceholder) {
		panic("radio: static/index.html без " + ogPlaceholder) // ловится тестом, до выкладки не доедет
	}
	return string(b)
}()

// hostOK — только то, что бывает в имени хоста: Host приходит от клиента и попадает в разметку.
var hostOK = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]+)?$|^\[[0-9A-Fa-f:]+\](:[0-9]+)?$`)

// prefixOK — путь, под которым радио отдаёт web (/radio): тоже от клиента и тоже в разметку.
var prefixOK = regexp.MustCompile(`^(/[a-z0-9-]+)+$`)

// baseURL — абсолютный адрес радио: og:image обязан быть абсолютным. За туннелем Cloudflare
// и обратным прокси о https говорит X-Forwarded-Proto, о пути (/radio за web) — X-Forwarded-Prefix.
func baseURL(r *http.Request) string {
	prefix := r.Header.Get("X-Forwarded-Prefix")
	if !prefixOK.MatchString(prefix) {
		prefix = ""
	}
	return hostURL(r) + prefix
}

// hostURL — схема и хост без пути: короткие ссылки /r/ живут в корне домена (web шлёт их радио).
func hostURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if !hostOK.MatchString(host) {
		host = "localhost"
	}
	return scheme + "://" + host
}

func (h *Hub) liveStation(f int) (name, title string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.st[f]; s != nil {
		return s.name, s.title, true
	}
	return "", "", false
}

// shareParams — частота из пути, имя и трек из запроса; пустое имя берётся у станции в эфире.
// Трек у живой станции НЕ подставляется: ссылка — про то, что играло, когда ею поделились.
func (h *Hub) shareParams(r *http.Request) (f int, name, track, lang string, ok bool) {
	f, ok = ParseFreq(strings.TrimSuffix(r.PathValue("freq"), ".png"))
	if !ok {
		return 0, "", "", "", false
	}
	q := r.URL.Query()
	name, track, lang = clean(q.Get("n"), nameRunes), clean(q.Get("t"), titleRunes), langParam(q.Get("l"))
	if name == "" {
		name, _, _ = h.liveStation(f)
	}
	return f, name, track, lang, true
}

func (h *Hub) serveIndex(w http.ResponseWriter, r *http.Request) {
	base, lang := baseURL(r), acceptLang(r)
	t := ogTexts[lang]
	img := base + "/og/radio.png"
	if lang == "en" {
		img += "?l=en"
	}
	meta := ogMeta(lang, map[string]string{
		"og:title":       t.Site,
		"og:description": t.Desc,
		"og:url":         base + "/",
		"og:image":       img,
	})
	writePage(w, strings.Replace(indexHTML, ogPlaceholder, meta+h.airPortMeta(), 1))
}

func (h *Hub) serveShare(w http.ResponseWriter, r *http.Request) {
	f, name, track, lang, ok := h.shareParams(r)
	if !ok {
		http.Error(w, "frequency out of range 87.5–108.0", http.StatusNotFound)
		return
	}
	h.shares.Inc("page")
	h.writeShare(w, r, f, name, track, lang)
}

// serveShort — короткая ссылка /r/{code}: та же страница волны с той же карточкой превью, без
// перенаправления (не все мессенджеры по нему ходят). og:url — полная ссылка: по ней страница
// узнаёт волну и путь радио (/r/ открывается от корня домена, а радио живёт под /radio/).
func (h *Hub) serveShort(w http.ResponseWriter, r *http.Request) {
	l, ok := h.links.Get(r.PathValue("code"))
	if !ok {
		http.Redirect(w, r, baseURL(r)+"/", http.StatusFound) // устаревшая ссылка — просто на радио
		return
	}
	h.shares.Inc("short")
	h.writeShare(w, r, l.Freq, l.Name, l.Track, langParam(l.Lang))
}

// serveShortNew — POST /api/short {"f":1017,"n":"…","t":"…","l":"en"} → {"url":"https://…/r/k7Qx2"}.
func (h *Hub) serveShortNew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		F       int `json:"f"`
		N, T, L string
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil || req.F < MinFreq || req.F > MaxFreq {
		http.Error(w, `{"f":1017,"n":"station","t":"track","l":"en"}`, http.StatusBadRequest)
		return
	}
	if !h.shortRate.allow() {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	code, err := h.links.Code(req.F, clean(req.N, nameRunes), clean(req.T, titleRunes), langParam(req.L))
	if code == "" {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]string{"url": hostURL(r) + "/r/" + code})
}

func (h *Hub) writeShare(w http.ResponseWriter, r *http.Request, f int, name, track, lang string) {
	base, t := baseURL(r), ogTexts[lang]
	q := url.Values{}
	if name != "" {
		q.Set("n", name)
	}
	if track != "" {
		q.Set("t", track)
	}
	if lang == "en" {
		q.Set("l", "en")
	}
	qs := ""
	if len(q) > 0 {
		qs = "?" + q.Encode()
	}
	desc := t.Live
	if _, _, live := h.liveStation(f); !live {
		desc = t.Silent
	}
	if track != "" {
		desc = fmt.Sprintf(t.Played, track) + desc
	}
	title := ogTitle(f, name, lang)
	meta := ogMeta(lang, map[string]string{
		"og:title":       title,
		"og:description": desc,
		"og:url":         base + "/w/" + FormatFreq(f) + qs,
		"og:image":       base + "/og/" + strconv.Itoa(f) + ".png" + qs,
	})
	page := strings.Replace(indexHTML, ogPlaceholder, meta+h.airPortMeta(), 1)
	page = strings.Replace(page, "<title>Открытое радио</title>", "<title>"+html.EscapeString(title)+"</title>", 1)
	writePage(w, page)
}

func writePage(w http.ResponseWriter, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(page))
}

func ogMeta(lang string, m map[string]string) string {
	var b strings.Builder
	for _, k := range []string{"og:title", "og:description", "og:url", "og:image"} {
		b.WriteString(`<meta property="` + k + `" content="` + html.EscapeString(m[k]) + `">` + "\n")
	}
	b.WriteString(`<meta property="og:type" content="website">` + "\n")
	b.WriteString(`<meta property="og:site_name" content="` + html.EscapeString(ogTexts[lang].Site) + `">` + "\n")
	if lang == "en" {
		b.WriteString(`<meta property="og:locale" content="en_US">` + "\n")
	} else {
		b.WriteString(`<meta property="og:locale" content="ru_RU">` + "\n")
	}
	b.WriteString(`<meta property="og:image:type" content="image/png">` + "\n")
	b.WriteString(`<meta property="og:image:width" content="` + strconv.Itoa(OGWidth) + `">` + "\n")
	b.WriteString(`<meta property="og:image:height" content="` + strconv.Itoa(OGHeight) + `">` + "\n")
	b.WriteString(`<meta name="twitter:card" content="summary_large_image">`)
	return b.String()
}

// Картинки кешируются: мессенджеры приходят за одной и той же ссылкой много раз.
// Кеш ограничен — подбором параметров его нельзя раздуть, только сбросить.
type ogCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

const ogCacheMax = 256

func (c *ogCache) get(key string, make func() []byte) []byte {
	c.mu.Lock()
	b, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return b
	}
	b = make()
	c.mu.Lock()
	if c.m == nil || len(c.m) >= ogCacheMax {
		c.m = map[string][]byte{}
	}
	c.m[key] = b
	c.mu.Unlock()
	return b
}

func (h *Hub) serveOG(w http.ResponseWriter, r *http.Request) {
	var img []byte
	if r.PathValue("freq") == "radio.png" {
		lang := langParam(r.URL.Query().Get("l"))
		img = h.og.get(lang, func() []byte { h.ogRenders.Inc(); return OGImage(0, "", "", lang) })
	} else {
		f, name, track, lang, ok := h.shareParams(r)
		if !ok || !strings.HasSuffix(r.PathValue("freq"), ".png") {
			http.NotFound(w, r)
			return
		}
		h.shares.Inc("image")
		key := strconv.Itoa(f) + "\x00" + name + "\x00" + track + "\x00" + lang
		img = h.og.get(key, func() []byte { h.ogRenders.Inc(); return OGImage(f, name, track, lang) })
	}
	if img == nil {
		http.Error(w, "image failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(img)
}
