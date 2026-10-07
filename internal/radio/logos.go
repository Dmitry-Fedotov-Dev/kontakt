package radio

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Логотипы станций: PNG 64×64, который ведущий выбирает на вкладке «Вещать». Браузер сам обрезает
// картинку до квадрата и уменьшает — в эфир уходит пиксельный значок в пару килобайт. Сервер
// проверяет, что это ровно PNG 64×64, и перекодирует его: всё лишнее (метаданные, скрытые блоки)
// отбрасывается.
//
// Логотип хранится по хешу содержимого и переживает эфир: ссылка на волну несёт хеш (g=…), и превью
// в мессенджере показывает тот логотип, с которым ею поделились, даже когда станция уже ушла, а волну
// занял другой. Файлов не больше max — сверх вытесняются давно не использованные; логотип станции,
// снятой баном, удаляется сразу. Кто загрузил, не хранится.

const (
	logoSide     = 64
	logoMaxBytes = 16 << 10 // PNG 64×64 после перекодирования; больше — это не значок
)

var logoHashOK = regexp.MustCompile(`^[0-9a-f]{16}$`)

var errBadLogo = errors.New("логотип: нужен PNG 64×64")

type logoStore struct {
	mu    sync.Mutex
	dir   string // пусто — только в памяти, до перезапуска
	max   int
	mem   map[string][]byte // хеш → PNG: всё (без диска) или кеш прочитанного с диска
	order []string          // для вытеснения из памяти, старые первыми
}

func openLogos(dir string, max int) (*logoStore, error) {
	s := &logoStore{dir: dir, max: max, mem: map[string][]byte{}}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return s, err
		}
	}
	return s, nil
}

// normalizeLogo — PNG 64×64 заново, без всего, что прислали сверх пикселей.
func normalizeLogo(b []byte) ([]byte, error) {
	if len(b) == 0 || len(b) > logoMaxBytes*2 {
		return nil, errBadLogo
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b)) // размер — до раскодирования пикселей
	if err != nil || cfg.Width != logoSide || cfg.Height != logoSide {
		return nil, errBadLogo
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, errBadLogo
	}
	out := image.NewNRGBA(image.Rect(0, 0, logoSide, logoSide))
	draw.Draw(out, out.Bounds(), img, img.Bounds().Min, draw.Src)
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, out); err != nil || buf.Len() > logoMaxBytes {
		return nil, errBadLogo
	}
	return buf.Bytes(), nil
}

// Put проверяет и сохраняет логотип, возвращает его хеш.
func (s *logoStore) Put(b []byte) (string, error) {
	nb, err := normalizeLogo(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(nb)
	hash := hex.EncodeToString(sum[:8])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remember(hash, nb)
	if s.dir == "" {
		return hash, nil
	}
	p := filepath.Join(s.dir, hash+".png")
	if _, err := os.Stat(p); err == nil {
		now := time.Now() // снова в эфире — свежий, вытесняться не должен
		os.Chtimes(p, now, now)
		return hash, nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, nb, 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	s.pruneLocked()
	return hash, nil
}

// Get — PNG по хешу.
func (s *logoStore) Get(hash string) ([]byte, bool) {
	if !logoHashOK.MatchString(hash) {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.mem[hash]; ok {
		return b, true
	}
	if s.dir == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(s.dir, hash+".png"))
	if err != nil {
		return nil, false
	}
	s.remember(hash, b)
	return b, true
}

// Delete — логотип станции, снятой баном.
func (s *logoStore) Delete(hash string) {
	if !logoHashOK.MatchString(hash) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mem, hash)
	if s.dir != "" {
		os.Remove(filepath.Join(s.dir, hash+".png"))
	}
}

// remember держит в памяти не больше max (без диска) или 512 (кеш) логотипов.
func (s *logoStore) remember(hash string, b []byte) {
	if _, ok := s.mem[hash]; ok {
		return
	}
	limit := 512
	if s.dir == "" {
		limit = s.max
	}
	s.mem[hash] = b
	s.order = append(s.order, hash)
	for len(s.order) > limit {
		delete(s.mem, s.order[0])
		s.order = s.order[1:]
	}
}

// pruneLocked — на диске не больше max файлов: удаляются давно не использованные.
func (s *logoStore) pruneLocked() {
	ents, err := os.ReadDir(s.dir)
	if err != nil || len(ents) <= s.max {
		return
	}
	type f struct {
		name string
		mod  time.Time
	}
	var fs []f
	for _, e := range ents {
		if info, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".png") {
			fs = append(fs, f{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod.Before(fs[j].mod) })
	for i := 0; i < len(fs)-s.max; i++ {
		os.Remove(filepath.Join(s.dir, fs[i].name))
		delete(s.mem, strings.TrimSuffix(fs[i].name, ".png"))
	}
}

// serveLogo — GET /logo/{hash}.png. Адрес меняется вместе с картинкой, поэтому кешируется навсегда.
func (h *Hub) serveLogo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	b, ok := h.logos.Get(strings.TrimSuffix(name, ".png"))
	if !ok || !strings.HasSuffix(name, ".png") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(b)
}

// setLogo — ведущий прислал логотип (base64 PNG) или "" — убрать.
func (h *Hub) setLogo(s *station, b64 string) {
	hash := ""
	if b64 != "" {
		b, err := decodeB64(b64)
		if err == nil {
			hash, err = h.logos.Put(b)
		}
		if err != nil {
			h.logoChanges.Inc("bad")
			s.sayHost(map[string]bool{"logoError": true})
			return
		}
	}
	h.mu.Lock()
	same := s.logo == hash
	s.logo = hash
	h.mu.Unlock()
	if hash == "" {
		h.logoChanges.Inc("removed")
	} else {
		h.logoChanges.Inc("set")
	}
	s.sayHost(map[string]string{"logo": hash})
	if !same {
		h.changed()
	}
}

// decodeB64 — base64 из браузера, можно с префиксом data:image/png;base64,.
func decodeB64(s string) ([]byte, error) {
	if i := strings.Index(s, ","); i >= 0 && strings.HasPrefix(s, "data:") {
		s = s[i+1:]
	}
	return base64.StdEncoding.DecodeString(s)
}
