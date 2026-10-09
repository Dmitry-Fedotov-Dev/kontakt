package radio

// Эфир по UDP (docs/UDP_BROADCAST.md). Звук ведущего — RTP PCMU (G.711 μ-law, 20 мс = наш кадр
// в 160 байт) по WebRTC: браузер сам кладёт его в пакеты, потерянное не ждётся. Сигналинг — по уже
// открытому /ws/host: {"rtc":{"offer":…}} → {"rtc":{"answer":…}}. Сервер — ICE-lite с одним
// UDP-портом на всех (UDPMux): в файрволе один порт, а не диапазон.
//
// Путь звука выбирает страница: {"audio":"rtc"} / {"audio":"ws"}. Сообщения /ws/host идут по
// порядку, так что последний кадр WebSocket приходит раньше команды — смена без дубля. Кадры
// не активного пути не раздаются, но RTP считается всегда: по счёту «Авто» решает, переходить ли.

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

const (
	rtcGather     = 2 * time.Second     // ICE-lite: кандидаты свои, собираются мгновенно — это предел
	rtcOfferEvery = 2 * time.Second     // не чаще: каждое предложение — рукопожатие DTLS
	rtcReport     = 2 * time.Second     // как часто ведущему уходит счёт пакетов
	rtcMaxOffer   = 16 << 10            // SDP браузера — пара килобайт
	rtcIdle       = 30 * time.Second    // ICE: тишина дольше — соединение мертво
	rtcKeepalive  = 2 * time.Second     // ICE: проверки связи
	rtcDisconnect = 5 * time.Second     // ICE: «disconnected» после стольких без ответа
	pcmuMime      = webrtc.MimeTypePCMU // единственный кодек: формат эфира и так μ-law
	pcmuRate      = 8000
	rtcAudioRTC   = "rtc" // путь звука — RTP (имена — те же, что в странице)
	rtcAudioWS    = "ws"  // путь звука — WebSocket
)

// rtcServer — общий для всех эфиров WebRTC: кодеки, UDP-порт, настройки ICE.
type rtcServer struct {
	api  *webrtc.API
	conn net.PacketConn
	mux  interface{ Close() error }
}

// newRTCServer слушает UDP addr (":443"); ips — публичные адреса для кандидатов (за NAT; пусто —
// адреса интерфейсов: на сервере с белым адресом этого хватает).
func newRTCServer(addr string, ips []string) (*rtcServer, error) {
	conn, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return nil, err
	}
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: pcmuMime, ClockRate: pcmuRate, Channels: 1},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		conn.Close()
		return nil, err
	}
	// Только отчёты RTCP (потери и джиттер видны браузеру в getStats). NACK не нужен: опоздавший
	// звук бесполезен, переспрашивать его — только лишний трафик.
	ir := &interceptor.Registry{}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		conn.Close()
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICETimeouts(rtcDisconnect, rtcIdle, rtcKeepalive)
	se.SetIncludeLoopbackCandidate(true) // тесты и локальный стенд
	if len(ips) > 0 {
		se.SetNAT1To1IPs(ips, webrtc.ICECandidateTypeHost)
	}
	mux := webrtc.NewICEUDPMux(nil, conn)
	se.SetICEUDPMux(mux)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se), webrtc.WithInterceptorRegistry(ir))
	return &rtcServer{api: api, conn: conn, mux: mux}, nil
}

func (r *rtcServer) Close() {
	r.mux.Close()
	r.conn.Close()
}

// Port — UDP-порт, на котором слушает (для тестов с ":0").
func (r *rtcServer) Port() int { return r.conn.LocalAddr().(*net.UDPAddr).Port }

// hostAudio — звук одного эфира: путь (WebSocket или RTP), ведро скорости, соединение WebRTC.
type hostAudio struct {
	h *Hub
	s *station

	mu        sync.Mutex
	rtc       bool // путь звука — RTP
	tokens    float64
	last      time.Time
	pc        *webrtc.PeerConnection
	reorder   *rtpReorder
	bad       uint64 // пакеты не той длины
	sent      [4]uint64
	lastOffer time.Time
	done      chan struct{} // закрывается вместе с pc: стоп отчётам
}

func newHostAudio(h *Hub, s *station) *hostAudio {
	return &hostAudio{h: h, s: s, tokens: burstBytes, last: time.Now()}
}

// frame — кадр звука с пути rtc (true) или WebSocket; раздаётся, только если путь активный.
func (a *hostAudio) frame(data []byte, rtc bool) {
	a.mu.Lock()
	if a.rtc != rtc {
		a.mu.Unlock()
		return
	}
	// Ведро токенов: больше 64 кбит/с в эфир не уходит, что бы ни прислал браузер.
	now := time.Now()
	a.tokens += now.Sub(a.last).Seconds() * bytesPerSec
	a.last = now
	if a.tokens > burstBytes {
		a.tokens = burstBytes
	}
	if float64(len(data)) > a.tokens {
		a.mu.Unlock()
		a.h.dropped.Inc("host_rate")
		return
	}
	a.tokens -= float64(len(data))
	a.mu.Unlock()
	a.h.framesIn.Add(uint64((len(data) + FrameBytes - 1) / FrameBytes))
	a.h.bytesIn.Add(uint64(len(data)))
	a.h.broadcast(a.s, data)
}

