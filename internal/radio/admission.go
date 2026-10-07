package radio

import (
	"net/http"
	"sync"
	"time"

	"kontakt/internal/admission"
	"kontakt/internal/moderation"
)

// Допуск радио: ворота приёмников и частот (internal/admission). Свои — кто идёт с пропуском,
// кому модерация засчитала сессии, кого назначили вручную, кто отключился меньше seatHold назад
// (моргнула сеть — возвращается на своё место через резерв, даже когда общие места заняты).

const (
	seatHold    = 90 * time.Second // столько после обрыва приёмник считается своим
	levelMaxAge = time.Minute      // уровень из модерации кешируется: под атакой не ходить в неё на каждое подключение
)

type levelCache struct {
	mu sync.Mutex
	m  map[string]levelAt
}

type levelAt struct {
	lv admission.Level
	at time.Time
}

func (h *Hub) initAdmission() {
	o := h.opt
	h.listenGate = admission.Gate{Max: o.MaxListeners, Reserve: min(o.ReserveListeners, o.MaxListeners), OwnerExtra: o.OwnerExtra,
		PerID: o.PerIDListeners, PerIP: o.PerIPListeners}
	h.hostGate = admission.Gate{Max: o.MaxStations, Reserve: min(o.ReserveStations, o.MaxStations),
		PerID: o.PerIDStations, PerIP: o.PerIPStations}
	h.srcs.Exempt = o.Exempt
	if h.srcs.Exempt == nil {
		h.srcs.Exempt = admission.DefaultExempt
	}
	if o.ConnRate > 0 {
		h.conns = admission.Rate{PerSec: o.ConnRate, Burst: max(o.ConnBurst, o.ConnRate)}
	}
	h.seats = map[string]time.Time{}
	h.levels.m = map[string]levelAt{}
	h.listenAdmit = h.reg.CounterVec("kontakt_radio_listen_admission_total",
		"Допуск приёмников: public — общие места, reserve — резерв своим, owner — владелец сверх предела; "+
			"отказы: per_id/per_ip — у куки или IP уже предел мест, rate_ip — с IP слишком часто, full — всё занято",
		"result", admission.Results...)
	h.hostAdmit = h.reg.CounterVec("kontakt_radio_host_admission_total",
		"Допуск ведущих (частоты): исходы как у kontakt_radio_listen_admission_total", "result", admission.Results...)
	gauge := func(name, help string, g *admission.Gate, reserved bool) {
		h.reg.Gauge(name, help, func() float64 {
			u, r := g.Usage()
			if reserved {
				return float64(r)
			}
			return float64(u)
		})
	}
	gauge("kontakt_radio_listen_slots_used", "Занятые места приёмников (вместе с резервом)", &h.listenGate, false)
	gauge("kontakt_radio_listen_slots_reserved", "Из них — в резерве своих", &h.listenGate, true)
	gauge("kontakt_radio_host_slots_used", "Занятые частоты (вместе с резервом)", &h.hostGate, false)
	gauge("kontakt_radio_host_slots_reserved", "Из них — в резерве доверенных ведущих", &h.hostGate, true)
	h.reg.Gauge("kontakt_radio_listen_slots_max", "Предел мест приёмников", func() float64 { return float64(h.listenGate.Max) })
	h.reg.Gauge("kontakt_radio_listen_slots_public", "Из предела — общие места (остальное — резерв)",
		func() float64 { return float64(h.listenGate.Max - h.listenGate.Reserve) })
}

// admit — место в воротах g для куки id: сначала частота новых подключений с IP, потом ворота.
func (h *Hub) admit(r *http.Request, id string, g *admission.Gate) (*admission.Ticket, admission.Reason) {
	ip := h.srcs.IP(r)
	if h.opt.ConnRate > 0 && !h.conns.Allow(ip) {
		return nil, admission.RateIP
	}
	return g.Acquire(admission.Src{ID: id, IP: ip}, func() admission.Level { return h.level(r, id) })
}

// level — свой ли: пропуск, недавний обрыв, уровень из модерации (кеш levelMaxAge).
func (h *Hub) level(r *http.Request, id string) admission.Level {
	if id == "" {
		return admission.Anon
	}
	if lv := h.opt.Passes.Check(r, id); lv > admission.Anon {
		return lv
	}
	h.mu.Lock()
	left, ok := h.seats[id]
	h.mu.Unlock()
	if ok && time.Since(left) < seatHold {
		return admission.Known
	}
	return h.modLevel(id)
}

// modLevel — уровень по базе модерации (с кешем).
func (h *Hub) modLevel(id string) admission.Level {
	if h.opt.Mod == nil || id == "" {
		return admission.Anon
	}
	h.levels.mu.Lock()
	if c, ok := h.levels.m[id]; ok && time.Since(c.at) < levelMaxAge {
		h.levels.mu.Unlock()
		return c.lv
	}
	h.levels.mu.Unlock()
	lv := levelOf(h.opt.Mod.Status(id, moderation.Air))
	h.levels.mu.Lock()
	if len(h.levels.m) > 20000 { // под атакой новыми куками — не копить
		h.levels.m = map[string]levelAt{}
	}
	h.levels.m[id] = levelAt{lv, time.Now()}
	h.levels.mu.Unlock()
	return lv
}

func levelOf(st moderation.Status) admission.Level {
	switch {
	case st.Banned:
		return admission.Anon
	case st.Level > 0:
		return admission.Level(st.Level)
	case st.Trusted:
		return admission.Known
	}
	return admission.Anon
}

// seatLeftLocked — приёмник отключился: ещё seatHold он свой (под h.mu).
func (h *Hub) seatLeftLocked(id string) {
	if id == "" {
		return
	}
	now := time.Now()
	if len(h.seats) > 5000 {
		for k, t := range h.seats {
			if now.Sub(t) > seatHold {
				delete(h.seats, k)
			}
		}
	}
	h.seats[id] = now
}
