package mesh

import (
	"sort"
	"sync"
	"time"
)

// AdvertLink — ребро в объявлении узла: к кому, каким путём, с каким качеством.
type AdvertLink struct {
	To      NodeID      `json:"to"`
	Kind    PathKind    `json:"kind"`
	Metrics LinkMetrics `json:"m"`
}

// Advert — объявление узла о себе и своих рёбрах (§14). Распространяется не дальше
// MaxHops от источника, живёт TTL, новее — по Version. Полного графа сети ни у кого
// нет: только окрестность.
type Advert struct {
	Origin      NodeID       `json:"origin"`
	Version     uint64       `json:"v"`
	Issued      int64        `json:"issued"` // Unix нс
	TTL         int64        `json:"ttl"`    // нс
	Hops        int          `json:"hops"`   // сколько раз уже пересылали
	Role        Role         `json:"role"`
	Health      Health       `json:"health"`
	Utilization float64      `json:"util"` // -1 — неизвестна
	CallsFree   int          `json:"calls_free"`
	Links       []AdvertLink `json:"links"`
}

// Topology — ограниченный граф вокруг узла.
type Topology struct {
	Self    NodeID
	MaxHops int

	mu      sync.Mutex
	adverts map[NodeID]*Advert
	now     func() time.Time
}

func NewTopology(self NodeID, maxHops int) *Topology {
	if maxHops <= 0 {
		maxHops = 4
	}
	return &Topology{Self: self, MaxHops: maxHops, adverts: map[NodeID]*Advert{}, now: time.Now}
}

// Apply — пришло объявление. Возвращает true, если оно новее известного и его стоит
// переслать дальше (с Hops+1): так рассылается только изменившееся (delta), а
// дубликаты гасятся.
func (t *Topology) Apply(a *Advert) (forward bool) {
	if a.Origin == "" || a.Hops > t.MaxHops {
		return false
	}
	if t.now().UnixNano() > a.Issued+a.TTL {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur := t.adverts[a.Origin]; cur != nil && cur.Version >= a.Version {
		return false
	}
	cp := *a
	t.adverts[a.Origin] = &cp
	return a.Origin != t.Self && a.Hops < t.MaxHops
}

// Expire убирает протухшие объявления (узел молчит дольше TTL — его нет).
func (t *Topology) Expire() (gone []NodeID) {
	now := t.now().UnixNano()
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, a := range t.adverts {
		if now > a.Issued+a.TTL {
			delete(t.adverts, id)
			gone = append(gone, id)
		}
	}
	return gone
}

// Snapshot — копия известных объявлений.
func (t *Topology) Snapshot() map[NodeID]Advert {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[NodeID]Advert, len(t.adverts))
	for id, a := range t.adverts {
		out[id] = *a
	}
	return out
}

// Nodes — известные узлы, по порядку.
func (t *Topology) Nodes() []NodeID {
	s := t.Snapshot()
	ids := make([]NodeID, 0, len(s))
	for id := range s {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
