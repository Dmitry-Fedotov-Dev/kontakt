// Package moderation — жалобы и баны, общие для Контакта и Открытого радио.
//
// Человек опознаётся по вечной куке kontakt_id (случайные 128 бит, её ставит web-сервис на
// весь домен — и рулетке, и радио под /radio/). На диск пишется только хеш этого
// идентификатора, зоны бана, засчитанные жалобы (без того, кто жаловался) и счётчик сессий
// новичка — ни голоса, ни IP, ни времени разговоров.
//
// Правила:
//   - бан действует в зоне: calls — снять трубку, air — выйти в эфир, all — и то и другое;
//     слушать радио может любой;
//   - бан — когда за окно (WindowDays) пожаловались Reporters РАЗНЫХ людей; до этого —
//     жёлтая карточка. Бан вечный, снимает его только админ;
//   - жалоба новичка (меньше TrustTalks засчитанных сессий) принимается, но в зачёт не идёт:
//     иначе новая кука в инкогнито — и банить можно кого угодно.
package moderation

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Zone — где действует бан.
type Zone string

const (
	Calls Zone = "calls" // рулетка: снять трубку
	Air   Zone = "air"   // радио: выйти в эфир
	All   Zone = "all"   // везде (только админ)
)

func ParseZone(s string) (Zone, bool) {
	switch z := Zone(s); z {
	case Calls, Air, All:
		return z, true
	}
	return "", false
}

// Policy — пороги; signal берёт их из горячего конфига.
type Policy struct {
	Reporters  int // разных жалобщиков за окно для бана; 1 — бан с первой жалобы
	WindowDays int // сколько дней жалоба идёт в зачёт
	TrustTalks int // столько засчитанных сессий нужно, чтобы жалобы шли в зачёт; 0 — всем сразу
}

var DefaultPolicy = Policy{Reporters: 2, WindowDays: 7, TrustTalks: 3}

// Результаты жалобы.
const (
	ResultYellow = "yellow" // жёлтая карточка
	ResultBanned = "banned"
	ResultNoted  = "noted" // жалоба новичка: принята, в зачёт не пошла
)

// Status — то, что человек видит о себе (и что проверяют сервисы).
type Status struct {
	Banned  bool `json:"banned"`
	Cards   int  `json:"cards"` // разных жалобщиков в окне
	Trusted bool `json:"trusted"`
}

type report struct {
	From string `json:"from"` // хеш (на кого + кто): с записью жалобщика не связать
	Zone Zone   `json:"zone"`
	Day  string `json:"day"` // дата без времени
}

type record struct {
	Bans    []Zone   `json:"bans,omitempty"`
	Reports []report `json:"reports,omitempty"`
	Talks   int      `json:"talks,omitempty"` // засчитанные сессии новичка; у доверенного не растут
	Trusted bool     `json:"trusted,omitempty"`
	// Формат до общей модерации: читается при загрузке и переводится.
	OldCards  int  `json:"cards,omitempty"`
	OldBanned bool `json:"banned,omitempty"`
}

func (r *record) empty() bool {
	return len(r.Bans) == 0 && len(r.Reports) == 0 && r.Talks == 0 && !r.Trusted
}

func (r *record) banned(z Zone) bool {
	return slices.Contains(r.Bans, z) || slices.Contains(r.Bans, All)
}

// Entry — строка журнала действий модерации.
type Entry struct {
	Day    string `json:"day"`
	Action string `json:"action"` // yellow | ban | unban
	Zone   Zone   `json:"zone"`
	Key    string `json:"key"` // ключ записи (хеш куки), его принимает админка
	By     string `json:"by"`  // auto | admin
}

const journalKeep = 1000 // столько последних строк журнала отдаёт админка

type Store struct {
	path, journalPath string
	pol               atomic.Pointer[Policy]
	now               func() time.Time // подменяется в тестах

	mu      sync.Mutex
	recs    map[string]*record
	journal []Entry
}

// Open загружает базу; пустой path — только в памяти (для тестов). Журнал действий лежит
// рядом: bans.json → bans-journal.jsonl.
func Open(path string) (*Store, error) {
	s := &Store{path: path, recs: map[string]*record{}, now: time.Now}
	s.SetPolicy(DefaultPolicy)
	if path == "" {
		return s, nil
	}
	s.journalPath = strings.TrimSuffix(path, filepath.Ext(path)) + "-journal.jsonl"
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.recs); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	for _, r := range s.recs {
		if r.OldBanned {
			r.Bans = append(r.Bans, Calls)
		} else if r.OldCards > 0 { // кто жаловался, старый формат не хранил: одна карточка
			r.Reports = append(r.Reports, report{From: "legacy", Zone: Calls, Day: s.day()})
		}
		r.OldBanned, r.OldCards = false, 0
	}
	if f, err := os.Open(s.journalPath); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				s.journal = append(s.journal, e)
			}
		}
		f.Close()
		if len(s.journal) > journalKeep {
			s.journal = slices.Clone(s.journal[len(s.journal)-journalKeep:])
		}
	}
	return s, nil
}

func (s *Store) SetPolicy(p Policy) { s.pol.Store(&p) }
func (s *Store) Policy() Policy     { return *s.pol.Load() }

// Key — ключ записи по значению куки. Его видно в журнале, по нему разбанивает админка.
func Key(id string) string {
	h := sha256.Sum256([]byte("kontakt:" + id))
	return hex.EncodeToString(h[:16])
}

// pairKey — отпечаток «этот человек пожаловался на этого». Без значения куки жалобщика его не
// получить, а из базы известны только хеши — так что по базе не узнать, кто на кого жаловался.
func pairKey(target, reporter string) string {
	h := sha256.Sum256([]byte("kontakt:report:" + target + ":" + reporter))
	return hex.EncodeToString(h[:8])
}

