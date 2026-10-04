package signal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"kontakt/internal/identity"
	"kontakt/internal/moderation"
	"kontakt/internal/sip"
)

// ---------- SIP поверх UDP (обычные софтфоны в локальной сети) ----------

type udpTransport struct {
	srv  *UDPServer
	addr *net.UDPAddr
	id   string
}

func (t *udpTransport) SendSIP(m *sip.Msg) error {
	_, err := t.srv.conn.WriteToUDP(m.Bytes(), t.addr)
	return err
}
func (t *udpTransport) Via() string {
	return fmt.Sprintf("SIP/2.0/UDP %s:%d", t.srv.AdvIP, t.srv.Port)
}
func (t *udpTransport) Contact() string {
	return fmt.Sprintf("<sip:kontakt@%s:%d>", t.srv.AdvIP, t.srv.Port)
}
func (t *udpTransport) Name() string     { return "SIP-телефон" }
func (t *udpTransport) Identity() string { return t.id }

type UDPServer struct {
	AdvIP string
	Port  int
	conn  *net.UDPConn
}

// ServeUDP слушает SIP по UDP. Куки у софтфона нет, поэтому человек опознаётся по
// хешу «IP + имя пользователя из From» — для бана в локальной сети этого достаточно.
func (s *Server) ServeUDP(addr, advIP string) (*UDPServer, error) {
	c, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	u := &UDPServer{AdvIP: advIP, conn: c.(*net.UDPConn)}
	u.Port = u.conn.LocalAddr().(*net.UDPAddr).Port
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := u.conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if strings.TrimSpace(string(buf[:n])) == "" {
				continue // keep-alive
			}
			m, err := sip.ParseMsg(append([]byte(nil), buf[:n]...))
			if err != nil {
				continue
			}
			h := sha256.Sum256([]byte(from.IP.String() + "|" + sip.UserOf(sip.URIOf(m.Get("From")))))
			go s.Handle(m, &udpTransport{srv: u, addr: from, id: "udp" + hex.EncodeToString(h[:14])})
		}
	}()
	return u, nil
}

func (u *UDPServer) Close() error { return u.conn.Close() }

// ---------- SIP поверх WebSocket (RFC 7118) ----------

type wsTransport struct {
	conn *websocket.Conn
	id   string
	wmu  sync.Mutex
}

func (t *wsTransport) SendSIP(m *sip.Msg) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	t.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	return t.conn.WriteMessage(websocket.TextMessage, m.Bytes())
}
func (t *wsTransport) Via() string      { return "SIP/2.0/WSS kontakt.invalid" }
func (t *wsTransport) Contact() string  { return "<sip:kontakt@kontakt.invalid;transport=ws>" }
func (t *wsTransport) Name() string     { return "веб-трубка" }
func (t *wsTransport) Identity() string { return t.id }

var upgrader = websocket.Upgrader{Subprotocols: []string{"sip"}, CheckOrigin: func(*http.Request) bool { return true }}

func (s *Server) ServeWS(w http.ResponseWriter, r *http.Request) {
	id := identity.FromRequest(r)
	if id == "" {
		http.Error(w, "нет куки — откройте страницу заново", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(16 * 1024)
	t := &wsTransport{conn: conn, id: id}
	myCalls := map[string]bool{}
	defer func() { // вкладку закрыли — все её звонки кончились
		for callID := range myCalls {
			s.mu.Lock()
			l := s.legs[callID]
			s.mu.Unlock()
			if l != nil && l.tr == Transport(t) {
				s.End(l, "")
			}
		}
		conn.Close()
	}()
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			continue
		}
		m, err := sip.ParseMsg(data)
		if err != nil {
			continue
		}
		if m.IsRequest && m.Method == "INVITE" {
			myCalls[m.Get("Call-ID")] = true
		}
		s.Handle(m, t)
	}
}

// ---------- HTTP API ----------

func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sip", s.ServeWS)
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Stats()) })
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		c := s.Cfg.Get()
		writeJSON(w, map[string]any{
			"default_line": c.DefaultLine, "allowed_lines": c.AllowedLines, "ban_reporters": c.BanReporters,
			"report_window_sec": c.ReportWindowSec, "maintenance": c.Maintenance, "max_call_minutes": c.MaxCallMinutes,
		})
	})
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		st := s.Mod.Status(identity.FromRequest(r), moderation.Calls)
		writeJSON(w, map[string]any{"banned": st.Banned, "cards": st.Cards})
	})
	return mux
}

// AdminHandler — только для localhost: перезагрузка конфига, статистика, модерация.
func (s *Server) AdminHandler(mediaStats func() (any, error)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		msg, err := s.Cfg.Reload(true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, msg)
	})
	mux.Handle("/metrics", s.m.reg.Handler())
	mux.HandleFunc("/admin/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Cfg.Get()) })
	mux.HandleFunc("/admin/stats", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"signal": s.Stats(), "moderation": s.Mod.Count()}
		if mediaStats != nil {
			if ms, err := mediaStats(); err == nil {
				out["media"] = ms
			} else {
				out["media_error"] = err.Error()
			}
		}
		writeJSON(w, out)
	})
	// Бан, разбан, журнал; бан в calls/all сразу выгоняет со станции, эфир радио снимет само.
	admin := s.Mod.AdminHandler(func(key string, z moderation.Zone) {
		if z != moderation.Air {
			s.kickKey(key)
		}
	})
	for _, p := range []string{"/admin/ban", "/admin/unban", "/admin/journal"} {
		mux.Handle(p, admin)
	}
	// База модерации для радио: оно банит и проверяет эфир по ней же.
	mux.Handle("/mod/", s.Mod.Handler())
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
