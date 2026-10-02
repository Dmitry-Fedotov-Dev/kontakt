// Package media — медиа-движок: RTP-точки (UDP или WebSocket), мост между ними, эффект линии
// и служебные звуки станции (гудки, белый шум). Ничего не знает про SIP: им управляют
// только через gRPC (mediaapi).
package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"kontakt/internal/dsp"
	"kontakt/internal/mediaapi"
	"kontakt/internal/sip"
)

const frame = 160 // 20 мс при 8 кГц

type Engine struct {
	advIP          string
	rtpMin, rtpMax int

	mu      sync.Mutex
	eps     map[string]*endpoint
	byToken map[string]*endpoint
	rtpNext int

	pktIn, pktOut atomic.Uint64
	stop          chan struct{}
	wake          chan struct{} // будит тикер служебных звуков, когда появилась первая точка
}

func NewEngine(advIP string, rtpMin, rtpMax int) *Engine {
	e := &Engine{advIP: advIP, rtpMin: rtpMin, rtpMax: rtpMax, rtpNext: rtpMin,
		eps: map[string]*endpoint{}, byToken: map[string]*endpoint{}, stop: make(chan struct{}), wake: make(chan struct{}, 1)}
	go e.toneLoop()
	return e
}

func (e *Engine) Close() { close(e.stop) }

type endpoint struct {
	e         *Engine
	id, token string
	transport string
	pt        byte

	// fxMu защищает эффект линии и генератор: их трогает либо горутина собеседника (мост),
	// либо тикер служебных звуков.
	fxMu     sync.Mutex
	rx       *dsp.Line
	tone     dsp.ToneGen
	toneFor  float64
	toneThen dsp.Tone
	pcm      [frame * 10]int16 // буфер под декодирование, чтобы не аллоцировать на каждый пакет

	peer atomic.Pointer[endpoint]

	udp       *net.UDPConn
	port      int
	udpRemote atomic.Pointer[net.UDPAddr]

	wsMu sync.Mutex
	ws   *websocket.Conn

	sendMu sync.Mutex
	seq    uint16
	ts     uint32
	ssrc   uint32
	marker bool
	out    [12 + frame*10]byte

	closed atomic.Bool
}

// ---------- gRPC ----------

func (e *Engine) CreateEndpoint(_ context.Context, r *mediaapi.CreateEndpointRequest) (*mediaapi.Endpoint, error) {
	if r.PayloadType != 0 && r.PayloadType != 8 {
		return nil, status.Error(codes.InvalidArgument, "только PCMA/PCMU")
	}
	mode, ok := dsp.ParseLineMode(r.LineMode)
	if !ok {
		mode = dsp.Mode32
	}
	ep := &endpoint{e: e, id: sip.RandHex(8), transport: r.Transport, pt: byte(r.PayloadType),
		rx: dsp.NewLine(mode), seq: uint16(rand.Uint32()), ts: rand.Uint32(), ssrc: rand.Uint32(), marker: true}
	out := &mediaapi.Endpoint{ID: ep.id, Transport: r.Transport}

	switch r.Transport {
	case "udp":
		if err := e.openUDP(ep); err != nil {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		if r.RemoteIP != "" && r.RemoteIP != "0.0.0.0" && r.RemotePort > 0 {
			if a, err := net.ResolveUDPAddr("udp", net.JoinHostPort(r.RemoteIP, fmt.Sprint(r.RemotePort))); err == nil {
				ep.udpRemote.Store(a)
			}
		}
		out.LocalIP, out.LocalPort = e.advIP, int32(ep.port)
	case "ws":
		ep.token = sip.RandHex(16)
		out.WSPath = "media?ep=" + ep.token
	default:
		return nil, status.Error(codes.InvalidArgument, "transport: udp | ws")
	}

	e.mu.Lock()
	e.eps[ep.id] = ep
	if ep.token != "" {
		e.byToken[ep.token] = ep
	}
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return out, nil
}

func (e *Engine) get(id string) (*endpoint, error) {
	e.mu.Lock()
	ep := e.eps[id]
	e.mu.Unlock()
	if ep == nil {
		return nil, status.Error(codes.NotFound, "нет такой точки")
	}
	return ep, nil
}

func (e *Engine) Bridge(_ context.Context, r *mediaapi.BridgeRequest) (*mediaapi.Empty, error) {
	a, err := e.get(r.A)
	if err != nil {
		return nil, err
	}
	b, err := e.get(r.B)
	if err != nil {
		return nil, err
	}
	for _, ep := range []*endpoint{a, b} {
		ep.fxMu.Lock()
		ep.tone.Set(dsp.ToneNone)
		ep.toneFor = 0
		ep.fxMu.Unlock()
	}
	a.peer.Store(b)
	b.peer.Store(a)
	return &mediaapi.Empty{}, nil
}

func (e *Engine) Delete(_ context.Context, r *mediaapi.DeleteRequest) (*mediaapi.Empty, error) {
	e.mu.Lock()
	ep := e.eps[r.ID]
	delete(e.eps, r.ID)
	if ep != nil && ep.token != "" {
		delete(e.byToken, ep.token)
	}
	e.mu.Unlock()
	if ep != nil {
		ep.close()
	}
	return &mediaapi.Empty{}, nil
}

func (e *Engine) SetTone(_ context.Context, r *mediaapi.SetToneRequest) (*mediaapi.Empty, error) {
	ep, err := e.get(r.ID)
	if err != nil {
		return nil, err
	}
	// служебный звук имеет смысл только для точки без собеседника — отвязываем
	if p := ep.peer.Swap(nil); p != nil {
		p.peer.CompareAndSwap(ep, nil)
	}
	ep.fxMu.Lock()
	ep.tone.Set(dsp.ParseTone(r.Tone))
	ep.toneFor, ep.toneThen = r.Seconds, dsp.ParseTone(r.Then)
	ep.fxMu.Unlock()
	return &mediaapi.Empty{}, nil
}

func (e *Engine) SetLine(_ context.Context, r *mediaapi.SetLineRequest) (*mediaapi.Empty, error) {
	ep, err := e.get(r.ID)
	if err != nil {
		return nil, err
	}
	mode, ok := dsp.ParseLineMode(r.LineMode)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "line_mode: 64 | 32 | 16 | 8 | clean")
	}
	ep.fxMu.Lock()
	ep.rx = dsp.NewLine(mode)
	ep.fxMu.Unlock()
	return &mediaapi.Empty{}, nil
}

