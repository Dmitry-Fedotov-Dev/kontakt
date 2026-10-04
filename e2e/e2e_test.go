// Сквозные тесты: web-прокси + signal + media (по настоящему gRPC) + радио под /radio/ с общей
// модерацией (через /mod/* signal'а), браузерные трубки по WebSocket с куками и обычный
// SIP-софтфон по UDP.
package e2e

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"

	"kontakt/internal/config"
	"kontakt/internal/dsp"
	"kontakt/internal/identity"
	"kontakt/internal/media"
	"kontakt/internal/mediaapi"
	"kontakt/internal/moderation"
	"kontakt/internal/radio"
	"kontakt/internal/signal"
	"kontakt/internal/sip"
	"kontakt/internal/webapp"
)

type stack struct {
	t       *testing.T
	web     *httptest.Server
	sig     *signal.Server
	eng     *media.Engine
	mc      *mediaapi.Client
	udp     *signal.UDPServer
	admin   *httptest.Server
	radio   *radio.Hub
	cfgPath string
}

func writeConfig(t *testing.T, path string, mutate func(*config.Config)) {
	c := config.Default()
	c.ApplyDelayMs = 300
	c.TrustTalks = 0 // новые куки в тестах — не новички; доверие проверяет отдельный тест
	if mutate != nil {
		mutate(&c)
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newStack(t *testing.T, mutate func(*config.Config)) *stack {
	t.Helper()
	dir := t.TempDir()
	st := &stack{t: t, cfgPath: filepath.Join(dir, "kontakt.json")}
	writeConfig(t, st.cfgPath, mutate)

	// media
	st.eng = media.NewEngine("127.0.0.1", 23000, 23400)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	mediaapi.RegisterMediaServer(gs, st.eng)
	go gs.Serve(lis)
	mmux := http.NewServeMux()
	mmux.HandleFunc("/media", st.eng.ServeWS)
	mediaHTTP := httptest.NewServer(mmux)

	// signal
	st.mc, err = mediaapi.Dial(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(st.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, _ := moderation.Open(filepath.Join(dir, "bans.json"))
	st.sig = signal.New(cfg, mod, st.mc)
	sigHTTP := httptest.NewServer(st.sig.HTTPHandler())
	st.udp, err = st.sig.ServeUDP("127.0.0.1:0", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	st.admin = httptest.NewServer(st.sig.AdminHandler(nil))

	// радио: модерация — база signal'а через его админку, как в cmd/radio -mod
	st.radio = radio.NewHub(radio.Options{Mod: moderation.NewRemote(st.admin.URL), ReportAfter: 50 * time.Millisecond})
	radioHTTP := httptest.NewServer(st.radio.Handler())

	// web
	su, _ := url.Parse(sigHTTP.URL)
	mu, _ := url.Parse(mediaHTTP.URL)
	ru, _ := url.Parse(radioHTTP.URL)
	st.web = httptest.NewServer(webapp.Handler(webapp.Upstreams{Signal: su, Media: mu, Radio: ru}))

	t.Cleanup(func() {
		st.web.Close()
		radioHTTP.Close()
		st.admin.Close()
		sigHTTP.Close()
		st.udp.Close()
		st.mc.Close()
		mediaHTTP.Close()
		gs.Stop()
		st.eng.Close()
	})
	return st
}

// ---------- веб-трубка ----------

type phone struct {
	t      *testing.T
	st     *stack
	id     string
	sipWS  *websocket.Conn
	wmu    sync.Mutex
	media  *websocket.Conn
	callID string
	tag    string
	toTag  string
	cseq   int
	infos  chan map[string]string
	byes   chan string
	resps  chan *sip.Msg
	rtp    chan []byte
}

func newPhone(t *testing.T, st *stack, id string) *phone {
	if id == "" {
		id = identity.New()
	}
	p := &phone{t: t, st: st, id: id, callID: sip.RandHex(8), tag: sip.RandHex(4), cseq: 1,
		infos: make(chan map[string]string, 32), byes: make(chan string, 4), resps: make(chan *sip.Msg, 32), rtp: make(chan []byte, 4096)}
	h := http.Header{"Cookie": {identity.CookieName + "=" + id}}
	d := websocket.Dialer{Subprotocols: []string{"sip"}}
	c, _, err := d.Dial("ws"+strings.TrimPrefix(st.web.URL, "http")+"/sip", h)
	if err != nil {
		t.Fatal(err)
	}
	p.sipWS = c
	go p.readSIP()
	t.Cleanup(func() {
		c.Close()
		if p.media != nil {
			p.media.Close()
		}
	})
	return p
}

func (p *phone) readSIP() {
	for {
		_, data, err := p.sipWS.ReadMessage()
		if err != nil {
			return
		}
		m, err := sip.ParseMsg(data)
		if err != nil {
			continue
		}
		if !m.IsRequest {
			p.resps <- m
			continue
		}
		p.send(string(m.Response(200, "OK", "").Bytes()))
		switch m.Method {
		case "INFO":
			var st map[string]string
			json.Unmarshal(m.Body, &st)
			p.infos <- st
		case "BYE":
			p.byes <- strings.Trim(sip.Param(m.Get("Reason"), "text"), `"`)
		}
	}
}

func (p *phone) send(raw string) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.sipWS.WriteMessage(websocket.TextMessage, []byte(raw))
}

func (p *phone) dial(line string) *sip.Msg {
	sdp := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nc=IN IP4 0.0.0.0\r\nt=0 0\r\nm=audio 9 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"
	p.send(fmt.Sprintf("INVITE sip:%s@kontakt SIP/2.0\r\nVia: SIP/2.0/WSS t.invalid;branch=%s\r\nMax-Forwards: 70\r\n"+
		"From: <sip:anon@t.invalid>;tag=%s\r\nTo: <sip:%s@kontakt>\r\nCall-ID: %s\r\nCSeq: 1 INVITE\r\n"+
		"Contact: <sip:anon@t.invalid;transport=ws>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s",
		line, sip.NewBranch(), p.tag, line, p.callID, len(sdp), sdp))
	return p.final()
}

func (p *phone) final() *sip.Msg {
	p.t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case m := <-p.resps:
			if m.Status >= 200 {
				return m
			}
		case <-timeout:
			p.t.Fatal("нет финального ответа")
			return nil
		}
	}
}

// pickUp: снять трубку → 200 OK → ACK → медиа-WebSocket.
func (p *phone) pickUp(line string) {
	p.t.Helper()
	ok := p.dial(line)
	if ok.Status != 200 {
		p.t.Fatalf("ждали 200, пришло %d (%s)", ok.Status, ok.Get("Reason"))
	}
	p.toTag = sip.TagOf(ok.Get("To"))
	p.inDialog("ACK", "")
	var path string
	for _, l := range strings.Split(string(ok.Body), "\r\n") {
		if strings.HasPrefix(l, "a=x-kontakt-media:ws ") {
			path = strings.TrimPrefix(l, "a=x-kontakt-media:ws ")
		}
	}
	if path == "" {
		p.t.Fatalf("в SDP нет медиа-пути: %s", ok.Body)
	}
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(p.st.web.URL, "http")+"/"+path,
		http.Header{"Cookie": {identity.CookieName + "=" + p.id}})
	if err != nil {
		p.t.Fatal(err)
	}
	p.media = c
	go func() {
		for {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			select {
			case p.rtp <- b:
			default:
			}
		}
	}()
}

func (p *phone) inDialog(method, body string) {
	if method != "ACK" {
		p.cseq++
	}
	ct := ""
	if body != "" {
		ct = "Content-Type: application/json\r\n"
	}
	p.send(fmt.Sprintf("%s sip:kontakt@kontakt.invalid SIP/2.0\r\nVia: SIP/2.0/WSS t.invalid;branch=%s\r\n"+
		"From: <sip:anon@t.invalid>;tag=%s\r\nTo: <sip:kontakt@kontakt>;tag=%s\r\nCall-ID: %s\r\nCSeq: %d %s\r\n%sContent-Length: %d\r\n\r\n%s",
		method, sip.NewBranch(), p.tag, p.toTag, p.callID, p.cseq, method, ct, len(body), body))
}

func (p *phone) expectState(state, reason string) {
	p.t.Helper()
	timeout := time.After(4 * time.Second)
	for {
		select {
		case s := <-p.infos:
			if s["state"] == state && (reason == "*" || s["reason"] == reason) {
				return
			}
		case <-timeout:
			p.t.Fatalf("не дождались состояния %s/%s", state, reason)
		}
	}
}

func (p *phone) expectBye(reason string) {
	p.t.Helper()
	select {
	case r := <-p.byes:
		if r != reason {
			p.t.Fatalf("BYE с причиной %q, ждали %q", r, reason)
		}
	case <-time.After(3 * time.Second):
		p.t.Fatalf("не дождались BYE (%s)", reason)
	}
}

func (p *phone) report() map[string]string {
	p.t.Helper()
	p.inDialog("INFO", `{"report":true}`)
	timeout := time.After(3 * time.Second)
	for {
		select {
		case m := <-p.resps:
			if strings.HasSuffix(m.Get("CSeq"), "INFO") {
				var out map[string]string
				json.Unmarshal(m.Body, &out)
				return out
			}
		case <-timeout:
			p.t.Fatal("нет ответа на жалобу")
		}
	}
}

func (p *phone) talk(n int, freq float64) {
	for i := 0; i < n; i++ {
		p.media.WriteMessage(websocket.BinaryMessage, rtpPacket(uint16(i), freq))
	}
}

func (p *phone) drain() {
	for {
		select {
		case <-p.rtp:
		default:
			return
		}
	}
}

// level — средний уровень (RMS) в n следующих пакетах.
func (p *phone) level(n int) float64 {
	p.t.Helper()
	var sum float64
	for i := 0; i < n; i++ {
		select {
		case pkt := <-p.rtp:
			sum += rms(pkt)
		case <-time.After(2 * time.Second):
			p.t.Fatalf("получено только %d из %d пакетов", i, n)
		}
	}
	return sum / float64(n)
}

func rtpPacket(seq uint16, freq float64) []byte {
	pkt := make([]byte, 172)
	pkt[0], pkt[1] = 0x80, 8
	binary.BigEndian.PutUint16(pkt[2:], seq)
	binary.BigEndian.PutUint32(pkt[4:], uint32(seq)*160)
	binary.BigEndian.PutUint32(pkt[8:], 0xC0FFEE)
	for i := 0; i < 160; i++ {
		n := int(seq)*160 + i
		pkt[12+i] = dsp.AlawEncode(int16(12000 * math.Sin(2*math.Pi*freq*float64(n)/8000)))
	}
	return pkt
}

func rms(pkt []byte) float64 {
	payload, _, _ := media.ParseRTP(pkt)
	var e float64
	for _, b := range payload {
		v := float64(dsp.AlawDecode(b))
		e += v * v
	}
	return math.Sqrt(e / float64(len(payload)))
}

// ---------- тесты ----------

func TestRouletteFlow(t *testing.T) {
	st := newStack(t, nil)
	a, b := newPhone(t, st, ""), newPhone(t, st, "")

	a.pickUp("32")
	a.expectState("searching", "")
	if lv := a.level(15); lv < 1000 {
		t.Fatalf("пока ждём, гудков не слышно: уровень %.0f", lv)
	} else {
		t.Logf("A ждёт и слышит гудки: уровень %.0f", lv)
	}

	b.pickUp("16")
	a.expectState("talking", "")
	b.expectState("talking", "")
	a.drain()
	b.drain()
	a.talk(25, 700)
	if lv := b.level(25); lv < 1000 {
		t.Fatalf("B не слышит A: уровень %.0f", lv)
	} else {
		t.Logf("B слышит A через линию 16 кбит/с: уровень %.0f", lv)
	}

	// B кладёт трубку → A слышит шум эфира, потом снова гудки
	b.inDialog("BYE", "")
	a.expectState("searching", signal.ReasonPeerLeft)
	time.Sleep(100 * time.Millisecond)
	a.drain()
	noise := a.level(25)
	t.Logf("A после ухода B слышит белый шум: уровень %.0f", noise)
	if noise < 500 {
		t.Fatalf("нет белого шума: %.0f", noise)
	}
	a.expectState("searching", "") // через 2 с — снова в очереди с гудками

	// новый собеседник
	c := newPhone(t, st, "")
	c.pickUp("64")
	a.expectState("talking", "")
	if s := st.sig.Stats(); s.Talking != 2 || s.Online != 2 {
		t.Fatalf("статистика: %+v", s)
	}
}

func TestSameCookieNotMatched(t *testing.T) {
	st := newStack(t, nil)
	id := identity.New()
	a, b := newPhone(t, st, id), newPhone(t, st, id)
	a.pickUp("32")
	b.pickUp("32")
	time.Sleep(200 * time.Millisecond)
	if s := st.sig.Stats(); s.Talking != 0 || s.Waiting != 2 {
		t.Fatalf("две вкладки одного человека соединились сами с собой: %+v", s)
	}
}

func TestYellowCardThenBan(t *testing.T) {
	st := newStack(t, nil) // по умолчанию бан — от двух разных жалобщиков
	bad := identity.New()

	a, b := newPhone(t, st, ""), newPhone(t, st, bad)
	a.pickUp("32")
	b.pickUp("32")
	a.expectState("talking", "")

	res := a.report()
	if res["result"] != "yellow" {
		t.Fatalf("первая жалоба: %v", res)
	}
	b.expectBye(signal.ReasonYellow)
	a.expectState("searching", signal.ReasonReported)
	if again := a.report(); again["result"] != "already" {
		t.Fatalf("повторная жалоба на того же: %v", again)
	}

	// нарушитель с жёлтой карточкой может зайти снова
	me := getJSON(t, st.web.URL+"/api/me", bad)
	if me["banned"] != false || me["cards"] != float64(1) {
		t.Fatalf("/api/me после жёлтой: %v", me)
	}
	b2 := newPhone(t, st, bad)
	b2.pickUp("32")
	c := newPhone(t, st, "")
	c.pickUp("32")
	// A не должен снова попасть на B (жалоба), значит B2 соединится с C
	c.expectState("talking", "")
	if res := c.report(); res["result"] != "banned" {
		t.Fatalf("вторая жалоба: %v", res)
	}
	b2.expectBye(signal.ReasonBanned)

	// вход закрыт навсегда (по этой куке)
	b3 := newPhone(t, st, bad)
	if r := b3.dial("32"); r.Status != 403 || strings.Trim(sip.Param(r.Get("Reason"), "text"), `"`) != "banned" {
		t.Fatalf("забаненного пустили: %d %s", r.Status, r.Get("Reason"))
	}
	if me := getJSON(t, st.web.URL+"/api/me", bad); me["banned"] != true {
		t.Fatalf("/api/me: %v", me)
	}
}

func TestHotConfigInstantBan(t *testing.T) {
	st := newStack(t, nil)
	go st.sig.Cfg.Watch(make(chan struct{}))
	writeConfig(t, st.cfgPath, func(c *config.Config) { c.BanReporters = 1 })
	start := time.Now()
	for st.sig.Cfg.Get().BanReporters != 1 {
		if time.Since(start) > 3*time.Second {
			t.Fatal("конфиг не применился")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("новый конфиг применён через %s (объявление + задержка 300 мс)", time.Since(start).Round(10*time.Millisecond))

	bad := identity.New()
	a, b := newPhone(t, st, ""), newPhone(t, st, bad)
	a.pickUp("32")
	b.pickUp("32")
	a.expectState("talking", "")
	if res := a.report(); res["result"] != "banned" {
		t.Fatalf("бан с первой жалобы: %v", res)
	}
	b.expectBye(signal.ReasonBanned)

	// битый конфиг не применяется
	os.WriteFile(st.cfgPath, []byte(`{"ban_reporters":0}`), 0o644)
	time.Sleep(1200 * time.Millisecond)
	if st.sig.Cfg.Get().BanReporters != 1 {
		t.Fatal("применился конфиг с ошибкой")
	}
}

// Жалоба новичка разводит пару, но карточки не даёт: новая кука в инкогнито не должна банить.
func TestNewcomerReportNoted(t *testing.T) {
	st := newStack(t, func(c *config.Config) { c.TrustTalks = 3; c.BanReporters = 1 })
	bad := identity.New()
	a, b := newPhone(t, st, ""), newPhone(t, st, bad)
	a.pickUp("32")
	b.pickUp("32")
	a.expectState("talking", "")
	if res := a.report(); res["result"] != "noted" {
		t.Fatalf("жалоба новичка: %v", res)
	}
	a.expectState("searching", signal.ReasonReported)
	b.expectState("searching", signal.ReasonPeerLeft)
	if me := getJSON(t, st.web.URL+"/api/me", bad); me["banned"] != false || me["cards"] != float64(0) {
		t.Fatalf("жалоба новичка дала карточку: %v", me)
	}
	// после жалобы этих двоих больше не соединяют
	time.Sleep(300 * time.Millisecond)
	if s := st.sig.Stats(); s.Talking != 0 {
		t.Fatalf("пару после жалобы соединили снова: %+v", s)
	}
}

// Новичок набирает доверие разговорами: после засчитанного разговора его жалоба идёт в зачёт.
func TestTrustAfterTalk(t *testing.T) {
	old := signal.CountedTalk
	signal.CountedTalk = 100 * time.Millisecond
	t.Cleanup(func() { signal.CountedTalk = old })
	st := newStack(t, func(c *config.Config) { c.TrustTalks = 1 })
	me, bad := identity.New(), identity.New()

	a, x := newPhone(t, st, me), newPhone(t, st, "")
	a.pickUp("32")
	x.pickUp("32")
	a.expectState("talking", "")
	time.Sleep(200 * time.Millisecond)
	x.inDialog("BYE", "")
	a.expectState("searching", signal.ReasonPeerLeft)

	b := newPhone(t, st, bad)
	b.pickUp("32")
	a.expectState("talking", "")
	if res := a.report(); res["result"] != "yellow" {
		t.Fatalf("жалоба после засчитанного разговора: %v", res)
	}
}

// radioWS — сокет радио через web (/radio/…) с кукой id.
func radioWS(t *testing.T, st *stack, path, id string) *websocket.Conn {
	t.Helper()
	h := http.Header{"Cookie": {identity.CookieName + "=" + id}}
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(st.web.URL, "http")+webapp.RadioPrefix+path, h)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// radioText ждёт текстовое сообщение с ключом key, пропуская звук и списки станций.
func radioText(t *testing.T, c *websocket.Conn, key string) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		typ, b, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("ждали %q: %v", key, err)
		}
		var m map[string]any
		if typ == websocket.TextMessage && json.Unmarshal(b, &m) == nil && m[key] != nil {
			return fmt.Sprint(m[key])
		}
	}
}

