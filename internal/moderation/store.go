// Package moderation — жалобы и баны.
//
// Человек опознаётся по вечной куке kontakt_id (случайные 128 бит, её ставит web-сервис).
// На диск пишется только SHA-256 от этого идентификатора и счётчик карточек — ни голоса,
// ни IP, ни времени разговоров.
package moderation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Record struct {
	Cards     int       `json:"cards"`
	Banned    bool      `json:"banned"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	ResultYellow = "yellow"
	ResultBanned = "banned"
)

type Store struct {
	path string
	mu   sync.Mutex
	recs map[string]*Record
}

// Open загружает базу банов; пустой path — только в памяти (для тестов).
func Open(path string) (*Store, error) {
	s := &Store{path: path, recs: map[string]*Record{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.recs); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func key(id string) string {
	h := sha256.Sum256([]byte("kontakt:" + id))
	return hex.EncodeToString(h[:16])
}

func (s *Store) Status(id string) Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.recs[key(id)]; r != nil {
		return *r
	}
	return Record{}
}

func (s *Store) Banned(id string) bool { return s.Status(id).Banned }

// Report применяет жалобу на id по политике "instant" | "yellow".
func (s *Store) Report(id, policy string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(id)
	r := s.recs[k]
	if r == nil {
		r = &Record{}
		s.recs[k] = r
	}
	r.Cards++
	r.UpdatedAt = time.Now().UTC().Truncate(24 * time.Hour) // дата без времени
	if policy == "instant" || r.Cards >= 2 {
		r.Banned = true
	}
	result := ResultYellow
	if r.Banned {
		result = ResultBanned
	}
	return result, s.saveLocked()
}

// Unban снимает бан и карточки (для админки).
func (s *Store) Unban(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, key(id))
	return s.saveLocked()
}

func (s *Store) Count() (banned, yellow int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.Banned {
			banned++
		} else {
			yellow++
		}
	}
	return
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.recs, "", " ")
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
