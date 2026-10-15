package radio

// Анимированный логотип: GIF 64×64. Холст браузера анимацию теряет, поэтому гифку обрабатывает
// сервер: POST /api/logo-gif — исходная гифка (до logoGifUpload), в ответ — значок, который
// ведущий шлёт в эфир, как PNG. Кадры склеиваются с учётом disposal, обрезаются до квадрата (его
// выбирает страница по первому кадру — той же обрезкой полей, что у PNG), уменьшаются усреднением
// до 64×64 и сводятся к одной палитре (median cut, без дизеринга — пиксельный вид). Кадров не
// больше logoFrames: лишние прореживаются, их время отходит оставшимся. Не влезло в logoGifMax —
// прореживается ещё. Один кадр — обычный PNG.
//
// Telegram и другие сохраняют «гифки» видеороликом (.gif.mp4): такой ролик раскадровывает браузер и
// шлёт на тот же адрес готовые кадры 64×64 (framesMagic, logoFromFrames) — палитру и GIF делает сервер.
//
// Гифка — сжатые LZW кадры: файл в несколько мегабайт разворачивается в гигабайты. Поэтому до
// раскодирования кадры и размер считаются по структуре файла (gifScan), а одновременно
// обрабатывается не больше gifJobs гифок.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"sort"
	"strconv"

	"kontakt/internal/identity"
)

const (
	logoGifMax    = 64 << 10 // готовый GIF 64×64
	logoFrames    = 60
	logoGifUpload = 4 << 20  // исходная гифка
	gifMaxSide    = 1024     // сторона исходника
	gifMaxPixels  = 40 << 20 // кадры × площадь исходника: столько байт займут раскодированные кадры
	gifMaxFrames  = 500
	gifJobs       = 2
)

var errBadGIF = errors.New("логотип: гифка не читается или слишком большая")

// framesMagic — тело POST /api/logo-gif с кадрами из видео: за ним кадры подряд, каждый — задержка
// (uint16 LE, сотые секунды) и 64×64 пикселя RGBA.
var framesMagic = []byte("KFRM")

const frameBytes = 2 + logoSide*logoSide*4

// logoFromFrames — кадры из видео (framesMagic) → значок.
func logoFromFrames(b []byte) ([]byte, error) {
	b = b[len(framesMagic):]
	n := len(b) / frameBytes
	if n < 1 || n > logoFrames || len(b)%frameBytes != 0 {
		return nil, errBadGIF
	}
	frames := make([]*image.NRGBA, n)
	delays := make([]int, n)
	for i := range frames {
		f := b[i*frameBytes:]
		delays[i] = max(2, int(binary.LittleEndian.Uint16(f)))
		frames[i] = image.NewNRGBA(image.Rect(0, 0, logoSide, logoSide))
		copy(frames[i].Pix, f[2:frameBytes])
	}
	return encodeLogo(frames, delays)
}

func isGIF(b []byte) bool {
	return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
}

// gifScan — размер и число кадров по блокам файла, без раскодирования.
func gifScan(b []byte) (w, h, frames int, err error) {
	if len(b) < 13 || !isGIF(b) {
		return 0, 0, 0, errBadGIF
	}
	w, h = int(binary.LittleEndian.Uint16(b[6:])), int(binary.LittleEndian.Uint16(b[8:]))
	p := 13
	if b[10]&0x80 != 0 {
		p += 3 << (b[10]&7 + 1)
	}
	skip := func() bool { // подблоки до пустого
		for p < len(b) {
			n := int(b[p])
			p++
			if n == 0 {
				return true
			}
			p += n
		}
		return false
	}
	for p < len(b) {
		switch b[p] {
		case 0x3b: // конец
			return w, h, frames, nil
		case 0x21: // расширение
			p += 2
			if !skip() {
				return w, h, frames, nil // оборванный хвост — как конец
			}
		case 0x2c: // кадр
			if p+10 > len(b) {
				return w, h, frames, nil
			}
			fl := b[p+9]
			p += 10
			if fl&0x80 != 0 {
				p += 3 << (fl&7 + 1)
			}
			p++ // минимальный размер кода LZW
			frames++
			if !skip() {
				return w, h, frames, nil
			}
		default:
			return 0, 0, 0, errBadGIF
		}
	}
	return w, h, frames, nil
}

