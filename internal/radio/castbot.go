package radio

// Бот вещателей — свой токен (RADIO_BOT_TOKEN), отдельный от бота владельца (cmd/notify): тот
// говорит только с владельцем, этот — с любым ведущим. Пока умеет одно — выдавать доступ к логотипу
// станции (logoaccess.go). Владелец (TELEGRAM_CHAT_ID) получает запросы с кнопками и команду /list;
// чтобы бот мог ему писать, владелец один раз открывает бота сам.
//
// Бот работает внутри радио: доступ проверяется при каждом логотипе, ходить за ним в другой
// процесс незачем. В журнал — только ошибки связи, без имён и id.

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kontakt/internal/identity"
	"kontakt/internal/notify"
)

const (
	botStartPrefix = "logo_"
	botListMax     = 50 // кнопок «Отозвать» в одном ответе /list
)

type castBot struct {
	h     *Hub
	bot   *notify.Bot
	owner int64
	stop  chan struct{}
	once  sync.Once

	mu   sync.Mutex
	name string // @username без @, из getMe
}

func newCastBot(h *Hub, api, token string, owner int64) *castBot {
	if api == "" {
		api = "https://api.telegram.org"
	}
	return &castBot{h: h, bot: notify.NewBot(api, token, owner), owner: owner, stop: make(chan struct{})}
}

func (b *castBot) Close() { b.once.Do(func() { close(b.stop) }) }

func (b *castBot) username() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.name
}

func (b *castBot) pause(d time.Duration) bool {
	select {
	case <-b.stop:
		return false
	case <-time.After(d):
		return true
	}
}

func (b *castBot) run() {
	var offset int64
	for {
		select {
		case <-b.stop:
			return
		default:
		}
		if b.username() == "" {
			n, err := b.bot.Me()
			if err != nil || n == "" {
				log.Printf("бот вещателей: %v", err)
				if !b.pause(10 * time.Second) {
					return
				}
				continue
			}
			b.mu.Lock()
			b.name = n
			b.mu.Unlock()
			log.Printf("бот вещателей на связи: @%s — логотип по доступу", n)
		}
		ups, err := b.bot.Updates(offset, 25*time.Second)
		if err != nil {
			log.Printf("бот вещателей: %v", err)
			if !b.pause(5 * time.Second) {
				return
			}
			continue
		}
		for _, u := range ups {
			offset = u.ID + 1
			b.handle(u)
		}
	}
}

func (b *castBot) say(chat int64, text string, rows ...[]notify.Button) {
	if err := b.bot.SendTo(chat, text, rows...); err != nil {
		log.Printf("бот вещателей: %v", err)
	}
}

func userName(u *notify.User) string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if u.Username != "" {
		n = strings.TrimSpace(n + " @" + u.Username)
	}
	return clean(n, 64)
}

const (
	botAbout = "Это бот вещателей <b>Открытого радио</b>. Доступ к логотипу станции — по ссылке со страницы: " +
		"вкладка «Вещать» → «+» у названия станции."
	botOwnerHelp = "<b>Бот вещателей</b>\n/list — у кого доступ к логотипу (и кнопки «Отозвать»)\n" +
		"Запросы на доступ приходят сюда с кнопками «Выдать» / «Отказать»."
)

func (b *castBot) handle(u notify.Update) {
	if c := u.Callback; c != nil {
		if c.Message == nil || c.Message.Chat.ID != b.owner {
			return
		}
		b.callback(c.ID, c.Data, c.Message.ID, c.Message.Text)
		return
	}
	m := u.Message
	if m == nil || m.From == nil {
		return
	}
	owner := m.From.ID == b.owner
	f := strings.Fields(m.Text)
	cmd := ""
	if len(f) > 0 {
		cmd = strings.SplitN(f[0], "@", 2)[0]
	}
	switch {
	case cmd == "/start" && len(f) == 2 && strings.HasPrefix(f[1], botStartPrefix):
		b.start(m.Chat.ID, m.From, strings.TrimPrefix(f[1], botStartPrefix), owner)
	case owner && cmd == "/list":
		b.list()
	case owner:
		b.say(m.Chat.ID, botOwnerHelp)
	default:
		b.say(m.Chat.ID, botAbout)
	}
}

func (b *castBot) start(chat int64, from *notify.User, code string, owner bool) {
	name := userName(from)
	res, key := b.h.access.Start(code, from.ID, name, owner)
	switch res {
	case startBad:
		b.say(chat, "Ссылка устарела. Нажми «+» у названия станции ещё раз — будет новая.")
	case startGranted:
		b.h.logoAccess.Inc("rejoined")
		b.h.accessChanged(key, true)
		b.say(chat, "✅ Логотип открыт. Вернись на страницу радио — можно ставить.")
	case startAsked:
		b.say(chat, "Запрос уже у владельца радио — напишу сюда, когда решит.")
	case startNew:
		b.h.logoAccess.Inc("request")
		b.say(chat, "Запрос отправлен владельцу радио. Напишу сюда, когда решит.")
		b.say(b.owner, fmt.Sprintf("🖼 <b>Логотип станции</b> — просит доступ\n%s · id <code>%d</code>", html.EscapeString(name), from.ID),
			[]notify.Button{{Text: "Выдать", Data: "g:" + code}, {Text: "Отказать", Data: "d:" + code}})
	}
}

