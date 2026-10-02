package dsp

// «Старая линия»: всё, что слышит абонент, проходит через эту цепочку.
//
//   PCM 8 кГц → полоса 300–3400 Гц (как у ТфОП) → кодек-деградация → шипение/треск → G.711
//
// Режимы (выбираются номером, который «набирает» абонент, или флагом -line):
//   64     — чистый G.711 A-law, 8 бит × 8 кГц = 64 кбит/с (обычный цифровой телефон)
//   32     — ADPCM 4 бита × 8 кГц = 32 кбит/с (как G.726-32 / DECT, IMA ADPCM)
//   16     — ADPCM 2 бита × 8 кГц = 16 кбит/с (как G.726-16, сильно «хрустит»)
//   8      — 1 бит дельта-модуляция CVSD-подобная (военная/спутниковая связь 70-х), совсем каша
//   clean  — без эффектов, только G.711 (для отладки)

import (
	"math"
	"math/rand"
	"strings"
)

type LineMode int

const (
	ModeClean LineMode = iota
	Mode64
	Mode32
	Mode16
	Mode8
)

func ParseLineMode(s string) (LineMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "clean", "0":
		return ModeClean, true
	case "64", "pcma", "g711":
		return Mode64, true
	case "32", "adpcm4", "g726-32":
		return Mode32, true
	case "16", "adpcm2", "g726-16":
		return Mode16, true
	case "8", "cvsd", "1bit":
		return Mode8, true
	}
	return 0, false
}

func (m LineMode) String() string {
	return [...]string{"clean", "64 кбит/с G.711", "32 кбит/с ADPCM-4", "16 кбит/с ADPCM-2", "8 кбит/с 1-бит"}[m]
}

// --- биквадратный фильтр (RBJ cookbook) ---

type biquad struct{ b0, b1, b2, a1, a2, x1, x2, y1, y2 float64 }

func newBiquad(kind string, f0, fs, q float64) *biquad {
	w := 2 * math.Pi * f0 / fs
	cw, sw := math.Cos(w), math.Sin(w)
	alpha := sw / (2 * q)
	var b0, b1, b2 float64
	if kind == "lp" {
		b0, b1, b2 = (1-cw)/2, 1-cw, (1-cw)/2
	} else {
		b0, b1, b2 = (1+cw)/2, -(1 + cw), (1+cw)/2
	}
	a0 := 1 + alpha
	return &biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: -2 * cw / a0, a2: (1 - alpha) / a0}
}

func (f *biquad) do(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1, f.y2, f.y1 = f.x1, x, f.y1, y
	return y
}

// Line — состояние эффекта для одного направления звука.
type Line struct {
	mode       LineMode
	hp1, hp2   biquad
	lp1, lp2   biquad
	ima        imaState
	two        twoBitState
	cvsd       cvsdState
	rng        uint64 // xorshift64*: дёшево и без замков
	crackleEnv float64
	humPos     int
}

// hum — ровно один период 50 Гц при 8 кГц (160 отсчётов), считаем один раз.
var hum [160]float64

func init() {
	for i := range hum {
		hum[i] = 40 * math.Sin(2*math.Pi*float64(i)/160)
	}
}

func (l *Line) next() uint64 {
	l.rng ^= l.rng >> 12
	l.rng ^= l.rng << 25
	l.rng ^= l.rng >> 27
	return l.rng * 2685821657736338717
}

// uni — равномерно в [-1, 1).
func (l *Line) uni() float64 { return float64(int64(l.next())) / (1 << 63) }

// gauss — приближённо нормальный шум (сумма трёх равномерных), σ ≈ 1.
func (l *Line) gauss() float64 { return (l.uni() + l.uni() + l.uni()) * 1.0 }

// softClip — рациональное приближение tanh, без вызова math.Tanh.
func softClip(x float64) float64 {
	if x > 3 {
		return 1
	}
	if x < -3 {
		return -1
	}
	x2 := x * x
	return x * (27 + x2) / (27 + 9*x2)
}

func NewLine(mode LineMode) *Line {
	return &Line{
		mode: mode,
		hp1:  *newBiquad("hp", 300, 8000, 0.707), hp2: *newBiquad("hp", 300, 8000, 0.707),
		lp1: *newBiquad("lp", 3400, 8000, 0.707), lp2: *newBiquad("lp", 3400, 8000, 0.707),
		two:  twoBitState{step: 64},
		cvsd: cvsdState{step: 40},
		rng:  rand.Uint64() | 1,
	}
}

