// Package signal — сигнальный сервис «Контакта»: SIP (UDP и WebSocket), очередь, жалобы и баны.
// Звук сам не трогает — только командует медиа-сервисом по gRPC.
//
// Как идёт звонок:
//
//	INVITE → сразу 200 OK (трубку «сняли» — вы на станции) → гудки из медиа-сервиса
//	→ нашёлся собеседник: мост, INFO {"state":"talking"}
//	→ собеседник ушёл: 2 с белого шума, затем снова гудки и очередь — тот же SIP-диалог
//	→ вы положили трубку (BYE) — конец.
//
// Так работает и браузер, и любой обычный SIP-софтфон: набрал один раз — и крутишь рулетку.
package signal

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kontakt/internal/config"
	"kontakt/internal/mediaapi"
	"kontakt/internal/moderation"
	"kontakt/internal/sip"
)

// Media — то, что сигналке нужно от медиа-сервиса (реализует *mediaapi.Client).
type Media interface {
	CreateEndpoint(*mediaapi.CreateEndpointRequest) (*mediaapi.Endpoint, error)
	Bridge(a, b string) error
	Delete(id string) error
	SetTone(id, tone string, seconds float64, then string) error
	SetLine(id, mode string) error
}

type Transport interface {
	SendSIP(m *sip.Msg) error
	Via() string
	Contact() string
	Name() string
	Identity() string
}

const (
	legQueued  = iota // ждёт собеседника (слышит гудки или шум)
	legTalking        // в разговоре
	legEnded
)

// Причины — в INFO и в Reason у BYE; по ним веб-трубка показывает, что случилось.
const (
	ReasonPeerLeft   = "peer-left"       // собеседник положил трубку
	ReasonReported   = "report-accepted" // вы пожаловались, вас переключили
	ReasonYellow     = "yellow-card"
	ReasonBanned     = "banned"
	ReasonTimeLimit  = "time-limit"
	ReasonMediaError = "media-error"
)

// NoiseSeconds — сколько звучит «пустой эфир» после ухода собеседника.
const NoiseSeconds = 2.0

type Leg struct {
	callID   string
	tr       Transport
	identity string
	invite   *sip.Msg
	toTag    string
	pt       int32
	ep       *mediaapi.Endpoint

	notifyMu   sync.Mutex // уведомления одной ноге уходят строго по очереди
	mu         sync.Mutex
	line       string
	state      int
	inQueue    bool
	queuedAt   time.Time
	peer       *Leg
	lastPeerID string
	lastResp   *sip.Msg
	acked      atomic.Bool
	cseq       int
	timers     []*time.Timer
}

type pairRec struct {
	peerID  string
	peerLeg *Leg
	since   time.Time
	endedAt time.Time
	report  bool
}

// CountedTalk — разговор не короче этого засчитывается обоим как сессия: так новичок
// набирает доверие (см. moderation.Policy.TrustTalks). Переменная — для тестов.
var CountedTalk = 30 * time.Second

type Server struct {
	Cfg   *config.Live
	Mod   *moderation.Store
	Media Media

	mu       sync.Mutex
	legs     map[string]*Leg
	queue    []*Leg
	lastPeer map[string]*pairRec
	blocked  map[[2]string]bool // пары, которых больше не соединяем (жалоба)
	reports  map[string][]time.Time

	totalCalls atomic.Int64
	m          *stationMetrics
}

func New(cfg *config.Live, mod *moderation.Store, media Media) *Server {
	s := &Server{Cfg: cfg, Mod: mod, Media: media, legs: map[string]*Leg{},
		lastPeer: map[string]*pairRec{}, blocked: map[[2]string]bool{}, reports: map[string][]time.Time{}}
	s.initMetrics()
	// Пороги модерации — из горячего конфига; по той же базе банит и радио (через /mod/*).
	mod.SetPolicy(PolicyOf(cfg.Get()))
	prev := cfg.OnApply
	cfg.OnApply = func(old, c config.Config) {
		if prev != nil {
			prev(old, c)
		}
		mod.SetPolicy(PolicyOf(c))
	}
	return s
}

func PolicyOf(c config.Config) moderation.Policy {
	return moderation.Policy{Reporters: c.BanReporters, WindowDays: c.BanWindowDays, TrustTalks: c.TrustTalks}
}

