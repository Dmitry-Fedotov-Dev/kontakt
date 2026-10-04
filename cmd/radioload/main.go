// radioload — нагрузка на «Открытое радио»: станции в эфире и приёмники, которые крутят ручку.
//
//	go run ./cmd/radioload -url http://127.0.0.1:27620 -stations 50 -listeners 500 -dur 3m
//
// Станция шлёт 50 кадров μ-law в секунду и меняет трек в среднем раз в -track; приёмник
// настраивается на случайную волну (станцию или шорох между ними) в среднем раз в -turn.
// Раз в секунду в CSV (-csv) и в /metrics (-metrics) — что получили приёмники: списки станций,
// сообщения о треке и звук. Меряется со стороны приёмника, поэтому сравнимо между версиями
// сервера, в том числе без его собственных метрик.
//
// Звонки этим не тестируются — только xk6-sip; это генератор для радио.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"kontakt/internal/identity"
	"kontakt/internal/metrics"
)

var (
	listBytes, titleBytes, otherText, audioBytes atomic.Uint64
	connected                                    atomic.Int64
)

func main() {
	base := flag.String("url", "http://127.0.0.1:27620", "адрес радио (отдельно или за web: …/radio)")
	stations := flag.Int("stations", 50, "станций в эфире")
	listeners := flag.Int("listeners", 500, "приёмников")
	dur := flag.Duration("dur", 3*time.Minute, "длительность")
	turn := flag.Duration("turn", 10*time.Second, "в среднем столько приёмник стоит на волне, потом крутит ручку")
	track := flag.Duration("track", 210*time.Second, "средняя длина трека")
	ramp := flag.Duration("ramp", 20*time.Second, "за сколько подключаются все приёмники")
	csvPath := flag.String("csv", "", "куда писать посекундные байты (пусто — только в консоль раз в 10 с)")
	maddr := flag.String("metrics", "", "адрес /metrics генератора для Prometheus, например 127.0.0.1:27650")
	flag.Parse()
	log.SetPrefix("[radioload] ")
	ws := "ws" + strings.TrimPrefix(strings.TrimSuffix(*base, "/"), "http")

	if *maddr != "" {
		reg := metrics.NewRegistry()
		reg.CounterFunc("radioload_list_bytes_total", "Байты списков станций, полученные приёмниками",
			func() float64 { return float64(listBytes.Load()) })
		reg.CounterFunc("radioload_title_bytes_total", "Байты сообщений о смене трека, полученные приёмниками",
			func() float64 { return float64(titleBytes.Load()) })
		reg.CounterFunc("radioload_audio_bytes_total", "Байты звука, полученные приёмниками",
			func() float64 { return float64(audioBytes.Load()) })
		reg.Gauge("radioload_listeners", "Подключённые приёмники", func() float64 { return float64(connected.Load()) })
		go func() { log.Fatal(http.ListenAndServe(*maddr, reg.Handler())) }()
	}

	stop := make(chan struct{})
	freqs := make([]int, *stations)
	var wg sync.WaitGroup
	for i := range freqs {
		freqs[i] = 876 + i*4 // 87.6, 88.0, … — шаг 0,4 МГц, ручка их различает
		wg.Add(1)
		go func(f int) { defer wg.Done(); host(ws, f, *track, stop) }(freqs[i])
	}
	time.Sleep(time.Second)
	for i := 0; i < *listeners; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); listener(ws, freqs, *turn, stop) }()
		time.Sleep(*ramp / time.Duration(*listeners))
	}

	var csv *os.File
	if *csvPath != "" {
		var err error
		if csv, err = os.Create(*csvPath); err != nil {
			log.Fatal(err)
		}
		defer csv.Close()
		fmt.Fprintln(csv, "t,listeners,list_bps,title_bps,audio_bps")
	}
	start := time.Now()
	tick := time.NewTicker(time.Second)
	var pl, pt, pa uint64
	for now := range tick.C {
		l, t, a := listBytes.Load(), titleBytes.Load(), audioBytes.Load()
		sec := int(now.Sub(start).Seconds())
		if csv != nil {
			fmt.Fprintf(csv, "%d,%d,%d,%d,%d\n", sec, connected.Load(), (l-pl)*8, (t-pt)*8, (a-pa)*8)
		}
		if sec%10 == 0 {
			log.Printf("%3d с: приёмников %d, списки %.2f Мбит/с, треки %.3f Мбит/с, звук %.2f Мбит/с",
				sec, connected.Load(), float64(l-pl)*8/1e6, float64(t-pt)*8/1e6, float64(a-pa)*8/1e6)
		}
		pl, pt, pa = l, t, a
		if now.Sub(start) >= *dur {
			break
		}
	}
	close(stop)
	wg.Wait()
	if otherText.Load() > 0 {
		log.Printf("прочие текстовые сообщения: %d байт", otherText.Load())
	}
}