func expectClose(t *testing.T, c *websocket.Conn, code int) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		if !websocket.IsCloseError(err, code) {
			t.Fatalf("ждали закрытие %d, а: %v", code, err)
		}
		return
	}
}

func radioListener(t *testing.T, st *stack, id string, f int) *websocket.Conn {
	l := radioWS(t, st, "/ws/listen", id)
	l.WriteJSON(map[string]int{"tune": f})
	for st.radio.Stations()[0].Listeners == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	return l
}

// Радио под /radio/ на том же домене: одна кука, одна база. Две жалобы слушателей закрывают
// ведущему эфир, но не рулетку; бан из админки в зоне all выгоняет и из разговора.
func TestRadioSharedModeration(t *testing.T) {
	st := newStack(t, nil)
	bad := identity.New()

	resp, err := http.Get(st.web.URL + "/radio/w/101.7?n=X")
	if err != nil {
		t.Fatal(err)
	}
	page := new(strings.Builder)
	io.Copy(page, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(page.String(), "/radio/og/1017.png") {
		t.Fatalf("ссылка на волну за web: %d, og без /radio:\n%.400s", resp.StatusCode, page)
	}

	if tr, err := http.Get(st.web.URL + "/radio/terms.html"); err != nil || tr.StatusCode != 200 {
		t.Fatalf("правила радио за web: %v %v", err, tr)
	} else {
		tr.Body.Close()
	}

	host := radioWS(t, st, "/ws/host?f=1017&name=BAD", bad)
	host.WriteMessage(websocket.BinaryMessage, make([]byte, radio.FrameBytes))
	waitFor(t, func() bool { return len(st.radio.Stations()) == 1 })

	l1 := radioListener(t, st, identity.New(), 1017)
	time.Sleep(100 * time.Millisecond) // ReportAfter — 50 мс
	l1.WriteJSON(map[string]bool{"report": true})
	if r := radioText(t, l1, "report"); r != "yellow" {
		t.Fatalf("первая жалоба на станцию: %s", r)
	}
	if c := radioText(t, host, "card"); c != "yellow" {
		t.Fatalf("ведущему: %s", c)
	}
	l1.WriteJSON(map[string]bool{"report": true})
	if r := radioText(t, l1, "report"); r != "already" {
		t.Fatalf("повторная жалоба: %s", r)
	}
	l2 := radioListener(t, st, identity.New(), 1017)
	l2.WriteJSON(map[string]bool{"report": true})
	if r := radioText(t, l2, "report"); r != "listen_more" {
		t.Fatalf("жалоба сразу после настройки: %s", r)
	}
	time.Sleep(100 * time.Millisecond)
	l2.WriteJSON(map[string]bool{"report": true})
	if r := radioText(t, l2, "report"); r != "banned" {
		t.Fatalf("вторая жалоба от другого слушателя: %s", r)
	}
	expectClose(t, host, radio.CloseBanned)
	expectClose(t, radioWS(t, st, "/ws/host?f=1017", bad), radio.CloseBanned)
	if me := getJSON(t, st.web.URL+"/radio/api/me", bad); me["banned"] != true {
		t.Fatalf("/radio/api/me: %v", me)
	}

	// эфир закрыт, рулетка — нет
	if me := getJSON(t, st.web.URL+"/api/me", bad); me["banned"] != false {
		t.Fatalf("бан эфира закрыл и звонки: %v", me)
	}
	b, c := newPhone(t, st, bad), newPhone(t, st, "")
	b.pickUp("32")
	c.pickUp("32")
	b.expectState("talking", "")

	post := func(path string) {
		resp, err := http.Post(st.admin.URL+path, "", nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, err, resp)
		}
		resp.Body.Close()
	}
	post("/admin/ban?zone=all&key=" + moderation.Key(bad))
	b.expectBye(signal.ReasonBanned)
	if r := b.dial("32"); r.Status != 403 {
		t.Fatalf("после бана all пустили звонить: %d", r.Status)
	}

	var journal []moderation.Entry
	jr, _ := http.Get(st.admin.URL + "/admin/journal")
	json.NewDecoder(jr.Body).Decode(&journal)
	jr.Body.Close()
	var got []string
	for _, e := range journal {
		got = append(got, e.Action+"/"+string(e.Zone)+"/"+e.By)
	}
	if strings.Join(got, " ") != "yellow/air/auto ban/air/auto ban/all/admin" {
		t.Fatalf("журнал: %v", got)
	}

	post("/admin/unban?zone=all&key=" + moderation.Key(bad))
	if me := getJSON(t, st.web.URL+"/radio/api/me", bad); me["banned"] != false {
		t.Fatalf("после разбана: %v", me)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("не дождались")
}

func TestMaintenance(t *testing.T) {
	st := newStack(t, func(c *config.Config) { c.Maintenance = true })
	p := newPhone(t, st, "")
	if r := p.dial("32"); r.Status != 503 {
		t.Fatalf("в техработы пустили: %d", r.Status)
	}
}

// Обычный софтфон по UDP: набрал раз — и попал на веб-трубку.
func TestUDPSoftphone(t *testing.T) {
	st := newStack(t, nil)
	sipC, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	rtpC, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer sipC.Close()
	defer rtpC.Close()
	srv := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: st.udp.Port}
	sdp := fmt.Sprintf("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP 0 8 101\r\n",
		rtpC.LocalAddr().(*net.UDPAddr).Port)
	inv := fmt.Sprintf("INVITE sip:64@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP %s;branch=%s\r\nMax-Forwards: 70\r\n"+
		"From: <sip:alice@127.0.0.1>;tag=u1\r\nTo: <sip:64@127.0.0.1>\r\nCall-ID: udpcall1\r\nCSeq: 1 INVITE\r\n"+
		"Contact: <sip:alice@%s>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s",
		sipC.LocalAddr(), sip.NewBranch(), sipC.LocalAddr(), len(sdp), sdp)
	sipC.WriteToUDP([]byte(inv), srv)

	var ok *sip.Msg
	buf := make([]byte, 4096)
	for ok == nil {
		sipC.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _, err := sipC.ReadFromUDP(buf)
		if err != nil {
			t.Fatal("софтфон не получил 200 OK")
		}
		if m, _ := sip.ParseMsg(buf[:n]); m != nil && m.Status == 200 {
			ok = m
		}
	}
	var port int
	for _, l := range strings.Split(string(ok.Body), "\r\n") {
		if strings.HasPrefix(l, "m=audio ") {
			fmt.Sscan(strings.Fields(l)[1], &port)
		}
	}
	// гудки идут и софтфону
	rtpC.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := rtpC.ReadFromUDP(buf); err != nil {
		t.Fatal("софтфон не слышит гудков")
	}

	w := newPhone(t, st, "")
	w.pickUp("32")
	w.expectState("talking", "")
	w.drain()
	media := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	for i := uint16(0); i < 20; i++ {
		rtpC.WriteToUDP(rtpPacket(i, 500), media)
	}
	if lv := w.level(15); lv < 1000 {
		t.Fatalf("веб-трубка не слышит софтфон: %.0f", lv)
	}
}