// normalizeGIF — готовый значок: ровно 64×64, кадров не больше logoFrames, перекодирован заново
// (без комментариев и чужих расширений).
func normalizeGIF(b []byte) ([]byte, error) {
	if len(b) > logoGifMax {
		return nil, errBadLogo
	}
	w, h, n, err := gifScan(b)
	if err != nil || w != logoSide || h != logoSide || n < 1 || n > logoFrames {
		return nil, errBadLogo
	}
	g, err := gif.DecodeAll(bytes.NewReader(b))
	if err != nil {
		return nil, errBadLogo
	}
	out := &gif.GIF{Image: g.Image, Delay: g.Delay, Disposal: g.Disposal, Config: g.Config, BackgroundIndex: g.BackgroundIndex}
	for i, d := range out.Delay {
		if d < 2 {
			out.Delay[i] = 10 // 0 и 1 браузеры и так показывают как 100 мс
		}
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, out); err != nil || buf.Len() > logoGifMax {
		return nil, errBadLogo
	}
	return buf.Bytes(), nil
}

// decodeLogo — картинка логотипа для превью ссылок: у гифки — первый кадр.
func decodeLogo(b []byte) (image.Image, error) {
	if isGIF(b) {
		return gif.Decode(bytes.NewReader(b))
	}
	return png.Decode(bytes.NewReader(b))
}

// gifCrop — квадрат обрезки в пикселях исходника; негодный — квадрат по центру.
func gifCrop(x, y, s, w, h int) image.Rectangle {
	if s <= 0 || x < 0 || y < 0 || x+s > w || y+s > h {
		s = min(w, h)
		x, y = (w-s)/2, (h-s)/2
	}
	return image.Rect(x, y, x+s, y+s)
}

// logoFromGIF — исходная гифка → значок (GIF 64×64 или PNG, если кадр один).
func logoFromGIF(b []byte, crop func(w, h int) image.Rectangle) ([]byte, error) {
	w, h, n, err := gifScan(b)
	if err != nil || w < 1 || h < 1 || w > gifMaxSide || h > gifMaxSide || n < 1 || n > gifMaxFrames || n*w*h > gifMaxPixels {
		return nil, errBadGIF
	}
	g, err := gif.DecodeAll(bytes.NewReader(b))
	if err != nil || len(g.Image) == 0 {
		return nil, errBadGIF
	}
	sq := crop(w, h)
	step := (len(g.Image) + logoFrames - 1) / logoFrames // каждый step-й кадр
	canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
	var frames []*image.NRGBA
	var delays []int
	for i, fr := range g.Image {
		var prev *image.NRGBA
		disp := byte(gif.DisposalNone)
		if i < len(g.Disposal) {
			disp = g.Disposal[i]
		}
		if disp == gif.DisposalPrevious {
			prev = image.NewNRGBA(canvas.Bounds())
			copy(prev.Pix, canvas.Pix)
		}
		draw.Draw(canvas, fr.Bounds(), fr, fr.Bounds().Min, draw.Over)
		d := 10
		if i < len(g.Delay) && g.Delay[i] >= 2 {
			d = g.Delay[i]
		}
		if i%step == 0 {
			frames = append(frames, shrink(canvas, sq))
			delays = append(delays, d)
		} else {
			delays[len(delays)-1] += d
		}
		switch disp {
		case gif.DisposalBackground:
			draw.Draw(canvas, fr.Bounds(), image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			canvas = prev
		}
	}
	return encodeLogo(frames, delays)
}

// encodeLogo — кадры 64×64 → GIF не больше logoGifMax (PNG, если кадр один).
func encodeLogo(frames []*image.NRGBA, delays []int) ([]byte, error) {
	if len(frames) == 1 {
		var buf bytes.Buffer
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, frames[0]); err != nil {
			return nil, errBadGIF
		}
		return normalizeLogo(buf.Bytes())
	}
	pal := medianCut(frames, 255)
	for len(frames) >= 2 {
		out, err := encodeFrames(frames, delays, pal)
		if err != nil {
			return nil, errBadGIF
		}
		if len(out) <= logoGifMax {
			return normalizeLogo(out)
		}
		// не влезло — каждый второй кадр, время выкинутых — оставшимся
		var f2 []*image.NRGBA
		var d2 []int
		for i := range frames {
			if i%2 == 0 {
				f2, d2 = append(f2, frames[i]), append(d2, delays[i])
			} else {
				d2[len(d2)-1] += delays[i]
			}
		}
		frames, delays = f2, d2
	}
	return nil, errBadGIF
}