// dial подключается с повтором до 10 с: радио могли запустить только что.
func dial(url string) (*websocket.Conn, error) {
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var c *websocket.Conn
		if c, _, err = websocket.DefaultDialer.Dial(url, cookie()); err == nil {
			return c, nil
		}
	}
	return nil, err
}

func cookie() http.Header {
	return http.Header{"Cookie": {identity.CookieName + "=" + identity.New()}}
}

// exp — случайный интервал со средним mean (события независимы, как живые люди).
func exp(mean time.Duration) time.Duration {
	return time.Duration(rand.ExpFloat64() * float64(mean))
}

func host(ws string, f int, track time.Duration, stop <-chan struct{}) {
	c, err := dial(fmt.Sprintf("%s/ws/host?f=%d&name=LOAD-%d", ws, f, f))
	if err != nil {
		log.Printf("станция %d: %v", f, err)
		return
	}
	defer c.Close()
	go func() { // ведущему тоже пишут (число слушателей) — читаем, чтобы не копилось
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
	frame := bytes.Repeat([]byte{0xff}, 160)
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	next, n := time.Now().Add(exp(track)), 0
	c.WriteJSON(map[string]string{"title": fmt.Sprintf("Исполнитель — трек номер %d на волне %d", n, f)})
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if c.WriteMessage(websocket.BinaryMessage, frame) != nil {
				return
			}
			if time.Now().After(next) {
				n++
				next = time.Now().Add(exp(track))
				c.WriteJSON(map[string]string{"title": fmt.Sprintf("Исполнитель — трек номер %d на волне %d", n, f)})
			}
		}
	}
}

func listener(ws string, freqs []int, turn time.Duration, stop <-chan struct{}) {
	c, err := dial(ws + "/ws/listen")
	if err != nil {
		log.Printf("приёмник: %v", err)
		return
	}
	defer c.Close()
	connected.Add(1)
	defer connected.Add(-1)
	go func() {
		for {
			typ, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			switch {
			case typ == websocket.BinaryMessage:
				audioBytes.Add(uint64(len(b)))
			case bytes.HasPrefix(b, []byte(`{"stations"`)):
				listBytes.Add(uint64(len(b)))
			case bytes.HasPrefix(b, []byte(`{"title"`)):
				titleBytes.Add(uint64(len(b)))
			default:
				otherText.Add(uint64(len(b)))
			}
		}
	}()
	tune := func() {
		f := 875 + rand.Intn(206) // половина времени — шорох между станциями, как у живой ручки
		if rand.Intn(2) == 0 {
			f = freqs[rand.Intn(len(freqs))]
		}
		b, _ := json.Marshal(map[string]int{"tune": f})
		c.WriteMessage(websocket.TextMessage, b)
	}
	tune()
	for {
		select {
		case <-stop:
			return
		case <-time.After(exp(turn)):
			tune()
		}
	}
}
