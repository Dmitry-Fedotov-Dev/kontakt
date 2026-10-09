package radio

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func seqFrames(r *rtpReorder, seqs ...uint16) (out []byte) {
	for _, q := range seqs {
		for _, f := range r.push(q, frame(byte(q))) {
			out = append(out, f[0])
		}
	}
	return out
}

// Буфер перестановки: по порядку — сразу; перестановка в пределах reorderHold — исправляется;
// повтор и опоздавший — выкидываются; дыра — заменой, после plcFrames подряд — тишиной.
func TestRTPReorder(t *testing.T) {
	r := newRTPReorder()
	if got := seqFrames(r, 10, 11, 13, 12, 14); !bytes.Equal(got, []byte{10, 11, 12, 13, 14}) {
		t.Fatalf("перестановка: %v", got)
	}
	if got := seqFrames(r, 12, 14); len(got) != 0 || r.late != 2 {
		t.Fatalf("повторы: %v, late=%d", got, r.late)
	}
	// 15 и 16 потеряны: кадр ждут, пока не придёт reorderHold номеров после него (60 мс)
	if got := seqFrames(r, 17); len(got) != 0 {
		t.Fatalf("рано заменили: %v", got)
	}
	if got := seqFrames(r, 18); len(got) != 1 || r.lost != 1 { // 18 − 15 = 3: 15 — замена, 16 ещё ждём
		t.Fatalf("после 18: %v, lost=%d", got, r.lost)
	}
	if got := seqFrames(r, 19); len(got) != 4 || got[1] != 17 || got[3] != 19 || r.lost != 2 {
		t.Fatalf("после 19: %v, lost=%d", got, r.lost)
	}
	// замена — прошлый звук тише, а не тишина; после plcFrames подряд — тишина
	r2 := newRTPReorder()
	r2.push(1, frame(0x80)) // громкий отрицательный отсчёт μ-law
	var fills [][]byte
	for q := uint16(7); q <= 9; q++ {
		fills = append(fills, r2.push(q, frame(0x80))...)
	}
	// 2–6 потеряны: замены 1–3 — тише прошлого, 4–5 — тишина
	if len(fills) < 5 {
		t.Fatalf("замен: %d", len(fills))
	}
	if fills[0][0] == 0x80 || fills[0][0] == 0xFF {
		t.Fatalf("первая замена — не ослабленный прошлый звук: %#x", fills[0][0])
	}
	if fills[3][0] != 0xFF || fills[4][0] != 0xFF {
		t.Fatalf("после %d замен должна быть тишина: %#x %#x", plcFrames, fills[3][0], fills[4][0])
	}
	// долгий провал — без заполнения, с нового места
	r3 := newRTPReorder()
	r3.push(100, frame(1))
	if got := r3.push(100+reorderFill+10, frame(2)); len(got) != 1 || got[0][0] != 2 {
		t.Fatalf("после долгого провала: %d кадров", len(got))
	}
	// номер переходит через 65535
	r4 := newRTPReorder()
	if got := seqFrames(r4, 65534, 65535, 0, 1); !bytes.Equal(got, []byte{254, 255, 0, 1}) {
		t.Fatalf("переход через 0: %v", got)
	}
}

