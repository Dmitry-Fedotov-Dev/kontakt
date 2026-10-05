// radio — «Открытое радио»: своя волна, G.711 μ-law 64 кбит/с, очередь mp3 и микрофон.
//
//	go run ./cmd/radio -http :27620
//
// Модерация: с -mod — общая с рулеткой база signal'а (его админ-порт), иначе своя (-bans).
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"

	"kontakt/internal/moderation"
	"kontakt/internal/radio"
)

func main() {
	addr := flag.String("http", ":27620", "адрес страницы и эфира (его открывать в туннель)")
	stations := flag.Int("max-stations", 50, "сколько станций может быть в эфире одновременно")
	listeners := flag.Int("max-listeners", 500, "сколько приёмников может быть подключено (64 кбит/с каждый, пока настроен на станцию)")
	admin := flag.String("admin", "", "отдельный адрес для /metrics (и админки модерации без -mod), например 127.0.0.1:27621; задан — со страницы радио (и из туннеля) /metrics убирается")
	modURL := flag.String("mod", "", "модерация signal'а — его админ-порт, например http://127.0.0.1:8091: одна база банов с рулеткой")
	bansPath := flag.String("bans", "data/radio-bans.json", "своя база модерации, если -mod не задан")
	linksPath := flag.String("links", "", "где хранить короткие ссылки /r/… (пусто — только в памяти, до перезапуска)")
	flag.Parse()
	log.SetPrefix("[radio]  ")

	var mod moderation.Moderator
	var store *moderation.Store
	if *modURL != "" {
		mod = moderation.NewRemote(strings.TrimSuffix(*modURL, "/"))
		log.Printf("модерация: общая, %s", *modURL)
	} else {
		var err error
		if store, err = moderation.Open(*bansPath); err != nil {
			log.Fatalf("модерация: %v", err)
		}
		mod = store
		log.Printf("модерация: своя база %s", *bansPath)
	}
	h := radio.NewHub(radio.Options{MaxStations: *stations, MaxListeners: *listeners, PrivateMetrics: *admin != "", Mod: mod, LinksPath: *linksPath})
	if *admin != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", h.MetricsHandler())
		if store != nil { // с -mod банят через админку signal'а
			mux.Handle("/admin/", store.AdminHandler(nil)) // эфир снимается при ближайшей проверке, до 30 с
		}
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