// ---------- входящие SIP-запросы ----------

func (s *Server) Handle(m *sip.Msg, tr Transport) {
	if !m.IsRequest {
		return // ответы на наши INFO/BYE
	}
	s.mu.Lock()
	leg := s.legs[m.Get("Call-ID")]
	s.mu.Unlock()

	switch m.Method {
	case "REGISTER":
		r := m.Response(200, "OK", sip.RandHex(4))
		if c := m.Get("Contact"); c != "" {
			r.Add("Contact", c)
		}
		r.Add("Expires", orDefault(m.Get("Expires"), "3600"))
		tr.SendSIP(r)
	case "OPTIONS":
		r := m.Response(200, "OK", sip.RandHex(4))
		r.Add("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO, REGISTER")
		tr.SendSIP(r)
	case "INVITE":
		if leg == nil {
			s.newCall(m, tr)
			return
		}
		if sip.TagOf(m.Get("To")) == "" { // ретрансмит
			leg.mu.Lock()
			r := leg.lastResp
			leg.mu.Unlock()
			if r != nil {
				tr.SendSIP(r)
			}
			return
		}
		r := m.Response(200, "OK", leg.toTag) // re-INVITE (hold и т.п.) — тот же SDP
		r.Add("Contact", tr.Contact())
		r.Add("Content-Type", "application/sdp")
		r.Body = leg.sdp()
		tr.SendSIP(r)
	case "ACK":
		if leg != nil {
			leg.acked.Store(true)
		}
	case "INFO":
		if leg == nil {
			tr.SendSIP(m.Response(481, "Call/Transaction Does Not Exist", ""))
			return
		}
		s.onInfo(leg, m)
	case "CANCEL":
		tr.SendSIP(m.Response(200, "OK", ""))
		if leg != nil {
			s.End(leg, "")
		}
	case "BYE":
		tr.SendSIP(m.Response(200, "OK", ""))
		if leg != nil {
			s.End(leg, "")
		}
	default:
		tr.SendSIP(m.Response(501, "Not Implemented", ""))
	}
}

func (s *Server) reject(inv *sip.Msg, tr Transport, code int, reason, text string) {
	s.m.calls.Inc(text)
	r := inv.Response(code, reason, sip.RandHex(6))
	r.Add("Reason", fmt.Sprintf(`SIP;cause=%d;text="%s"`, code, text))
	if code == 503 {
		r.Add("Retry-After", "30")
	}
	tr.SendSIP(r)
}

func (s *Server) newCall(inv *sip.Msg, tr Transport) {
	cfg := s.Cfg.Get()
	tr.SendSIP(inv.Response(100, "Trying", ""))

	id := tr.Identity()
	switch {
	case id == "":
		s.reject(inv, tr, 403, "Forbidden", "no-identity")
		return
	case s.Mod.Banned(id, moderation.Calls):
		s.reject(inv, tr, 403, "Forbidden", ReasonBanned)
		return
	case cfg.Maintenance:
		s.reject(inv, tr, 503, "Service Unavailable", "maintenance")
		return
	}

	sdp := parseSDP(inv.Body)
	var pt int32 = -1
	if sdp.hasPT(8) {
		pt = 8
	} else if sdp.hasPT(0) {
		pt = 0
	}
	if pt < 0 {
		s.reject(inv, tr, 488, "Not Acceptable Here", "only-pcma-pcmu")
		return
	}

	leg := &Leg{callID: inv.Get("Call-ID"), tr: tr, identity: id, invite: inv, toTag: sip.RandHex(6),
		line: s.pickLine(sip.UserOf(sip.URIOf(inv.RURI))), pt: pt, cseq: 1}

	req := &mediaapi.CreateEndpointRequest{CallID: leg.callID, Transport: "udp", PayloadType: pt,
		LineMode: leg.line, RemoteIP: sdp.ip, RemotePort: int32(sdp.port)}
	if _, isWS := tr.(*wsTransport); isWS {
		req.Transport, req.RemoteIP, req.RemotePort = "ws", "", 0
	}
	ep, err := s.Media.CreateEndpoint(req)
	if err != nil {
		log.Printf("media: %v", err)
		s.reject(inv, tr, 503, "Service Unavailable", ReasonMediaError)
		return
	}
	leg.ep = ep

	s.mu.Lock()
	s.legs[leg.callID] = leg
	s.mu.Unlock()

	// Трубку сняли — отвечаем сразу, дальше вы «на станции».
	r := inv.Response(200, "OK", leg.toTag)
	r.Add("Contact", tr.Contact())
	r.Add("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO")
	r.Add("Content-Type", "application/sdp")
	r.Body = leg.sdp()
	leg.mu.Lock()
	leg.lastResp = r
	leg.mu.Unlock()
	tr.SendSIP(r)
	if _, isUDP := tr.(*udpTransport); isUDP {
		go leg.retransmit200(r)
	}

	s.m.calls.Inc("accepted")
	log.Printf("☎  %s снял трубку (линия %s)", tr.Name(), leg.line)
	s.Media.SetTone(ep.ID, "ring", 0, "")
	s.enqueue(leg, "")
}