func (s *Store) day() string { return s.now().UTC().Format(time.DateOnly) }

// pruneLocked выбрасывает жалобы старше окна.
func (s *Store) pruneLocked(r *record) {
	oldest := s.now().UTC().AddDate(0, 0, -s.Policy().WindowDays).Format(time.DateOnly)
	r.Reports = slices.DeleteFunc(r.Reports, func(x report) bool { return x.Day <= oldest })
}

func cards(r *record, z Zone) int {
	n := 0
	for _, x := range r.Reports {
		if x.Zone == z {
			n++
		}
	}
	return n
}

func (s *Store) Status(id string, z Zone) Status {
	if id == "" {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[Key(id)]
	if r == nil {
		return Status{Trusted: s.Policy().TrustTalks == 0}
	}
	s.pruneLocked(r)
	return Status{Banned: r.banned(z), Cards: cards(r, z), Trusted: s.trustedLocked(r)}
}

func (s *Store) Banned(id string, z Zone) bool { return s.Status(id, z).Banned }

func (s *Store) trustedLocked(r *record) bool {
	t := s.Policy().TrustTalks
	return t == 0 || (r != nil && (r.Trusted || r.Talks >= t))
}

// Report — reporter жалуется на target в зоне z (calls или air).
func (s *Store) Report(target, reporter string, z Zone) (string, error) {
	if z != Calls && z != Air {
		return "", fmt.Errorf("жалоба в зоне %q", z)
	}
	p := s.Policy()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.trustedLocked(s.recs[Key(reporter)]) {
		return ResultNoted, nil
	}
	k := Key(target)
	r := s.recs[k]
	if r == nil {
		r = &record{}
		s.recs[k] = r
	}
	if r.banned(z) {
		return ResultBanned, nil
	}
	s.pruneLocked(r)
	from := pairKey(target, reporter)
	if !slices.ContainsFunc(r.Reports, func(x report) bool { return x.From == from && x.Zone == z }) {
		r.Reports = append(r.Reports, report{From: from, Zone: z, Day: s.day()})
	} else {
		return ResultYellow, nil // тот же человек повторно: в зачёт не идёт, ничего не изменилось
	}
	result, action := ResultYellow, "yellow"
	if cards(r, z) >= max(p.Reporters, 1) {
		r.Bans = append(r.Bans, z)
		result, action = ResultBanned, "ban"
	}
	return result, s.commitLocked(Entry{Action: action, Zone: z, Key: k, By: "auto"})
}

// Seen засчитывает человеку сессию (разговор или прослушивание); набравший TrustTalks —
// доверенный, его жалобы идут в зачёт. Дальше счётчик не растёт и база не пишется.
func (s *Store) Seen(id string) error {
	t := s.Policy().TrustTalks
	if id == "" || t == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := Key(id)
	r := s.recs[k]
	if r == nil {
		r = &record{}
		s.recs[k] = r
	}
	if r.Trusted {
		return nil
	}
	r.Talks++
	if r.Talks >= t {
		r.Trusted, r.Talks = true, 0
	}
	return s.commitLocked()
}

// Ban — бан админом по ключу записи (Key или из журнала).
func (s *Store) Ban(key string, z Zone) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[key]
	if r == nil {
		r = &record{}
		s.recs[key] = r
	}
	if !slices.Contains(r.Bans, z) {
		r.Bans = append(r.Bans, z)
	}
	return s.commitLocked(Entry{Action: "ban", Zone: z, Key: key, By: "admin"})
}

// Unban снимает бан в зоне и её карточки; All — все баны и карточки.
func (s *Store) Unban(key string, z Zone) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[key]
	if r == nil {
		return nil
	}
	if z == All {
		r.Bans, r.Reports = nil, nil
	} else {
		r.Bans = slices.DeleteFunc(r.Bans, func(b Zone) bool { return b == z })
		r.Reports = slices.DeleteFunc(r.Reports, func(x report) bool { return x.Zone == z })
	}
	return s.commitLocked(Entry{Action: "unban", Zone: z, Key: key, By: "admin"})
}

// Counts — сколько записей в бане и с карточками, по зонам.
type Counts struct {
	Banned map[Zone]int `json:"banned"`
	Yellow map[Zone]int `json:"yellow"`
}

func (s *Store) Count() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Counts{Banned: map[Zone]int{}, Yellow: map[Zone]int{}}
	for _, r := range s.recs {
		s.pruneLocked(r)
		for _, b := range r.Bans {
			c.Banned[b]++
		}
		for _, z := range []Zone{Calls, Air} {
			if !r.banned(z) && cards(r, z) > 0 {
				c.Yellow[z]++
			}
		}
	}
	return c
}

// Journal — последние n строк журнала (все, если n <= 0), старые первыми.
func (s *Store) Journal(n int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.journal) {
		n = len(s.journal)
	}
	return slices.Clone(s.journal[len(s.journal)-n:])
}

// commitLocked дописывает журнал и сохраняет базу.
func (s *Store) commitLocked(entries ...Entry) error {
	for k, r := range s.recs {
		if r.empty() {
			delete(s.recs, k)
		}
	}
	for i := range entries {
		entries[i].Day = s.day()
	}
	s.journal = append(s.journal, entries...)
	if len(s.journal) > 2*journalKeep {
		s.journal = slices.Clone(s.journal[len(s.journal)-journalKeep:])
	}
	if s.path == "" {
		return nil
	}
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	if len(entries) > 0 {
		f, err := os.OpenFile(s.journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		for _, e := range entries {
			enc.Encode(e)
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(s.recs, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
