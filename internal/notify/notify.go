package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kontakt/internal/moderation"
)

// Service — бот: раз в Interval сверяет тревоги Prometheus, принимает события модерации
// от signal (POST /event) и выполняет команды владельца через админку signal'а.
//
// Тревоги берутся прямо из Prometheus (/api/v1/alerts), без Alertmanager: так их не нужно
// заводить в трёх вариантах стека мониторинга. Цена — нет заглушения и группировки.
type Service struct {
	Bot        *Bot
	Prometheus string        // http://127.0.0.1:9092; пусто — без тревог
	Admin      string        // админка signal'а; пусто — команды модерации недоступны
	Interval   time.Duration // опрос тревог, по умолчанию 15 с
	Log        *log.Logger

	once    sync.Once
	c       *http.Client
	mu      sync.Mutex
	firing  map[string]alert // отпечаток → тревога
	promOK  bool             // Prometheus отвечал в прошлый раз
	promErr int              // ошибок подряд
	offset  int64
}

type alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
}

func (s *Service) init() { s.once.Do(s.setup) }

func (s *Service) setup() {
	if s.Interval <= 0 {
		s.Interval = 15 * time.Second
	}
	if s.Log == nil {
		s.Log = log.Default()
	}
	s.c = &http.Client{Timeout: 5 * time.Second}
	s.firing = map[string]alert{}
}

// Run работает до отмены ctx; на выходе пишет в чат, что стенд остановлен.
func (s *Service) Run(ctx context.Context) {
	s.init()
	s.send("▶️ <b>Стенд запущен</b>, бот на связи. /help — команды.")
	var wg sync.WaitGroup
	if s.Prometheus != "" {
		wg.Add(1)
		go func() { defer wg.Done(); s.alertsLoop(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); s.updatesLoop(ctx) }()
	<-ctx.Done()
	s.send("⏹ <b>Стенд остановлен</b> (или упал): звонки, радио и тревоги не работают, пока его не запустят снова.")
	wg.Wait()
}

func (s *Service) send(text string, rows ...[]Button) {
	if err := s.Bot.Send(text, rows...); err != nil {
		s.Log.Printf("telegram: %v", err)
	}
}

// ---------- тревоги ----------

func fingerprint(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + l[k] + ",")
	}
	return b.String()
}