func (e *Engine) Stats(context.Context, *mediaapi.Empty) (*mediaapi.StatsReply, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := &mediaapi.StatsReply{Endpoints: int32(len(e.eps)), PacketsIn: e.pktIn.Load(), PacketsOut: e.pktOut.Load()}
	for _, ep := range e.eps {
		if ep.peer.Load() != nil {
			st.Bridges++
		}
	}
	st.Bridges /= 2
	return st, nil
}

func (ep *endpoint) close() {
	if ep.closed.Swap(true) {
		return
	}
	if p := ep.peer.Swap(nil); p != nil {
		p.peer.CompareAndSwap(ep, nil)
	}
	if ep.udp != nil {
		ep.udp.Close()
	}
	ep.wsMu.Lock()
	if ep.ws != nil {
		ep.ws.Close()
	}
	ep.wsMu.Unlock()
}

// ---------- служебные звуки ----------

// toneLoop раз в 20 мс шлёт кадр гудков/шума всем точкам без собеседника.
// Один тикер на весь сервис, а не горутина на каждую точку; пока станция пуста — спит.
func (e *Engine) toneLoop() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var list []*endpoint
	var buf [frame]int16
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
		}
		list = list[:0]
		e.mu.Lock()
		for _, ep := range e.eps {
			list = append(list, ep)
		}
		e.mu.Unlock()
		if len(list) == 0 {
			t.Stop()
			select {
			case <-e.stop:
				return
			case <-e.wake:
			}
			t.Reset(20 * time.Millisecond)
			continue
		}
		for _, ep := range list {
			if ep.peer.Load() != nil || ep.closed.Load() {
				continue
			}
			ep.fxMu.Lock()
			if ep.tone.Kind == dsp.ToneNone {
				ep.fxMu.Unlock()
				continue
			}
			ep.tone.Fill(buf[:])
			if ep.toneFor > 0 && ep.tone.Elapsed() >= ep.toneFor {
				ep.tone.Set(ep.toneThen)
				ep.toneFor = 0
			}
			ep.rx.Process(buf[:])
			ep.fxMu.Unlock()
			ep.send(buf[:])
		}
	}
}

// ---------- UDP RTP ----------