// shrink — квадрат sq холста, уменьшенный усреднением до 64×64 (прозрачность учитывается весом).
func shrink(src *image.NRGBA, sq image.Rectangle) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, logoSide, logoSide))
	s := sq.Dx()
	for oy := 0; oy < logoSide; oy++ {
		y0, y1 := sq.Min.Y+oy*s/logoSide, sq.Min.Y+(oy+1)*s/logoSide
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for ox := 0; ox < logoSide; ox++ {
			x0, x1 := sq.Min.X+ox*s/logoSide, sq.Min.X+(ox+1)*s/logoSide
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n int
			for y := y0; y < y1; y++ {
				i := src.PixOffset(x0, y)
				for x := x0; x < x1; x++ {
					pa := int(src.Pix[i+3])
					r, g, b, a = r+int(src.Pix[i])*pa, g+int(src.Pix[i+1])*pa, b+int(src.Pix[i+2])*pa, a+pa
					n++
					i += 4
				}
			}
			o := out.PixOffset(ox, oy)
			if a > 0 {
				out.Pix[o], out.Pix[o+1], out.Pix[o+2] = uint8(r/a), uint8(g/a), uint8(b/a)
			}
			out.Pix[o+3] = uint8(a / n)
		}
	}
	return out
}

// medianCut — палитра не больше k цветов на все кадры; прозрачное (альфа < 128) — отдельный индекс.
func medianCut(frames []*image.NRGBA, k int) color.Palette {
	hist := map[[3]uint8]int{}
	transparent := false
	for _, f := range frames {
		for i := 0; i < len(f.Pix); i += 4 {
			if f.Pix[i+3] < 128 {
				transparent = true
				continue
			}
			hist[[3]uint8{f.Pix[i], f.Pix[i+1], f.Pix[i+2]}]++
		}
	}
	type entry struct {
		c [3]uint8
		n int
	}
	all := make([]entry, 0, len(hist))
	for c, n := range hist {
		all = append(all, entry{c, n})
	}
	sort.Slice(all, func(i, j int) bool { // порядок карты случаен — палитра должна быть одна и та же
		return binary.BigEndian.Uint32([]byte{0, all[i].c[0], all[i].c[1], all[i].c[2]}) < binary.BigEndian.Uint32([]byte{0, all[j].c[0], all[j].c[1], all[j].c[2]})
	})
	boxes := [][]entry{all}
	if len(all) == 0 {
		boxes = nil
	}
	for len(boxes) < k {
		// делим коробку с наибольшим разбросом по самой широкой оси
		best, axis, spread := -1, 0, 0
		for i, bx := range boxes {
			if len(bx) < 2 {
				continue
			}
			for a := 0; a < 3; a++ {
				lo, hi := 255, 0
				for _, e := range bx {
					lo, hi = min(lo, int(e.c[a])), max(hi, int(e.c[a]))
				}
				if hi-lo > spread {
					best, axis, spread = i, a, hi-lo
				}
			}
		}
		if best < 0 {
			break
		}
		bx := boxes[best]
		sort.SliceStable(bx, func(i, j int) bool { return bx[i].c[axis] < bx[j].c[axis] })
		total, half, cut := 0, 0, 1
		for _, e := range bx {
			total += e.n
		}
		for i, e := range bx {
			half += e.n
			if half*2 >= total {
				cut = max(1, min(i+1, len(bx)-1))
				break
			}
		}
		boxes = append(boxes[:best], append([][]entry{bx[:cut], bx[cut:]}, boxes[best+1:]...)...)
	}
	pal := color.Palette{}
	for _, bx := range boxes {
		var r, g, b, n int
		for _, e := range bx {
			r, g, b, n = r+int(e.c[0])*e.n, g+int(e.c[1])*e.n, b+int(e.c[2])*e.n, n+e.n
		}
		pal = append(pal, color.NRGBA{uint8(r / n), uint8(g / n), uint8(b / n), 255})
	}
	if transparent || len(pal) == 0 {
		pal = append(pal, color.NRGBA{})
	}
	return pal
}

