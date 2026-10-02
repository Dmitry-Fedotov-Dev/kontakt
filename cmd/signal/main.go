// signal — сигнальный сервис «Контакта»: SIP (UDP и WebSocket), очередь, жалобы и баны,
// горячая перезагрузка конфига. Звуком управляет через gRPC медиа-сервиса.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"kontakt/internal/config"
	"kontakt/internal/mediaapi"
	"kontakt/internal/moderation"
	"kontakt/internal/netutil"
	sigsvc "kontakt/internal/signal"
)

func main() {
	httpAddr := flag.String("http", "127.0.0.1:8081", "HTTP: /sip (WebSocket) и /api (за web-сервисом)")
	adminAddr := flag.String("admin", "127.0.0.1:8091", "админка: /admin/reload, /admin/stats, /admin/unban (только localhost!)")
	sipAddr := flag.String("sip", "", "UDP SIP для софтфонов, например :5060 (пусто — выключено)")
	ip := flag.String("ip", "", "IP для Via/Contact у UDP-софтфонов (по умолчанию — определить)")
	mediaAddr := flag.String("media", "127.0.0.1:7002", "адрес gRPC медиа-сервиса")
	cfgPath := flag.String("config", "config/kontakt.json", "конфиг поведения (перечитывается на лету)")
	bansPath := flag.String("bans", "data/bans.json", "база банов (хеши кук и число карточек)")
	flag.Parse()
	log.SetPrefix("[signal] ")
	if *ip == "" {
		*ip = netutil.DetectIP()
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("конфиг: %v", err)
	}
	mod, err := moderation.Open(*bansPath)
	if err != nil {
		log.Fatalf("баны: %v", err)
	}
	mc, err := mediaapi.Dial(*mediaAddr)
	if err != nil {
		log.Fatalf("media: %v", err)
	}
	srv := sigsvc.New(cfg, mod, mc)

	stop := make(chan struct{})
	go cfg.Watch(stop)

	if *sipAddr != "" {
		u, err := srv.ServeUDP(*sipAddr, *ip)
		if err != nil {
			log.Fatalf("SIP UDP: %v", err)
		}
		log.Printf("SIP/UDP: sip:kontakt@%s:%d — звонить с любого софтфона", *ip, u.Port)
	}
	go func() {
		log.Fatal(http.ListenAndServe(*adminAddr, srv.AdminHandler(func() (any, error) { return mc.Stats() })))
	}()
	go func() { log.Fatal(http.ListenAndServe(*httpAddr, srv.HTTPHandler())) }()
	c := cfg.Get()
	log.Printf("HTTP %s, админка %s, media %s; линия по умолчанию %s, жалобы: %s", *httpAddr, *adminAddr, *mediaAddr, c.DefaultLine, c.BanPolicy)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	for s := range sig {
		if s == syscall.SIGHUP {
			if _, err := cfg.Reload(true); err != nil {
				log.Printf("SIGHUP: %v", err)
			}
			continue
		}
		close(stop)
		return
	}
}