func (e *Engine) openUDP(ep *endpoint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := 0; i <= (e.rtpMax-e.rtpMin)/2; i++ {
		port := e.rtpNext
		e.rtpNext += 2
		if e.rtpNext > e.rtpMax {
			e.rtpNext = e.rtpMin
		}
		c, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if err != nil {
			continue
		}
		ep.udp, ep.port = c, port
		go ep.readUDP()
		return nil
	}
	return fmt.Errorf("нет свободных RTP-портов в %d-%d", e.rtpMin, e.rtpMax)
}

func (ep *endpoint) readUDP() {
	buf := make([]byte, 2048)
	for {
		n, from, err := ep.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if cur := ep.udpRemote.Load(); cur == nil || !cur.IP.Equal(from.IP) || cur.Port != from.Port {
			ep.udpRemote.Store(from) // симметричный RTP — спасает за NAT
		}
		ep.onRTP(buf[:n])
	}
}

// ---------- WebSocket-медиа ----------
// Браузер открывает /media?ep=<токен> и шлёт RTP-пакеты бинарными кадрами.

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (e *Engine) ServeWS(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	ep := e.byToken[r.URL.Query().Get("ep")]
	e.mu.Unlock()
	if ep == nil || ep.closed.Load() {
		http.Error(w, "нет такой медиа-точки", http.StatusNotFound)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(4096)
	ep.wsMu.Lock()
	if ep.ws != nil {
		ep.ws.Close() // переподключение — старое соединение выкидываем
	}
	ep.ws = conn
	ep.wsMu.Unlock()
	if ep.closed.Load() {
		conn.Close()
		return
	}
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if kind == websocket.BinaryMessage {
			ep.onRTP(data)
		}
	}
	ep.wsMu.Lock()
	if ep.ws == conn {
		ep.ws = nil
	}
	ep.wsMu.Unlock()
	conn.Close()
}

// ---------- мост ----------

// onRTP: пакет от этой точки → декодирование → эффект линии собеседника → собеседнику.
// Вызывается только из горутины чтения этой точки.
func (ep *endpoint) onRTP(pkt []byte) {
	ep.e.pktIn.Add(1)
	payload, pt, ok := ParseRTP(pkt)
	if !ok || (pt != 0 && pt != 8) || len(payload) == 0 || len(payload) > len(ep.pcm) {
		return
	}
	peer := ep.peer.Load()
	if peer == nil {
		return
	}
	pcm := ep.pcm[:len(payload)]
	dsp.Decode(pt, payload, pcm)
	peer.fxMu.Lock()
	if peer.peer.Load() == ep { // пока ждали замок, могли перекоммутировать
		peer.rx.Process(pcm)
		peer.fxMu.Unlock()
		peer.send(pcm)
		return
	}
	peer.fxMu.Unlock()
}

func (ep *endpoint) send(pcm []int16) {
	ep.sendMu.Lock()
	defer ep.sendMu.Unlock()
	pkt := ep.out[:12+len(pcm)]
	pkt[0], pkt[1] = 0x80, ep.pt
	if ep.marker {
		pkt[1] |= 0x80
		ep.marker = false
	}
	binary.BigEndian.PutUint16(pkt[2:], ep.seq)
	binary.BigEndian.PutUint32(pkt[4:], ep.ts)
	binary.BigEndian.PutUint32(pkt[8:], ep.ssrc)
	ep.seq++
	ep.ts += uint32(len(pcm))
	dsp.Encode(ep.pt, pcm, pkt[12:])

	if ep.udp != nil {
		if dst := ep.udpRemote.Load(); dst != nil {
			ep.udp.WriteToUDP(pkt, dst)
			ep.e.pktOut.Add(1)
		}
		return
	}
	ep.wsMu.Lock()
	if ep.ws != nil {
		ep.ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if ep.ws.WriteMessage(websocket.BinaryMessage, pkt) == nil {
			ep.e.pktOut.Add(1)
		}
	}
	ep.wsMu.Unlock()
}

func ParseRTP(p []byte) (payload []byte, pt byte, ok bool) {
	if len(p) < 12 || p[0]>>6 != 2 {
		return nil, 0, false
	}
	off := 12 + 4*int(p[0]&0x0F)
	if p[0]&0x10 != 0 {
		if len(p) < off+4 {
			return nil, 0, false
		}
		off += 4 + 4*int(binary.BigEndian.Uint16(p[off+2:]))
	}
	end := len(p)
	if p[0]&0x20 != 0 {
		end -= int(p[end-1])
	}
	if off > end {
		return nil, 0, false
	}
	return p[off:end], p[1] & 0x7F, true
}