func encodeFrames(frames []*image.NRGBA, delays []int, pal color.Palette) ([]byte, error) {
	transp := -1
	for i, c := range pal {
		if _, _, _, a := c.RGBA(); a == 0 {
			transp = i
		}
	}
	near := map[[3]uint8]uint8{}
	out := &gif.GIF{Config: image.Config{ColorModel: pal, Width: logoSide, Height: logoSide}}
	for fi, f := range frames {
		p := image.NewPaletted(f.Bounds(), pal)
		for i, j := 0, 0; i < len(f.Pix); i, j = i+4, j+1 {
			if f.Pix[i+3] < 128 && transp >= 0 {
				p.Pix[j] = uint8(transp)
				continue
			}
			c := [3]uint8{f.Pix[i], f.Pix[i+1], f.Pix[i+2]}
			idx, ok := near[c]
			if !ok {
				idx = uint8(pal.Index(color.NRGBA{c[0], c[1], c[2], 255}))
				near[c] = idx
			}
			p.Pix[j] = idx
		}
		disp := byte(gif.DisposalNone)
		if transp >= 0 {
			disp = gif.DisposalBackground // прозрачное место не должно показывать прошлый кадр
		}
		out.Image = append(out.Image, p)
		out.Delay = append(out.Delay, delays[fi])
		out.Disposal = append(out.Disposal, disp)
	}
	if transp >= 0 {
		out.BackgroundIndex = uint8(transp)
	}
	var buf bytes.Buffer
	err := gif.EncodeAll(&buf, out)
	return buf.Bytes(), err
}

// serveLogoGIF — POST /api/logo-gif?x=&y=&s= (квадрат обрезки в пикселях исходника), тело — гифка
// или кадры из видео (framesMagic, без x, y, s).
// Ответ — {"logo": "data:…"}: его ведущий шлёт в эфир, как PNG.
func (h *Hub) serveLogoGIF(w http.ResponseWriter, r *http.Request) {
	host := identity.FromRequest(r)
	if !h.canLogo(host) {
		http.Error(w, "логотип — по доступу", http.StatusForbidden)
		return
	}
	if !h.gifPer.allow(host) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	select {
	case h.gifJobs <- struct{}{}:
		defer func() { <-h.gifJobs }()
	default:
		http.Error(w, "сервер занят — попробуйте через минуту", http.StatusServiceUnavailable)
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, logoGifUpload))
	if err != nil {
		http.Error(w, "гифка больше 4 МБ", http.StatusRequestEntityTooLarge)
		return
	}
	q := r.URL.Query()
	x, _ := strconv.Atoi(q.Get("x"))
	y, _ := strconv.Atoi(q.Get("y"))
	s, _ := strconv.Atoi(q.Get("s"))
	var out []byte
	if bytes.HasPrefix(b, framesMagic) {
		out, err = logoFromFrames(b)
	} else {
		out, err = logoFromGIF(b, func(w, h int) image.Rectangle { return gifCrop(x, y, s, w, h) })
	}
	if err != nil {
		h.logoChanges.Inc("bad")
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	kind := "image/png"
	if isGIF(out) {
		kind = "image/gif"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]string{"logo": "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(out)})
}
