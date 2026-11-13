package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"kontakt/internal/identity"
	"kontakt/internal/signal"
)

// ringer — трубка на рычаге: слушает свою линию /api/ring через web, как страница.
type ringer struct {
	t   *testing.T
	c   *websocket.Conn
	msg chan map[string]string
}

func listen(t *testing.T, st *stack, id, line string) *ringer {
	t.Helper()
	if id == "" {
		id = identity.New()
	}
	before := ringListeners(st)
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(st.web.URL, "http")+"/api/ring?line="+line,
		http.Header{"Cookie": {identity.CookieName + "=" + id}})
	if err != nil {
		t.Fatal(err)
	}
	r := &ringer{t: t, c: c, msg: make(chan map[string]string, 16)}
	go func() { // читает одна горутина: после тайм-аута чтения gorilla-соединение мёртвое
		defer close(r.msg)
		for {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]string
			json.Unmarshal(b, &m)
			r.msg <- m
		}
	}()
	t.Cleanup(func() { c.Close() })
	waitFor(t, func() bool { return ringListeners(st) > before })
	return r
}

func ringListeners(st *stack) int {
	resp, err := http.Get(st.admin.URL + "/metrics")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "kontakt_signal_ring_listeners ") {
			var n int
			for _, c := range strings.TrimPrefix(l, "kontakt_signal_ring_listeners ") {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + int(c-'0')
			}
			return n
		}
	}
	return 0
}

func (r *ringer) expect(kind string) map[string]string {
	r.t.Helper()
	select {
	case m, ok := <-r.msg:
		if !ok {
			r.t.Fatalf("слушатель отключён, ждали %s", kind)
		}
		if m["ring"] != kind {
			r.t.Fatalf("пришло %v, ждали %s", m, kind)
		}
		return m
	case <-time.After(3 * time.Second):
		r.t.Fatalf("не дождались %s", kind)
	}
	return nil
}

func (r *ringer) quiet(d time.Duration) {
	r.t.Helper()
	select {
	case m := <-r.msg:
		r.t.Fatalf("лишнее сообщение: %v", m)
	case <-time.After(d):
	}
}

func (r *ringer) send(v any) {
	b, _ := json.Marshal(v)
	if err := r.c.WriteMessage(websocket.TextMessage, b); err != nil {
		r.t.Fatal(err)
	}
}

