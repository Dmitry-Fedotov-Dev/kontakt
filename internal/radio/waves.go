package radio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Память волн: последнее название и логотип станции на каждой частоте. Ссылка на волну — чистый
// адрес /w/93.4 (без названия в запросе), и когда станция молчит, превью берёт её отсюда: адрес
// работает как постоянный адрес станции. Хранится только то, что и так было видно всем в эфире, —
// без людей и времени эфира; станцию, снятую баном, волна забывает. Файл waves.json рядом с
// логотипами (-logos); без него — только в памяти.

type waveInfo struct {
	Name string `json:"n"`
	Logo string `json:"g,omitempty"`
	Day  string `json:"d"` // когда звучала в последний раз — устаревшие можно чистить
}

type waveMemo struct {
	mu   sync.Mutex
	path string
	m    map[int]waveInfo
}

func openWaves(dir string) *waveMemo {
	w := &waveMemo{m: map[int]waveInfo{}}
	if dir == "" {
		return w
	}
	w.path = filepath.Join(dir, "waves.json")
	if b, err := os.ReadFile(w.path); err == nil {
		json.Unmarshal(b, &w.m)
	}
	return w
}

// Seen — на волне f звучит станция name с логотипом logo.
func (w *waveMemo) Seen(f int, name, logo string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info := waveInfo{name, logo, time.Now().UTC().Format(time.DateOnly)}
	if w.m[f] == info {
		return
	}
	w.m[f] = info
	w.saveLocked()
}

// Forget — станцию на волне сняли баном: её название и логотип в превью больше не показываем.
func (w *waveMemo) Forget(f int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.m[f]; !ok {
		return
	}
	delete(w.m, f)
	w.saveLocked()
}

func (w *waveMemo) Get(f int) (waveInfo, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, ok := w.m[f]
	return info, ok
}

func (w *waveMemo) saveLocked() {
	if w.path == "" {
		return
	}
	b, _ := json.Marshal(w.m)
	if os.WriteFile(w.path+".tmp", b, 0o640) == nil {
		os.Rename(w.path+".tmp", w.path)
	}
}
