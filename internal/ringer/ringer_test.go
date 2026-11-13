package ringer

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kontakt/internal/sip"
)

// fakePhone — SIP-устройство на UDP: на INVITE звонит (180) и дальше делает то, что велели.
type fakePhone struct {
	conn *net.UDPConn
	got  chan *sip.Msg // все запросы, кроме повторов INVITE
}

func newFakePhone(t *testing.T, mode string) (*fakePhone, string) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	p := &fakePhone{conn: c, got: make(chan *sip.Msg, 16)}
	go func() {
		buf := make([]byte, 65535)
		var inv *sip.Msg
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			m, err := sip.ParseMsg(append([]byte(nil), buf[:n]...))
			if err != nil || !m.IsRequest {
				continue
			}
			reply := func(r *sip.Msg) { c.WriteToUDP(r.Bytes(), from) }
			switch m.Method {
			case "INVITE":
				first := inv == nil
				inv = m
				if mode == "silent" {
					continue
				}
				if !first {
					continue
				}
				p.got <- m
				reply(m.Response(100, "Trying", ""))
				reply(m.Response(180, "Ringing", "dev1"))
				switch mode {
				case "answer":
					r := m.Response(200, "OK", "dev1")
					r.Add("Contact", "<sip:dev@"+c.LocalAddr().String()+">")
					reply(r)
				case "busy":
					reply(m.Response(486, "Busy Here", "dev1"))
				}
			case "CANCEL":
				p.got <- m
				reply(m.Response(200, "OK", "dev1"))
				reply(inv.Response(487, "Request Terminated", "dev1"))
			default:
				p.got <- m
				if m.Method != "ACK" {
					reply(m.Response(200, "OK", "dev1"))
				}
			}
		}
	}()
	return p, "sip:100@" + c.LocalAddr().String()
}

func (p *fakePhone) expect(t *testing.T, methods ...string) []*sip.Msg {
	t.Helper()
	var out []*sip.Msg
	for _, want := range methods {
		select {
		case m := <-p.got:
			if m.Method != want {
				t.Fatalf("пришёл %s, ждали %s", m.Method, want)
			}
			out = append(out, m)
		case <-time.After(3 * time.Second):
			t.Fatalf("не дождались %s", want)
		}
	}
	return out
}

func TestCallSIPRingsAndCancels(t *testing.T) {
	p, target := newFakePhone(t, "ring")
	start := time.Now()
	r, err := CallSIP(context.Background(), target, "Claude Code", "готово", 600*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Rang || r.Answered || r.Status != 487 {
		t.Fatalf("итог: %+v", r)
	}
	if d := time.Since(start); d < 600*time.Millisecond || d > 3*time.Second {
		t.Fatalf("звонил %v, ждали ~600 мс", d)
	}
	ms := p.expect(t, "INVITE", "CANCEL", "ACK")
	inv := ms[0]
	if !strings.Contains(inv.Get("From"), `"Claude Code: готово"`) || inv.Get("Subject") != "Claude Code: готово" {
		t.Fatalf("от кого и текст: From=%q Subject=%q", inv.Get("From"), inv.Get("Subject"))
	}
	if !strings.Contains(string(inv.Body), "PCMA/8000") {
		t.Fatalf("SDP без кодека: %s", inv.Body)
	}
	if b1, b2 := sip.Param(inv.Get("Via"), "branch"), sip.Param(ms[1].Get("Via"), "branch"); b1 != b2 {
		t.Fatalf("CANCEL в другой транзакции: %s ≠ %s", b1, b2)
	}
}

func TestCallSIPAnsweredHangsUp(t *testing.T) {
	p, target := newFakePhone(t, "answer")
	r, err := CallSIP(context.Background(), target, "Kontakt", "", 5*time.Second)
	if err != nil || !r.Answered {
		t.Fatalf("итог: %+v %v", r, err)
	}
	ms := p.expect(t, "INVITE", "ACK", "BYE")
	if sip.TagOf(ms[2].Get("To")) != "dev1" || ms[2].RURI != sip.URIOf(target) && !strings.HasPrefix(ms[2].RURI, "sip:dev@") {
		t.Fatalf("BYE не в диалог: %s %s", ms[2].RURI, ms[2].Get("To"))
	}
}

func TestCallSIPBusyAndSilent(t *testing.T) {
	p, target := newFakePhone(t, "busy")
	if r, err := CallSIP(context.Background(), target, "K", "", 5*time.Second); err != nil || r.Status != 486 {
		t.Fatalf("занято: %+v %v", r, err)
	}
	p.expect(t, "INVITE", "ACK")

	_, silent := newFakePhone(t, "silent")
	if _, err := CallSIP(context.Background(), silent, "K", "", 700*time.Millisecond); err == nil {
		t.Fatal("молчащее устройство — а ошибки нет")
	}
}

func TestNotify(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ring" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"listeners":2}`))
	}))
	defer srv.Close()
	n, err := Notify(context.Background(), srv.URL+"/", "abcdefgh12", "Claude Code", "готово")
	if err != nil || n != 2 || got["line"] != "abcdefgh12" || got["text"] != "готово" || got["from"] != "Claude Code" {
		t.Fatalf("%d %v %v", n, err, got)
	}
	if _, err := Notify(context.Background(), srv.URL+"/nope", "x", "", ""); err == nil {
		t.Fatal("404 — а ошибки нет")
	}
}

func TestBaseOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://openline.one/?call=abcdefgh12":      "https://openline.one",
		"http://localhost:8080/kontakt/?call=x#frag": "http://localhost:8080/kontakt",
		"abcdefgh12": "",
	} {
		if got := BaseOf(in); got != want {
			t.Errorf("BaseOf(%q) = %q, ждали %q", in, got, want)
		}
	}
}

func TestHook(t *testing.T) {
	for in, want := range map[string]string{
		`{"hook_event_name":"Stop","cwd":"/home/u/kontakt"}`:                                 "готово — kontakt",
		`{"hook_event_name":"Stop","cwd":"C:\\Users\\u\\proj"}`:                              "готово — proj",
		`{"hook_event_name":"Notification","message":"Claude needs your permission"}`:        "Claude needs your permission",
		`{"hook_event_name":"Stop","stop_hook_active":true}`:                                 "",
		`{"hook_event_name":"Notification","message":"Claude is waiting for your input"}`:    "",
		`{"hook_event_name":"Notification","notification_type":"idle_prompt","message":"x"}`: "",
		`{"hook_event_name":"PreToolUse"}`:                                                   "",
		`не json`:                                                                            "",
	} {
		got, ok := Hook(strings.NewReader(in))
		if got != want || ok != (want != "") {
			t.Errorf("Hook(%s) = %q %v, ждали %q", in, got, ok, want)
		}
	}
}
