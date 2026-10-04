package moderation

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, p Policy) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bans.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetPolicy(p)
	return s
}

func TestTwoDifferentReportersBan(t *testing.T) {
	s := open(t, Policy{Reporters: 2, WindowDays: 7})
	if r, _ := s.Report("bad", "a", Calls); r != ResultYellow {
		t.Fatalf("первая жалоба: %s", r)
	}
	if r, _ := s.Report("bad", "a", Calls); r != ResultYellow {
		t.Fatalf("тот же жалобщик ещё раз: %s", r)
	}
	if st := s.Status("bad", Calls); st.Banned || st.Cards != 1 {
		t.Fatalf("повтор от того же засчитан: %+v", st)
	}
	if r, _ := s.Report("bad", "b", Calls); r != ResultBanned {
		t.Fatalf("второй жалобщик: %s", r)
	}
	if !s.Banned("bad", Calls) || s.Banned("bad", Air) {
		t.Fatal("бан в зоне calls должен закрыть только звонки")
	}
}

func TestZonesSeparate(t *testing.T) {
	s := open(t, Policy{Reporters: 2, WindowDays: 7})
	s.Report("bad", "a", Calls)
	s.Report("bad", "b", Air)
	if s.Banned("bad", Calls) || s.Banned("bad", Air) {
		t.Fatal("карточки разных зон сложились")
	}
	s.Ban(Key("bad"), All)
	if !s.Banned("bad", Calls) || !s.Banned("bad", Air) {
		t.Fatal("бан all закрывает обе зоны")
	}
	s.Unban(Key("bad"), All)
	if st := s.Status("bad", Air); st.Banned || st.Cards != 0 {
		t.Fatalf("после разбана all: %+v", st)
	}
}

func TestWindowExpires(t *testing.T) {
	s := open(t, Policy{Reporters: 2, WindowDays: 7})
	day := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return day }
	s.Report("bad", "a", Calls)
	day = day.AddDate(0, 0, 7)
	if s.Status("bad", Calls).Cards != 0 {
		t.Fatal("жалоба недельной давности ещё в зачёте")
	}
	if r, _ := s.Report("bad", "b", Calls); r != ResultYellow {
		t.Fatalf("после окна вторая жалоба должна быть первой: %s", r)
	}
}

func TestNewcomerReportNotCounted(t *testing.T) {
	s := open(t, Policy{Reporters: 1, WindowDays: 7, TrustTalks: 2})
	if r, _ := s.Report("bad", "fresh", Calls); r != ResultNoted {
		t.Fatalf("жалоба новичка: %s", r)
	}
	if s.Status("bad", Calls).Cards != 0 {
		t.Fatal("жалоба новичка пошла в зачёт")
	}
	s.Seen("fresh")
	if s.Status("fresh", Calls).Trusted {
		t.Fatal("доверие после одной сессии из двух")
	}
	s.Seen("fresh")
	if !s.Status("fresh", Calls).Trusted {
		t.Fatal("нет доверия после двух сессий")
	}
	if r, _ := s.Report("bad", "fresh", Calls); r != ResultBanned {
		t.Fatalf("жалоба доверенного при пороге 1: %s", r)
	}
}

// По базе на диске нельзя узнать, кто жаловался: ни куки, ни её ключа там нет.
func TestReporterNotStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	s, _ := Open(path)
	s.SetPolicy(Policy{Reporters: 2, WindowDays: 7})
	s.Report("bad", "reporter-cookie", Calls)
	b, _ := os.ReadFile(path)
	j, _ := os.ReadFile(strings.TrimSuffix(path, ".json") + "-journal.jsonl")
	for _, leak := range []string{"reporter-cookie", Key("reporter-cookie"), "bad"} {
		if strings.Contains(string(b)+string(j), leak) {
			t.Fatalf("на диске %q:\n%s\n%s", leak, b, j)
		}
	}
	if !strings.Contains(string(j), `"action":"yellow"`) || !strings.Contains(string(j), Key("bad")) {
		t.Fatalf("журнал: %s", j)
	}
}

func TestPersistAndLegacyFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	old := `{"` + Key("banned") + `":{"cards":2,"banned":true,"updated_at":"2026-09-01T00:00:00Z"},` +
		`"` + Key("yellow") + `":{"cards":1,"banned":false,"updated_at":"2026-09-01T00:00:00Z"}}`
	os.WriteFile(path, []byte(old), 0o600)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPolicy(Policy{Reporters: 2, WindowDays: 7})
	if !s.Banned("banned", Calls) || s.Status("yellow", Calls).Cards != 1 {
		t.Fatal("старый формат не перевёлся")
	}
	if r, _ := s.Report("yellow", "x", Calls); r != ResultBanned {
		t.Fatalf("старая карточка + новая жалоба: %s", r)
	}
	s2, _ := Open(path)
	if !s2.Banned("yellow", Calls) || len(s2.Journal(0)) != 1 {
		t.Fatalf("после перезапуска: бан %v, журнал %+v", s2.Banned("yellow", Calls), s2.Journal(0))
	}
}

func TestRemote(t *testing.T) {
	s := open(t, Policy{Reporters: 1, WindowDays: 7})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	m := NewRemote(srv.URL)
	if r, err := m.Report("host", "listener", Air); err != nil || r != ResultBanned {
		t.Fatalf("Report: %s %v", r, err)
	}
	if st, err := m.StatusErr("host", Air); err != nil || !st.Banned {
		t.Fatalf("Status: %+v %v", st, err)
	}
	if err := m.Seen("x"); err != nil {
		t.Fatal(err)
	}
	if st, err := NewRemote("http://127.0.0.1:1").StatusErr("host", Air); err == nil || st.Banned {
		t.Fatal("недоступная модерация должна давать ошибку и «не забанен»")
	}
}