// Эфир по UDP целиком: ведущий (pion вместо браузера) договаривается по /ws/host, шлёт RTP PCMU,
// переключает путь на rtc — слушатель слышит его кадры; потерянный пакет заменён, а не пропущен;
// кадры WebSocket при пути rtc в эфир не идут; отчёт о пакетах приходит ведущему.
func TestRTCBroadcast(t *testing.T) {
	h := NewHub(Options{RTCUDP: "127.0.0.1:0"})
	defer h.Close()
	if h.rtc == nil {
		t.Fatal("UDP не поднялся")
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	l, _, err := dial(t, srv, "/ws/listen")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.WriteMessage(websocket.TextMessage, []byte(`{"tune":1017}`))
	heard := make(chan byte, 4096)
	go func() {
		for {
			typ, b, err := l.ReadMessage()
			if err != nil {
				return
			}
			if typ == websocket.BinaryMessage {
				for i := 0; i+FrameBytes <= len(b); i += FrameBytes {
					heard <- b[i]
				}
			}
		}
	}()

	host, _, err := dial(t, srv, "/ws/host?f=101.7&name=UDP")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ht := readTexts(host)
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })

	// ведущий — pion с одним треком PCMU
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		t.Fatal(err)
	}
	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gathered
	b, _ := json.Marshal(map[string]any{"rtc": map[string]string{"offer": pc.LocalDescription().SDP}})
	host.WriteMessage(websocket.TextMessage, b)

	var answer string
	for deadline := time.After(5 * time.Second); answer == ""; {
		select {
		case s := <-ht:
			var r struct {
				RTC struct{ Answer, Error string } `json:"rtc"`
			}
			if json.Unmarshal([]byte(s), &r) == nil && (r.RTC.Answer != "" || r.RTC.Error != "") {
				if r.RTC.Error != "" {
					t.Fatalf("сервер отказал: %s", r.RTC.Error)
				}
				answer = r.RTC.Answer
			}
		case <-deadline:
			t.Fatal("нет ответа на предложение WebRTC")
		}
	}
	if !strings.Contains(answer, "PCMU") {
		t.Fatalf("в ответе нет PCMU:\n%s", answer)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("WebRTC не соединился")
	}

	// путь звука — RTP; кадр по WebSocket после этого в эфир не идёт
	host.WriteMessage(websocket.TextMessage, []byte(`{"audio":"rtc"}`))
	time.Sleep(100 * time.Millisecond)
	host.WriteMessage(websocket.BinaryMessage, frame(0xEE))
	send := func(seq uint16, v byte) {
		track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq, Timestamp: uint32(seq) * 160, SSRC: 7}, Payload: frame(v)})
	}
	// 1..20, без 10 (потерян)
	for q := uint16(1); q <= 20; q++ {
		if q != 10 {
			send(q, byte(q))
		}
		time.Sleep(20 * time.Millisecond)
	}
	var got []byte
	for deadline := time.After(5 * time.Second); len(got) < 19; {
		select {
		case v := <-heard:
			got = append(got, v)
		case <-deadline:
			t.Fatalf("слушатель услышал %d кадров: %v", len(got), got)
		}
	}
	for _, v := range got {
		if v == 0xEE {
			t.Fatal("кадр WebSocket ушёл в эфир при пути rtc")
		}
	}
	// 1..9 как есть, на месте 10 — замена (ослабленный 9), дальше 11..
	if got[0] != 1 || got[8] != 9 || got[9] == 10 || got[10] != 11 {
		t.Fatalf("порядок или замена: %v", got)
	}

	// отчёт ведущему: дошло 19, потерян 1
	var ok, lost uint64
	for deadline := time.After(6 * time.Second); ok < 19; {
		select {
		case s := <-ht:
			var r struct {
				UDP *struct{ OK, Lost uint64 } `json:"udp"`
			}
			if json.Unmarshal([]byte(s), &r) == nil && r.UDP != nil {
				ok += r.UDP.OK
				lost += r.UDP.Lost
			}
		case <-deadline:
			t.Fatalf("отчёт: ok=%d lost=%d", ok, lost)
		}
	}
	if lost != 1 {
		t.Fatalf("отчёт: потеряно %d, ждали 1", lost)
	}
	if h.rtcConnects.Value("ok") != 1 || h.audioPath.Value(rtcAudioRTC) != 1 {
		t.Fatalf("метрики: connects ok=%d, path rtc=%d", h.rtcConnects.Value("ok"), h.audioPath.Value(rtcAudioRTC))
	}

	// обратно на WebSocket: его кадры снова в эфире
	host.WriteMessage(websocket.TextMessage, []byte(`{"audio":"ws"}`))
	host.WriteMessage(websocket.BinaryMessage, frame(0xAB))
	for deadline := time.After(3 * time.Second); ; {
		select {
		case v := <-heard:
			if v == 0xAB {
				return
			}
		case <-deadline:
			t.Fatal("после {\"audio\":\"ws\"} кадр WebSocket не дошёл")
		}
	}
}

// UDP на сервере выключен — на предложение ответ {"rtc":{"error":"off"}}, эфир по WebSocket живёт.
func TestRTCOff(t *testing.T) {
	h := NewHub(Options{})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	host, _, err := dial(t, srv, "/ws/host?f=101.7&name=X")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ht := readTexts(host)
	host.WriteMessage(websocket.TextMessage, []byte(`{"rtc":{"offer":"v=0"}}`))
	for _, s := range ht.collect(500 * time.Millisecond) {
		if strings.Contains(s, `"error":"off"`) {
			return
		}
	}
	t.Fatal("нет ответа «UDP выключен»")
}
