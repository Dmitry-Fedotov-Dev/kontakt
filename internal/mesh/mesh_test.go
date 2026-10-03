package mesh

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdentityPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.key")
	a, err := LoadOrCreateIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || !a.Pub.Equal(b.Pub) {
		t.Fatalf("после перезапуска другая идентичность: %s → %s", a.ID, b.ID)
	}
	if st, _ := filepath.Glob(p); len(st) != 1 {
		t.Fatal("ключ не сохранён")
	}
}

func TestEnvelopeVerify(t *testing.T) {
	id, _ := NewIdentity()
	other, _ := NewIdentity()
	s := NewSigner(id)
	e, _ := s.Seal(MsgPing, map[string]int{"n": 1})

	if err := NewVerifier().Verify(e); err != nil {
		t.Fatalf("честное сообщение отвергнуто: %v", err)
	}
	v := NewVerifier()
	_ = v.Verify(e)
	if err := v.Verify(e); err != ErrReplay {
		t.Fatalf("повтор принят: %v", err)
	}
	tampered := *e
	tampered.Body = []byte(`{"n":2}`)
	if err := NewVerifier().Verify(&tampered); err != ErrBadSignature {
		t.Fatalf("подменённое тело принято: %v", err)
	}
	spoof := *e
	spoof.From = other.ID // чужое имя со своим ключом
	if err := NewVerifier().Verify(&spoof); err != ErrIDMismatch {
		t.Fatalf("чужое имя принято: %v", err)
	}
	old := NewSigner(id)
	old.now = func() time.Time { return time.Now().Add(-time.Hour) }
	stale, _ := old.Seal(MsgPing, nil)
	if err := NewVerifier().Verify(stale); err == nil {
		t.Fatal("сообщение часовой давности принято")
	}
}

