package radio

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testLogo(t *testing.T, side int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			img.Set(x, y, c)
		}
	}
	img.Set(32, 30, color.NRGBA{255, 0, 0, 255})
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// Логотип: только PNG 64×64; одинаковое — один хеш; на диске переживает перезапуск; удаляется.
func TestLogoStore(t *testing.T) {
	dir := t.TempDir()
	s, _ := openLogos(dir, 2)
	if _, err := s.Put(testLogo(t, 32, color.White)); err == nil {
		t.Fatal("32×32 принят")
	}
	if _, err := s.Put([]byte("<svg/>")); err == nil {
		t.Fatal("не PNG принят")
	}
	a, err := s.Put(testLogo(t, 64, color.White))
	if err != nil || !logoHashOK.MatchString(a) {
		t.Fatalf("хеш %q: %v", a, err)
	}
	if b, _ := s.Put(testLogo(t, 64, color.White)); b != a {
		t.Fatal("одинаковый логотип — разные хеши")
	}
	s2, _ := openLogos(dir, 2) // как после перезапуска
	got, ok := s2.Get(a)
	if !ok {
		t.Fatal("после перезапуска логотипа нет")
	}
	if img, err := png.Decode(bytes.NewReader(got)); err != nil || img.Bounds().Dx() != logoSide {
		t.Fatalf("сохранён не PNG 64×64: %v", err)
	}
	// не больше max файлов: старые уходят
	b, _ := s.Put(testLogo(t, 64, color.Black))
	time.Sleep(20 * time.Millisecond)
	c, _ := s.Put(testLogo(t, 64, color.NRGBA{0, 0, 255, 255}))
	if n, _ := filepath.Glob(filepath.Join(dir, "*.png")); len(n) != 2 {
		t.Fatalf("на диске %d файлов, ждали 2", len(n))
	}
	if _, ok := s2.Get(c); !ok {
		t.Fatal("свежий логотип вытеснен")
	}
	s.Delete(b)
	if _, err := os.Stat(filepath.Join(dir, b+".png")); !os.IsNotExist(err) {
		t.Fatal("удалённый логотип на диске")
	}
	if _, ok := s.Get("../../etc/passwd"); ok {
		t.Fatal("путь вместо хеша")
	}
}

// Ведущий шлёт логотип: слушатели видят хеш в списке, картинка отдаётся, превью и ссылка — с ним.
func TestLogoOnAir(t *testing.T) {
	h := NewHub(Options{LogosDir: t.TempDir(), HostLogos: true})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=934&name=Platform")
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })

	host.WriteJSON(map[string]string{"logo": "data:image/png;base64," + base64.StdEncoding.EncodeToString(testLogo(t, 64, color.White))})
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Logo != "" })
	g := h.Stations()[0].Logo

	r, err := http.Get(srv.URL + "/logo/" + g + ".png")
	if err != nil || r.StatusCode != 200 || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("картинка логотипа: %v %v", err, r)
	}
	r.Body.Close()
	if r, _ := http.Get(srv.URL + "/logo/zzz.png"); r.StatusCode != 404 {
		t.Fatalf("чужой путь: %d", r.StatusCode)
	}

	// ссылка без имени — про живую станцию: og:image несёт логотип
	r, _ = http.Get(srv.URL + "/w/93.4")
	page, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(page), "g="+g) {
		t.Fatal("в превью ссылки нет логотипа")
	}
	with, _ := http.Get(srv.URL + "/og/934.png?n=Platform&g=" + g)
	without, _ := http.Get(srv.URL + "/og/934.png?n=Platform")
	bw, _ := io.ReadAll(with.Body)
	bo, _ := io.ReadAll(without.Body)
	with.Body.Close()
	without.Body.Close()
	img, err := png.Decode(bytes.NewReader(bw))
	if err != nil || img.Bounds().Dx() != OGWidth || bytes.Equal(bw, bo) {
		t.Fatalf("превью с логотипом: %v", err)
	}
	// логотип нарисован: красный пиксель (32, 30) тестовой картинки — в круге, углы срезаются
	px := img.At(ogLogoX*ogScale+32*3+1, ogLogoY*ogScale+30*3+1)
	if r, g, b, _ := px.RGBA(); r>>8 != 255 || g>>8 != 0 || b>>8 != 0 {
		t.Fatalf("пиксель логотипа на превью: %v", px)
	}

	// плохой логотип — ведущему ошибка, старый остаётся
	host.WriteJSON(map[string]string{"logo": base64.StdEncoding.EncodeToString(testLogo(t, 16, color.White))})
	time.Sleep(100 * time.Millisecond)
	if h.Stations()[0].Logo != g {
		t.Fatal("плохой логотип заменил хороший")
	}
	host.WriteJSON(map[string]string{"logo": ""})
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Logo == "" })
}

