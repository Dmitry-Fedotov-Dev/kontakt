package mesh

import (
	"crypto/ed25519"
	"sort"
	"sync"
	"time"
)

// Heartbeat — что Worker сообщает о себе (§10).
type Heartbeat struct {
	Health       Health  `json:"health"`
	CPU          float64 `json:"cpu"`       // ядер занято процессом; -1 — неизвестно
	MemoryMB     float64 `json:"memory_mb"` //
	ActiveCalls  int     `json:"active_calls"`
	BandwidthBps float64 `json:"bandwidth_bps"` // трафик по факту
	Utilization  float64 `json:"utilization"`   // -1 — ёмкость канала неизвестна
	CallsFree    int     `json:"calls_free"`
	Version      string  `json:"version"`
}

// PeerInfo — что Master сообщает о другом узле.
type PeerInfo struct {
	ID     NodeID            `json:"id"`
	Pub    ed25519.PublicKey `json:"pub"`
	Role   Role              `json:"role"`
	Addr   string            `json:"addr"`
	Health Health            `json:"health"`
}

// Entry — запись реестра (§9).
type Entry struct {
	PeerInfo
	Transports []PathKind `json:"transports"`
	Version    string     `json:"version"`
	Heartbeat  Heartbeat  `json:"heartbeat"`
	LastSeen   time.Time  `json:"last_seen"`
}

// Registry — реестр Master'а. Запись без heartbeat дольше TTL — OFFLINE, дольше
// 3×TTL — удаляется: реестр не копит мёртвые узлы.
type Registry struct {
	TTL time.Duration

	mu  sync.Mutex
	m   map[NodeID]*Entry
	now func() time.Time
}

func NewRegistry(ttl time.Duration) *Registry {
	return &Registry{TTL: orDefault(ttl, 30*time.Second), m: map[NodeID]*Entry{}, now: time.Now}
}

func (r *Registry) Upsert(info PeerInfo, transports []PathKind, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[info.ID]
	if e == nil {
		e = &Entry{}
		r.m[info.ID] = e
	}
	e.PeerInfo, e.Transports, e.Version, e.LastSeen = info, transports, version, r.now()
	if e.Health == "" {
		e.Health = Healthy
	}
}

func (r *Registry) Beat(id NodeID, hb Heartbeat) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[id]
	if e == nil {
		return false
	}
	e.Heartbeat, e.LastSeen, e.Health = hb, r.now(), hb.Health
	if e.Health == "" {
		e.Health = Healthy
	}
	return true
}

// Expire помечает молчащих OFFLINE и удаляет совсем пропавших.
func (r *Registry) Expire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for id, e := range r.m {
		switch age := now.Sub(e.LastSeen); {
		case age > 3*r.TTL:
			delete(r.m, id)
		case age > r.TTL:
			e.Health = Offline
		}
	}
}

// Peers — не больше max живых узлов для except: обнаружение ограничено (§11).
// Сначала здоровые, среди них — самые свежие.
func (r *Registry) Peers(except NodeID, max int) []PeerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var es []*Entry
	for id, e := range r.m {
		if id != except && e.Health != Offline && e.Role != RoleMaster && e.Addr != "" {
			es = append(es, e)
		}
	}
	sort.Slice(es, func(i, j int) bool {
		if (es[i].Health == Healthy) != (es[j].Health == Healthy) {
			return es[i].Health == Healthy
		}
		return es[i].LastSeen.After(es[j].LastSeen)
	})
	if max > 0 && len(es) > max {
		es = es[:max]
	}
	out := make([]PeerInfo, len(es))
	for i, e := range es {
		out[i] = e.PeerInfo
	}
	return out
}

// Snapshot — копия реестра (админка, метрики).
func (r *Registry) Snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.m))
	for _, e := range r.m {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
