// radio — «Открытое радио»: своя волна, G.711 μ-law 64 кбит/с, очередь mp3 и микрофон.
//
//	go run ./cmd/radio -http :27620
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"

	"kontakt/internal/radio"
)

func main() {
	addr := flag.String("http", ":27620", "адрес страницы и эфира (его открывать в туннель)")
	stations := flag.Int("max-stations", 50, "сколько станций может быть в эфире одновременно")
	listeners := flag.Int("max-listeners", 500, "сколько приёмников может быть подключено (64 кбит/с каждый, пока настроен на станцию)")
	admin := flag.String("admin", "", "отдельный адрес для /metrics, например 127.0.0.1:27621; задан — со страницы радио (и из туннеля) /metrics убирается")
	flag.Parse()
	log.SetPrefix("[radio]  ")
	h := radio.NewHub(radio.Options{MaxStations: *stations, MaxListeners: *listeners, PrivateMetrics: *admin != ""})
	if *admin != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", h.MetricsHandler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
		go func() { log.Fatal(http.ListenAndServe(*admin, mux)) }()
		log.Printf("метрики: http://%s/metrics", *admin)
	}
	shown := *addr
	if strings.HasPrefix(shown, ":") {
		shown = "localhost" + shown
	}
	log.Printf("Открытое радио: http://%s", shown)
	log.Fatal(http.ListenAndServe(*addr, h.Handler()))
}
