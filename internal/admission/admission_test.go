package admission

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestGatePoolsAndSources(t *testing.T) {
	g := &Gate{Max: 10, Reserve: 3, OwnerExtra: 2, PerID: 2, PerIP: 4}
	anon := func() Level { return Anon }
	known := func() Level { return Known }
	owner := func() Level { return Owner }

	// пределы на источник
	a1, r1 := g.Acquire(Src{"a", "ip1"}, anon)
	a2, _ := g.Acquire(Src{"a", "ip1"}, anon)
	if _, r := g.Acquire(Src{"a", "ip1"}, anon); r != PerID || r1 != Public {
		t.Fatalf("третье место куки: %s", r)
	}
	g.Acquire(Src{"b", "ip1"}, anon)
	g.Acquire(Src{"c", "ip1"}, anon)
	if _, r := g.Acquire(Src{"d", "ip1"}, anon); r != PerIP {
		t.Fatalf("пятое место IP: %s", r)
	}
	a1.Release()
	a1.Release() // второй раз — ничего
	a2.Release()
	if u, _ := g.Usage(); u != 2 {
		t.Fatalf("занято %d, ждали 2", u)
	}

	// общие места (10 − 3 = 7) — анонимам, резерв — своим, сверх — владельцу
	for i := 0; i < 5; i++ {
		if _, r := g.Acquire(Src{string(rune('k' + i)), ""}, anon); r != Public {
			t.Fatalf("общее место %d: %s", i, r)
		}
	}
	if _, r := g.Acquire(Src{"x", ""}, anon); r != Full {
		t.Fatalf("аноним в резерв: %s", r)
	}
	for i := 0; i < 3; i++ {
		if _, r := g.Acquire(Src{string(rune('p' + i)), ""}, known); r != Reserve {
			t.Fatalf("свой %d: %s", i, r)
		}
	}
	if _, r := g.Acquire(Src{"y", ""}, known); r != Full {
		t.Fatalf("своему сверх Max: %s", r)
	}
	if _, r := g.Acquire(Src{"o", ""}, owner); r != Extra {
		t.Fatalf("владельцу сверх Max: %s", r)
	}
	if u, res := g.Usage(); u != 11 || res != 4 {
		t.Fatalf("занято %d, в резерве %d", u, res)
	}
}

// Одновременный штурм не проскакивает предел.
func TestGateConcurrent(t *testing.T) {
	g := &Gate{Max: 50}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if tk, _ := g.Acquire(Src{string(rune(i)), ""}, nil); tk != nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 50 {
		t.Fatalf("пустили %d из 50", ok)
	}
}

func TestRate(t *testing.T) {
	r := &Rate{PerSec: 10, Burst: 3}
	for i := 0; i < 3; i++ {
		if !r.Allow("ip") {
			t.Fatalf("запас %d", i)
		}
	}
	if r.Allow("ip") {
		t.Fatal("сверх запаса пустили")
	}
	if !r.Allow("other") || !r.Allow("") {
		t.Fatal("чужой ключ задело")
	}
	time.Sleep(150 * time.Millisecond)
	if !r.Allow("ip") {
		t.Fatal("ведро не наполняется")
	}
}

func TestSourcesIP(t *testing.T) {
	s := &Sources{Exempt: append(ParseExempt("10.9.0.0/16"), DefaultExempt...)}
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	a := s.IP(req("127.0.0.1:5000", "6.6.6.6, 1.2.3.4")) // за прокси — последний адрес
	b := s.IP(req("1.2.3.4:5000", ""))
	c := s.IP(req("1.2.3.4:6000", "9.9.9.9")) // не с localhost — заголовку не верим
	if a == "" || a != b || a != c {
		t.Fatalf("один клиент — разные ключи: %q %q %q", a, b, c)
	}
	if a == "1.2.3.4" || len(a) != 16 {
		t.Fatalf("ключ — не хеш: %q", a)
	}
	if s.IP(req("127.0.0.1:1", "")) != "" || s.IP(req("127.0.0.1:1", "10.9.1.1")) != "" {
		t.Fatal("исключённые адреса ограничиваются")
	}
}

func TestPasses(t *testing.T) {
	p := &Passes{Key: []byte("0123456789abcdef0123456789abcdef")}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	p.Issue(rec, r, "id1", Known)
	ck := rec.Result().Cookies()
	if len(ck) != 1 || ck[0].Name != PassCookie || !ck[0].HttpOnly {
		t.Fatalf("пропуск не выдан: %v", ck)
	}
	with := func(id, v string) Level {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: PassCookie, Value: v})
		return p.Check(r, id)
	}
	if with("id1", ck[0].Value) != Known {
		t.Fatal("свой пропуск не принят")
	}
	if with("id2", ck[0].Value) != Anon {
		t.Fatal("пропуск подошёл к чужой куке")
	}
	forged := "3" + ck[0].Value[1:] // повысить уровень без ключа
	if with("id1", forged) != Anon {
		t.Fatal("подделка уровня принята")
	}
	if with("id1", p.Value("id1", Owner, time.Now().Add(-PassTTL-time.Hour))) != Anon {
		t.Fatal("просроченный принят")
	}
	if (&Passes{}).Check(r, "id1") != Anon {
		t.Fatal("без ключа — уровень из пропуска")
	}
}
