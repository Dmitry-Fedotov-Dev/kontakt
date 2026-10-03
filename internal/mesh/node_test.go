package mesh

import (
	"fmt"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("не дождались: %s", what)
}

type cluster struct {
	t      *testing.T
	master *Node
	mid    *Identity
	token  string
}

func fast(c *Config) {
	c.ProbeInterval = 30 * time.Millisecond
	c.ProbeTimeout = 500 * time.Millisecond // под -race ответы опаздывают: это не потери сети
	c.AdvertInterval = 100 * time.Millisecond
	c.AdvertTTL = time.Second
	c.HeartbeatInterval = 100 * time.Millisecond
	c.RegistryTTL = 400 * time.Millisecond
}

func newCluster(t *testing.T) *cluster {
	mid, _ := NewIdentity()
	cfg := Config{Role: RoleMaster, Identity: mid, Listen: "127.0.0.1:0", JoinToken: "секрет"}
	fast(&cfg)
	m, err := NewNode(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOnce(m) })
	return &cluster{t: t, master: m, mid: mid, token: "секрет"}
}

var closed = map[*Node]bool{}

func closeOnce(n *Node) {
	if !closed[n] {
		closed[n] = true
		n.Close()
	}
}

func (c *cluster) worker(mod func(*Config)) *Node {
	id, _ := NewIdentity()
	cfg := Config{Role: RoleWorker, Identity: id, Listen: "127.0.0.1:0", MasterAddr: c.master.Addr(),
		MasterPub: c.mid.Pub, JoinToken: c.token}
	fast(&cfg)
	if mod != nil {
		mod(&cfg)
	}
	w, err := NewNode(cfg)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { closeOnce(w) })
	return w
}

func pathOf(r *Route) string {
	if r == nil {
		return "нет"
	}
	return fmt.Sprint(r.Path)
}

// Критерии успеха §48 на настоящих TCP-соединениях: Master + 4 Worker'а.
func TestMeshEndToEnd(t *testing.T) {
	c := newCluster(t)
	var w1, w3 *Node
	// Прямое ребро W1–W3 теряет половину проб: короткий путь, но непригодный (§39).
	lossy := func(self **Node, other **Node) func(NodeID) float64 {
		return func(p NodeID) float64 {
			if *other != nil && p == (*other).ID {
				return 0.5
			}
			return 0
		}
	}
	w1 = c.worker(func(cfg *Config) { cfg.testDrop = lossy(&w1, &w3) })
	w2 := c.worker(nil)
	w3 = c.worker(func(cfg *Config) { cfg.testDrop = lossy(&w3, &w1) })
	w4 := c.worker(nil)
	ws := []*Node{w1, w2, w3, w4}

	waitFor(t, "все зарегистрированы и получили допуск", 5*time.Second, func() bool {
		for _, w := range ws {
			if !w.MasterUp() {
				return false
			}
		}
		return len(c.master.Registry.Snapshot()) == 4
	})
	waitFor(t, "каждый видит троих соседей", 5*time.Second, func() bool {
		for _, w := range ws {
			if len(w.Peers()) != 3 {
				return false
			}
		}
		return true
	})
	for _, w := range ws {
		for id := range w.Peers() {
			if w.Trust.State(id) != Connected {
				t.Fatalf("%s → %s: %s", w.ID, id, w.Trust.State(id))
			}
		}
	}

	// Маршрут W1→W3 идёт в обход потерь, через ретранслятор, а не напрямую.
	var r *Route
	// Ждём, пока прямое ребро измерено и потери на нём видны: до замеров маршрут в
	// обход мог получиться и случайно (ребро ещё не подтверждено второй стороной).
	waitFor(t, "прямое W1–W3 измерено, маршрут через ретранслятор", 5*time.Second, func() bool {
		m := w1.Peers()[w3.ID]
		r, _ = w1.Route(w3.ID)
		return m.Samples >= 10 && m.Loss >= 0.2 && r != nil && r.Hops() == 2
	})
	relay := r.Path[1]
	if relay != w2.ID && relay != w4.ID {
		t.Fatalf("ретранслятор %s — не W2 и не W4", relay)
	}
	_, alts := w1.Route(w3.ID)
	t.Logf("основной %v, альтернативы %d", r.Path, len(alts))
	if len(alts) == 0 {
		t.Fatal("нет альтернативы основному маршруту")
	}

	// Граф системы у Master'а: все узлы, рёбра данных с качеством, control plane.
	waitFor(t, "граф у Master'а", 3*time.Second, func() bool {
		nodes, edges := c.master.Graph()
		data, ctl := 0, 0
		for _, e := range edges {
			if e.Control {
				ctl++
			} else {
				data++
			}
		}
		return len(nodes) == 5 && data == 6 && ctl == 4
	})
	_, edges := c.master.Graph()
	for _, e := range edges {
		lossyPair := (e.A == w1.ID && e.B == w3.ID) || (e.A == w3.ID && e.B == w1.ID)
		if lossyPair && e.Quality != "critical" {
			t.Fatalf("ребро W1–W3 с потерями на графе %q, ждали critical: %+v", e.Quality, e.Metrics)
		}
		if !lossyPair && !e.Control && e.Quality == "critical" {
			t.Fatalf("исправное ребро на графе critical: %+v", e)
		}
	}

	// 1. Отказ Master'а не рвёт связи Worker'ов и маршруты (§6, §48.1).
	closeOnce(c.master)
	waitFor(t, "Worker'ы заметили отказ Master'а", 3*time.Second, func() bool { return !w1.MasterUp() })
	time.Sleep(500 * time.Millisecond)
	for _, w := range ws {
		if len(w.Peers()) != 3 {
			t.Fatalf("после отказа Master'а у %s соседей %d", w.ID, len(w.Peers()))
		}
	}
	if r2, _ := w1.Route(w3.ID); r2 == nil {
		t.Fatalf("после отказа Master'а маршрута нет; рёбра:\n%s", dumpLinks(w1))
	}

	// 2. Отказ Worker'а-ретранслятора — автоматический failover (§48.2). Ретранслятор
	// берётся заново: за время проверок маршрут мог честно перейти на другой.
	r, _ = w1.Route(w3.ID)
	relay = r.Path[1]
	var dead, other *Node = w2, w4
	if relay == w4.ID {
		dead, other = w4, w2
	}
	before := w1.RouteFailovers()
	closeOnce(dead)
	waitFor(t, "failover на другой ретранслятор", 5*time.Second, func() bool {
		r, _ = w1.Route(w3.ID)
		return r != nil && r.Hops() == 2 && r.Path[1] == other.ID
	})
	if w1.RouteFailovers() <= before {
		t.Fatalf("failover не посчитан: %d → %d", before, w1.RouteFailovers())
	}
	t.Logf("после отказа %s: %v", dead.ID, pathOf(r))
}

