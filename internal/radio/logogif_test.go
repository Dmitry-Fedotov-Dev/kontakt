package radio

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// testGIF — анимация w×h из n кадров: цветной квадрат едет слева направо по прозрачному фону.
func testGIF(t *testing.T, w, h, n int) []byte {
	t.Helper()
	pal := color.Palette{color.NRGBA{}, color.NRGBA{255, 0, 0, 255}, color.NRGBA{0, 0, 255, 255}}
	g := &gif.GIF{Config: image.Config{ColorModel: pal, Width: w, Height: h}}
	for i := 0; i < n; i++ {
		p := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		x := i * (w - h/2) / n
		for y := h / 4; y < h*3/4; y++ {
			for xx := x; xx < x+h/2; xx++ {
				p.SetColorIndex(xx, y, uint8(1+i%2))
			}
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 5)
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func center(w, h int) image.Rectangle { return gifCrop(0, 0, 0, w, h) }

// Гифка 200×120 из 60 кадров → GIF 64×64, кадров не больше logoFrames, время сохранено, анимация
// осталась (кадры разные).
func TestLogoFromGIF(t *testing.T) {
	out, err := logoFromGIF(testGIF(t, 200, 120, 60), center)
	if err != nil {
		t.Fatal(err)
	}
	if !isGIF(out) || len(out) > logoGifMax {
		t.Fatalf("не GIF или больше %d: %d байт", logoGifMax, len(out))
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if g.Config.Width != logoSide || g.Config.Height != logoSide || len(g.Image) < 2 || len(g.Image) > logoFrames {
		t.Fatalf("%d×%d, кадров %d", g.Config.Width, g.Config.Height, len(g.Image))
	}
	total := 0
	for _, d := range g.Delay {
		total += d
	}
	if total != 60*5 {
		t.Fatalf("длительность %d, ждали %d: прореживание потеряло время", total, 60*5)
	}
	if bytes.Equal(g.Image[0].Pix, g.Image[len(g.Image)/2].Pix) {
		t.Fatal("кадры одинаковые — анимация потерялась")
	}
	// готовый значок проходит проверку эфира как есть
	if _, err := normalizeLogo(out); err != nil {
		t.Fatal(err)
	}
}

// Кадры из видео (.gif.mp4 из Telegram, раскадровал браузер) → GIF с теми же задержками; кривое тело — отказ.
func TestLogoFromFrames(t *testing.T) {
	body := append([]byte{}, framesMagic...)
	for i := 0; i < 20; i++ {
		f := make([]byte, frameBytes)
		binary.LittleEndian.PutUint16(f, 10)
		for p := 2; p < frameBytes; p += 4 {
			f[p], f[p+1], f[p+2], f[p+3] = uint8(i*12), uint8(p), 200, 255
		}
		body = append(body, f...)
	}
	out, err := logoFromFrames(body)
	if err != nil || !isGIF(out) {
		t.Fatalf("ждали GIF: %v", err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil || len(g.Image) < 2 {
		t.Fatalf("кадров %v: %v", g, err)
	}
	total := 0
	for _, d := range g.Delay {
		total += d
	}
	if total != 200 {
		t.Fatalf("длительность %d, ждали 200", total)
	}
	for _, bad := range [][]byte{framesMagic, body[:len(body)-1], append(append([]byte{}, framesMagic...), make([]byte, frameBytes*(logoFrames+1))...)} {
		if _, err := logoFromFrames(bad); err == nil {
			t.Fatalf("приняли кривое тело в %d байт", len(bad))
		}
	}
}

// Один кадр — обычный PNG.
func TestLogoFromStaticGIF(t *testing.T) {
	out, err := logoFromGIF(testGIF(t, 100, 100, 1), center)
	if err != nil || isGIF(out) {
		t.Fatalf("ждали PNG: %v", err)
	}
}

// Гифка-бомба: в заголовке огромный размер — отказ до раскодирования; не гифка — отказ.
func TestLogoGIFLimits(t *testing.T) {
	b := testGIF(t, 64, 64, 2)
	binary.LittleEndian.PutUint16(b[6:], 5000)
	binary.LittleEndian.PutUint16(b[8:], 5000)
	if _, err := logoFromGIF(b, center); err == nil {
		t.Fatal("гифку 5000×5000 приняли")
	}
	if _, err := logoFromGIF([]byte("not a gif at all"), center); err == nil {
		t.Fatal("не гифку приняли")
	}
	// в эфир — только ровно 64×64 и не больше logoFrames кадров
	if _, err := normalizeLogo(testGIF(t, 65, 64, 2)); err == nil {
		t.Fatal("GIF 65×64 в эфир приняли")
	}
	if _, err := normalizeLogo(testGIF(t, 64, 64, logoFrames+1)); err == nil {
		t.Fatal("GIF с лишними кадрами в эфир приняли")
	}
	if w, h, n, err := gifScan(testGIF(t, 30, 20, 7)); err != nil || w != 30 || h != 20 || n != 7 {
		t.Fatalf("gifScan: %d×%d, %d кадров, %v", w, h, n, err)
	}
}

// Вся цепочка: гифка → /api/logo-gif → ведущий шлёт значок в эфир → /logo/{хеш}.png отдаёт
// image/gif → превью ссылки рисуется с первым кадром. Без доступа к логотипу — 403.
func TestLogoGIFOnAir(t *testing.T) {
	h := NewHub(Options{HostLogos: true})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	post := func(id string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/api/logo-gif?x=0&y=0&s=120", bytes.NewReader(testGIF(t, 200, 120, 30)))
		req.Header = cookieHdr(id)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	id := strings.Repeat("ab", 16)
	r := post(id)
	var m struct{ Logo string }
	json.NewDecoder(r.Body).Decode(&m)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.HasPrefix(m.Logo, "data:image/gif;base64,") {
		t.Fatalf("ответ %d: %.40s", r.StatusCode, m.Logo)
	}

	host, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/host?f=996&name=K", cookieHdr(id))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	host.WriteJSON(map[string]string{"logo": m.Logo})
	st := waitStations(t, h, func(s []Station) bool { return len(s) == 1 && s[0].Logo != "" })
	lr, err := http.Get(srv.URL + "/logo/" + st[0].Logo + ".png")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(lr.Body)
	lr.Body.Close()
	want, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(m.Logo, "data:image/gif;base64,"))
	if lr.Header.Get("Content-Type") != "image/gif" || !isGIF(body) || len(body) != len(want) {
		t.Fatalf("/logo: %s, %d байт", lr.Header.Get("Content-Type"), len(body))
	}
	og, err := http.Get(srv.URL + "/og/996.png?g=" + st[0].Logo)
	if err != nil || og.StatusCode != 200 {
		t.Fatalf("превью: %v %v", err, og)
	}
	og.Body.Close()

	h2 := NewHub(Options{})
	srv2 := httptest.NewServer(h2.Handler())
	defer srv2.Close()
	req, _ := http.NewRequest("POST", srv2.URL+"/api/logo-gif", bytes.NewReader(testGIF(t, 64, 64, 2)))
	req.Header = cookieHdr(id)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("без доступа: %d", r.StatusCode)
	}
}
