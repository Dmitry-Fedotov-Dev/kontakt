// Package ringer — «позвонить и сказать»: оповещение на трубку «Контакта» (POST /api/ring) или прямой
// звонок на SIP-устройство (софтфон на ПК, приложение на телефоне, IP-телефон на столе) — INVITE по UDP,
// трубка звонит своим звонком, на экране — от кого и текст; через заданное время звонок снимается сам
// (CANCEL), ответили — сразу кладём (BYE). Разговора нет: это звонок-оповещение.
//
// Используют cmd/kontakt-ring и хуки Claude Code (claude/kontakt-ring): хук — просто команда, модель
// не вызывается, поэтому оповещение не стоит ни одного токена.
package ringer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"kontakt/internal/sip"
)

// Notify — оповещение на линию трубки «Контакта». base — адрес станции (https://openline.one), line —
// линия или вся ссылка …/?call=<линия>. Возвращает, у скольких трубок зазвонило (0 — никто не слушает).
func Notify(ctx context.Context, base, line, from, text string) (int, error) {
	body, _ := json.Marshal(map[string]string{"line": line, "from": from, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api/ring", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kontakt-ring")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("станция ответила %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct{ Listeners int }
	if err := json.Unmarshal(b, &out); err != nil {
		return 0, fmt.Errorf("ответ станции: %w", err)
	}
	return out.Listeners, nil
}

// BaseOf — адрес станции из ссылки на линию (https://host/путь/?call=… → https://host/путь); "" — не ссылка.
func BaseOf(link string) string {
	if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
		return ""
	}
	if i := strings.IndexAny(link, "?#"); i >= 0 {
		link = link[:i]
	}
	return strings.TrimRight(link, "/")
}

// SIPResult — чем кончился звонок на SIP-устройство.
type SIPResult struct {
	Rang     bool // устройство ответило 180/183 — звонило
	Answered bool // сняли трубку (200) — мы сразу положили
	Status   int  // последний финальный ответ (486 — занято, 603 — отклонили, 487 — сняли по CANCEL)
}

// CallSIP звонит на target (sip:user@host[:port], по UDP) ring времени, с именем from и текстом text
// (в отображаемом имени и в Subject — софтфоны показывают одно или другое).
func CallSIP(ctx context.Context, target, from, text string, ring time.Duration) (SIPResult, error) {
	var res SIPResult
	hostport := strings.TrimPrefix(strings.TrimPrefix(target, "sips:"), "sip:")
	if i := strings.IndexByte(hostport, '@'); i >= 0 {
		hostport = hostport[i+1:]
	}
	if i := strings.IndexByte(hostport, ';'); i >= 0 {
		hostport = hostport[:i]
	}
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		hostport = net.JoinHostPort(hostport, "5060")
	}
	raddr, err := net.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return res, err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr)
	me := net.JoinHostPort(local.IP.String(), fmt.Sprint(local.Port))

	name := from
	if text != "" {
		name = from + ": " + text
	}
	name = strings.NewReplacer(`"`, "'", `\`, "/").Replace(name)
	callID, tag, branch := sip.RandHex(10), sip.RandHex(4), sip.NewBranch()
	inv := &sip.Msg{IsRequest: true, Method: "INVITE", RURI: target}
	inv.Add("Via", "SIP/2.0/UDP "+me+";branch="+branch+";rport")
	inv.Add("Max-Forwards", "70")
	inv.Add("From", fmt.Sprintf(`"%s" <sip:kontakt-ring@%s>;tag=%s`, name, me, tag))
	inv.Add("To", "<"+target+">")
	inv.Add("Call-ID", callID)
	inv.Add("CSeq", "1 INVITE")
	inv.Add("Contact", "<sip:kontakt-ring@"+me+">")
	inv.Add("Subject", name)
	inv.Add("User-Agent", "kontakt-ring")
	inv.Add("Allow", "INVITE, ACK, CANCEL, BYE")
	inv.Add("Content-Type", "application/sdp")
	// голоса не будет: порт 9 (discard), но кодек честный — иначе софтфон отклонит 488
	inv.Body = []byte(fmt.Sprintf("v=0\r\no=- %d 1 IN IP4 %s\r\ns=kontakt-ring\r\nc=IN IP4 %s\r\nt=0 0\r\n"+
		"m=audio 9 RTP/AVP 8 0\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=sendrecv\r\n", time.Now().Unix(), local.IP, local.IP))

	send := func(m *sip.Msg) { conn.Write(m.Bytes()) }
	// ACK на финальный ответ: на 2xx — новая транзакция, на ошибку — та же ветка (RFC 3261, 17.1.1.3)
	ack := func(r *sip.Msg) {
		a := &sip.Msg{IsRequest: true, Method: "ACK", RURI: target}
		b := branch
		if r.Status < 300 {
			b = sip.NewBranch()
			if c := sip.URIOf(r.Get("Contact")); c != "" {
				a.RURI = c
			}
		}
		a.Add("Via", "SIP/2.0/UDP "+me+";branch="+b+";rport")
		a.Add("Max-Forwards", "70")
		a.Add("From", inv.Get("From"))
		a.Add("To", r.Get("To"))
		a.Add("Call-ID", callID)
		a.Add("CSeq", "1 ACK")
		send(a)
	}
	bye := func(r *sip.Msg) {
		b := &sip.Msg{IsRequest: true, Method: "BYE", RURI: target}
		if c := sip.URIOf(r.Get("Contact")); c != "" {
			b.RURI = c
		}
		b.Add("Via", "SIP/2.0/UDP "+me+";branch="+sip.NewBranch()+";rport")
		b.Add("Max-Forwards", "70")
		b.Add("From", inv.Get("From"))
		b.Add("To", r.Get("To"))
		b.Add("Call-ID", callID)
		b.Add("CSeq", "2 BYE")
		send(b)
	}
	cancel := func() {
		c := &sip.Msg{IsRequest: true, Method: "CANCEL", RURI: target}
		c.Add("Via", "SIP/2.0/UDP "+me+";branch="+branch+";rport")
		c.Add("Max-Forwards", "70")
		c.Add("From", inv.Get("From"))
		c.Add("To", inv.Get("To"))
		c.Add("Call-ID", callID)
		c.Add("CSeq", "1 CANCEL")
		send(c)
	}

	send(inv)
	start := time.Now()
	resend, resendAt := 500*time.Millisecond, time.Now().Add(500*time.Millisecond)
	cancelled := time.Time{}
	got := false
	buf := make([]byte, 65535)
	for {
		if err := ctx.Err(); err != nil && cancelled.IsZero() {
			cancel()
			cancelled = time.Now()
		}
		now := time.Now()
		switch {
		case !cancelled.IsZero() && now.Sub(cancelled) > 4*time.Second:
			return res, nil // на CANCEL не ответили — звонок всё равно смолкнет по таймеру устройства
		case cancelled.IsZero() && now.Sub(start) >= ring:
			if !got {
				return res, errors.New("SIP-устройство не отвечает")
			}
			cancel()
			cancelled = now
		case !got && cancelled.IsZero() && now.After(resendAt): // UDP: INVITE повторяем, пока не ответят
			send(inv)
			resend *= 2
			resendAt = now.Add(resend)
		}
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return res, err
		}
		r, err := sip.ParseMsg(append([]byte(nil), buf[:n]...))
		if err != nil || r.Get("Call-ID") != callID {
			continue
		}
		if r.IsRequest { // BYE/CANCEL с той стороны и прочее — вежливо подтверждаем
			send(r.Response(200, "OK", ""))
			continue
		}
		if _, method := r.CSeq(); method != "INVITE" {
			continue // 200 на CANCEL/BYE
		}
		got = true
		switch {
		case r.Status == 180 || r.Status == 183:
			res.Rang = true
		case r.Status >= 200 && r.Status < 300:
			ack(r)
			bye(r)
			res.Answered, res.Status = true, r.Status
			return res, nil
		case r.Status >= 300:
			ack(r)
			res.Status = r.Status
			return res, nil
		}
	}
}

// Hook — что сказать по событию хука Claude Code (JSON на stdin): Stop — «готово», Notification —
// его сообщение (просит разрешения, ждёт ввода). ok == false — событие не для звонка.
func Hook(r io.Reader) (text string, ok bool) {
	var ev struct {
		Event   string `json:"hook_event_name"`
		Message string `json:"message"`
		Cwd     string `json:"cwd"`
		Active  bool   `json:"stop_hook_active"`
		Kind    string `json:"notification_type"`
	}
	if json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&ev) != nil {
		return "", false
	}
	project := ""
	if ev.Cwd != "" {
		project = filepath.Base(strings.ReplaceAll(ev.Cwd, `\`, "/"))
	}
	switch ev.Event {
	case "Stop", "SubagentStop":
		if ev.Active { // Stop-хук уже продолжил работу — второй раз не звоним
			return "", false
		}
		if project != "" {
			return "готово — " + project, true
		}
		return "готово", true
	case "Notification":
		// «ждёт ввода» приходит через минуту после Stop — второй звонок о том же
		if ev.Kind == "idle_prompt" || strings.Contains(ev.Message, "waiting for your input") {
			return "", false
		}
		if ev.Message == "" {
			return "ждёт вас", true
		}
		return ev.Message, true
	}
	return "", false
}
