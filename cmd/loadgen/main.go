// loadgen — нагрузочный генератор для «Контакта».
//
// Поднимает N веб-абонентов ровно тем же путём, что и браузер (web-прокси → SIP по WebSocket →
// медиа-WebSocket), разбивает их станцией на пары А↔Б и гоняет G.711 RTP 50 пакетов/с
// в обе стороны. Параллельно снимает CPU и память процессов web/signal/media из /proc.
//
//	go run ./cmd/loadgen -url http://127.0.0.1:8080 -channels 100 -dur 60s
//
// Канал = одно подключение абонента к станции. Разговор А↔Б = 2 канала.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"kontakt/internal/dsp"
	"kontakt/internal/identity"
	"kontakt/internal/sip"
)

var (
	txPkts, rxPkts, rxTalkPkts atomic.Int64
	txBytes, rxBytes           atomic.Int64
	talking, failed            atomic.Int64
)

type sub struct {
	base   string
	sipURL string
	medURL string
	id     string
	sipWS  *websocket.Conn
	wmu    sync.Mutex
	media  *websocket.Conn
	callID string
	tag    string
	toTag  string
	isTalk atomic.Bool
	ok     chan *sip.Msg
}

func (s *sub) send(b string) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.sipWS.WriteMessage(websocket.TextMessage, []byte(b))
}

func (s *sub) run(stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	hdr := http.Header{"Cookie": {identity.CookieName + "=" + s.id}}
	d := websocket.Dialer{Subprotocols: []string{"sip"}, HandshakeTimeout: 10 * time.Second}
	c, _, err := d.Dial(strings.Replace(s.sipURL, "http", "ws", 1)+"/sip", hdr)
	if err != nil {
		failed.Add(1)
		log.Printf("sip ws: %v", err)
		return
	}
	s.sipWS = c
	defer c.Close()
	s.ok = make(chan *sip.Msg, 1)
	go s.readSIP()

	sdp := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nc=IN IP4 0.0.0.0\r\nt=0 0\r\nm=audio 9 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"
	s.send(fmt.Sprintf("INVITE sip:32@kontakt SIP/2.0\r\nVia: SIP/2.0/WSS lg.invalid;branch=%s\r\nMax-Forwards: 70\r\n"+
		"From: <sip:lg@lg.invalid>;tag=%s\r\nTo: <sip:32@kontakt>\r\nCall-ID: %s\r\nCSeq: 1 INVITE\r\n"+
		"Contact: <sip:lg@lg.invalid;transport=ws>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s",
		sip.NewBranch(), s.tag, s.callID, len(sdp), sdp))
	var ok *sip.Msg
	select {
	case ok = <-s.ok:
	case <-time.After(10 * time.Second):
		failed.Add(1)
		return
	}
	s.toTag = sip.TagOf(ok.Get("To"))
	s.send(fmt.Sprintf("ACK sip:kontakt@kontakt.invalid SIP/2.0\r\nVia: SIP/2.0/WSS lg.invalid;branch=%s\r\n"+
		"From: <sip:lg@lg.invalid>;tag=%s\r\nTo: <sip:32@kontakt>;tag=%s\r\nCall-ID: %s\r\nCSeq: 1 ACK\r\nContent-Length: 0\r\n\r\n",
		sip.NewBranch(), s.tag, s.toTag, s.callID))
	var path string
	for _, l := range strings.Split(string(ok.Body), "\r\n") {
		if strings.HasPrefix(l, "a=x-kontakt-media:ws ") {
			path = strings.TrimPrefix(l, "a=x-kontakt-media:ws ")
		}
	}
	m, _, err := websocket.DefaultDialer.Dial(strings.Replace(s.medURL, "http", "ws", 1)+"/"+path, hdr)
	if err != nil {
		failed.Add(1)
		log.Printf("media ws: %v", err)
		return
	}
	s.media = m
	defer m.Close()
	go func() {
		for {
			_, b, err := m.ReadMessage()
			if err != nil {
				return
			}
			rxPkts.Add(1)
			rxBytes.Add(int64(len(b)))
			if s.isTalk.Load() {
				rxTalkPkts.Add(1)
			}
		}
	}()

	// говорим 50 пакетов/с, как браузер (только в разговоре)
	pkt := make([]byte, 172)
	pkt[0], pkt[1] = 0x80, 8
	binary.BigEndian.PutUint32(pkt[8:], rand.Uint32())
	var seq uint16
	pcm := make([]int16, 160)
	phase := rand.Float64()
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			s.send(fmt.Sprintf("BYE sip:kontakt@kontakt.invalid SIP/2.0\r\nVia: SIP/2.0/WSS lg.invalid;branch=%s\r\n"+
				"From: <sip:lg@lg.invalid>;tag=%s\r\nTo: <sip:32@kontakt>;tag=%s\r\nCall-ID: %s\r\nCSeq: 2 BYE\r\nContent-Length: 0\r\n\r\n",
				sip.NewBranch(), s.tag, s.toTag, s.callID))
			time.Sleep(200 * time.Millisecond)
			return
		case <-t.C:
		}
		if !s.isTalk.Load() {
			continue
		}
		for i := range pcm { // «голос»: 300 Гц с гармониками и модуляцией
			phase += 2 * math.Pi * 300 / 8000
			pcm[i] = int16(6000*math.Sin(phase) + 2500*math.Sin(3*phase) + 1200*math.Sin(7.3*phase))
		}
		binary.BigEndian.PutUint16(pkt[2:], seq)
		binary.BigEndian.PutUint32(pkt[4:], uint32(seq)*160)
		dsp.Encode(8, pcm, pkt[12:])
		seq++
		if err := m.WriteMessage(websocket.BinaryMessage, pkt); err != nil {
			return
		}
		txPkts.Add(1)
		txBytes.Add(int64(len(pkt)))
	}
}

