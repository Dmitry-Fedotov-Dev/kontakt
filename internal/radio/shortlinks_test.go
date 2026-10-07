package radio

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortLinksStableAndPersistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	s, _ := openShortLinks(path, 100)
	a, _ := s.Code(1017, "Mobile", "Смысловые Галлюцинации — Вечно молодой", "", "")
	b, _ := s.Code(1017, "Mobile", "Смысловые Галлюцинации — Вечно молодой", "", "")
	c, _ := s.Code(1017, "Mobile", "другой трек", "", "")
	if a != b || a == c || len(a) != shortCodeLen {
		t.Fatalf("коды: %q %q %q", a, b, c)
	}
	s2, err := openShortLinks(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := s2.Get(a); !ok || l.Freq != 1017 || l.Track != "Смысловые Галлюцинации — Вечно молодой" {
		t.Fatalf("после перезапуска: %+v %v", l, ok)
	}
}

func TestShortLinksEvictOldest(t *testing.T) {
	s, _ := openShortLinks("", 3)
	first, _ := s.Code(900, "", "1", "", "")
	for i := 0; i < 3; i++ {
		s.Code(900, "", strings.Repeat("x", i+2), "", "")
	}
	if _, ok := s.Get(first); ok || s.Len() != 3 {
		t.Fatalf("старейшая не вытеснена: len %d", s.Len())
	}
}

// Короткая ссылка отдаёт ту же страницу с теми же метатегами превью, что и длинная, —
// без перенаправления: не все мессенджеры по нему ходят.
func TestShortLinkSamePreview(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/short", "application/json",
		strings.NewReader(`{"f":1065,"n":"Mobile","t":"Смысловые Галлюцинации — Вечно молодой"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ URL string }
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if !strings.HasPrefix(out.URL, srv.URL+"/r/") || len(out.URL) > len(srv.URL)+3+shortCodeLen+2 {
		t.Fatalf("короткая ссылка: %q", out.URL)
	}

	get := func(u string) (int, string) {
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		r, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var b strings.Builder
		buf := make([]byte, 64<<10)
		for {
			n, err := r.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return r.StatusCode, b.String()
	}
	metas := func(page string) []string {
		var m []string
		for _, line := range strings.Split(page, "\n") {
			if strings.Contains(line, `<meta property="og:`) {
				m = append(m, line)
			}
		}
		return m
	}
	code, short := get(out.URL)
	_, long := get(srv.URL + "/w/106.5?n=Mobile&t=" + "%D0%A1%D0%BC%D1%8B%D1%81%D0%BB%D0%BE%D0%B2%D1%8B%D0%B5+%D0%93%D0%B0%D0%BB%D0%BB%D1%8E%D1%86%D0%B8%D0%BD%D0%B0%D1%86%D0%B8%D0%B8+%E2%80%94+%D0%92%D0%B5%D1%87%D0%BD%D0%BE+%D0%BC%D0%BE%D0%BB%D0%BE%D0%B4%D0%BE%D0%B9")
	if code != http.StatusOK {
		t.Fatalf("короткая ссылка: %d (ждали страницу, а не перенаправление)", code)
	}
	ms, ml := metas(short), metas(long)
	if len(ms) == 0 || strings.Join(ms, "\n") != strings.Join(ml, "\n") {
		t.Fatalf("превью разные:\nкороткая:\n%s\nдлинная:\n%s", strings.Join(ms, "\n"), strings.Join(ml, "\n"))
	}
	if c, _ := get(srv.URL + "/r/nosuch"); c != http.StatusFound {
		t.Fatalf("неизвестный код: %d, ждали перенаправление на радио", c)
	}
}

// Язык — часть ссылки: английская карточка — свой код; у русской код прежний (lang в хеш не входит).
func TestShortLinksLang(t *testing.T) {
	s, _ := openShortLinks("", 10)
	ru, _ := s.Code(1017, "Mobile", "x", "", "")
	ru2, _ := s.Code(1017, "Mobile", "x", "ru", "")
	en, _ := s.Code(1017, "Mobile", "x", "en", "")
	if ru != ru2 || ru == en {
		t.Fatalf("коды: ru %q, ru2 %q, en %q", ru, ru2, en)
	}
	if l, _ := s.Get(en); l.Lang != "en" {
		t.Fatalf("язык не сохранён: %+v", l)
	}
}
