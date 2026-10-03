package mesh

import (
	"container/heap"
	"math"
	"sort"
	"strings"
)

// Route — путь от узла к узлу: узлы по порядку, включая концы, и его счёт.
type Route struct {
	Path  []NodeID `json:"path"`
	Score float64  `json:"score"`
}

func (r Route) Hops() int { return len(r.Path) - 1 }

func (r Route) key() string {
	s := make([]string, len(r.Path))
	for i, id := range r.Path {
		s[i] = string(id)
	}
	return strings.Join(s, ">")
}

// Router считает маршруты по графу объявлений.
type Router struct {
	W  Weights
	Th Thresholds
}

type graph struct {
	nodeCost map[NodeID]float64            // цена провести через узел; нет в карте — узел непригоден
	edges    map[NodeID]map[NodeID]float64 // цена ребра (симметрично; при двух оценках — худшая)
}

// build собирает граф. Ребро A→B берётся из объявления A; если B тоже объявил ребро к
// A, используется худшая из двух оценок: каждый конец видит своё, и честнее
// поверить пессимисту.
func (r Router) build(adverts map[NodeID]Advert) graph {
	g := graph{nodeCost: map[NodeID]float64{}, edges: map[NodeID]map[NodeID]float64{}}
	for id, a := range adverts {
		if c, ok := NodeCost(a.Health, a.Utilization, r.W, r.Th); ok {
			g.nodeCost[id] = c
		}
	}
	set := func(a, b NodeID, c float64) {
		if g.edges[a] == nil {
			g.edges[a] = map[NodeID]float64{}
		}
		if old, ok := g.edges[a][b]; !ok || c > old {
			g.edges[a][b] = c
		}
	}
	// Двусторонняя проверка (как в OSPF): ребро берётся, только если его объявили оба
	// конца. Иначе последнее объявление умершего узла держало бы в графе его рёбра
	// до конца TTL, и маршрут через мертвеца оставался бы «живым». Если второго конца
	// не видно вовсе (за горизонтом max_hops), достаточно одной стороны.
	lists := map[[2]NodeID]bool{}
	for id, a := range adverts {
		for _, l := range a.Links {
			lists[[2]NodeID{id, l.To}] = true
		}
	}
	bad := map[[2]NodeID]bool{}
	for id, a := range adverts {
		for _, l := range a.Links {
			if _, seen := adverts[l.To]; seen && !lists[[2]NodeID{l.To, id}] {
				continue
			}
			c, ok := LinkCost(l.Metrics, r.W, r.Th)
			if !ok {
				bad[[2]NodeID{id, l.To}], bad[[2]NodeID{l.To, id}] = true, true
				continue
			}
			set(id, l.To, c)
			set(l.To, id, c)
		}
	}
	for k := range bad { // ребро, плохое хотя бы с одного конца, непригодно
		if g.edges[k[0]] != nil {
			delete(g.edges[k[0]], k[1])
		}
	}
	return g
}

// Routes — до k лучших путей от src к dst без петель (алгоритм Йена): первый —
// основной, остальные — альтернативы (§15). Цена пути — рёбра плюс узлы-
// ретрансляторы (концы не считаются: через них звонок не транзитный).
func (r Router) Routes(adverts map[NodeID]Advert, src, dst NodeID, k int) []Route {
	g := r.build(adverts)
	first, ok := g.shortest(src, dst, nil, nil)
	if !ok {
		return nil
	}
	found := []Route{first}
	var cands []Route
	seen := map[string]bool{first.key(): true}
	for len(found) < k {
		prev := found[len(found)-1]
		for i := 0; i < len(prev.Path)-1; i++ {
			spur := prev.Path[i]
			root := prev.Path[:i+1]
			banEdges := map[[2]NodeID]bool{}
			for _, p := range found {
				if len(p.Path) > i+1 && equalPrefix(p.Path, root) {
					banEdges[[2]NodeID{p.Path[i], p.Path[i+1]}] = true
				}
			}
			banNodes := map[NodeID]bool{}
			for _, n := range root[:len(root)-1] {
				banNodes[n] = true
			}
			tail, ok := g.shortest(spur, dst, banNodes, banEdges)
			if !ok {
				continue
			}
			path := append(append([]NodeID{}, root[:len(root)-1]...), tail.Path...)
			cand := Route{Path: path, Score: g.cost(path)}
			if !seen[cand.key()] {
				seen[cand.key()] = true
				cands = append(cands, cand)
			}
		}
		if len(cands) == 0 {
			break
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score < cands[j].Score })
		found = append(found, cands[0])
		cands = cands[1:]
	}
	return found
}

func equalPrefix(p, root []NodeID) bool {
	for i := range root {
		if p[i] != root[i] {
			return false
		}
	}
	return true
}

func (g graph) cost(path []NodeID) float64 {
	c := 0.0
	for i := 0; i+1 < len(path); i++ {
		c += g.edges[path[i]][path[i+1]]
		if i > 0 {
			c += g.nodeCost[path[i]]
		}
	}
	return c
}

type item struct {
	id   NodeID
	cost float64
	idx  int
}
type pq []*item

func (q pq) Len() int           { return len(q) }
func (q pq) Less(i, j int) bool { return q[i].cost < q[j].cost }
func (q pq) Swap(i, j int)      { q[i], q[j] = q[j], q[i]; q[i].idx, q[j].idx = i, j }
func (q *pq) Push(x any)        { it := x.(*item); it.idx = len(*q); *q = append(*q, it) }
func (q *pq) Pop() any          { old := *q; it := old[len(old)-1]; *q = old[:len(old)-1]; return it }

// shortest — Дейкстра с ценой узлов-ретрансляторов.
func (g graph) shortest(src, dst NodeID, banNodes map[NodeID]bool, banEdges map[[2]NodeID]bool) (Route, bool) {
	if _, ok := g.nodeCost[src]; !ok {
		return Route{}, false
	}
	if _, ok := g.nodeCost[dst]; !ok {
		return Route{}, false
	}
	dist := map[NodeID]float64{src: 0}
	prev := map[NodeID]NodeID{}
	q := &pq{{id: src}}
	for q.Len() > 0 {
		it := heap.Pop(q).(*item)
		if it.cost > dist[it.id] {
			continue
		}
		if it.id == dst {
			break
		}
		relay := 0.0
		if it.id != src {
			relay = g.nodeCost[it.id]
		}
		for nb, ec := range g.edges[it.id] {
			if banNodes[nb] || banEdges[[2]NodeID{it.id, nb}] {
				continue
			}
			if _, ok := g.nodeCost[nb]; !ok {
				continue
			}
			c := it.cost + relay + ec
			if d, ok := dist[nb]; !ok || c < d {
				dist[nb], prev[nb] = c, it.id
				heap.Push(q, &item{id: nb, cost: c})
			}
		}
	}
	d, ok := dist[dst]
	if !ok || math.IsInf(d, 1) {
		return Route{}, false
	}
	path := []NodeID{dst}
	for at := dst; at != src; at = prev[at] {
		path = append([]NodeID{prev[at]}, path...)
	}
	return Route{Path: path, Score: d}, true
}