// После перезапуска узла номера сообщений продолжают расти — соседи не считают его
// сообщения повтором.
func TestSignerSurvivesRestart(t *testing.T) {
	id, _ := NewIdentity()
	v := NewVerifier()
	e1, _ := NewSigner(id).Seal(MsgPing, nil)
	if err := v.Verify(e1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	e2, _ := NewSigner(id).Seal(MsgPing, nil) // «перезапуск»: новый Signer
	if err := v.Verify(e2); err != nil {
		t.Fatalf("после перезапуска сообщения отвергаются: %v", err)
	}
}

func TestAdmissionAndTrust(t *testing.T) {
	master, _ := NewIdentity()
	rogue, _ := NewIdentity()
	w, _ := NewIdentity()
	adm := IssueAdmission(master, w.ID, w.Pub, RoleWorker, time.Hour)
	now := time.Now()
	if err := adm.Check(master.Pub, now); err != nil {
		t.Fatalf("честный допуск: %v", err)
	}
	if err := adm.Check(rogue.Pub, now); err == nil {
		t.Fatal("допуск принят чужим ключом Master'а")
	}
	if err := adm.Check(master.Pub, now.Add(2*time.Hour)); err == nil {
		t.Fatal("истёкший допуск принят")
	}
	forged := IssueAdmission(rogue, w.ID, w.Pub, RoleWorker, time.Hour)
	forged.Master = master.ID
	if err := forged.Check(master.Pub, now); err == nil {
		t.Fatal("поддельный допуск принят")
	}

	tr := NewTrust(master.Pub)
	if st := tr.Evaluate(w.ID, nil, now); st != Authenticated {
		t.Fatalf("без допуска: %v, ждали AUTHENTICATED", st)
	}
	tr.MarkKnown(w.ID)
	if st := tr.State(w.ID); st != Known {
		t.Fatalf("из реестра: %v, ждали KNOWN", st)
	}
	if st := tr.Evaluate(w.ID, &adm, now); st != Authorized {
		t.Fatalf("с допуском: %v, ждали AUTHORIZED", st)
	}
	tr.SetConnected(w.ID, true)
	if st := tr.State(w.ID); st != Connected {
		t.Fatalf("связь: %v", st)
	}
	// Допуск чужому узлу не делает авторизованным предъявителя.
	x, _ := NewIdentity()
	if st := tr.Evaluate(x.ID, &adm, now); st != Authenticated {
		t.Fatalf("чужой допуск сработал: %v", st)
	}
	// Локальный список доверенных работает без Master'а.
	tr2 := NewTrust(nil)
	tr2.AddTrusted(x.ID)
	if st := tr2.Evaluate(x.ID, nil, now); st != Authorized {
		t.Fatalf("доверенный из списка: %v", st)
	}
}

func TestProber(t *testing.T) {
	clock := time.Now()
	p := NewProber(10, time.Second)
	p.now = func() time.Time { return clock }
	for i := 0; i < 8; i++ {
		n := p.Next()
		clock = clock.Add(40 * time.Millisecond)
		p.Pong(n)
		clock = clock.Add(time.Second)
	}
	lost := p.Next() // без ответа
	clock = clock.Add(1500 * time.Millisecond)
	p.Pong(lost) // опоздал: не RTT, а потеря
	m := p.Metrics()
	if m.Samples != 9 || m.Loss < 0.1 || m.Loss > 0.12 {
		t.Fatalf("потери: %+v", m)
	}
	if m.RTT != 40*time.Millisecond {
		t.Fatalf("RTT %v, ждали 40 мс", m.RTT)
	}
	if m.Jitter != 0 {
		t.Fatalf("джиттер при ровном RTT: %v", m.Jitter)
	}
}

// §24: свободно 17 Мбит/с, звонок 400 кбит/с — около 42 звонков.
func TestCallCapacitySpecExample(t *testing.T) {
	nc := Capacity(3e6, 20, 0, 0) // канал 20, занято 3 → свободно 17
	if nc.AvailableMbps != 17 {
		t.Fatalf("свободно %v", nc.AvailableMbps)
	}
	cc := Calls(nc, 0, 0)
	if cc.Calls != 42 || cc.Measured {
		t.Fatalf("звонков %d (measured=%v), ждали 42 по плановой полосе", cc.Calls, cc.Measured)
	}
	// Есть звонки — полоса звонка по замеру, а не плановая.
	cc = Calls(Capacity(2.4e6, 20, 0.3, 0), 10, 0.3) // 10 звонков = 2,4 Мбит/с → 240 кбит/с
	if !cc.Measured || cc.CallKbps != 240 {
		t.Fatalf("полоса звонка %v measured=%v", cc.CallKbps, cc.Measured)
	}
	if cc.Calls != 48 { // (20×0,7 − 2,4) Мбит/с / 240 кбит/с = 48,3
		t.Fatalf("с запасом 30%%: %d", cc.Calls)
	}
	// Ёмкость канала неизвестна — звонков 0, а не выдуманное число.
	if cc := Calls(Capacity(1e6, 0, 0.3, 0), 2, 0.3); cc.Calls != 0 {
		t.Fatalf("без ёмкости канала: %d", cc.Calls)
	}
}

func good(rtt time.Duration, loss float64) LinkMetrics {
	return LinkMetrics{RTT: rtt, Loss: loss, Jitter: 2 * time.Millisecond, Samples: 20}
}

func adv(id NodeID, util float64, links ...AdvertLink) Advert {
	return Advert{Origin: id, Health: Healthy, Utilization: util, Links: links}
}

// net строит объявления, где каждое ребро объявлено обоими концами — как в живой
// сети (двусторонняя проверка в Router).
func net_(util map[NodeID]float64, edges ...[3]any) map[NodeID]Advert {
	ads := map[NodeID]Advert{}
	for id, u := range util {
		ads[id] = adv(id, u)
	}
	for _, e := range edges {
		a, b, m := e[0].(NodeID), e[1].(NodeID), e[2].(LinkMetrics)
		x, y := ads[a], ads[b]
		x.Links = append(x.Links, AdvertLink{To: b, Metrics: m})
		y.Links = append(y.Links, AdvertLink{To: a, Metrics: m})
		ads[a], ads[b] = x, y
	}
	return ads
}

func e(a, b NodeID, m LinkMetrics) [3]any { return [3]any{a, b, m} }

// §39: A→C короче по RTT, но 5% потерь и 90% загрузки — выбираем B.
func TestRoutingAvoidsLossyOverloaded(t *testing.T) {
	r := Router{W: DefaultWeights(), Th: DefaultThresholds()}
	ads := net_(map[NodeID]float64{"A": 0.1, "B": 0.35, "C": 0.9, "Z": 0.1},
		e("A", "B", good(40*time.Millisecond, 0.002)), e("A", "C", good(25*time.Millisecond, 0.05)),
		e("B", "Z", good(10*time.Millisecond, 0)), e("C", "Z", good(10*time.Millisecond, 0)))
	rs := r.Routes(ads, "A", "Z", 3)
	if len(rs) == 0 || rs[0].Path[1] != "B" {
		t.Fatalf("маршрут %v, ждали через B", rs)
	}
	for _, rt := range rs {
		for _, n := range rt.Path {
			if n == "C" {
				t.Fatalf("непригодный C в альтернативах: %v", rs)
			}
		}
	}
	// Тот же C без потерь и без перегруза — уже лучший.
	ads = net_(map[NodeID]float64{"A": 0.1, "B": 0.35, "C": 0.2, "Z": 0.1},
		e("A", "B", good(40*time.Millisecond, 0.002)), e("A", "C", good(25*time.Millisecond, 0)),
		e("B", "Z", good(10*time.Millisecond, 0)), e("C", "Z", good(10*time.Millisecond, 0)))
	if rs := r.Routes(ads, "A", "Z", 3); rs[0].Path[1] != "C" {
		t.Fatalf("исправный C не выбран: %v", rs)
	}
}

// §14-пример: A-B, A-C, B-D, C-E, D-E. Основной и альтернативы без петель.
func TestRoutesAlternatives(t *testing.T) {
	m := good(20*time.Millisecond, 0)
	r := Router{W: DefaultWeights(), Th: DefaultThresholds()}
	ads := net_(map[NodeID]float64{"A": 0.1, "B": 0.1, "C": 0.1, "D": 0.1, "E": 0.1},
		e("A", "B", m), e("A", "C", good(30*time.Millisecond, 0)), e("B", "D", m), e("C", "E", m), e("D", "E", m))
	rs := r.Routes(ads, "A", "D", 3)
	if len(rs) != 2 {
		t.Fatalf("ждали 2 пути A→D, есть %v", rs)
	}
	if got := rs[0].key(); got != "A>B>D" {
		t.Fatalf("основной %s", got)
	}
	if got := rs[1].key(); got != "A>C>E>D" {
		t.Fatalf("альтернатива %s", got)
	}
	if rs[1].Score <= rs[0].Score {
		t.Fatal("альтернатива не хуже основного")
	}
	// OFFLINE-узел в маршрут не попадает.
	b := ads["B"]
	b.Health = Offline
	ads["B"] = b
	if rs := r.Routes(ads, "A", "D", 3); len(rs) != 1 || rs[0].key() != "A>C>E>D" {
		t.Fatalf("при OFFLINE B: %v", rs)
	}
}

func TestFailoverProbation(t *testing.T) {
	p1 := Route{Path: []NodeID{"A", "B", "D"}, Score: 50}
	p2 := Route{Path: []NodeID{"A", "C", "E", "D"}, Score: 90}
	f := NewFailover(3, 0.2)
	if a, _ := f.Update([]Route{p1, p2}); a.key() != p1.key() {
		t.Fatal("начальный не основной")
	}
	if a, ch := f.Update([]Route{p2}); !ch || a.key() != p2.key() || f.Failovers != 1 {
		t.Fatalf("отказ основного: %v ch=%v failovers=%d", a, ch, f.Failovers)
	}
	// p1 вернулся, но на испытании: остаёмся на p2 Probation пересчётов.
	for i := 0; i < 2; i++ {
		if a, _ := f.Update([]Route{p1, p2}); a.key() != p2.key() {
			t.Fatalf("вернулись на неперемеренный маршрут на %d-м пересчёте", i+1)
		}
	}
	if a, ch := f.Update([]Route{p1, p2}); !ch || a.key() != p1.key() {
		t.Fatalf("после испытательного срока не вернулись на лучший: %v", a)
	}
	// Почти равный путь не вытесняет текущий (гистерезис).
	p3 := Route{Path: []NodeID{"A", "X", "D"}, Score: 45}
	if a, ch := f.Update([]Route{p3, p1}); ch || a.key() != p1.key() {
		t.Fatal("прыгнули на путь лучше всего на 10%")
	}
	// Пропуск на испытании обнуляет счёт.
	f2 := NewFailover(2, 0.2)
	f2.Update([]Route{p1, p2})
	f2.Update([]Route{p2})     // p1 упал
	f2.Update([]Route{p1, p2}) // 1 хороший
	f2.Update([]Route{p2})     // снова пропал
	if a, _ := f2.Update([]Route{p1, p2}); a.key() != p2.key() {
		t.Fatal("испытательный срок не начался заново после пропуска")
	}
}

func TestTopologyBounded(t *testing.T) {
	tp := NewTopology("A", 2)
	now := time.Now().UnixNano()
	a := &Advert{Origin: "B", Version: 1, Issued: now, TTL: int64(time.Minute), Hops: 1}
	if !tp.Apply(a) {
		t.Fatal("новое объявление не переслано")
	}
	if tp.Apply(a) {
		t.Fatal("дубликат переслан")
	}
	if tp.Apply(&Advert{Origin: "B", Version: 1, Issued: now, TTL: int64(time.Minute)}) {
		t.Fatal("та же версия переслана")
	}
	if fw := tp.Apply(&Advert{Origin: "C", Version: 1, Issued: now, TTL: int64(time.Minute), Hops: 2}); fw {
		t.Fatal("объявление на пределе MaxHops переслано дальше")
	}
	if tp.Apply(&Advert{Origin: "D", Version: 1, Issued: now, TTL: int64(time.Minute), Hops: 3}) {
		t.Fatal("объявление дальше MaxHops принято")
	}
	if len(tp.Nodes()) != 2 {
		t.Fatalf("узлы %v", tp.Nodes())
	}
	tp.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if gone := tp.Expire(); len(gone) != 2 || len(tp.Nodes()) != 0 {
		t.Fatalf("протухшие не убраны: %v", gone)
	}
}

// Ребро, которое объявил только один конец при видимом втором, в граф не берётся:
// так последнее объявление умершего узла не держит маршрут через него.
func TestTwoWayCheck(t *testing.T) {
	m := good(20*time.Millisecond, 0)
	r := Router{W: DefaultWeights(), Th: DefaultThresholds()}
	ads := map[NodeID]Advert{
		"A": adv("A", 0.1, AdvertLink{To: "C", Metrics: good(30*time.Millisecond, 0)}),       // A уже не видит B
		"B": adv("B", 0.1, AdvertLink{To: "A", Metrics: m}, AdvertLink{To: "C", Metrics: m}), // старое объявление B
		"C": adv("C", 0.1, AdvertLink{To: "A", Metrics: good(30*time.Millisecond, 0)}, AdvertLink{To: "B", Metrics: m}),
	}
	for _, rt := range r.Routes(ads, "A", "C", 3) {
		for _, n := range rt.Path {
			if n == "B" {
				t.Fatalf("маршрут через ребро, которое A не подтверждает: %v", rt.Path)
			}
		}
	}
	// Второй конец за горизонтом — достаточно одной стороны.
	delete(ads, "C")
	ads["A"] = adv("A", 0.1, AdvertLink{To: "C", Metrics: m})
	if rs := r.Routes(map[NodeID]Advert{"A": ads["A"], "C": adv("C", 0.1, AdvertLink{To: "A", Metrics: m})}, "A", "C", 1); len(rs) != 1 {
		t.Fatal("подтверждённое ребро не взято")
	}
}

// Отказ основного без замены всё равно считается отказом.
func TestFailoverToNothingCounts(t *testing.T) {
	p1 := Route{Path: []NodeID{"A", "B", "D"}, Score: 50}
	p2 := Route{Path: []NodeID{"A", "C", "D"}, Score: 70}
	f := NewFailover(3, 0.2)
	f.Update([]Route{p1})
	if a, ch := f.Update(nil); !ch || a != nil || f.Failovers != 1 {
		t.Fatalf("отказ в никуда: active=%v failovers=%d", a, f.Failovers)
	}
	if a, _ := f.Update([]Route{p2}); a == nil || a.key() != p2.key() || f.Failovers != 1 {
		t.Fatalf("восстановление через другой путь: %v, failovers=%d", a, f.Failovers)
	}
}

// Перестановка соседних сообщений — не повтор; настоящий повтор и слишком старое —
// отвергаются.
func TestReplayWindow(t *testing.T) {
	id, _ := NewIdentity()
	s := NewSigner(id)
	e5, _ := s.Seal(MsgPing, nil)
	e6, _ := s.Seal(MsgPing, nil)
	v := NewVerifier()
	if err := v.Verify(e6); err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(e5); err != nil {
		t.Fatalf("переставленное сообщение отвергнуто как повтор: %v", err)
	}
	if err := v.Verify(e5); err != ErrReplay {
		t.Fatalf("повтор переставленного принят: %v", err)
	}
	if err := v.Verify(e6); err != ErrReplay {
		t.Fatalf("повтор принят: %v", err)
	}
	old, _ := s.Seal(MsgPing, nil)
	for i := 0; i < replayBits+1; i++ {
		e, _ := s.Seal(MsgPing, nil)
		if err := v.Verify(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Verify(old); err != ErrReplay {
		t.Fatalf("сообщение за окном принято: %v", err)
	}
}

// Метрики cloudflared читаются по факту и суммируются по меткам; Мбит/с из них не
// выводятся (§19, §47).
func TestCloudflaredPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`# HELP cloudflared_tcp_total_sessions x
cloudflared_tcp_total_sessions 12
cloudflared_tunnel_ha_connections{conn="0"} 1
cloudflared_tunnel_ha_connections{conn="1"} 1
cloudflared_unrelated 5
`))
	}))
	defer srv.Close()
	cf := &Cloudflared{URL: srv.URL}
	cf.Poll()
	v, ok := cf.Values()
	if !ok || v["cloudflared_tcp_total_sessions"] != 12 || v["cloudflared_tunnel_ha_connections"] != 2 {
		t.Fatalf("%v ok=%v", v, ok)
	}
	if _, has := v["cloudflared_unrelated"]; has {
		t.Fatal("взята посторонняя метрика")
	}
	(&Cloudflared{URL: "http://127.0.0.1:1/metrics"}).Poll() // нет cloudflared — не паника
}

// Без ёмкости канала метрика свободных звонков — NaN, а не ноль и не выдумка.
func TestNodeMetricsUnknownCapacity(t *testing.T) {
	id, _ := NewIdentity()
	n, _ := NewNode(Config{Role: RoleWorker, Identity: id})
	rec := httptest.NewRecorder()
	n.MetricsRegistry(nil).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{"kontakt_available_capacity NaN", "kontakt_bandwidth_utilization NaN",
		"kontakt_call_bandwidth_measured 0", "# TYPE kontakt_route_failovers_total counter"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}
}