// Модератор ставит логотип станции из админки; он закрепляется за ведущим и возвращается на новом эфире.
func TestLogoAdminPin(t *testing.T) {
	dir := t.TempDir()
	h := NewHub(Options{LogosDir: dir})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	adm := httptest.NewServer(h.AdminHandler())
	defer adm.Close()
	hdr := http.Header{"Cookie": {"kontakt_id=" + strings.Repeat("ab", 16)}}
	d := websocket.DefaultDialer
	host, _, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/host?f=934&name=X", hdr)
	if err != nil {
		t.Fatal(err)
	}
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	r, _ := http.Post(adm.URL+"/admin/station-logo?f=934", "image/png", bytes.NewReader(testLogo(t, 64, color.White)))
	if r.StatusCode != 200 {
		t.Fatalf("админка: %d", r.StatusCode)
	}
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Logo != "" })
	g := h.Stations()[0].Logo
	host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 0 })

	h2 := NewHub(Options{LogosDir: dir}) // и после перезапуска радио
	srv2 := httptest.NewServer(h2.Handler())
	defer srv2.Close()
	host2, _, err := d.Dial("ws"+strings.TrimPrefix(srv2.URL, "http")+"/ws/host?f=1000&name=X", hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer host2.Close()
	waitStations(t, h2, func(s []Station) bool { return len(s) == 1 && s[0].Logo == g })
	for i := 0; i < 100; i++ { // память волны пишется на диск сразу после выхода в эфир — дождаться,
		if _, ok := h2.waves.Get(1000); ok { // иначе уборка временной папки гонится с записью
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r, _ := http.Post(adm.URL+"/admin/station-logo?f=1017", "image/png", nil); r.StatusCode != 404 {
		t.Fatalf("пустая волна: %d", r.StatusCode)
	}
}

// Без HostLogos логотип от ведущего не принимается, ячейки на странице нет.
func TestHostLogosOff(t *testing.T) {
	h := NewHub(Options{LogosDir: t.TempDir()})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=934&name=X")
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	host.WriteJSON(map[string]string{"logo": base64.StdEncoding.EncodeToString(testLogo(t, 64, color.White))})
	time.Sleep(150 * time.Millisecond)
	if g := h.Stations()[0].Logo; g != "" {
		t.Fatalf("ведущий поставил логотип сам: %s", g)
	}
	r, _ := http.Get(srv.URL + "/")
	page, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(page), `<meta name="host-logos"`) {
		t.Fatal("страница разрешает ведущему логотип")
	}
}

// Чистая ссылка /w/93.4: станция ушла — превью помнит её название и логотип; после перезапуска тоже.
func TestWaveMemory(t *testing.T) {
	dir := t.TempDir()
	h := NewHub(Options{LogosDir: dir, HostLogos: true})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, _ := dial(t, srv, "/ws/host?f=934&name=Platform")
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	host.WriteJSON(map[string]string{"logo": base64.StdEncoding.EncodeToString(testLogo(t, 64, color.White))})
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Logo != "" })
	g := h.Stations()[0].Logo
	host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 0 })

	h2 := NewHub(Options{LogosDir: dir})
	srv2 := httptest.NewServer(h2.Handler())
	defer srv2.Close()
	r, _ := http.Get(srv2.URL + "/w/93.4")
	page, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(page), "Platform · 93.4") || !strings.Contains(string(page), "g="+g) {
		t.Fatal("превью молчащей волны без названия или логотипа")
	}
}
