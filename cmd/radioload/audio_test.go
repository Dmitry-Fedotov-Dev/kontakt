package main

import (
	"math"
	"testing"
	"time"
)

// ulawDecode — обратное G.711 μ-law, как таблица ULAW страницы.
func ulawDecode(u byte) float64 {
	u = ^u
	t := (int(u&0x0f)<<3 + 132) << ((u >> 4) & 7)
	if u&0x80 != 0 {
		return -float64(t-132) / 32768
	}
	return float64(t-132) / 32768
}

func TestUlawRoundTrip(t *testing.T) {
	for v := -1.0; v <= 1; v += 0.001 {
		if d := math.Abs(ulawDecode(ulaw(v)) - v); d > 0.002+0.07*math.Abs(v) {
			t.Fatalf("%.3f → %.4f", v, ulawDecode(ulaw(v)))
		}
	}
}

// Слух: ровный поток — без потерь; пропуск кадров, рывок сети и переполнение — потери.
func TestEarCountsLoss(t *testing.T) {
	a := stationAudio(1001)
	reset := func() { heardUs.Store(0); silenceUs.Store(0); overflowUs.Store(0); gapFrames.Store(0) }
	feed := func(e *ear, from, n int, at time.Time) {
		b := []byte{}
		for i := from; i < from+n; i++ {
			b = append(b, a[i%loopFrames]...)
		}
		e.hear(b, at)
	}
	t0 := time.Now()

	reset()
	var e ear
	e.reset()
	for k := 0; k < 100; k++ { // 10 с пачками по 5 кадров ровно в срок
		feed(&e, k*5, 5, t0.Add(time.Duration(k)*100*time.Millisecond))
	}
	if s, g := silenceUs.Load(), gapFrames.Load(); s != 0 || g != 0 || heardUs.Load() < 9e6 {
		t.Fatalf("ровный поток: тишина %d мкс, пропуски %d, слышно %d мкс", s, g, heardUs.Load())
	}

	reset()
	e.reset()
	for k := 0; k < 50; k++ {
		feed(&e, k*5, 5, t0.Add(time.Duration(k)*100*time.Millisecond))
	}
	feed(&e, 50*5+10, 5, t0.Add(5100*time.Millisecond)) // сервер потерял 10 кадров
	if g := gapFrames.Load(); g != 10 {
		t.Fatalf("пропуск: %d кадров, ждали 10", g)
	}
	feed(&e, 50*5+15, 5, t0.Add(6100*time.Millisecond)) // рывок сети 1 с при буфере ~0,4 с
	if s := silenceUs.Load(); s < 400e3 || s > 900e3 {
		t.Fatalf("рывок 1 с: тишина %d мкс", s)
	}
}