func notifyRing(t *testing.T, st *stack, body string) (int, map[string]int) {
	t.Helper()
	resp, err := http.Post(st.web.URL+"/api/ring", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]int
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Друг открыл ссылку, а хозяин линии не ждёт в комнате: трубка хозяина звонит, у друга — «ringing».
// Хозяин «ответил» (набрал свою комнату) — разговор, звонок смолк с answered.
func TestRingCall(t *testing.T) {
	st := newStack(t, nil)
	const line = "r1ngcall0001"
	owner := newPhone(t, st, "")
	home := listen(t, st, owner.id, line)

	friend := newPhone(t, st, "")
	friend.room = line
	friend.pickUp("32")
	ring := home.expect(signal.RingCall)
	friend.expectState("searching", signal.ReasonRinging)

	late := listen(t, st, owner.id, line) // вторая трубка хозяина, открытая посреди звонка, звонит сразу
	if m := late.expect(signal.RingCall); m["id"] != ring["id"] {
		t.Fatalf("другой звонок у поздней трубки: %v ≠ %v", m, ring)
	}

	owner.room = line
	owner.pickUp("32")
	friend.expectState("talking", "")
	if m := home.expect("stop"); m["reason"] != "answered" || m["id"] != ring["id"] {
		t.Fatalf("звонок смолк не так: %v", m)
	}
	late.expect("stop")

	// хозяин сам ждёт в комнате — своим же трубкам не звоним
	owner.inDialog("BYE", "")
	friend.expectBye(signal.ReasonPeerLeft)
	waitFor(t, func() bool { return st.sig.Stats().Online == 0 })
	again := newPhone(t, st, owner.id)
	again.room = line
	again.pickUp("32")
	again.expectState("searching", "")
	home.quiet(400 * time.Millisecond)
}

// «Положить», пока звонит: друг получает BYE declined, остальные трубки линии смолкают.
// Друг ушёл сам — звонок смолкает с gone. Комната без слушателей — никаких «ringing».
func TestRingDecline(t *testing.T) {
	st := newStack(t, nil)
	const line = "r1ngdecl0001"
	home, pocket := listen(t, st, "", line), listen(t, st, "", line)

	friend := newPhone(t, st, "")
	friend.room = line
	friend.pickUp("32")
	ring := home.expect(signal.RingCall)
	pocket.expect(signal.RingCall)
	home.send(map[string]string{"decline": ring["id"]})
	friend.expectBye(signal.ReasonDeclined)
	for _, r := range []*ringer{home, pocket} {
		if m := r.expect("stop"); m["reason"] != "declined" {
			t.Fatalf("после отказа: %v", m)
		}
	}

	again := newPhone(t, st, "")
	again.room = line
	again.pickUp("32")
	home.expect(signal.RingCall)
	again.inDialog("BYE", "")
	if m := home.expect("stop"); m["reason"] != "gone" {
		t.Fatalf("друг ушёл, а звонок: %v", m)
	}

	lonely := newPhone(t, st, "")
	lonely.room = "n0b0dy000001"
	lonely.pickUp("32")
	lonely.expectState("searching", "")
	select {
	case s := <-lonely.infos:
		t.Fatalf("никто не слушает линию, а пришло %v", s)
	case <-time.After(300 * time.Millisecond):
	}
}

// Оповещение POST /api/ring (так звонит cmd/kontakt-ring из хуков Claude Code): звонит всем трубкам
// линии, текст в одну строку и не длиннее предела; «прочитано» на одной — смолкает у других; частота
// ограничена; без ответа смолкает само.
func TestRingNotify(t *testing.T) {
	st := newStack(t, nil, func(s *signal.Server) {
		s.Ring.NotifyFor = 500 * time.Millisecond
		s.Ring.NotifyGap = 300 * time.Millisecond
	})
	const line = "n0tify000001"
	if code, out := notifyRing(t, st, `{"line":"`+line+`","text":"x"}`); code != 200 || out["listeners"] != 0 {
		t.Fatalf("без слушателей: %d %v", code, out)
	}
	if code, _ := notifyRing(t, st, `{"line":"../etc","text":"x"}`); code != 400 {
		t.Fatalf("кривую линию приняли: %d", code)
	}
	time.Sleep(350 * time.Millisecond)

	pc, phone := listen(t, st, "", line), listen(t, st, "", line)
	long := strings.Repeat("я", 200)
	code, out := notifyRing(t, st, `{"line":"https://openline.one/?call=`+line+`","from":"Claude Code","text":"готово\n\u0007`+long+`"}`)
	if code != 200 || out["listeners"] != 2 {
		t.Fatalf("оповещение: %d %v", code, out)
	}
	m := pc.expect(signal.RingNotify)
	if m["from"] != "Claude Code" || !strings.HasPrefix(m["text"], "готово яя") || len([]rune(m["text"])) != 80 || strings.ContainsAny(m["text"], "\n\a") {
		t.Fatalf("текст оповещения: %q от %q", m["text"], m["from"])
	}
	phone.expect(signal.RingNotify)
	if code, _ := notifyRing(t, st, `{"line":"`+line+`"}`); code != http.StatusTooManyRequests {
		t.Fatalf("второе оповещение сразу же: %d, ждали 429", code)
	}
	phone.send(map[string]string{"answer": m["id"]})
	if s := pc.expect("stop"); s["reason"] != "answered" {
		t.Fatalf("прочитали на телефоне, а на ПК: %v", s)
	}
	phone.expect("stop")

	time.Sleep(350 * time.Millisecond)
	resp, err := http.PostForm(st.web.URL+"/api/ring", map[string][]string{"line": {line}, "text": {"форма"}})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("оповещение формой: %v %v", err, resp)
	}
	resp.Body.Close()
	if m := pc.expect(signal.RingNotify); m["text"] != "форма" || m["from"] != "Kontakt" {
		t.Fatalf("форма: %v", m)
	}
	if s := pc.expect("stop"); s["reason"] != "timeout" {
		t.Fatalf("без ответа: %v", s)
	}
}

