package sip

// Минимальный SIP (RFC 3261): разбор и сборка сообщений.
// Ровно столько, сколько нужно B2BUA-матчмейкеру: INVITE/ACK/BYE/CANCEL/OPTIONS/REGISTER.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Header struct{ Name, Value string }

type Msg struct {
	IsRequest bool
	Method    string
	RURI      string
	Status    int
	Reason    string
	Headers   []Header
	Body      []byte
}

var compact = map[string]string{
	"v": "Via", "f": "From", "t": "To", "i": "Call-ID", "m": "Contact",
	"l": "Content-Length", "c": "Content-Type", "k": "Supported",
}

func canon(name string) string {
	n := strings.TrimSpace(name)
	if full, ok := compact[strings.ToLower(n)]; ok {
		return full
	}
	switch strings.ToLower(n) {
	case "call-id":
		return "Call-ID"
	case "cseq":
		return "CSeq"
	case "www-authenticate":
		return "WWW-Authenticate"
	}
	parts := strings.Split(strings.ToLower(n), "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

func ParseMsg(data []byte) (*Msg, error) {
	sep := []byte("\r\n\r\n")
	idx := bytes.Index(data, sep)
	if idx < 0 {
		sep = []byte("\n\n")
		idx = bytes.Index(data, sep)
	}
	var head, body []byte
	if idx < 0 {
		head = data
	} else {
		head, body = data[:idx], data[idx+len(sep):]
	}
	lines := strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, errors.New("empty message")
	}
	m := &Msg{}
	first := strings.TrimSpace(lines[0])
	if strings.HasPrefix(first, "SIP/2.0 ") {
		f := strings.SplitN(first, " ", 3)
		code, err := strconv.Atoi(f[1])
		if err != nil {
			return nil, fmt.Errorf("bad status line %q", first)
		}
		m.Status = code
		if len(f) > 2 {
			m.Reason = f[2]
		}
	} else {
		f := strings.Fields(first)
		if len(f) != 3 || f[2] != "SIP/2.0" {
			return nil, fmt.Errorf("bad request line %q", first)
		}
		m.IsRequest, m.Method, m.RURI = true, strings.ToUpper(f[0]), f[1]
	}
	for _, l := range lines[1:] {
		if l == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && len(m.Headers) > 0 { // folded line
			m.Headers[len(m.Headers)-1].Value += " " + strings.TrimSpace(l)
			continue
		}
		c := strings.IndexByte(l, ':')
		if c < 0 {
			continue
		}
		m.Headers = append(m.Headers, Header{canon(l[:c]), strings.TrimSpace(l[c+1:])})
	}
	if cl := m.Get("Content-Length"); cl != "" {
		if n, err := strconv.Atoi(cl); err == nil && n <= len(body) {
			body = body[:n]
		}
	}
	m.Body = body
	return m, nil
}

func (m *Msg) Get(name string) string {
	name = canon(name)
	for _, h := range m.Headers {
		if h.Name == name {
			return h.Value
		}
	}
	return ""
}

func (m *Msg) All(name string) []string {
	name = canon(name)
	var out []string
	for _, h := range m.Headers {
		if h.Name == name {
			out = append(out, h.Value)
		}
	}
	return out
}

func (m *Msg) Set(name, value string) {
	name = canon(name)
	for i, h := range m.Headers {
		if h.Name == name {
			m.Headers[i].Value = value
			return
		}
	}
	m.Headers = append(m.Headers, Header{name, value})
}

func (m *Msg) Add(name, value string) { m.Headers = append(m.Headers, Header{canon(name), value}) }

func (m *Msg) CSeq() (int, string) {
	f := strings.Fields(m.Get("CSeq"))
	if len(f) != 2 {
		return 0, ""
	}
	n, _ := strconv.Atoi(f[0])
	return n, strings.ToUpper(f[1])
}

func (m *Msg) Bytes() []byte {
	var b bytes.Buffer
	if m.IsRequest {
		fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", m.Method, m.RURI)
	} else {
		fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", m.Status, m.Reason)
	}
	for _, h := range m.Headers {
		if h.Name == "Content-Length" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\r\n", h.Name, h.Value)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(m.Body))
	b.Write(m.Body)
	return b.Bytes()
}

// Response строит ответ на запрос: Via/From/To/Call-ID/CSeq копируются, к To добавляется тег.
func (m *Msg) Response(code int, reason, toTag string) *Msg {
	r := &Msg{Status: code, Reason: reason}
	for _, v := range m.All("Via") {
		r.Add("Via", v)
	}
	r.Add("From", m.Get("From"))
	to := m.Get("To")
	if toTag != "" && TagOf(to) == "" && code > 100 {
		to += ";tag=" + toTag
	}
	r.Add("To", to)
	r.Add("Call-ID", m.Get("Call-ID"))
	r.Add("CSeq", m.Get("CSeq"))
	r.Add("Server", "Kontakt/0.1")
	return r
}

// TagOf достаёт ;tag= из From/To.
func TagOf(h string) string { return Param(h, "tag") }

func Param(h, name string) string {
	// параметры заголовка — после '>' (если есть угловые скобки)
	if i := strings.LastIndexByte(h, '>'); i >= 0 {
		h = h[i+1:]
	}
	for _, p := range strings.Split(h, ";") {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 && strings.EqualFold(kv[0], name) {
			return kv[1]
		}
	}
	return ""
}

// URIOf достаёт URI из name-addr: "Bob" <sip:bob@x>;tag=1 → sip:bob@x
func URIOf(h string) string {
	if i := strings.IndexByte(h, '<'); i >= 0 {
		if j := strings.IndexByte(h[i:], '>'); j > 0 {
			return h[i+1 : i+j]
		}
	}
	if i := strings.IndexByte(h, ';'); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSpace(h)
}

// UserOf: sip:16@host → "16"
func UserOf(uri string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(uri, "sips:"), "sip:")
	if i := strings.IndexByte(u, '@'); i >= 0 {
		return u[:i]
	}
	return ""
}

func RandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func NewBranch() string { return "z9hG4bK" + RandHex(8) }
