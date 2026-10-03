package mesh

import (
	"math"
	"sync"
	"time"
)

// PlanningCallKbps — плановая полоса звонка на Worker'е (§23: 100 кбит/с в сторону,
// ~400 кбит/с на ретрансляцию двустороннего звонка). Это ДОПУЩЕНИЕ для старта, а не
// ёмкость: как только есть замер, используется он, и CallCapacity.Measured = true.
const PlanningCallKbps = 400

// NetworkCapacity — полоса узла по факту (§47). Никакой константы Cloudflare: всё
// из замеров и того, что оператор указал о своём канале.
type NetworkCapacity struct {
	ObservedMbps   float64 `json:"observed_mbps"`  // текущий трафик узла
	CapacityMbps   float64 `json:"capacity_mbps"`  // ёмкость канала: указана оператором или 0 — неизвестна
	AvailableMbps  float64 `json:"available_mbps"` // свободно с учётом запаса
	Utilization    float64 `json:"utilization"`    // ObservedMbps / CapacityMbps, 0..1; -1 — неизвестна
	ActiveSessions int     `json:"active_sessions"`
}

// CallCapacity — сколько ещё звонков узел примет (§24).
type CallCapacity struct {
	Calls        int     `json:"calls"`
	CallKbps     float64 `json:"call_kbps"` // полоса звонка, по которой посчитано
	Measured     bool    `json:"measured"`  // true — CallKbps из замера, false — плановое допущение
	ActiveCalls  int     `json:"active_calls"`
	ReserveRatio float64 `json:"reserve"` // доля канала, которую держим свободной (§25)
}

// Meter считает полосу по счётчику байт: Sample(всего_байт) раз в интервал.
type Meter struct {
	mu        sync.Mutex
	lastBytes uint64
	lastAt    time.Time
	bps       float64 // сглаженная полоса, бит/с
	have      bool
	now       func() time.Time
}

func NewMeter() *Meter { return &Meter{now: time.Now} }

// Sample — текущий накопленный счётчик байт (в обе стороны).
func (m *Meter) Sample(totalBytes uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if !m.lastAt.IsZero() && totalBytes >= m.lastBytes {
		if dt := now.Sub(m.lastAt).Seconds(); dt > 0 {
			cur := float64(totalBytes-m.lastBytes) * 8 / dt
			if !m.have {
				m.bps, m.have = cur, true
			} else {
				m.bps += (cur - m.bps) / 4
			}
		}
	}
	m.lastBytes, m.lastAt = totalBytes, now
}

// Bps — сглаженная полоса, бит/с.
func (m *Meter) Bps() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bps
}

// Capacity — полоса узла с учётом ёмкости канала и запаса.
func Capacity(observedBps, capacityMbps, reserve float64, sessions int) NetworkCapacity {
	nc := NetworkCapacity{ObservedMbps: observedBps / 1e6, CapacityMbps: capacityMbps, ActiveSessions: sessions, Utilization: -1}
	if capacityMbps > 0 {
		nc.Utilization = nc.ObservedMbps / capacityMbps
		nc.AvailableMbps = math.Max(0, capacityMbps*(1-reserve)-nc.ObservedMbps)
	}
	return nc
}

// Calls — сколько ещё звонков влезет: свободно / полоса звонка (§24). Полоса звонка —
// по замеру (трафик / активные звонки), пока звонков нет — плановая.
func Calls(nc NetworkCapacity, activeCalls int, reserve float64) CallCapacity {
	cc := CallCapacity{ActiveCalls: activeCalls, ReserveRatio: reserve, CallKbps: PlanningCallKbps}
	if activeCalls > 0 && nc.ObservedMbps > 0 {
		cc.CallKbps, cc.Measured = nc.ObservedMbps*1000/float64(activeCalls), true
	}
	if nc.CapacityMbps > 0 && cc.CallKbps > 0 {
		cc.Calls = int(nc.AvailableMbps * 1000 / cc.CallKbps)
	}
	return cc
}