func getJSON(t *testing.T, url, id string) map[string]any {
	req, _ := http.NewRequest("GET", url, nil)
	req.AddCookie(&http.Cookie{Name: identity.CookieName, Value: id})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestWebSetsEternalCookie(t *testing.T) {
	st := newStack(t, nil)
	resp, err := http.Get(st.web.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var c *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == identity.CookieName {
			c = ck
		}
	}
	if c == nil || c.MaxAge < 3*365*24*3600 || !c.HttpOnly {
		t.Fatalf("кука: %+v", c)
	}
}

func scrapeMetrics(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// Метрики отражают настоящий звонок: два снятия трубки, одна пара, мост и полоса по факту.
func TestMetricsReflectCall(t *testing.T) {
	st := newStack(t, nil)
	a := newPhone(t, st, "")
	b := newPhone(t, st, "")
	a.pickUp("32")
	b.pickUp("32")
	a.expectState("talking", "")
	b.expectState("talking", "")
	a.talk(25, 700)
	b.level(25)

	sig := scrapeMetrics(t, st.sig.Metrics().Handler())
	for _, want := range []string{
		`kontakt_signal_calls_total{result="accepted"} 2`,
		`kontakt_signal_calls_total{result="banned"} 0`, // объявлен заранее: rate() видит первый отказ
		"kontakt_pairs_total 1",
		`kontakt_signal_legs{state="talking"} 2`,
		"kontakt_signal_queue_wait_seconds_count 2",
	} {
		if !strings.Contains(sig, want) {
			t.Fatalf("signal: нет %q:\n%s", want, sig)
		}
	}
	med := scrapeMetrics(t, st.eng.Metrics.Handler())
	for _, want := range []string{"kontakt_media_bridges 1", `kontakt_media_endpoints{transport="ws"} 2`, "# TYPE kontakt_media_bytes_in_total counter"} {
		if !strings.Contains(med, want) {
			t.Fatalf("media: нет %q:\n%s", want, med)
		}
	}
	if strings.Contains(med, "kontakt_media_bytes_out_total 0\n") {
		t.Fatalf("media: полоса не посчитана, хотя B слышал A:\n%s", med)
	}

	b.inDialog("BYE", "")
	a.expectState("searching", signal.ReasonPeerLeft)
	if sig := scrapeMetrics(t, st.sig.Metrics().Handler()); !strings.Contains(sig, `kontakt_signal_hangups_total{reason="user"} 1`) {
		t.Fatalf("signal: отбой не посчитан:\n%s", sig)
	}
}