// Хук Claude Code из плагина claude/kontakt-ring: ring.sh без kontakt-ring в PATH звонит curl'ом, событие
// Stop — «готово — <проект>», повторный Stop (stop_hook_active) и «ждёт ввода» — тишина.
func TestRingClaudeHook(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("нет curl")
	}
	st := newStack(t, nil, func(s *signal.Server) { s.Ring.NotifyGap = 0 })
	const line = "c1aude000001"
	home := listen(t, st, "", line)
	script, _ := filepath.Abs("../claude/kontakt-ring/bin/ring.sh")
	run := func(ev string) {
		t.Helper()
		cmd := exec.Command("sh", script)
		cmd.Stdin = strings.NewReader(ev)
		// только системные каталоги: kontakt-ring из PATH разработчика не подхватится
		cmd.Env = []string{"PATH=" + filepath.Dir(curl) + ":/usr/bin:/bin", "HOME=" + t.TempDir(),
			"KONTAKT_LINE=" + st.web.URL + "/?call=" + line}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ring.sh: %v %s", err, out)
		}
	}
	run(`{"hook_event_name":"Stop","stop_hook_active":true}`)
	run(`{"hook_event_name":"Notification","message":"Claude is waiting for your input"}`)
	run(`{"session_id":"s","cwd":"/home/u/kontakt","hook_event_name":"Stop","stop_hook_active":false}`)
	if m := home.expect(signal.RingNotify); m["from"] != "Claude Code" || m["text"] != "готово — kontakt" {
		t.Fatalf("Stop: %v", m)
	}
	run(`{"hook_event_name": "Notification", "message": "Claude needs your permission to use \"Bash\""}`)
	home.expect("stop") // новое оповещение гасит прежнее
	if m := home.expect(signal.RingNotify); m["text"] != `Claude needs your permission to use "Bash"` {
		t.Fatalf("Notification: %v", m)
	}
}

// Манифесты плагина и маркетплейса — валидный JSON, хуки смотрят на существующий скрипт.
func TestClaudePluginManifest(t *testing.T) {
	var mk struct {
		Name    string
		Plugins []struct{ Name, Source string }
	}
	readJSON(t, "../.claude-plugin/marketplace.json", &mk)
	if mk.Name == "" || len(mk.Plugins) != 1 || mk.Plugins[0].Name != "kontakt-ring" {
		t.Fatalf("marketplace.json: %+v", mk)
	}
	dir := filepath.Join("..", mk.Plugins[0].Source)
	var pl struct{ Name, Version string }
	readJSON(t, filepath.Join(dir, ".claude-plugin/plugin.json"), &pl)
	if pl.Name != "kontakt-ring" || pl.Version == "" {
		t.Fatalf("plugin.json: %+v", pl)
	}
	var hk struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	readJSON(t, filepath.Join(dir, "hooks/hooks.json"), &hk)
	for _, ev := range []string{"Stop", "Notification"} {
		h := hk.Hooks[ev]
		if len(h) != 1 || len(h[0].Hooks) != 1 || !strings.Contains(h[0].Hooks[0].Command, "${CLAUDE_PLUGIN_ROOT}/bin/ring.sh") {
			t.Fatalf("хук %s: %+v", ev, h)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/ring.sh")); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
