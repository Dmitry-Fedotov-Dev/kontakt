package main

import (
	"log"
	"math"
	"sync/atomic"
	"time"
)

// Звук станций и счёт фактических потерь звука у приёмника.
//
// Станция играет свою мелодию с лёгким шумом — кольцо из loopFrames кадров. Каждый кадр кольца
// уникален, поэтому приёмник по самому кадру знает, какая это станция и какое место в кольце, и
// видит пропуски. Приёмник моделирует буфер страницы (worklet в static/index.html): звук начинается,
// когда накопилось startMs, опустевший буфер — тишина до нового накопления, сверх capMs лишнее
// выкидывается. Потеря звука = тишина + выкинутое + пропущенные кадры — то, что человек не услышал.

const (
	loopFrames = 400 // 8 с
	frameMs    = 20.0
	startMs    = 400.0  // TARGET/START страницы: 3200 отсчётов
	capMs      = 2500.0 // MAXN страницы: 20000 отсчётов
)

var (
	// frameAt — кадр → станция и место в кольце; заполняется до старта, дальше только читается.
	frameAt = map[string]framePos{}

	heardUs, silenceUs, overflowUs atomic.Int64 // мкс: прослушано, тишина, выкинуто
	gapFrames, strayFrames         atomic.Int64 // пропущенные кадры; чужие/повторы
)

type framePos struct{ f, i int }

// stationAudio — кольцо кадров μ-law станции f: арпеджио по пентатонике (своё у каждой волны)
// и шум на −34 дБ, чтобы кадры не повторялись.
func stationAudio(f int) [][]byte {
	scale := []float64{0, 2, 4, 7, 9, 12, 14, 16}
	base := 196 * math.Pow(2, float64(f%12)/12) // соль малой октавы и выше
	seed := uint32(f)*2654435761 + 1
	out := make([][]byte, loopFrames)
	for k := range out {
		fr := make([]byte, 160)
		for j := range fr {
			n := k*160 + j
			note := (n / 2000) % 16 // нота — 250 мс
			step := scale[(note*int(3+f%5))%len(scale)]
			hz := base * math.Pow(2, step/12)
			ph := float64(n%2000) / 2000
			env := math.Min(1, ph*20) * (1 - 0.6*ph) // атака и затухание
			t := float64(n) / 8000
			v := env * (0.22*math.Sin(2*math.Pi*hz*t) + 0.08*math.Sin(math.Pi*hz*t))
			seed = seed*1664525 + 1013904223
			v += 0.02 * (float64(seed>>8)/float64(1<<24) - 0.5)
			fr[j] = ulaw(v)
		}
		out[k] = fr
		if _, dup := frameAt[string(fr)]; dup {
			log.Fatalf("станция %d: кадр %d не уникален", f, k)
		}
		frameAt[string(fr)] = framePos{f, k}
	}
	return out
}

// ulaw — G.711 μ-law для отсчёта −1…1, как кодирует страница ведущего.
func ulaw(v float64) byte {
	s := int(math.Max(-1, math.Min(1, v)) * 32767)
	sign := 0
	if s < 0 {
		s, sign = -s, 0x80
	}
	s += 132
	if s > 32767 {
		s = 32767
	}
	exp := 7
	for mask := 0x4000; s&mask == 0 && exp > 0; mask >>= 1 {
		exp--
	}
	mant := (s >> (exp + 3)) & 0x0f
	return ^byte(sign | exp<<4 | mant)
}

// ear — слух одного приёмника: буфер страницы и непрерывность кадров. Только для читающей горутины.
type ear struct {
	f, next          int
	started, playing bool
	buf              float64 // мс в буфере
	last             time.Time
}

// reset — ручку повернули: прошлая станция не в счёт, новая сначала копит буфер.
func (e *ear) reset() { *e = ear{f: -1} }

// hear — пришло сообщение звука (один или пачка кадров).
func (e *ear) hear(b []byte, now time.Time) {
	if e.started { // что успело проиграться с прошлого сообщения
		dt := float64(now.Sub(e.last).Microseconds()) / 1000
		heardUs.Add(int64(dt * 1000))
		if e.playing {
			e.buf -= dt
			if e.buf < 0 {
				silenceUs.Add(int64(-e.buf * 1000))
				e.buf, e.playing = 0, false
			}
		} else {
			silenceUs.Add(int64(dt * 1000))
		}
	}
	e.last = now
	for k := 0; k+160 <= len(b); k += 160 {
		p, ok := frameAt[string(b[k:k+160])]
		if !ok {
			strayFrames.Add(1)
			continue
		}
		if p.f != e.f { // первый кадр новой станции
			*e = ear{f: p.f, next: p.i, last: now}
		}
		switch gap := (p.i - e.next + loopFrames) % loopFrames; {
		case gap == 0:
		case gap < loopFrames/2:
			gapFrames.Add(int64(gap))
		default: // повтор или кадр из прошлого
			strayFrames.Add(1)
			continue
		}
		e.next = (p.i + 1) % loopFrames
		e.buf += frameMs
		if !e.playing && e.buf >= startMs {
			e.playing, e.started = true, true
		}
		if e.buf > capMs {
			overflowUs.Add(int64((e.buf - startMs) * 1000))
			e.buf = startMs
		}
	}
}
