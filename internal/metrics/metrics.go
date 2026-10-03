// Package metrics — минимальные метрики в текстовом формате Prometheus (без внешних зависимостей):
// счётчики, счётчики с одной меткой, датчики (в том числе вычисляемые при сборе) и гистограммы.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type Registry struct {
	mu    sync.Mutex
	items []writer
}

type writer interface{ write(b *strings.Builder) }

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(w writer) {
	r.mu.Lock()
	r.items = append(r.items, w)
	r.mu.Unlock()
}

// Handler отдаёт /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		r.mu.Lock()
		items := append([]writer(nil), r.items...)
		r.mu.Unlock()
		for _, it := range items {
			it.write(&b)
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write([]byte(b.String()))
	})
}

func header(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func num(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// ---------- счётчик ----------

type Counter struct {
	name, help string
	v          atomic.Uint64
}

func (r *Registry) Counter(name, help string) *Counter {
	c := &Counter{name: name, help: help}
	r.add(c)
	return c
}

func (c *Counter) Inc()          { c.v.Add(1) }
func (c *Counter) Add(n uint64)  { c.v.Add(n) }
func (c *Counter) Value() uint64 { return c.v.Load() }
func (c *Counter) write(b *strings.Builder) {
	header(b, c.name, c.help, "counter")
	fmt.Fprintf(b, "%s %d\n", c.name, c.v.Load())
}

// ---------- счётчик с меткой ----------

type CounterVec struct {
	name, help, label string
	mu                sync.Mutex
	vals              map[string]*atomic.Uint64
}

// CounterVec — счётчик с одной меткой. known — значения метки, которые отдаются нулём с первого
// сбора. Без этого ряд рождается только при первом событии и сразу с ненулевым значением, а
// rate()/increase() прирост от рождения ряда не видят: пачка звонков между двумя сборами
// на графике выглядит как ноль. Значения, не перечисленные в known, по-прежнему появляются
// при первом Inc.
func (r *Registry) CounterVec(name, help, label string, known ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, label: label, vals: map[string]*atomic.Uint64{}}
	for _, k := range known {
		c.vals[k] = &atomic.Uint64{}
	}
	r.add(c)
	return c
}

func (c *CounterVec) Inc(value string) {
	c.mu.Lock()
	v := c.vals[value]
	if v == nil {
		v = &atomic.Uint64{}
		c.vals[value] = v
	}
	c.mu.Unlock()
	v.Add(1)
}

func (c *CounterVec) write(b *strings.Builder) {
	header(b, c.name, c.help, "counter")
	c.mu.Lock()
	keys := make([]string, 0, len(c.vals))
	for k := range c.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", c.name, c.label, k, c.vals[k].Load())
	}
	c.mu.Unlock()
}

// ---------- счётчик, вычисляемый при сборе ----------

type counterFunc struct {
	name, help string
	f          func() float64
}

// CounterFunc — счётчик, значение которого берётся при сборе (например, атомик, который
// уже ведёт горячий путь): тип в выдаче — counter, а не gauge.
func (r *Registry) CounterFunc(name, help string, f func() float64) {
	r.add(&counterFunc{name: name, help: help, f: f})
}

func (c *counterFunc) write(b *strings.Builder) {
	header(b, c.name, c.help, "counter")
	fmt.Fprintf(b, "%s %s\n", c.name, num(c.f()))
}

// ---------- датчик, вычисляемый при сборе ----------

type gaugeFunc struct {
	name, help, label string
	f                 func() map[string]float64 // "" — без метки
}

// Gauge — датчик без меток, значение берётся при каждом сборе.
func (r *Registry) Gauge(name, help string, f func() float64) {
	r.add(&gaugeFunc{name: name, help: help, f: func() map[string]float64 { return map[string]float64{"": f()} }})
}

// GaugeVec — датчик с одной меткой (например, role или device).
func (r *Registry) GaugeVec(name, help, label string, f func() map[string]float64) {
	r.add(&gaugeFunc{name: name, help: help, label: label, f: f})
}

func (g *gaugeFunc) write(b *strings.Builder) {
	header(b, g.name, g.help, "gauge")
	vals := g.f()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" {
			fmt.Fprintf(b, "%s %s\n", g.name, num(vals[k]))
		} else {
			fmt.Fprintf(b, "%s{%s=%q} %s\n", g.name, g.label, k, num(vals[k]))
		}
	}
}

// ---------- набор рядов с произвольными метками ----------

// Sample — один ряд: метки по порядку и значение.
type Sample struct {
	Labels [][2]string
	Value  float64
}

type gaugeSet struct {
	name, help string
	f          func() []Sample
}

// GaugeSet — датчик, ряды и метки которого вычисляются при сборе (например, рёбра
// графа: source, target, kind). Меток может быть сколько угодно.
func (r *Registry) GaugeSet(name, help string, f func() []Sample) {
	r.add(&gaugeSet{name: name, help: help, f: f})
}

func (g *gaugeSet) write(b *strings.Builder) {
	header(b, g.name, g.help, "gauge")
	for _, s := range g.f() {
		b.WriteString(g.name)
		if len(s.Labels) > 0 {
			b.WriteByte('{')
			for i, l := range s.Labels {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(b, "%s=\"%s\"", l[0], escapeLabel(l[1]))
			}
			b.WriteByte('}')
		}
		fmt.Fprintf(b, " %s\n", num(s.Value))
	}
}

// escapeLabel — экранирование значения метки по формату Prometheus: \, " и перевод строки.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// ---------- гистограмма ----------

type Histogram struct {
	name, help string
	bounds     []float64
	mu         sync.Mutex
	counts     []uint64
	sum        float64
	n          uint64
}

func (r *Registry) Histogram(name, help string, bounds []float64) *Histogram {
	h := &Histogram{name: name, help: help, bounds: bounds, counts: make([]uint64, len(bounds))}
	r.add(h)
	return h
}

func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	for i, ub := range h.bounds {
		if v <= ub {
			h.counts[i]++
		}
	}
	h.sum += v
	h.n++
	h.mu.Unlock()
}

func (h *Histogram) write(b *strings.Builder) {
	header(b, h.name, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, ub := range h.bounds {
		fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", h.name, num(ub), h.counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %s\n%s_count %d\n", h.name, h.n, h.name, num(h.sum), h.name, h.n)
}
