// media — медиа-сервис «Контакта»: RTP, кодеки, эффект линии, гудки и шум.
// Наружу: RTP по UDP (софтфоны) и WebSocket /media (браузер, через web-сервис).
// Управление: только gRPC от signal.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"kontakt/internal/media"
	"kontakt/internal/mediaapi"
	"kontakt/internal/netutil"
)

func main() {
	grpcAddr := flag.String("grpc", "127.0.0.1:7002", "gRPC для signal (наружу не открывать)")
	wsAddr := flag.String("ws", "127.0.0.1:8082", "HTTP/WebSocket для медиа из браузера (за web-сервисом)")
	ip := flag.String("ip", "", "IP для SDP у UDP-софтфонов (по умолчанию — определить)")
	rtp := flag.String("rtp", "10000-10200", "диапазон UDP-портов RTP")
	flag.Parse()
	log.SetPrefix("[media]  ")
	if *ip == "" {
		*ip = netutil.DetectIP()
	}
	rmin, rmax := netutil.ParseRange(*rtp, 10000, 10200)

	eng := media.NewEngine(*ip, rmin, rmax)

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("gRPC: %v", err)
	}
	gs := grpc.NewServer()
	mediaapi.RegisterMediaServer(gs, eng)
	go func() { log.Fatal(gs.Serve(lis)) }()

	mux := http.NewServeMux()
	mux.HandleFunc("/media", eng.ServeWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	go func() { log.Fatal(http.ListenAndServe(*wsAddr, mux)) }()

	log.Printf("gRPC %s, WebSocket %s/media, RTP %s:%d-%d", *grpcAddr, *wsAddr, *ip, rmin, rmax)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	gs.GracefulStop()
	eng.Close()
}
