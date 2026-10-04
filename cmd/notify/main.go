// notify — Telegram-бот владельца: тревоги Prometheus, карточки и баны, команды модерации.
//
//	TELEGRAM_BOT_TOKEN=… TELEGRAM_CHAT_ID=… bin/notify -admin http://127.0.0.1:8091 -prometheus http://127.0.0.1:9092
//
// Токен и чат — только из окружения (scripts/run.sh берёт их из .env): в флагах их было бы
// видно в списке процессов. Без них бот не запускается, остальное работает и так.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"kontakt/internal/notify"
)

func main() {
	addr := flag.String("http", "127.0.0.1:27640", "куда signal шлёт события модерации (POST /event); только localhost")
	admin := flag.String("admin", "", "админка signal'а для команд: http://127.0.0.1:8091")
	prom := flag.String("prometheus", "", "Prometheus для тревог: http://127.0.0.1:9092 (пусто — без тревог)")
	api := flag.String("api", "https://api.telegram.org", "Bot API")
	flag.Parse()
	log.SetPrefix("[notify] ")

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chat, err := strconv.ParseInt(os.Getenv("TELEGRAM_CHAT_ID"), 10, 64)
	if token == "" || err != nil {
		log.Fatal("нужны TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID (в .env)")
	}
	s := &notify.Service{Bot: notify.NewBot(*api, token, chat), Prometheus: *prom, Admin: *admin}

	mux := http.NewServeMux()
	mux.Handle("/event", s.EventHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	go func() { log.Fatal(http.ListenAndServe(*addr, mux)) }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("бот на связи; события — http://%s/event, тревоги — %s", *addr, orNone(*prom))
	s.Run(ctx)
}

func orNone(s string) string {
	if s == "" {
		return "выключены"
	}
	return s
}