func (s *sub) readSIP() {
	for {
		_, data, err := s.sipWS.ReadMessage()
		if err != nil {
			return
		}
		m, err := sip.ParseMsg(data)
		if err != nil {
			continue
		}
		if !m.IsRequest {
			if m.Status == 200 && strings.HasSuffix(m.Get("CSeq"), "INVITE") {
				select {
				case s.ok <- m:
				default:
				}
			}
			continue
		}
		s.send(string(m.Response(200, "OK", "").Bytes()))
		if m.Method == "INFO" {
			var st map[string]string
			json.Unmarshal(m.Body, &st)
			was := s.isTalk.Load()
			now := st["state"] == "talking"
			s.isTalk.Store(now)
			if now && !was {
				talking.Add(1)
			} else if !now && was {
				talking.Add(-1)
			}
		}
	}
}

// ---------- /proc ----------

type proc struct {
	name string
	pid  int
}

func findProcs(names []string) []proc {
	var out []proc
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		b, err := os.ReadFile(d + "/cmdline")
		if err != nil || len(b) == 0 {
			continue
		}
		argv0 := string(bytes.SplitN(b, []byte{0}, 2)[0])
		for _, n := range names {
			if strings.HasSuffix(argv0, "/"+n) {
				pid, _ := strconv.Atoi(filepath.Base(d))
				out = append(out, proc{n, pid})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// cpuTicks — utime+stime в тиках (обычно 100 в секунду).
func cpuTicks(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+2:]))
	u, _ := strconv.ParseInt(f[11], 10, 64)
	s, _ := strconv.ParseInt(f[12], 10, 64)
	return u + s
}

func status(pid int) (rssKB, threads int64) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "VmRSS:") {
			rssKB, _ = strconv.ParseInt(strings.Fields(l)[1], 10, 64)
		}
		if strings.HasPrefix(l, "Threads:") {
			threads, _ = strconv.ParseInt(strings.Fields(l)[1], 10, 64)
		}
	}
	return
}