// Новый Worker присоединяется без перенастройки Master'а и остальных (§48.3), а узел
// без допуска не принимается (§8).
func TestJoinAndReject(t *testing.T) {
	c := newCluster(t)
	w1 := c.worker(nil)
	w2 := c.worker(nil)
	waitFor(t, "W1 и W2 соединены", 5*time.Second, func() bool { return len(w1.Peers()) == 1 })

	w3 := c.worker(nil)
	waitFor(t, "W3 вошёл в mesh и виден в топологии", 5*time.Second, func() bool {
		r, _ := w1.Route(w3.ID)
		return len(w3.Peers()) == 2 && r != nil
	})

	// Чужой: свой ключ, без допуска и не в списке доверенных.
	rogueID, _ := NewIdentity()
	rogue, _ := NewNode(Config{Role: RoleWorker, Identity: rogueID, Listen: "127.0.0.1:0", Bootstrap: []string{w2.Addr()}})
	fast(&rogue.cfg)
	if err := rogue.Start(); err != nil {
		t.Fatal(err)
	}
	defer rogue.Close()
	time.Sleep(500 * time.Millisecond)
	if _, ok := w2.Peers()[rogueID.ID]; ok {
		t.Fatal("узел без допуска принят в соседи")
	}
	if st := w2.Trust.State(rogueID.ID); st != Authenticated {
		t.Fatalf("чужой: %s, ждали AUTHENTICATED (ключ подтверждён, допуска нет)", st)
	}

	// Неверный токен: Master не выдаёт допуск.
	bad := c.worker(func(cfg *Config) { cfg.JoinToken = "не тот" })
	time.Sleep(500 * time.Millisecond)
	if bad.MasterUp() || len(c.master.Registry.Snapshot()) != 3 {
		t.Fatal("регистрация с неверным токеном прошла")
	}

	// Старт без Master'а через доверенный Worker (§11): локальный список доверенных.
	lone, _ := NewIdentity()
	w2.Trust.AddTrusted(lone.ID)
	ln, _ := NewNode(Config{Role: RoleWorker, Identity: lone, Listen: "127.0.0.1:0",
		Bootstrap: []string{w2.Addr()}, Trusted: []NodeID{w2.ID}})
	fast(&ln.cfg)
	if err := ln.Start(); err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	waitFor(t, "доверенный узел вошёл без Master'а", 3*time.Second, func() bool {
		_, ok := w2.Peers()[lone.ID]
		return ok
	})
}

func dumpLinks(n *Node) string {
	var out string
	for id, a := range n.adverts() {
		for _, l := range a.Links {
			out += fmt.Sprintf("  %.6s→%.6s rtt=%v jit=%v loss=%.2f n=%d\n", id, l.To, l.Metrics.RTT, l.Metrics.Jitter, l.Metrics.Loss, l.Metrics.Samples)
		}
	}
	return out
}
