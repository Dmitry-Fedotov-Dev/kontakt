package mesh

import (
	"sort"
	"time"
)

// GraphNode, GraphEdge — граф системы, как его видит узел (для Grafana Node Graph).
type GraphNode struct {
	ID          NodeID
	Name        string
	Role        Role
	Health      Health
	Addr        string
	ActiveCalls int
	CallsFree   int
	Utilization float64 // -1 — неизвестна
	CPU         float64 // -1 — неизвестно
	MemoryMB    float64
	SeenAgo     time.Duration // давность последнего heartbeat (у Master'а) или объявления
}

type GraphEdge struct {
	A, B      NodeID
	Kind      PathKind
	Control   bool        // Worker → Master: control plane, не путь для голоса
	Metrics   LinkMetrics // худшая из двух оценок
	Confirmed bool        // ребро объявили оба конца (двусторонняя проверка)
	Quality   string      // ok | warning | critical | unmeasured | unconfirmed | control | lost
}

// Graph — граф системы глазами узла. У Master'а — весь реестр и рёбра из heartbeat'ов
// плюс рёбра control plane к нему самому; у Worker'а — его ограниченная окрестность.
func (n *Node) Graph() ([]GraphNode, []GraphEdge) {
	links := map[NodeID][]AdvertLink{}
	var nodes []GraphNode
	now := time.Now()
	var master NodeID
	if n.Registry != nil {
		master = n.ID
		nodes = append(nodes, GraphNode{ID: n.ID, Name: n.cfg.Name, Role: RoleMaster, Health: Healthy, Addr: n.cfg.Advertise,
			Utilization: -1, CPU: -1})
		for _, e := range n.Registry.Snapshot() {
			hb := e.Heartbeat
			nodes = append(nodes, GraphNode{ID: e.ID, Name: e.Name, Role: e.Role, Health: e.Health, Addr: e.Addr,
				ActiveCalls: hb.ActiveCalls, CallsFree: hb.CallsFree, Utilization: hb.Utilization, CPU: hb.CPU,
				MemoryMB: hb.MemoryMB, SeenAgo: now.Sub(e.LastSeen)})
			if e.Health != Offline {
				links[e.ID] = hb.Links
			}
		}
	} else {
		for id, a := range n.adverts() {
			ago := time.Duration(0)
			if a.Issued > 0 {
				ago = now.Sub(time.Unix(0, a.Issued))
			}
			nodes = append(nodes, GraphNode{ID: id, Name: a.Name, Role: a.Role, Health: a.Health, Utilization: a.Utilization,
				CallsFree: a.CallsFree, CPU: -1, SeenAgo: ago})
			links[id] = a.Links
		}
		n.mu.Lock()
		if n.masterUp && n.admission != nil {
			master = n.admission.Master
		}
		n.mu.Unlock()
		if master != "" {
			nodes = append(nodes, GraphNode{ID: master, Role: RoleMaster, Health: Healthy, Utilization: -1, CPU: -1})
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	known := map[NodeID]bool{}
	for _, gn := range nodes {
		known[gn.ID] = true
	}
	type side struct {
		m    LinkMetrics
		kind PathKind
	}
	seen := map[[2]NodeID]map[NodeID]side{} // пара (по порядку) → кто из концов объявил
	for from, ls := range links {
		for _, l := range ls {
			if !known[l.To] {
				continue
			}
			k := [2]NodeID{from, l.To}
			if k[1] < k[0] {
				k = [2]NodeID{l.To, from}
			}
			if seen[k] == nil {
				seen[k] = map[NodeID]side{}
			}
			seen[k][from] = side{l.Metrics, l.Kind}
		}
	}
	var edges []GraphEdge
	for k, sides := range seen {
		e := GraphEdge{A: k[0], B: k[1]}
		first := true
		for _, s := range sides {
			if first {
				e.Metrics, e.Kind, first = s.m, s.kind, false
				continue
			}
			e.Metrics = worse(e.Metrics, s.m)
		}
		_, a := sides[k[0]]
		_, b := sides[k[1]]
		e.Confirmed = a && b
		e.Quality = n.quality(e)
		edges = append(edges, e)
	}
	if master != "" {
		for _, gn := range nodes {
			if gn.ID != master && gn.Role != RoleMaster && (n.Registry != nil || gn.ID == n.ID) {
				// OFFLINE-узел остаётся связан с Master'ом красным ребром «heartbeat пропал»:
				// иначе он висел бы на графе без рёбер, вдали от остальных, и терялся из виду
				q := "control"
				if gn.Health == Offline {
					q = "lost"
				}
				edges = append(edges, GraphEdge{A: gn.ID, B: master, Control: true, Confirmed: true, Quality: q})
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].A != edges[j].A {
			return edges[i].A < edges[j].A
		}
		return edges[i].B < edges[j].B
	})
	return nodes, edges
}

func worse(a, b LinkMetrics) LinkMetrics {
	if b.RTT > a.RTT {
		a.RTT = b.RTT
	}
	if b.Loss > a.Loss {
		a.Loss = b.Loss
	}
	if b.Jitter > a.Jitter {
		a.Jitter = b.Jitter
	}
	if b.Samples < a.Samples {
		a.Samples = b.Samples
	}
	return a
}

// quality — класс ребра по тем же порогам, что и маршрутизация: на графе видно ровно
// то, чем руководствуется Router.
func (n *Node) quality(e GraphEdge) string {
	th := n.cfg.Thresholds
	m := e.Metrics
	switch {
	case !e.Confirmed:
		return "unconfirmed"
	case m.Samples < th.MinSamples:
		return "unmeasured"
	case m.Loss >= th.LossCritical || m.Jitter >= th.JitterCritical || m.RTT >= th.LatencyCritical:
		return "critical"
	case m.Loss >= th.LossWarning || m.Jitter >= th.JitterWarning || m.RTT >= th.LatencyWarning:
		return "warning"
	}
	return "ok"
}
