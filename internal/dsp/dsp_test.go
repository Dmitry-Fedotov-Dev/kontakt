package dsp

import (
	"math"
	"testing"
)

func TestG711Tables(t *testing.T) {
	pcm := []int16{0, 1, -1, 100, -100, 1000, -1000, 8000, -8000, 32767, -32768}
	for _, pt := range []byte{0, 8} {
		enc := make([]byte, len(pcm))
		dec := make([]int16, len(pcm))
		Encode(pt, pcm, enc)
		Decode(pt, enc, dec)
		for i, v := range pcm {
			if d := math.Abs(float64(dec[i]) - float64(v)); d > math.Abs(float64(v))/16+40 {
				t.Errorf("pt %d: %d → %d", pt, v, dec[i])
			}
		}
	}
}

// Каждый режим сохраняет сигнал (корреляция с исходной синусоидой), но портит его.
func TestLineModesKeepSignal(t *testing.T) {
	for _, m := range []LineMode{Mode64, Mode32, Mode16, Mode8} {
		l := NewLine(m)
		var corr, e1, e2 float64
		for f := 0; f < 50; f++ {
			pcm := make([]int16, 160)
			ref := make([]float64, 160)
			for i := range pcm {
				ref[i] = math.Sin(2 * math.Pi * 1000 * float64(f*160+i) / 8000)
				pcm[i] = int16(ref[i] * 10000)
			}
			l.Process(pcm)
			if f < 10 {
				continue
			}
			for i := range pcm {
				x := float64(pcm[i])
				corr += x * ref[i]
				e1 += x * x
				e2 += ref[i] * ref[i]
			}
		}
		c := math.Abs(corr / math.Sqrt(e1*e2))
		t.Logf("%-20s корреляция с оригиналом %.2f", m, c)
		if c < 0.3 {
			t.Errorf("%s: сигнал потерян", m)
		}
	}
}

func BenchmarkLineFrame(b *testing.B) {
	for _, m := range []LineMode{Mode64, Mode32, Mode16, Mode8} {
		b.Run(m.String(), func(b *testing.B) {
			l := NewLine(m)
			pcm := make([]int16, 160)
			for i := 0; i < b.N; i++ {
				l.Process(pcm)
			}
		})
	}
}