func (s *Server) pickLine(want string) string {
	cfg := s.Cfg.Get()
	for _, l := range cfg.AllowedLines {
		if l == want {
			return want
		}
	}
	return cfg.DefaultLine
}

func (l *Leg) retransmit200(r *sip.Msg) {
	for d := 500 * time.Millisecond; d <= 4*time.Second; d *= 2 {
		time.Sleep(d)
		l.mu.Lock()
		ended := l.state == legEnded
		l.mu.Unlock()
		if l.acked.Load() || ended {
			return
		}
		l.tr.SendSIP(r)
	}
}

func (l *Leg) sdp() []byte {
	name := "PCMA"
	if l.pt == 0 {
		name = "PCMU"
	}
	ip, port, extra := l.ep.LocalIP, l.ep.LocalPort, ""
	if l.ep.Transport == "ws" {
		ip, port, extra = "0.0.0.0", 9, "a=x-kontakt-media:ws "+l.ep.WSPath+"\r\n"
	}
	return []byte(fmt.Sprintf("v=0\r\no=kontakt %d 1 IN IP4 %s\r\ns=Kontakt\r\nc=IN IP4 %s\r\nt=0 0\r\n"+
		"m=audio %d RTP/AVP %d\r\na=rtpmap:%d %s/8000\r\na=ptime:20\r\na=sendrecv\r\n%s",
		time.Now().Unix(), ip, ip, port, l.pt, l.pt, name, extra))
}

// ---------- очередь и коммутация ----------

// enqueue ставит ногу в очередь (она уже слышит гудки) и пробует кого-нибудь соединить.
func (s *Server) enqueue(l *Leg, reason string) {
	l.mu.Lock()
	if l.state == legEnded {
		l.mu.Unlock()
		return
	}
	l.state, l.inQueue, l.queuedAt, l.peer = legQueued, true, time.Now(), nil
	l.mu.Unlock()
	s.mu.Lock()
	s.queue = append(s.queue, l)
	s.mu.Unlock()
	s.notify(l, reason)
	s.match()
}

// requeue: собеседник ушёл → белый шум → гудки → снова в очередь.
func (s *Server) requeue(l *Leg, reason string) {
	l.mu.Lock()
	if l.state == legEnded {
		l.mu.Unlock()
		return
	}
	l.state, l.inQueue, l.peer = legQueued, false, nil
	l.mu.Unlock()
	s.Media.SetTone(l.ep.ID, "noise", NoiseSeconds, "ring")
	s.notify(l, reason)
	t := time.AfterFunc(time.Duration(NoiseSeconds*float64(time.Second)), func() {
		l.mu.Lock()
		ok := l.state == legQueued && !l.inQueue
		l.mu.Unlock()
		if ok {
			s.enqueue(l, "")
		}
	})
	l.mu.Lock()
	l.timers = append(l.timers, t)
	l.mu.Unlock()
}

// match соединяет пары из очереди, пока это возможно.
func (s *Server) match() {
	for {
		a, b := s.pickPair()
		if a == nil {
			return
		}
		s.connect(a, b)
	}
}

