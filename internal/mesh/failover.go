package mesh

// Failover держит маршрут к одному адресату: основной и альтернативы (§15).
//
//   - основной пропал из расчёта (узел OFFLINE, ребро выше критических порогов,
//     перегруз) — сразу берётся лучшая пригодная альтернатива; это failover;
//   - упавший маршрут возвращается в предпочтительные только после Probation
//     хороших пересчётов подряд: восстановившийся сначала перемеряют;
//   - лучший маршрут вытесняет текущий, только если лучше на Hysteresis (доля):
//     иначе звонок прыгал бы между почти равными путями от каждого замера.
type Failover struct {
	Probation  int
	Hysteresis float64

	active    *Route
	failed    map[string]int // ключ маршрута → хороших пересчётов подряд после отказа
	Failovers int            // сколько раз основной отказал и был заменён
	Reroutes  int            // сколько раз заменён на заметно лучший
}

func NewFailover(probation int, hysteresis float64) *Failover {
	if probation <= 0 {
		probation = 3
	}
	if hysteresis <= 0 {
		hysteresis = 0.2
	}
	return &Failover{Probation: probation, Hysteresis: hysteresis, failed: map[string]int{}}
}

// Active — текущий маршрут (nil — пути нет).
func (f *Failover) Active() *Route { return f.active }

// Update — пересчитанные маршруты (лучший первым, как отдаёт Router.Routes).
// Возвращает текущий маршрут и признак смены.
func (f *Failover) Update(routes []Route) (*Route, bool) {
	present := map[string]Route{}
	for _, r := range routes {
		present[r.key()] = r
	}
	for k := range f.failed { // испытательный срок: подряд, без пропусков
		if _, ok := present[k]; ok {
			f.failed[k]++
		} else {
			f.failed[k] = 0
		}
	}
	eligible := func(r Route) bool {
		n, wasDown := f.failed[r.key()]
		return !wasDown || n >= f.Probation
	}
	best := func() *Route {
		for _, r := range routes {
			if eligible(r) {
				r := r
				return &r
			}
		}
		if len(routes) > 0 { // все на испытании: лучше путь на испытании, чем никакого
			r := routes[0]
			return &r
		}
		return nil
	}

	if f.active != nil {
		cur, still := present[f.active.key()]
		if !still {
			// Отказ считается всегда, даже если заменить пока нечем: иначе отказ «в
			// никуда» с последующим восстановлением выглядел бы первым маршрутом.
			f.failed[f.active.key()] = 0
			f.Failovers++
			f.active = best()
			return f.active, true
		}
		f.active = &cur
		if b := best(); b != nil && b.key() != cur.key() && b.Score < cur.Score*(1-f.Hysteresis) {
			f.active = b
			f.Reroutes++
			f.clear(b)
			return f.active, true
		}
		return f.active, false
	}
	f.active = best()
	if f.active != nil {
		f.clear(f.active)
	}
	return f.active, f.active != nil
}

// Alternatives — маршруты, на которые уйдём при отказе текущего (до двух).
func (f *Failover) Alternatives(routes []Route) []Route {
	var out []Route
	for _, r := range routes {
		if f.active != nil && r.key() == f.active.key() {
			continue
		}
		if n, down := f.failed[r.key()]; down && n < f.Probation {
			continue
		}
		out = append(out, r)
		if len(out) == 2 {
			break
		}
	}
	return out
}

func (f *Failover) clear(r *Route) {
	if n, ok := f.failed[r.key()]; ok && n >= f.Probation {
		delete(f.failed, r.key())
	}
}
