package radio

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Короткие ссылки на волну: /r/k7Qx2 вместо /w/101.7?n=…&t=… (русский трек в адресе — по шесть
// символов на букву, ссылка выходила на сотни символов).
//
// Код — из хеша «частота, станция, трек»: одинаковое даёт одинаковый код, дублей нет. Хранится
// только эта тройка (её и так видно в эфире) и дата — ни адресов, ни того, кто поделился.
// Ссылок не больше max: сверх — вытесняются самые старые.

const shortCodeLen = 6

type shortLink struct {
	Code  string `json:"c"`
	Freq  int    `json:"f"`
	Name  string `json:"n,omitempty"`
	Track string `json:"t,omitempty"`
	Lang  string `json:"l,omitempty"` // язык карточки: "en"; пусто — русский
	Logo  string `json:"g,omitempty"` // хеш логотипа станции (logos.go)
	Day   string `json:"d"`
}

type shortLinks struct {
	mu    sync.Mutex
	path  string // пусто — только в памяти
	max   int
	byKey map[string]*shortLink
	order []string // коды, старые первыми
}

func openShortLinks(path string, max int) (*shortLinks, error) {
	s := &shortLinks{path: path, max: max, byKey: map[string]*shortLink{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var list []*shortLink
	if err := json.Unmarshal(b, &list); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	for _, l := range list {
		s.byKey[l.Code] = l
		s.order = append(s.order, l.Code)
	}
	return s, nil
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// candidate — n-символьный код из хеша; при совпадении с чужой ссылкой берётся длиннее.
func candidate(sum [32]byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = base62[int(sum[i])%62]
	}
	return string(b)
}

// Code — код для ссылки (создаёт, если её ещё нет). Русский язык и пустой логотип в хеш не
// входят — у старых ссылок коды прежние.
func (s *shortLinks) Code(f int, name, track, lang, logo string) (string, error) {
	if lang != "en" {
		lang = ""
	}
	key := strconv.Itoa(f) + "\x00" + name + "\x00" + track
	if lang != "" {
		key += "\x00" + lang
	}
	if logo != "" {
		key += "\x00g" + logo
	}
	sum := sha256.Sum256([]byte(key))
	s.mu.Lock()
	defer s.mu.Unlock()
	for n := shortCodeLen; n <= len(sum); n++ {
		c := candidate(sum, n)
		l := s.byKey[c]
		if l == nil {
			l = &shortLink{Code: c, Freq: f, Name: name, Track: track, Lang: lang, Logo: logo, Day: time.Now().UTC().Format(time.DateOnly)}
			s.byKey[c] = l
			s.order = append(s.order, c)
			for len(s.order) > s.max {
				delete(s.byKey, s.order[0])
				s.order = s.order[1:]
			}
			return c, s.saveLocked()
		}
		if l.Freq == f && l.Name == name && l.Track == track && l.Lang == lang && l.Logo == logo {
			return c, nil
		}
	}
	return "", fmt.Errorf("короткие ссылки: не нашлось свободного кода")
}

func (s *shortLinks) Get(code string) (shortLink, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.byKey[code]; l != nil {
		return *l, true
	}
	return shortLink{}, false
}

func (s *shortLinks) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.order)
}

func (s *shortLinks) saveLocked() error {
	if s.path == "" {
		return nil
	}
	list := make([]*shortLink, 0, len(s.order))
	for _, c := range s.order {
		list = append(list, s.byKey[c])
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