func (s *Server) pickPair() (*Leg, *Leg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// выкидываем мёртвых
	live := s.queue[:0]
	for _, l := range s.queue {
		l.mu.Lock()
		ok := l.state == legQueued && l.inQueue
		l.mu.Unlock()
		if ok {
			live = append(live, l)
		}
	}
	s.queue = live
	for i := 0; i < len(s.queue); i++ {
		for j := i + 1; j < len(s.queue); j++ {
			a, b := s.queue[i], s.queue[j]
			if s.compatible(a, b) {
				s.queue = append(s.queue[:j], s.queue[j+1:]...)
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				return a, b
			}
		}
	}
	return nil, nil
}

// compatible: не сам с собой, не после жалобы, и не тот же собеседник сразу же
// (разрешаем, только если оба ждут дольше 15 с — значит, больше никого нет).
func (s *Server) compatible(a, b *Leg) bool {
	if a.identity == b.identity || s.blocked[[2]string{a.identity, b.identity}] || s.blocked[[2]string{b.identity, a.identity}] {
		return false
	}
	a.mu.Lock()
	aLast, aWait := a.lastPeerID, time.Since(a.queuedAt)
	a.mu.Unlock()
	b.mu.Lock()
	bLast, bWait := b.lastPeerID, time.Since(b.queuedAt)
	b.mu.Unlock()
	if (aLast == b.identity || bLast == a.identity) && (aWait < 15*time.Second || bWait < 15*time.Second) {
		return false
	}
	return true
}

func (s *Server) connect(a, b *Leg) {
	if err := s.Media.Bridge(a.ep.ID, b.ep.ID); err != nil {
		log.Printf("media bridge: %v", err)
		s.End(a, ReasonMediaError)
		s.End(b, ReasonMediaError)
		return
	}
	a.mu.Lock()
	aWait := time.Since(a.queuedAt)
	a.state, a.inQueue, a.peer, a.lastPeerID = legTalking, false, b, b.identity
	a.mu.Unlock()
	b.mu.Lock()
	bWait := time.Since(b.queuedAt)
	b.state, b.inQueue, b.peer, b.lastPeerID = legTalking, false, a, a.identity
	b.mu.Unlock()
	s.observeWait(aWait)
	s.observeWait(bWait)
	s.m.pairs.Inc()

	s.mu.Lock()
	now := time.Now()
	s.lastPeer[a.identity] = &pairRec{peerID: b.identity, peerLeg: b, since: now}
	s.lastPeer[b.identity] = &pairRec{peerID: a.identity, peerLeg: a, since: now}
	s.mu.Unlock()

	if mins := s.Cfg.Get().MaxCallMinutes; mins > 0 {
		t := time.AfterFunc(time.Duration(mins)*time.Minute, func() { s.split(a, b, ReasonTimeLimit, ReasonTimeLimit) })
		a.mu.Lock()
		a.timers = append(a.timers, t)
		a.mu.Unlock()
	}
	s.totalCalls.Add(1)
	log.Printf("☎☎ соединены: %s (%s) ↔ %s (%s)", a.tr.Name(), a.line, b.tr.Name(), b.line)
	s.notify(a, "")
	s.notify(b, "")
}

// split разводит пару, если она ещё вместе, и отправляет обоих искать дальше.
func (s *Server) split(a, b *Leg, reasonA, reasonB string) {
	a.mu.Lock()
	together := a.peer == b && a.state == legTalking
	a.mu.Unlock()
	if !together {
		return
	}
	s.markEnded(a, b)
	s.requeue(a, reasonA)
	s.requeue(b, reasonB)
}

// markEnded отмечает конец разговора (один раз, сколько бы горутин его ни заметили) и
// засчитывает его обоим, если он был не короче CountedTalk.
func (s *Server) markEnded(a, b *Leg) {
	s.mu.Lock()
	now := time.Now()
	var counted []string
	for _, p := range [][2]*Leg{{a, b}, {b, a}} {
		if r := s.lastPeer[p[0].identity]; r != nil && r.peerLeg == p[1] && r.endedAt.IsZero() {
			r.endedAt = now
			if now.Sub(r.since) >= CountedTalk {
				counted = append(counted, p[0].identity)
			}
		}
	}
	s.mu.Unlock()
	for _, id := range counted {
		if err := s.Mod.Seen(id); err != nil {
			log.Printf("moderation: не удалось сохранить: %v", err)
		}
	}
}