// Process обрабатывает кадр PCM 8 кГц на месте.
func (l *Line) Process(pcm []int16) {
	if l.mode == ModeClean {
		return
	}
	for i, s := range pcm {
		x := float64(s)
		// полоса пропускания телефонного канала, 4-й порядок
		x = l.lp2.do(l.lp1.do(l.hp2.do(l.hp1.do(x))))
		// кодек-деградация
		switch l.mode {
		case Mode32:
			x = float64(l.ima.roundtrip(clamp16(x)))
		case Mode16:
			x = float64(l.two.roundtrip(clamp16(x)))
		case Mode8:
			x = float64(l.cvsd.roundtrip(clamp16(x)))
		}
		// шум линии: белое шипение + еле слышный фон 50 Гц
		x += l.gauss()*90 + hum[l.humPos]
		if l.humPos++; l.humPos == len(hum) {
			l.humPos = 0
		}
		// треск: редкие щелчки (~0,4 в секунду) с быстрым затуханием
		if l.next()%20000 == 0 {
			l.crackleEnv = 3000 + 2500*(l.uni()+1)
		}
		if l.crackleEnv > 1 {
			x += l.uni() * l.crackleEnv
			l.crackleEnv *= 0.82
		}
		// лёгкая перегрузка, как у угольного микрофона
		x = 30000 * softClip(x/30000*1.3)
		pcm[i] = clamp16(x)
	}
}

func clamp16(x float64) int16 {
	if x > 32767 {
		return 32767
	}
	if x < -32768 {
		return -32768
	}
	return int16(x)
}

// --- IMA ADPCM, 4 бита на отсчёт = 32 кбит/с ---

var imaIndexTable = [16]int{-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8}
var imaStepTable = [89]int{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230,
	253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963,
	1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327,
	3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442,
	11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
}

type imaState struct{ pred, index int }

// roundtrip кодирует отсчёт в 4-битный код и сразу декодирует — так мы слышим потери кодека.
func (s *imaState) roundtrip(x int16) int16 {
	step := imaStepTable[s.index]
	diff := int(x) - s.pred
	code := 0
	if diff < 0 {
		code = 8
		diff = -diff
	}
	delta := step >> 3
	if diff >= step {
		code |= 4
		diff -= step
		delta += step
	}
	if diff >= step>>1 {
		code |= 2
		diff -= step >> 1
		delta += step >> 1
	}
	if diff >= step>>2 {
		code |= 1
		delta += step >> 2
	}
	if code&8 != 0 {
		s.pred -= delta
	} else {
		s.pred += delta
	}
	s.pred = max(-32768, min(32767, s.pred))
	s.index = max(0, min(88, s.index+imaIndexTable[code]))
	return int16(s.pred)
}

// --- 2-битный ADPCM = 16 кбит/с (упрощённый аналог G.726-16) ---
// код: знак + 1 бит величины; шаг адаптируется по величине.

type twoBitState struct {
	pred float64
	step float64
}

func (s *twoBitState) roundtrip(x int16) int16 {
	diff := float64(x) - s.pred
	neg := diff < 0
	if neg {
		diff = -diff
	}
	var q float64
	if diff >= s.step {
		q = 1.5 * s.step
		s.step *= 1.55
	} else {
		q = 0.5 * s.step
		s.step *= 0.87
	}
	if neg {
		q = -q
	}
	s.pred = (s.pred + q) * 0.985 // утечка предиктора, чтобы не уползал
	s.step = math.Max(24, math.Min(12000, s.step))
	return clamp16(s.pred)
}

// --- 1-битная дельта-модуляция с адаптивным шагом (CVSD-подобная) = 8 кбит/с ---

type cvsdState struct {
	pred, step float64
	hist       [3]bool
}

func (s *cvsdState) roundtrip(x int16) int16 {
	bit := float64(x) > s.pred
	s.hist[2], s.hist[1], s.hist[0] = s.hist[1], s.hist[0], bit
	if s.hist[0] == s.hist[1] && s.hist[1] == s.hist[2] {
		s.step = math.Min(s.step*1.5, 6000)
	} else {
		s.step = math.Max(s.step*0.9, 30)
	}
	if bit {
		s.pred += s.step
	} else {
		s.pred -= s.step
	}
	s.pred *= 0.97
	return clamp16(s.pred)
}

// --- G.711 ---

func AlawEncode(pcm int16) byte {
	// классическая реализация Sun g711.c
	segEnd := [8]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}
	s := int(pcm) >> 3
	mask := 0xD5
	if s < 0 {
		mask = 0x55
		s = -s - 1
	}
	seg := 0
	for seg < 8 && s > segEnd[seg] {
		seg++
	}
	if seg >= 8 {
		return byte(0x7F ^ mask)
	}
	aval := seg << 4
	if seg < 2 {
		aval |= (s >> 1) & 0x0F
	} else {
		aval |= (s >> seg) & 0x0F
	}
	return byte(aval ^ mask)
}

func AlawDecode(a byte) int16 {
	a ^= 0x55
	t := (int(a) & 0x0F) << 4
	seg := (int(a) & 0x70) >> 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if a&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}

func UlawEncode(pcm int16) byte {
	const bias = 0x84
	s := int(pcm)
	sign := 0
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > 32635 {
		s = 32635
	}
	s += bias
	exp := 7
	for mask := 0x4000; s&mask == 0 && exp > 0; mask >>= 1 {
		exp--
	}
	mant := (s >> (exp + 3)) & 0x0F
	return ^byte(sign | exp<<4 | mant)
}

func UlawDecode(u byte) int16 {
	u = ^u
	t := ((int(u) & 0x0F) << 3) + 0x84
	t <<= (int(u) & 0x70) >> 4
	if u&0x80 != 0 {
		return int16(0x84 - t)
	}
	return int16(t - 0x84)
}
