package radio

import (
	"bytes"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b), w.Header()
}

// Мессенджер видит карточку: заголовок со станцией, трек в описании, абсолютная https-картинка.
func TestSharePageOpenGraph(t *testing.T) {
	h := NewHub(Options{}).Handler()
	code, body, _ := get(t, h, "/w/101.7?n=%D0%9F%D0%B8%D1%80%D0%B0%D1%82&t=%D0%9A%D0%B8%D0%BD%D0%BE%20%E2%80%94%20%D0%9A%D1%83%D0%BA%D1%83%D1%88%D0%BA%D0%B0",
		map[string]string{"Host": "abc.trycloudflare.com", "X-Forwarded-Proto": "https"})
	if code != 200 {
		t.Fatalf("код %d", code)
	}
	for _, want := range []string{
		`<meta property="og:title" content="Пират · 101.7 МГц">`,
		`♪ Играло: Кино — Кукушка.`,
		`<meta property="og:image" content="https://abc.trycloudflare.com/og/1017.png?n=`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`<title>Пират · 101.7 МГц</title>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("нет %q", want)
		}
	}
	if strings.Contains(body, "<!--OG-->") {
		t.Error("заглушка не заменена")
	}
}

// Текст из ссылки попадает в разметку только экранированным, а мусорный Host — не попадает вовсе.
func TestSharePageEscapes(t *testing.T) {
	h := NewHub(Options{}).Handler()
	_, body, _ := get(t, h, `/w/99.9?t=%22%3E%3Cscript%3Ealert(1)%3C/script%3E`, map[string]string{"Host": `evil"><script>`})
	if strings.Contains(body, "<script>alert") || strings.Contains(body, `evil"`) {
		t.Fatal("в разметку попал неэкранированный текст")
	}
	if !strings.Contains(body, "http://localhost/og/999.png") {
		t.Fatal("подозрительный Host не заменён")
	}
}

func TestShareBadFreq(t *testing.T) {
	h := NewHub(Options{}).Handler()
	for _, p := range []string{"/w/120.0", "/w/abc", "/og/120.png", "/og/1017.jpg"} {
		if code, _, _ := get(t, h, p, nil); code != 404 {
			t.Errorf("%s: код %d, ждали 404", p, code)
		}
	}
}

// Картинка — настоящий PNG 1200×632 (пропорция большой карточки), и одинаковая ссылка рисуется один раз.
func TestOGImage(t *testing.T) {
	hub := NewHub(Options{})
	h := hub.Handler()
	for _, p := range []string{"/og/1017.png?n=Pirate&t=" + strings.Repeat("%D0%AF", 200), "/og/radio.png"} {
		code, body, hdr := get(t, h, p, nil)
		if code != 200 || hdr.Get("Content-Type") != "image/png" {
			t.Fatalf("%s: %d %s", p, code, hdr.Get("Content-Type"))
		}
		img, err := png.Decode(bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != OGWidth || b.Dy() != OGHeight {
			t.Fatalf("размер %v", b)
		}
	}
	get(t, h, "/og/1017.png?n=Pirate&t=x", nil)
	get(t, h, "/og/1017.png?n=Pirate&t=x", nil)
	if n := len(hub.og.m); n != 3 {
		t.Fatalf("в кеше %d картинок, ждали 3", n)
	}
	for i := 0; i < ogCacheMax+10; i++ { // кеш не растёт бесконечно
		hub.og.get(strings.Repeat("k", i+1), func() []byte { return []byte{1} })
	}
	if len(hub.og.m) > ogCacheMax {
		t.Fatalf("кеш вырос до %d", len(hub.og.m))
	}
}

// Пустое имя в ссылке берётся у станции в эфире; трек — только из ссылки.
func TestShareUsesLiveName(t *testing.T) {
	hub := NewHub(Options{})
	hub.st[1017] = &station{freq: 1017, name: "Живая", title: "Трек-из-эфира"}
	_, body, _ := get(t, hub.Handler(), "/w/101.7", nil)
	if !strings.Contains(body, "Живая · 101.7 МГц") || strings.Contains(body, "Трек-из-эфира") {
		t.Fatal("имя живой станции не подставилось или подставился трек")
	}
}

func TestWrapText(t *testing.T) {
	l := wrapText("Кино — Группа крови (Live 1988) очень длинное название трека", 24, 2)
	if len(l) != 2 || !strings.HasSuffix(l[1], "…") {
		t.Fatalf("%q", l)
	}
	for _, s := range l {
		if n := len([]rune(s)); n > 24 {
			t.Fatalf("строка %d символов: %q", n, s)
		}
	}
	if l := wrapText(strings.Repeat("Я", 50), 24, 3); len(l) != 3 || len([]rune(l[0])) != 24 {
		t.Fatalf("длинное слово: %q", l)
	}
}

// Все буквы русского, украинского, казахского и кыргызского алфавитов есть в шрифте.
func TestFontCoversAlphabets(t *testing.T) {
	for _, r := range "АБВГДЕЁЖЗИЙКЛМНОПРСТУФХЦЧШЩЪЫЬЭЮЯабвгдеёжзийклмнопрстуфхцчшщъыьэюяЄІЇҢӨҮӘҚҒҺҰ" +
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 .,-—!?:;'\"«»()/&+=*#%@·…♪<>" {
		if _, ok := glyph(r); !ok {
			t.Errorf("нет глифа %q", r)
		}
	}
}

// Карточка — на языке того, кто поделился (l=en), главная — по Accept-Language.
func TestShareLanguage(t *testing.T) {
	h := NewHub(Options{}).Handler()
	_, body, _ := get(t, h, "/w/101.7?n=Pirate&t=Song&l=en", nil)
	for _, want := range []string{
		`<meta property="og:title" content="Pirate · 101.7 FM">`,
		`♪ Was playing: Song.`,
		`<meta property="og:site_name" content="Open Radio">`,
		`/og/1017.png?l=en&amp;n=Pirate&amp;t=Song`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("en: нет %q", want)
		}
	}
	for hdr, want := range map[string]string{
		"en-US,en;q=0.9":        `content="Open Radio"`,
		"ky-KG,ky;q=0.9,en":     `content="Открытое радио"`,
		"de-DE,ru;q=0.8":        `content="Open Radio"`,
		"":                      `content="Открытое радио"`,
		"uk-UA,uk;q=0.9,en;q=0": `content="Открытое радио"`,
	} {
		_, body, _ := get(t, h, "/", map[string]string{"Accept-Language": hdr})
		if !strings.Contains(body, `<meta property="og:title" `+want) {
			t.Errorf("Accept-Language %q: ждали %s", hdr, want)
		}
	}
	// английская и русская картинка — разные
	_, ru, _ := get(t, h, "/og/1017.png?n=Pirate", nil)
	_, en, _ := get(t, h, "/og/1017.png?n=Pirate&l=en", nil)
	if ru == en {
		t.Error("картинки на двух языках одинаковые")
	}
}