// End — нога уходит со станции. byeReason != "" — её выгоняет станция (послать BYE с причиной);
// "" — абонент сам положил трубку. Собеседник получает шум и идёт искать дальше.
func (s *Server) End(l *Leg, byeReason string) {
	l.mu.Lock()
	if l.state == legEnded {
		l.mu.Unlock()
		return
	}
	l.state = legEnded
	peer := l.peer
	l.peer = nil
	for _, t := range l.timers {
		t.Stop()
	}
	l.mu.Unlock()

	s.mu.Lock()
	delete(s.legs, l.callID)
	s.mu.Unlock()
	if peer != nil {
		s.markEnded(l, peer)
	}

	if byeReason != "" {
		l.sendRequest("BYE", "", "", byeReason)
	}
	s.Media.Delete(l.ep.ID)
	if byeReason == "" {
		s.m.hangups.Inc("user")
	} else {
		s.m.hangups.Inc(byeReason)
	}
	log.Printf("☎  %s положил трубку", l.tr.Name())

	if peer != nil {
		peer.mu.Lock()
		stillMine := peer.peer == l
		peer.mu.Unlock()
		if stillMine {
			reason := ReasonPeerLeft
			if byeReason == ReasonYellow || byeReason == ReasonBanned {
				reason = ReasonReported
			}
			s.requeue(peer, reason)
		}
	}
}

// ---------- INFO: состояние станции ↔ трубка ----------

// notify сообщает трубке её ТЕКУЩЕЕ состояние (а не то, что было, когда решили сообщить):
// если два уведомления разминутся между горутинами, последнее всё равно окажется верным.
func (s *Server) notify(l *Leg, reason string) {
	l.notifyMu.Lock()
	defer l.notifyMu.Unlock()
	l.mu.Lock()
	st := l.state
	l.mu.Unlock()
	state := "searching"
	switch st {
	case legEnded:
		return
	case legTalking:
		state, reason = "talking", ""
	}
	body, _ := json.Marshal(map[string]string{"state": state, "reason": reason})
	l.sendRequest("INFO", "application/json", string(body), "")
}

func (l *Leg) sendRequest(method, ctype, body, reason string) {
	l.mu.Lock()
	l.cseq++
	cseq := l.cseq
	l.mu.Unlock()
	inv := l.invite
	target := sip.URIOf(inv.Get("Contact"))
	if target == "" {
		target = sip.URIOf(inv.Get("From"))
	}
	m := &sip.Msg{IsRequest: true, Method: method, RURI: target}
	m.Add("Via", l.tr.Via()+";branch="+sip.NewBranch()+";rport")
	m.Add("Max-Forwards", "70")
	m.Add("From", inv.Get("To")+";tag="+l.toTag)
	m.Add("To", inv.Get("From"))
	m.Add("Call-ID", l.callID)
	m.Add("CSeq", fmt.Sprintf("%d %s", cseq, method))
	if reason != "" {
		m.Add("Reason", fmt.Sprintf(`SIP;cause=200;text="%s"`, reason))
	}
	if ctype != "" {
		m.Add("Content-Type", ctype)
		m.Body = []byte(body)
	}
	l.tr.SendSIP(m)
}

type infoReq struct {
	Report bool   `json:"report"`
	Line   string `json:"line"`
}

func (s *Server) onInfo(l *Leg, m *sip.Msg) {
	var req infoReq
	json.Unmarshal(m.Body, &req) // INFO без JSON (например, DTMF) просто подтверждаем
	var resp any = map[string]string{"result": "ok"}
	switch {
	case req.Report:
		res, _ := s.Report(l)
		resp = res
	case req.Line != "":
		line := s.pickLine(req.Line)
		l.mu.Lock()
		l.line = line
		l.mu.Unlock()
		s.Media.SetLine(l.ep.ID, line)
		resp = map[string]string{"result": "ok", "line": line}
	}
	r := m.Response(200, "OK", l.toTag)
	r.Add("Content-Type", "application/json")
	r.Body, _ = json.Marshal(resp)
	l.tr.SendSIP(r)
}