func (b *castBot) callback(id, data string, msgID int64, msgText string) {
	kind, arg, _ := strings.Cut(data, ":")
	answer := func(res, mark string) {
		b.bot.Answer(id, res)
		if mark != "" {
			b.bot.Edit(msgID, html.EscapeString(msgText)+"\n\n"+mark)
		}
	}
	switch kind {
	case "g", "d":
		grant := kind == "g"
		tg, key, ok := b.h.access.Decide(arg, grant)
		if !ok {
			answer("запрос устарел", "⌛ устарел")
			return
		}
		if grant {
			b.h.logoAccess.Inc("granted")
			b.h.accessChanged(key, true)
			b.say(tg, "✅ Доступ к логотипу выдан. Вернись на страницу радио — можно ставить.")
			answer("выдано", "✅ выдано")
		} else {
			b.h.logoAccess.Inc("denied")
			b.say(tg, "Владелец радио пока не выдал доступ к логотипу.")
			answer("отказано", "✖ отказано")
		}
	case "r":
		tg, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			answer("не понял кнопку", "")
			return
		}
		keys, ok := b.h.access.Revoke(tg)
		if !ok {
			answer("доступа уже нет", "")
			return
		}
		b.h.logoAccess.Inc("revoked")
		for _, k := range keys {
			b.h.accessChanged(k, false)
		}
		b.say(tg, "Доступ к логотипу станции отозван.")
		answer("отозвано", "")
		b.say(b.owner, fmt.Sprintf("Отозвано: id <code>%d</code>", tg))
	default:
		answer("не понял кнопку", "")
	}
}

func (b *castBot) list() {
	es := b.h.access.List()
	if len(es) == 0 {
		b.say(b.owner, "Доступ к логотипу пока ни у кого.")
		return
	}
	var sb strings.Builder
	var rows [][]notify.Button
	fmt.Fprintf(&sb, "<b>Доступ к логотипу</b> — %d\n", len(es))
	for i, e := range es {
		fmt.Fprintf(&sb, "%d. %s · id <code>%d</code> · с %s · браузеров %d\n", i+1, html.EscapeString(e.Name), e.TG, e.Day, e.Browsers)
		if i < botListMax {
			rows = append(rows, []notify.Button{{Text: fmt.Sprintf("Отозвать %d. %s", i+1, e.Name), Data: "r:" + strconv.FormatInt(e.TG, 10)}})
		}
	}
	b.say(b.owner, sb.String(), rows...)
}

// ---------- в радио ----------

// logoMode — для метатега страницы: "1" — логотип ставит любой ведущий, "bot" — по доступу из
// бота, "" — только модератор.
func (h *Hub) logoMode() string {
	switch {
	case h.opt.HostLogos:
		return "1"
	case h.bot != nil:
		return "bot"
	}
	return ""
}

// canLogo — ведущий может поставить логотип сам.
func (h *Hub) canLogo(host string) bool {
	return h.opt.HostLogos || (h.bot != nil && h.access.Allowed(host))
}

// accessChanged — доступ браузера key выдан или отозван: его станции в эфире узнают сразу; при
// отзыве снимается логотип, поставленный ведущим (закреплённый модератором остаётся).
func (h *Hub) accessChanged(key string, ok bool) {
	h.mu.Lock()
	var mine []*station
	for _, s := range h.st {
		if s.host != "" && accessKey(s.host) == key {
			mine = append(mine, s)
		}
	}
	h.mu.Unlock()
	for _, s := range mine {
		s.sayHost(map[string]bool{"logoAccess": ok})
		if !ok {
			h.mu.Lock()
			own := s.logo != "" && s.logo != h.logos.Pinned(s.host, s.freq)
			h.mu.Unlock()
			if own {
				h.applyLogo(s, "")
			}
		}
	}
}

// serveLogoAccess — GET: выдан ли доступ этому браузеру; POST: ссылка в бота.
func (h *Hub) serveLogoAccess(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	host := identity.FromRequest(r)
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(map[string]bool{"ok": h.canLogo(host)})
		return
	}
	if h.bot == nil {
		http.Error(w, "логотип — без бота", http.StatusNotFound)
		return
	}
	if host == "" {
		http.Error(w, "нет куки kontakt_id: откройте страницу радио", http.StatusForbidden)
		return
	}
	name := h.bot.username()
	code := ""
	if name != "" {
		code = h.access.NewCode(host)
	}
	if code == "" {
		http.Error(w, "бот не на связи", http.StatusServiceUnavailable)
		return
	}
	h.logoAccess.Inc("link")
	json.NewEncoder(w).Encode(map[string]string{"link": "https://t.me/" + name + "?start=" + botStartPrefix + code})
}
