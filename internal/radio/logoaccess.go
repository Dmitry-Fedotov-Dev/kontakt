package radio

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Доступ к логотипу через бота вещателей (castbot.go). Ведущий на вкладке «Вещать» жмёт «+» у
// названия — страница получает ссылку t.me/бот?start=logo_<код>; код — на 15 минут и привязан к
// куке ведущего. В боте человек нажимает «Старт», владельцу приходит запрос с кнопками «Выдать» /
// «Отказать». Выданный доступ — у аккаунта Telegram: тот же человек из другого браузера получает
// его снова по ссылке сразу, без владельца. Отозвать — /list в боте.
//
// На диске (access.json рядом с логотипами): id Telegram, имя для списка владельца, день выдачи и
// хеши кук браузеров, откуда приходили по ссылке (саму куку не храним).

const (
	accessCodeTTL  = 15 * time.Minute // от ссылки на странице до «Старт» в боте
	accessAskTTL   = 72 * time.Hour   // запрос ждёт решения владельца
	accessMaxCodes = 2000
	accessKeysMax  = 20 // браузеров на один аккаунт; старые вытесняются
)

type accessUser struct {
	Name string   `json:"name"`
	Day  string   `json:"day"`
	Keys []string `json:"keys"`
}

type accessCode struct {
	key  string // хеш куки ведущего
	exp  time.Time
	tg   int64 // кто нажал «Старт» (0 — ещё никто)
	name string
}

type logoAccess struct {
	path string

	mu    sync.Mutex
	users map[int64]*accessUser
	byKey map[string]int64
	codes map[string]*accessCode
}

// accessKey — хеш куки ведущего: соль своя, чтобы он не совпадал с хешами модерации.
func accessKey(host string) string {
	if host == "" {
		return ""
	}
	h := sha256.Sum256([]byte("logo-access:" + host))
	return hex.EncodeToString(h[:16])
}

func openLogoAccess(dir string) *logoAccess {
	a := &logoAccess{users: map[int64]*accessUser{}, byKey: map[string]int64{}, codes: map[string]*accessCode{}}
	if dir == "" {
		return a
	}
	a.path = filepath.Join(dir, "access.json")
	if b, err := os.ReadFile(a.path); err == nil {
		json.Unmarshal(b, &a.users)
	}
	for id, u := range a.users {
		for _, k := range u.Keys {
			a.byKey[k] = id
		}
	}
	return a
}

func (a *logoAccess) saveLocked() {
	if a.path == "" {
		return
	}
	b, _ := json.Marshal(a.users)
	tmp := a.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, a.path)
	}
}

// Allowed — этому ведущему выдан доступ.
func (a *logoAccess) Allowed(host string) bool {
	k := accessKey(host)
	if k == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.byKey[k]
	return ok
}

// NewCode — код для ссылки в бота; пока прошлый код этого ведущего жив, возвращается он же.
func (a *logoAccess) NewCode(host string) string {
	k := accessKey(host)
	if k == "" {
		return ""
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for c, v := range a.codes {
		if now.After(v.exp) {
			delete(a.codes, c)
		} else if v.key == k && v.tg == 0 {
			return c
		}
	}
	if len(a.codes) >= accessMaxCodes {
		return ""
	}
	b := make([]byte, 12)
	rand.Read(b)
	c := hex.EncodeToString(b)
	a.codes[c] = &accessCode{key: k, exp: now.Add(accessCodeTTL)}
	return c
}

// Результаты Start.
const (
	startBad     = "bad"     // кода нет или истёк
	startGranted = "granted" // аккаунту доступ уже выдан — браузер добавлен
	startAsked   = "asked"   // запрос уже у владельца
	startNew     = "new"     // новый запрос: показать владельцу
)

// Start — человек tg нажал «Старт» по ссылке с кодом. owner — владелец: ему доступ сразу.
func (a *logoAccess) Start(code string, tg int64, name string, owner bool) (res, key string) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.codes[code]
	if c == nil || now.After(c.exp) {
		return startBad, ""
	}
	if _, ok := a.users[tg]; ok || owner {
		a.grantLocked(tg, name, c.key)
		delete(a.codes, code)
		return startGranted, c.key
	}
	if c.tg != 0 {
		if c.tg != tg {
			return startBad, "" // ссылку переслали другому: запрос уже от первого
		}
		return startAsked, ""
	}
	c.tg, c.name, c.exp = tg, name, now.Add(accessAskTTL)
	return startNew, ""
}

// Decide — решение владельца по коду; tg — кому сообщить, key — чей браузер ждёт.
func (a *logoAccess) Decide(code string, grant bool) (tg int64, key string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.codes[code]
	if c == nil || c.tg == 0 || time.Now().After(c.exp) {
		return 0, "", false
	}
	delete(a.codes, code)
	if grant {
		a.grantLocked(c.tg, c.name, c.key)
	}
	return c.tg, c.key, true
}

func (a *logoAccess) grantLocked(tg int64, name, key string) {
	u := a.users[tg]
	if u == nil {
		u = &accessUser{Day: time.Now().UTC().Format("2006-01-02")}
		a.users[tg] = u
	}
	if name != "" {
		u.Name = name
	}
	if _, has := a.byKey[key]; !has {
		u.Keys = append(u.Keys, key)
		if len(u.Keys) > accessKeysMax {
			delete(a.byKey, u.Keys[0])
			u.Keys = u.Keys[1:]
		}
	} else if a.byKey[key] != tg { // браузер был у другого аккаунта — теперь у этого
		old := a.users[a.byKey[key]]
		for i, k := range old.Keys {
			if k == key {
				old.Keys = append(old.Keys[:i], old.Keys[i+1:]...)
				break
			}
		}
		u.Keys = append(u.Keys, key)
	}
	a.byKey[key] = tg
	a.saveLocked()
}

// Revoke — отозвать доступ аккаунта; возвращает хеши его браузеров (снять логотипы в эфире).
func (a *logoAccess) Revoke(tg int64) (keys []string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.users[tg]
	if u == nil {
		return nil, false
	}
	for _, k := range u.Keys {
		delete(a.byKey, k)
	}
	delete(a.users, tg)
	a.saveLocked()
	return u.Keys, true
}

type accessEntry struct {
	TG       int64
	Name     string
	Day      string
	Browsers int
}

// List — у кого доступ, по дню выдачи.
func (a *logoAccess) List() []accessEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]accessEntry, 0, len(a.users))
	for id, u := range a.users {
		out = append(out, accessEntry{TG: id, Name: u.Name, Day: u.Day, Browsers: len(u.Keys)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].TG < out[j].TG
	})
	return out
}