// ---------- жалобы ----------

type ReportResult struct {
	// yellow | banned | noted (жалоба новичка: развели, в зачёт не пошла) | already |
	// no_recent_call | rate_limited
	Result string `json:"result"`
}

// Report — абонент ноги r жалуется на своего текущего или последнего собеседника.
func (s *Server) Report(r *Leg) (ReportResult, error) {
	cfg := s.Cfg.Get()
	var res ReportResult
	defer func() { s.m.reports.Inc(res.Result) }()

	s.mu.Lock()
	rec := s.lastPeer[r.identity]
	window := time.Duration(cfg.ReportWindowSec) * time.Second
	switch {
	case rec == nil || (!rec.endedAt.IsZero() && time.Since(rec.endedAt) > window):
		res.Result = "no_recent_call"
	case rec.report:
		res.Result = "already"
	}
	if res.Result != "" {
		s.mu.Unlock()
		return res, nil
	}
	now := time.Now()
	recent := s.reports[r.identity][:0]
	for _, t := range s.reports[r.identity] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if cfg.MaxReportsPerHour > 0 && len(recent) >= cfg.MaxReportsPerHour {
		s.reports[r.identity] = recent
		s.mu.Unlock()
		res.Result = "rate_limited"
		return res, nil
	}
	s.reports[r.identity] = append(recent, now)
	rec.report = true
	s.blocked[[2]string{r.identity, rec.peerID}] = true
	target, targetLeg := rec.peerID, rec.peerLeg
	s.mu.Unlock()

	result, err := s.Mod.Report(target, r.identity, moderation.Calls)
	if err != nil {
		log.Printf("moderation: не удалось сохранить: %v", err)
	}
	res.Result = result
	log.Printf("⚑  жалоба принята: «%s»", result)

	switch result {
	case moderation.ResultBanned:
		s.kick(target) // со станции — отовсюду
	case moderation.ResultNoted:
		s.split(r, targetLeg, ReasonReported, ReasonPeerLeft) // без карточки: просто развести
	default:
		targetLeg.mu.Lock()
		withReporter := targetLeg.peer == r && targetLeg.state == legTalking
		targetLeg.mu.Unlock()
		if withReporter {
			s.End(targetLeg, ReasonYellow) // нарушитель уходит с предупреждением, жалобщик ищет дальше
		}
	}
	return res, err
}

func (s *Server) kick(identity string) { s.kickKey(moderation.Key(identity)) }

// kickKey выгоняет со станции все ноги человека с ключом записи key (бан из админки — по ключу).
func (s *Server) kickKey(key string) {
	s.mu.Lock()
	var mine []*Leg
	for _, l := range s.legs {
		if moderation.Key(l.identity) == key {
			mine = append(mine, l)
		}
	}
	s.mu.Unlock()
	for _, l := range mine {
		s.End(l, ReasonBanned)
	}
}

// ---------- статистика ----------

type Stats struct {
	Online  int   `json:"online"`
	Waiting int   `json:"waiting"`
	Talking int   `json:"talking"`
	Total   int64 `json:"total_calls"`
}

func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Online: len(s.legs), Total: s.totalCalls.Load()}
	for _, l := range s.legs {
		l.mu.Lock()
		if l.state == legTalking {
			st.Talking++
		} else if l.state == legQueued {
			st.Waiting++
		}
		l.mu.Unlock()
	}
	return st
}

// ---------- SDP ----------

type sdpInfo struct {
	ip   string
	port int
	pts  []int
}

func (s sdpInfo) hasPT(pt int) bool {
	for _, p := range s.pts {
		if p == pt {
			return true
		}
	}
	return false
}

func parseSDP(b []byte) sdpInfo {
	var out sdpInfo
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "c="):
			if f := strings.Fields(line[2:]); len(f) == 3 {
				out.ip = f[2]
			}
		case strings.HasPrefix(line, "m=audio"):
			if f := strings.Fields(line[2:]); len(f) >= 3 {
				fmt.Sscan(f[1], &out.port)
				for _, p := range f[3:] {
					var n int
					if _, err := fmt.Sscan(p, &n); err == nil {
						out.pts = append(out.pts, n)
					}
				}
			}
		}
	}
	return out
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