// text — команды ведущего о звуке; true — сообщение было о звуке (дальше не разбирать).
func (a *hostAudio) text(data []byte) bool {
	if !strings.Contains(string(data), `"rtc"`) && !strings.Contains(string(data), `"audio"`) && !strings.Contains(string(data), `"ping"`) {
		return false
	}
	var m struct {
		RTC *struct {
			Offer string `json:"offer"`
		} `json:"rtc"`
		Audio *string `json:"audio"`
		Ping  any     `json:"ping"`
	}
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	if m.RTC != nil {
		a.offer(m.RTC.Offer)
	}
	if m.Audio != nil {
		a.setPath(*m.Audio == rtcAudioRTC)
	}
	return m.RTC != nil || m.Audio != nil || m.Ping != nil
}

func (a *hostAudio) setPath(rtc bool) {
	a.mu.Lock()
	changed := a.rtc != rtc
	a.rtc = rtc
	a.mu.Unlock()
	if changed {
		a.h.audioPath.Inc(map[bool]string{true: rtcAudioRTC, false: rtcAudioWS}[rtc])
	}
}

// offer — предложение WebRTC от страницы: ответ уходит по /ws/host.
func (a *hostAudio) offer(sdp string) {
	say := func(v map[string]string) { a.s.sayHost(map[string]any{"rtc": v}) }
	if a.h.rtc == nil {
		a.h.rtcConnects.Inc("off")
		say(map[string]string{"error": "off"})
		return
	}
	a.mu.Lock()
	if time.Since(a.lastOffer) < rtcOfferEvery || len(sdp) > rtcMaxOffer || sdp == "" {
		a.mu.Unlock()
		a.h.rtcConnects.Inc("bad")
		say(map[string]string{"error": "bad"})
		return
	}
	a.lastOffer = time.Now()
	a.mu.Unlock()
	a.closeRTC()

	answer, err := a.connect(sdp)
	if err != nil {
		log.Printf("радио, WebRTC: %v", err) // без адресов: в ошибке только SDP-разбор и ICE
		a.h.rtcConnects.Inc("bad")
		say(map[string]string{"error": "bad"})
		return
	}
	say(map[string]string{"answer": answer})
}

func (a *hostAudio) connect(offer string) (string, error) {
	pc, err := a.h.rtc.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) { pc.Close(); return "", err }
	done := make(chan struct{})
	reorder := newRTPReorder()
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if !strings.EqualFold(t.Codec().MimeType, pcmuMime) {
			return
		}
		for {
			p, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			if len(p.Payload) != FrameBytes { // браузер договорился о другой длине кадра — не наш формат
				a.mu.Lock()
				a.bad++
				a.mu.Unlock()
				continue
			}
			a.mu.Lock()
			out := reorder.push(p.SequenceNumber, append([]byte(nil), p.Payload...))
			a.mu.Unlock()
			for _, f := range out {
				a.frame(f, true)
			}
		}
	})
	var once sync.Once
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateConnected:
			once.Do(func() { a.h.rtcConnects.Inc("ok") })
		case webrtc.PeerConnectionStateFailed:
			once.Do(func() { a.h.rtcConnects.Inc("failed") })
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return fail(err)
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(ans); err != nil {
		return fail(err)
	}
	select {
	case <-gathered:
	case <-time.After(rtcGather):
		return fail(fmt.Errorf("кандидаты ICE не собрались за %v", rtcGather))
	}
	a.mu.Lock()
	a.pc, a.reorder, a.done, a.bad, a.sent = pc, reorder, done, 0, [4]uint64{}
	a.mu.Unlock()
	go a.report(done)
	return pc.LocalDescription().SDP, nil
}

// report — раз в rtcReport ведущему: сколько пакетов дошло, потеряно, опоздало, не той длины
// (за период). По нему страница показывает «UDP · потери N %» и решает в режиме «Авто».
func (a *hostAudio) report(done chan struct{}) {
	t := time.NewTicker(rtcReport)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		a.mu.Lock()
		r := a.reorder
		cur := [4]uint64{r.ok, r.lost, r.late, a.bad}
		d := [4]uint64{}
		for i := range cur {
			d[i] = cur[i] - a.sent[i]
		}
		a.sent = cur
		a.mu.Unlock()
		for i, k := range []string{"ok", "lost", "late", "bad_size"} {
			if d[i] > 0 {
				a.h.rtcPackets.Add(k, d[i])
			}
		}
		a.s.sayHost(map[string]any{"udp": map[string]uint64{"ok": d[0], "lost": d[1], "late": d[2], "bad": d[3]}})
	}
}

// closeRTC — закрыть соединение WebRTC (новое предложение или конец эфира); путь — на WebSocket.
func (a *hostAudio) closeRTC() {
	a.mu.Lock()
	pc, done := a.pc, a.done
	a.pc, a.done = nil, nil
	wasRTC := a.rtc
	a.rtc = false
	a.mu.Unlock()
	if done != nil {
		close(done)
	}
	if pc != nil {
		pc.Close()
	}
	if wasRTC {
		a.h.audioPath.Inc(rtcAudioWS)
	}
}
