package signal

import (
	"testing"
	"time"
)

func TestLineOf(t *testing.T) {
	for in, want := range map[string]string{
		"abcdefgh12":   "abcdefgh12",
		" ABCDEFGH12 ": "abcdefgh12",
		"https://openline.one/?call=k7f3q9zx2m1p":    "k7f3q9zx2m1p",
		"https://x/radio/?a=1&call=k7f3q9zx2m1p&b#c": "k7f3q9zx2m1p",
	} {
		if got := LineOf(in); got != want {
			t.Errorf("LineOf(%q) = %q, ждали %q", in, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"  готово \n\t тесты зелёные ": "готово тесты зелёные",
		"\x1b[31mкрасный":              "[31mкрасный",
		"абвгдежзий":                   "абвгдежзий",
		"абвгдежзийк":                  "абвгдежзи…",
	} {
		max := 40
		if []rune(in)[0] == 'а' {
			max = 10
		}
		if got := clean(in, max); got != want {
			t.Errorf("clean(%q) = %q, ждали %q", in, got, want)
		}
	}
}

func TestAllowNotify(t *testing.T) {
	h, o, now := newRingHub(), RingOptions{NotifyGap: 2 * time.Second, NotifyHour: 3}, time.Now()
	steps := []struct {
		at time.Duration
		ok bool
	}{{0, true}, {time.Second, false}, {3 * time.Second, true}, {6 * time.Second, true}, {9 * time.Second, false}, {61 * time.Minute, true}}
	for _, s := range steps {
		if got := h.allowNotify("line", o, now.Add(s.at)); got != s.ok {
			t.Errorf("через %v: %v, ждали %v", s.at, got, s.ok)
		}
	}
}
