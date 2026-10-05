package radio

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
)

// Картинка превью для ссылки на волну (Telegram, WhatsApp, VK, Discord… читают og:image).
// Рисуется на холсте 300×158 «пикселей» и увеличивается в 4 раза без сглаживания:
// 1200×632 — пропорция 1.91:1, которую мессенджеры показывают большой карточкой.
const (
	ogW, ogH, ogScale = 300, 158, 4
	OGWidth           = ogW * ogScale
	OGHeight          = ogH * ogScale
)

const (
	cSky = iota
	cSkyDot
	cInk
	cBody
	cHi
	cLo
	cCream
	cCream2
	cSpk
	cSpk2
	cCone
	cLCD
	cAmber
	cAmberDim
	cRed
	cGrey
	cYellow
	cKnob
	cKnobHi
	cKnobLo
	cRim
	cRimLo
	cLilac
	cLilacDim
)

var ogPalette = color.Palette{
	rgb(0x2B1B3D), rgb(0x3A2850), rgb(0x1E0F0A), rgb(0xE8573F), rgb(0xFF9274), rgb(0xA83424),
	rgb(0xFFE9C2), rgb(0xF2C98A), rgb(0x3B2620), rgb(0x5C3A2C), rgb(0x6E4838), rgb(0x21160B),
	rgb(0xFFC24B), rgb(0x7A5420), rgb(0xFF3B2F), rgb(0xA9A3B5), rgb(0xFFE36E), rgb(0xFFF1D6),
	rgb(0xFFFFFF), rgb(0xE2C38F), rgb(0xD9B67E), rgb(0x8E6A3A), rgb(0xC9B6E4), rgb(0x9C86BC),
}

func rgb(v uint32) color.RGBA { return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255} }

type canvas struct{ img *image.Paletted }

func (c canvas) px(x, y int, ci uint8) {
	if x < 0 || y < 0 || x >= ogW || y >= ogH {
		return
	}
	for dy := 0; dy < ogScale; dy++ {
		row := c.img.Pix[(y*ogScale+dy)*c.img.Stride:]
		for dx := 0; dx < ogScale; dx++ {
			row[x*ogScale+dx] = ci
		}
	}
}

func (c canvas) rect(x, y, w, h int, ci uint8) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			c.px(i, j, ci)
		}
	}
}

// rrect и disc — те же «пиксельные» скругления, что рисует страница: превью похоже на радио на сайте.
func (c canvas) rrect(x, y, w, h, r int, ci uint8) {
	for dy := 0; dy < h; dy++ {
		e := -1.0
		if dy < r {
			e = float64(r-dy) - 0.5
		} else if dy >= h-r {
			e = float64(dy-(h-r)) + 0.5
		}
		inset := 0
		if e >= 0 {
			inset = int(math.Round(float64(r) - math.Sqrt(math.Max(0, float64(r*r)-e*e))))
		}
		c.rect(x+inset, y+dy, w-2*inset, 1, ci)
	}
}

func (c canvas) disc(cx, cy, r int, ci uint8) {
	for y := -r; y <= r; y++ {
		w := int(math.Floor(math.Sqrt(float64(r*r-y*y)) + 0.5))
		c.rect(cx-w, cy+y, 2*w+1, 1, ci)
	}
}

