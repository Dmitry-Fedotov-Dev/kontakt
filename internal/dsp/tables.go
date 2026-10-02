package dsp

import (
	"math"
	"math/rand"
)

// ---------- таблицы G.711: кодирование и декодирование без ветвлений на каждом отсчёте ----------

var (
	alawEnc [8192]byte  // индекс: (pcm >> 3) + 4096
	ulawEnc [16384]byte // индекс: (pcm >> 2) + 8192
	alawDec [256]int16
	ulawDec [256]int16
)

func init() {
	for i := range alawEnc {
		alawEnc[i] = AlawEncode(int16((i - 4096) << 3))
	}
	for i := range ulawEnc {
		ulawEnc[i] = UlawEncode(int16((i - 8192) << 2))
	}
	for i := 0; i < 256; i++ {
		alawDec[i] = AlawDecode(byte(i))
		ulawDec[i] = UlawDecode(byte(i))
	}
}

// Encode / Decode кодируют кадр в нужный G.711 по payload type (8 = PCMA, 0 = PCMU).
func Encode(pt byte, pcm []int16, out []byte) {
	if pt == 8 {
		for i, s := range pcm {
			out[i] = alawEnc[(int(s)>>3)+4096]
		}
		return
	}
	for i, s := range pcm {
		out[i] = ulawEnc[(int(s)>>2)+8192]
	}
}

func Decode(pt byte, in []byte, pcm []int16) {
	t := &ulawDec
	if pt == 8 {
		t = &alawDec
	}
	for i, b := range in {
		pcm[i] = t[b]
	}
}

// ---------- генератор служебных звуков станции ----------
// Звуки потом проходят через ту же Line, что и голос, — гудки «жмутся» тем же кодеком.

type Tone int

const (
	ToneNone Tone = iota
	ToneSilence
	ToneRing  // контроль посылки вызова: 425 Гц, 1 с гудок / 4 с пауза
	ToneNoise // белый шум «пустого эфира», как у радио между станциями
)

func ParseTone(s string) Tone {
	switch s {
	case "ring":
		return ToneRing
	case "noise":
		return ToneNoise
	case "silence":
		return ToneSilence
	}
	return ToneNone
}

type ToneGen struct {
	Kind    Tone
	n       int // отсчётов с начала тона
	phase   float64
	flutter float64
	rng     *rand.Rand
}

func (g *ToneGen) Set(k Tone) {
	if g.Kind != k {
		g.Kind, g.n, g.phase = k, 0, 0
	}
	if g.rng == nil {
		g.rng = rand.New(rand.NewSource(rand.Int63()))
	}
}

func (g *ToneGen) Fill(pcm []int16) {
	switch g.Kind {
	case ToneRing:
		const w = 2 * math.Pi * 425 / 8000
		for i := range pcm {
			if (g.n % 40000) < 8000 {
				pcm[i] = int16(math.Sin(g.phase) * 7000)
			} else {
				pcm[i] = 0
			}
			g.phase += w
			if g.phase > 2*math.Pi {
				g.phase -= 2 * math.Pi
			}
			g.n++
		}
	case ToneNoise:
		for i := range pcm {
			// медленное «дыхание» уровня, как у слабого сигнала в эфире
			g.flutter += 2 * math.Pi * 0.7 / 8000
			amp := 2600 + 1100*math.Sin(g.flutter) + 500*math.Sin(g.flutter*3.7)
			pcm[i] = clamp16(g.rng.NormFloat64() * amp)
			g.n++
		}
	default:
		for i := range pcm {
			pcm[i] = 0
		}
		g.n += len(pcm)
	}
}

// Elapsed — сколько секунд звучит текущий тон.
func (g *ToneGen) Elapsed() float64 { return float64(g.n) / 8000 }
