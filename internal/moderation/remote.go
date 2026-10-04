package moderation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Moderator — то, что нужно сервису от модерации: *Store в том же процессе или Remote —
// база signal'а через его админ-порт (так радио и рулетка банят по одной базе).
type Moderator interface {
	Status(id string, z Zone) Status
	Report(target, reporter string, z Zone) (string, error)
	Seen(id string) error
}

// Handler — /mod/status, /mod/report, /mod/seen для Remote. Только на localhost-адресе:
// в запросах — значения кук.
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mod/status", func(w http.ResponseWriter, r *http.Request) {
		z, ok := ParseZone(r.URL.Query().Get("zone"))
		if !ok {
			http.Error(w, "zone: calls | air | all", http.StatusBadRequest)
			return
		}
		writeJSON(w, s.Status(r.URL.Query().Get("id"), z))
	})
	mux.HandleFunc("POST /mod/report", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target, Reporter string
			Zone             Zone
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" || req.Reporter == "" {
			http.Error(w, `{"target":…,"reporter":…,"zone":…}`, http.StatusBadRequest)
			return
		}
		res, err := s.Report(req.Target, req.Reporter, req.Zone)
		if err != nil && res == "" {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"result": res}) // не сохранилось на диск — бан в памяти всё равно действует
	})
	mux.HandleFunc("POST /mod/seen", func(w http.ResponseWriter, r *http.Request) {
		s.Seen(r.URL.Query().Get("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// AdminHandler — /admin/ban, /admin/unban, /admin/journal (только localhost). Человека называют
// значением его куки (id=, он присылает его сам) или ключом записи из журнала (key=); зона —
// calls | air | all. onBan — выгнать забаненного оттуда, где он сейчас (может быть nil).
func (s *Store) AdminHandler(onBan func(key string, z Zone)) http.Handler {
	mux := http.NewServeMux()
	action := func(name string, do func(key string, z Zone) error) {
		mux.HandleFunc("/admin/"+name, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			key := q.Get("key")
			if id := q.Get("id"); id != "" {
				key = Key(id)
			}
			z, ok := ParseZone(q.Get("zone"))
			if r.Method != http.MethodPost || key == "" || !ok {
				http.Error(w, "POST /admin/"+name+"?id=<кука kontakt_id>|key=<ключ из журнала>&zone=calls|air|all", http.StatusBadRequest)
				return
			}
			if err := do(key, z); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("admin: %s %s", name, z)
			fmt.Fprintln(w, "ok")
		})
	}
	action("ban", func(key string, z Zone) error {
		err := s.Ban(key, z)
		if onBan != nil {
			onBan(key, z)
		}
		return err
	})
	action("unban", s.Unban)
	mux.HandleFunc("/admin/journal", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		writeJSON(w, s.Journal(n))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// Remote — модерация signal'а по HTTP. Если signal не отвечает, Status говорит «не забанен»:
// радио не должно молчать из-за рулетки. Ошибку отдаёт StatusErr.
type Remote struct {
	base string
	c    *http.Client
}

func NewRemote(base string) *Remote {
	return &Remote{base: base, c: &http.Client{Timeout: 2 * time.Second}}
}

func (m *Remote) Status(id string, z Zone) Status {
	st, _ := m.StatusErr(id, z)
	return st
}

func (m *Remote) StatusErr(id string, z Zone) (Status, error) {
	var st Status
	resp, err := m.c.Get(m.base + "/mod/status?" + url.Values{"id": {id}, "zone": {string(z)}}.Encode())
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("модерация: %s", resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

func (m *Remote) Report(target, reporter string, z Zone) (string, error) {
	b, _ := json.Marshal(map[string]any{"target": target, "reporter": reporter, "zone": z})
	resp, err := m.c.Post(m.base+"/mod/report", "application/json", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("модерация: %s", resp.Status)
	}
	var out struct{ Result string }
	return out.Result, json.NewDecoder(resp.Body).Decode(&out)
}

func (m *Remote) Seen(id string) error {
	resp, err := m.c.Post(m.base+"/mod/seen?"+url.Values{"id": {id}}.Encode(), "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