type Result struct {
	Channels     int                `json:"channels"`
	Calls        int                `json:"calls"`
	TalkingLegs  int64              `json:"talking_legs"`
	Failed       int64              `json:"failed"`
	Seconds      float64            `json:"seconds"`
	CPUCores     map[string]float64 `json:"cpu_cores"` // доля ядра (1.0 = одно ядро целиком)
	RSSMB        map[string]float64 `json:"rss_mb"`    // пик за окно
	Threads      map[string]int64   `json:"threads"`
	TxPPS        float64            `json:"tx_pps"`
	RxTalkPPS    float64            `json:"rx_talk_pps"`
	DeliveryPct  float64            `json:"delivery_pct"`
	RTPkbpsPerCh float64            `json:"rtp_kbps_per_channel_each_way"`
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "адрес web-сервиса")
	channels := flag.Int("channels", 2, "сколько абонентов (каналов); разговоров будет вдвое меньше")
	dur := flag.Duration("dur", 30*time.Second, "окно замера")
	warm := flag.Duration("warm", 5*time.Second, "прогрев после того, как все соединились")
	procs := flag.String("procs", "web,signal,media", "какие процессы мерить (по имени бинарника)")
	jsonOut := flag.String("json", "", "сохранить результат в JSON")
	sipURL := flag.String("sip-url", "", "куда слать /sip (по умолчанию -url); прямой адрес signal — как при маршрутизации в cloudflared")
	medURL := flag.String("media-url", "", "куда слать /media (по умолчанию -url); прямой адрес media")
	flag.Parse()

	if *sipURL == "" {
		*sipURL = *base
	}
	if *medURL == "" {
		*medURL = *base
	}
	ps := findProcs(strings.Split(*procs, ","))
	if len(ps) == 0 {
		log.Printf("предупреждение: процессы %s не найдены — будет только сеть", *procs)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *channels; i++ {
		s := &sub{base: *base, sipURL: *sipURL, medURL: *medURL, id: identity.New(), callID: sip.RandHex(8), tag: sip.RandHex(4)}
		wg.Add(1)
		go s.run(stop, &wg)
		time.Sleep(2 * time.Millisecond) // плавный вход
	}
	want := int64(*channels / 2 * 2)
	deadline := time.Now().Add(60 * time.Second)
	for talking.Load() < want && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("в разговоре %d из %d каналов, ошибок %d; прогрев %s", talking.Load(), *channels, failed.Load(), *warm)
	time.Sleep(*warm)

	t0 := time.Now()
	tx0, rx0 := txPkts.Load(), rxTalkPkts.Load()
	c0 := map[int]int64{}
	for _, p := range ps {
		c0[p.pid] = cpuTicks(p.pid)
	}
	peak := map[string]float64{}
	threads := map[string]int64{}
	for time.Since(t0) < *dur {
		time.Sleep(500 * time.Millisecond)
		for _, p := range ps {
			r, th := status(p.pid)
			peak[p.name] = math.Max(peak[p.name], float64(r)/1024)
			threads[p.name] = th
		}
	}
	el := time.Since(t0).Seconds()
	res := Result{Channels: *channels, Calls: *channels / 2, TalkingLegs: talking.Load(), Failed: failed.Load(),
		Seconds: el, CPUCores: map[string]float64{}, RSSMB: peak, Threads: threads}
	var total float64
	for _, p := range ps {
		cores := float64(cpuTicks(p.pid)-c0[p.pid]) / 100 / el
		res.CPUCores[p.name] = math.Round(cores*10000) / 10000
		total += cores
	}
	res.CPUCores["total"] = math.Round(total*10000) / 10000
	res.TxPPS = float64(txPkts.Load()-tx0) / el
	res.RxTalkPPS = float64(rxTalkPkts.Load()-rx0) / el
	if res.TxPPS > 0 {
		res.DeliveryPct = math.Round(res.RxTalkPPS/res.TxPPS*10000) / 100
	}
	if res.TalkingLegs > 0 {
		res.RTPkbpsPerCh = res.TxPPS / float64(res.TalkingLegs) * 172 * 8 / 1000
	}
	close(stop)
	wg.Wait()

	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if *jsonOut != "" {
		os.WriteFile(*jsonOut, b, 0o644)
	}
}