func (s *Service) alertsLoop(ctx context.Context) {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		s.CheckAlerts()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckAlerts — один опрос: новые тревоги и прошедшие уходят в чат.
func (s *Service) CheckAlerts() {
	s.init()
	var r struct {
		Data struct {
			Alerts []alert `json:"alerts"`
		} `json:"data"`
	}
	err := s.getJSON(s.Prometheus+"/api/v1/alerts", &r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.promErr++
		// три промаха подряд, а до этого отвечал: сам мониторинг лёг — тревоги больше не придут
		if s.promOK && s.promErr == 3 {
			s.promOK = false
			s.send("⚠️ <b>Prometheus не отвечает</b> — тревоги сейчас не приходят. Мониторинг: <code>monitoring/stack.sh up</code>")
		}
		return
	}
	if !s.promOK && s.promErr >= 3 {
		s.send("✅ Prometheus снова отвечает, тревоги опять приходят.")
	}
	s.promOK, s.promErr = true, 0
	now := map[string]alert{}
	for _, a := range r.Data.Alerts {
		if a.State == "firing" {
			now[fingerprint(a.Labels)] = a
		}
	}
	for fp, a := range now {
		if _, was := s.firing[fp]; !was {
			icon := "🔴"
			if a.Labels["severity"] == "warning" {
				icon = "🟠"
			}
			s.send(fmt.Sprintf("%s <b>%s</b>\n%s\n<i>%s</i>", icon, html.EscapeString(a.Labels["alertname"]),
				html.EscapeString(a.Annotations["summary"]), html.EscapeString(a.Annotations["description"])))
		}
	}
	for fp, a := range s.firing {
		if _, still := now[fp]; !still {
			s.send(fmt.Sprintf("✅ Прошло: <b>%s</b>\n%s", html.EscapeString(a.Labels["alertname"]),
				html.EscapeString(a.Annotations["summary"])))
		}
	}
	s.firing = now
}

// ---------- события модерации ----------

var zoneName = map[moderation.Zone]string{moderation.Calls: "звонки", moderation.Air: "эфир", moderation.All: "везде"}

func entryText(e moderation.Entry) (string, []Button) {
	z := zoneName[e.Zone]
	key := "<code>" + html.EscapeString(e.Key) + "</code>"
	unban := []Button{{Text: "Снять", Data: "u:" + string(e.Zone) + ":" + e.Key}}
	switch e.Action {
	case "yellow":
		return "🟨 <b>Жёлтая карточка</b> · " + z + "\n" + key, []Button{{Text: "Снять карточку", Data: unban[0].Data}}
	case "ban":
		by := "по жалобам"
		if e.By == "admin" {
			by = "админ"
		}
		return "⛔ <b>Бан</b> · " + z + " (" + by + ")\n" + key, []Button{{Text: "Разбан", Data: unban[0].Data}}
	case "unban":
		return "✅ <b>Разбан</b> · " + z + "\n" + key, nil
	}
	return html.EscapeString(e.Action) + " · " + z + "\n" + key, nil
}

// EventHandler — POST /event: строка журнала модерации от signal'а (только localhost).
func (s *Service) EventHandler() http.Handler {
	s.init()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e moderation.Entry
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&e) != nil || e.Action == "" {
			http.Error(w, "POST JSON moderation.Entry", http.StatusBadRequest)
			return
		}
		text, btn := entryText(e)
		if btn != nil {
			s.send(text, btn)
		} else {
			s.send(text)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// PostEntry — то, чем signal шлёт событие боту (moderation.Store.OnEntry).
func PostEntry(url string) func(moderation.Entry) {
	c := &http.Client{Timeout: 3 * time.Second}
	return func(e moderation.Entry) {
		b, _ := json.Marshal(e)
		resp, err := c.Post(url+"/event", "application/json", strings.NewReader(string(b)))
		if err != nil {
			log.Printf("notify: бот не ответил: %v", err)
			return
		}
		resp.Body.Close()
	}
}

// ---------- команды ----------

const help = `<b>Kontakt</b> — команды:
/stats — кто на станции, баны и карточки
/journal [N] — последние действия модерации (по умолчанию 10)
/ban &lt;ключ&gt; &lt;calls|air|all&gt; — бан
/unban &lt;ключ&gt; &lt;calls|air|all&gt; — разбан
Ключ — из сообщений о карточках и банах или из /journal.`

var keyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Service) updatesLoop(ctx context.Context) {
	for ctx.Err() == nil {
		ups, err := s.Bot.Updates(s.offset, 25*time.Second)
		if err != nil {
			s.Log.Printf("telegram: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			s.offset = u.ID + 1
			s.Handle(u)
		}
	}
}

// Handle — одно обновление: команды и кнопки принимаются только из чата владельца.
func (s *Service) Handle(u Update) {
	s.init()
	switch {
	case u.Message != nil && u.Message.Chat.ID == s.Bot.Chat:
		s.send(s.command(u.Message.Text))
	case u.Callback != nil && u.Callback.Message != nil && u.Callback.Message.Chat.ID == s.Bot.Chat:
		parts := strings.SplitN(u.Callback.Data, ":", 3)
		if len(parts) != 3 || parts[0] != "u" {
			s.Bot.Answer(u.Callback.ID, "не понял кнопку")
			return
		}
		res := s.modAction("unban", parts[2], parts[1])
		s.Bot.Answer(u.Callback.ID, res)
		if res == "готово" {
			s.Bot.Edit(u.Callback.Message.ID, html.EscapeString(u.Callback.Message.Text)+"\n\n✅ снято")
		}
	}
}

func (s *Service) command(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 {
		return help
	}
	cmd := strings.SplitN(f[0], "@", 2)[0] // /stats@kontakt_alerts_bot в группах
	switch cmd {
	case "/stats":
		return s.stats()
	case "/journal":
		n := 10
		if len(f) > 1 {
			if v, err := strconv.Atoi(f[1]); err == nil && v > 0 && v <= 100 {
				n = v
			}
		}
		return s.journal(n)
	case "/ban", "/unban":
		if len(f) != 3 {
			return "формат: " + cmd + " &lt;ключ&gt; &lt;calls|air|all&gt;"
		}
		return html.EscapeString(s.modAction(cmd[1:], f[1], f[2]))
	}
	return help
}

func (s *Service) modAction(action, key, zone string) string {
	if s.Admin == "" {
		return "админка signal'а не задана"
	}
	if !keyRe.MatchString(key) {
		return "ключ — 32 шестнадцатеричных символа"
	}
	if _, ok := moderation.ParseZone(zone); !ok {
		return "зона: calls, air или all"
	}
	resp, err := s.c.Post(s.Admin+"/admin/"+action+"?"+url.Values{"key": {key}, "zone": {zone}}.Encode(), "", nil)
	if err != nil {
		return "signal не отвечает"
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "signal: " + resp.Status
	}
	return "готово"
}

func (s *Service) stats() string {
	if s.Admin == "" {
		return "админка signal'а не задана"
	}
	var st struct {
		Signal struct {
			Online, Waiting, Talking int
			Total                    int64 `json:"total_calls"`
		}
		Moderation moderation.Counts
	}
	if err := s.getJSON(s.Admin+"/admin/stats", &st); err != nil {
		return "signal не отвечает"
	}
	m := st.Moderation
	return fmt.Sprintf("<b>Станция</b>: на линии %d, ждут %d, говорят %d; всего разговоров %d\n"+
		"<b>Баны</b>: звонки %d, эфир %d, везде %d\n<b>Карточки</b>: звонки %d, эфир %d",
		st.Signal.Online, st.Signal.Waiting, st.Signal.Talking, st.Signal.Total,
		m.Banned[moderation.Calls], m.Banned[moderation.Air], m.Banned[moderation.All],
		m.Yellow[moderation.Calls], m.Yellow[moderation.Air])
}

func (s *Service) journal(n int) string {
	if s.Admin == "" {
		return "админка signal'а не задана"
	}
	var es []moderation.Entry
	if err := s.getJSON(s.Admin+"/admin/journal?n="+strconv.Itoa(n), &es); err != nil {
		return "signal не отвечает"
	}
	if len(es) == 0 {
		return "журнал пуст"
	}
	var b strings.Builder
	b.WriteString("<b>Журнал модерации</b>\n")
	for _, e := range es {
		fmt.Fprintf(&b, "%s %s · %s · %s · <code>%s</code>\n", html.EscapeString(e.Day), html.EscapeString(e.Action),
			zoneName[e.Zone], html.EscapeString(e.By), html.EscapeString(e.Key))
	}
	return b.String()
}

func (s *Service) getJSON(u string, out any) error {
	resp, err := s.c.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
