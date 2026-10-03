package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func scrape(r *Registry) string {
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// Известное значение метки отдаётся нулём до первого события: иначе ряд рождается сразу
// с ненулевым значением, и rate() не видит пачку событий, случившуюся между двумя сборами.
func TestCounterVecKnownValuesStartAtZero(t *testing.T) {
	r := NewRegistry()
	c := r.CounterVec("x_total", "help", "result", "ok", "busy")

	out := scrape(r)
	for _, want := range []string{`x_total{result="ok"} 0`, `x_total{result="busy"} 0`} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q до первого события:\n%s", want, out)
		}
	}

	c.Inc("ok")
	c.Inc("ok")
	c.Inc("other") // не объявленное заранее — появляется при первом событии, как раньше
	out = scrape(r)
	for _, want := range []string{`x_total{result="ok"} 2`, `x_total{result="busy"} 0`, `x_total{result="other"} 1`} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}
}

// Без known ничего не выдумывается: только заголовок.
func TestCounterVecWithoutKnownValues(t *testing.T) {
	r := NewRegistry()
	r.CounterVec("y_total", "help", "kind")
	if out := scrape(r); strings.Contains(out, "y_total{") {
		t.Fatalf("ряд без событий:\n%s", out)
	}
}

// CounterFunc отдаётся с типом counter: rate() и правила Prometheus на него рассчитаны.
func TestCounterFuncType(t *testing.T) {
	r := NewRegistry()
	r.CounterFunc("z_total", "help", func() float64 { return 42 })
	out := scrape(r)
	for _, want := range []string{"# TYPE z_total counter", "z_total 42"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}
}