func (c canvas) line(x0, y0, x1, y1, t int, ci uint8) {
	n := max(abs(x1-x0), abs(y1-y0))
	for i := 0; i <= n; i++ {
		x := int(math.Round(float64(x0) + float64(x1-x0)*float64(i)/float64(n)))
		y := int(math.Round(float64(y0) + float64(y1-y0)*float64(i)/float64(n)))
		c.rect(x, y, t, t, ci)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// text пишет строку шрифтом 5×7 с масштабом k и возвращает ширину в пикселях холста.
func (c canvas) text(s string, x, y, k int, ci uint8) int {
	x0 := x
	for _, r := range s {
		g, _ := glyph(r)
		for row := 0; row < 7; row++ {
			for col := 0; col < 5; col++ {
				if g[row][col] == '#' {
					c.rect(x+col*k, y+row*k, k, k, ci)
				}
			}
		}
		x += 6 * k
	}
	return x - x0
}

// drawRadio — то же радио, что на странице, со смещением (ox, oy); на шкале — частота.
func (c canvas) drawRadio(ox, oy, freq int, lang string) {
	// антенна и волны эфира от неё
	c.line(ox+100, oy+19, ox+116, oy+4, 2, cInk)
	c.rrect(ox+96, oy+16, 10, 5, 1, cInk)
	c.rect(ox+115, oy+2, 3, 3, cYellow)
	for k, ci := range []uint8{cAmber, cAmberDim} {
		r := 7 + k*5
		for deg := -70; deg <= 10; deg += 9 {
			a := float64(deg) * math.Pi / 180
			c.px(ox+116+int(math.Round(math.Cos(a)*float64(r))), oy+3+int(math.Round(math.Sin(a)*float64(r))), ci)
		}
	}
	// ножки и корпус
	c.rrect(ox+16, oy+72, 14, 12, 2, cInk)
	c.rrect(ox+98, oy+72, 14, 12, 2, cInk)
	c.rect(ox+18, oy+79, 10, 3, cLo)
	c.rect(ox+100, oy+79, 10, 3, cLo)
	c.rrect(ox+4, oy+19, 120, 60, 9, cInk)
	c.rrect(ox+6, oy+21, 116, 56, 7, cBody)
	c.rect(ox+13, oy+23, 96, 2, cHi)
	c.rect(ox+8, oy+30, 2, 30, cHi)
	c.rect(ox+14, oy+73, 100, 2, cLo)
	c.rect(ox+118, oy+32, 2, 34, cLo)
	// динамик
	c.disc(ox+34, oy+54, 18, cInk)
	c.disc(ox+34, oy+54, 16, cCream2)
	c.disc(ox+34, oy+54, 14, cInk)
	c.disc(ox+34, oy+54, 13, cSpk)
	for y := -12; y <= 12; y++ {
		for x := -12; x <= 12; x++ {
			if x*x+y*y <= 140 && (x+30)%3 == 0 && (y+30)%3 == 0 {
				c.px(ox+34+x, oy+54+y, cSpk2)
			}
		}
	}
	c.disc(ox+34, oy+54, 8, cInk)
	c.disc(ox+34, oy+54, 7, cCone)
	c.disc(ox+34, oy+54, 1, cCream2)
	// шкала
	c.rrect(ox+61, oy+26, 58, 22, 3, cInk)
	c.rect(ox+63, oy+28, 54, 18, cLCD)
	if freq > 0 {
		c.text(FormatFreq(freq), ox+66, oy+30, 1, cAmber)
		for i := 0; i < 5; i++ {
			h := i*2 + 2
			c.rect(ox+107+i*2, oy+37-h, 1, h, cAmber)
		}
		c.text(ogWords[lang].onAir, ox+66, oy+39, 1, cRed)
	} else {
		c.text(ogWords[lang].band, ox+66, oy+30, 1, cAmber)
	}
	// ручка: риска показывает частоту
	kx, ky, kr := ox+90, oy+63, 13
	c.disc(kx, ky, kr+2, cLo)
	a := float64(freq-MinFreq)/float64(MaxFreq-MinFreq)*2*math.Pi - math.Pi/2
	for dy := -kr - 1; dy <= kr+1; dy++ {
		for dx := -kr - 1; dx <= kr+1; dx++ {
			r := math.Hypot(float64(dx), float64(dy))
			if r > float64(kr)+0.5 {
				continue
			}
			th := math.Atan2(float64(dy), float64(dx))
			var ci uint8
			switch {
			case r > float64(kr)-0.5:
				ci = cInk
			case r > float64(kr)-3.5:
				ph := math.Mod((th-a)/(2*math.Pi)*16, 1)
				if ph < 0 {
					ph++
				}
				ci = cRim
				if ph < 0.45 {
					ci = cRimLo
				}
			case r > float64(kr)-4.5:
				ci = cInk
			default:
				da := math.Atan2(math.Sin(th-a), math.Cos(th-a))
				l := (-float64(dx) - float64(dy)) / (r + 0.01)
				switch {
				case math.Abs(da)*r < 1.1 && r > 2.5:
					ci = cRed
				case l > 0.55 && r > 4:
					ci = cKnobHi
				case l < -0.55 && r > 5:
					ci = cKnobLo
				default:
					ci = cKnob
				}
			}
			c.px(kx+dx, ky+dy, ci)
		}
	}
}

// ogWords — надписи картинки. На шкале нарисованного приёмника места на 6 знаков до столбиков
// сигнала: «ЭФИР», а не «В ЭФИРЕ».
var ogWords = map[string]struct {
	brand, big1, big2, codec, freq, played, live, open, onAir, band string
	pitch                                                           []string
}{
	"ru": {"ОТКРЫТОЕ РАДИО", "ОТКРЫТОЕ", "РАДИО", "G.711 · 64 КБИТ/С", " МГЦ", "♪ ИГРАЛО:", "В ЭФИРЕ ПРЯМО СЕЙЧАС",
		"> ОТКРОЙ И СЛУШАЙ", "ЭФИР", "УКВ",
		[]string{"СВОЯ ВОЛНА — ВЕЩАЙ", "МУЗЫКУ И ГОЛОС.", "ИЛИ КРУТИ РУЧКУ", "И СЛУШАЙ."}},
	"en": {"OPEN RADIO", "OPEN", "RADIO", "G.711 · 64 KBIT/S", " FM", "♪ WAS PLAYING:", "ON AIR RIGHT NOW",
		"> OPEN AND LISTEN", "ON AIR", "FM",
		[]string{"YOUR OWN FREQUENCY —", "BROADCAST MUSIC", "AND VOICE. OR TURN", "THE KNOB AND LISTEN."}},
}

// OGImage — PNG превью: радио, частота, название станции и что играло; lang — "ru" или "en".
// freq == 0 — общая картинка сайта.
func OGImage(freq int, name, track, lang string) []byte {
	wd, ok := ogWords[lang]
	if !ok {
		lang, wd = "ru", ogWords["ru"]
	}
	c := canvas{image.NewPaletted(image.Rect(0, 0, OGWidth, OGHeight), ogPalette)}
	for y := 2; y < ogH; y += 4 {
		for x := 2; x < ogW; x += 4 {
			c.px(x, y, cSkyDot)
		}
	}
	c.drawRadio(8, 38, freq, lang)

	const tx, cols = 146, 24 // справа от радио: 24 знака по 6 пикселей
	c.text(wd.brand, tx, 12, 1, cLilac)
	if freq == 0 {
		c.text(wd.big1, tx, 32, 2, cAmber)
		c.text(wd.big2, tx, 50, 2, cAmber)
		y := 76
		for _, l := range wd.pitch {
			c.text(l, tx, y, 1, cCream)
			y += 10
		}
		c.text(wd.codec, tx, 140, 1, cLilacDim)
		return encodePNG(c.img)
	}

	c.text(FormatFreq(freq)+wd.freq, tx, 26, 3, cAmber)
	y := 56
	if name == "" {
		name = "RADIO " + FormatFreq(freq)
	}
	if n := []rune(name); len(n) <= 12 {
		c.text(name, tx, y, 2, cCream)
		y += 22
	} else {
		for _, l := range wrapText(name, cols, 2) {
			c.text(l, tx, y, 1, cCream)
			y += 10
		}
		y += 4
	}
	if track != "" {
		c.text(wd.played, tx, y, 1, cAmber)
		y += 11
		for _, l := range wrapText(track, cols, max(1, (132-y)/10)) {
			c.text(l, tx, y, 1, cCream)
			y += 10
		}
	} else {
		c.text(wd.live, tx, y, 1, cAmber)
	}
	c.text(wd.open, tx, 142, 1, cLilacDim)
	return encodePNG(c.img)
}

func encodePNG(img image.Image) []byte {
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&b, img); err != nil {
		return nil // в память кодирование не падает; nil обработает вызывающий
	}
	return b.Bytes()
}

// ogTitle — заголовок карточки ссылки.
func ogTitle(freq int, name, lang string) string {
	t := ogTexts[lang]
	if name == "" {
		return FormatFreq(freq) + t.Freq + " · " + t.Site
	}
	return strings.TrimSpace(name) + " · " + FormatFreq(freq) + t.Freq
}
