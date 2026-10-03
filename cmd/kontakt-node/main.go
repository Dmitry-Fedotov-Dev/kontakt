// kontakt-node — узел Kontakt Mesh: master, worker или standalone (docs/MESH.md).
//
//	kontakt-node -role master -listen :7400 -admin 127.0.0.1:7401
//	  печатает ключ Master'а: его закрепляют у Worker'ов (-master-key)
//	KONTAKT_JOIN_TOKEN=… kontakt-node -role worker -listen :7410 -master 10.0.0.4:7400 -master-key <ключ>
//	kontakt-node -role worker -listen :7420 -bootstrap 10.0.0.5:7410 -trusted data/trusted.json   # без Master'а
//
// Админка (только localhost): /metrics, /mesh/status, /mesh/route?dst=<NodeID>.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kontakt/internal/mesh"
)

func main() {
	role := flag.String("role", "worker", "master | worker | standalone")
	keyPath := flag.String("identity", "data/node.key", "ключ узла: создаётся при первом запуске, переживает перезапуск")
	listen := flag.String("listen", ":7400", "адрес для mesh-соединений (TCP)")
	advertise := flag.String("advertise", "", "как до узла достучаться снаружи (по умолчанию -listen)")
	masterAddr := flag.String("master", "", "Worker: адрес Master'а; пусто — работать без Master'а")
	masterKey := flag.String("master-key", "", "Worker: открытый ключ Master'а (base64), закрепляется")
	bootstrap := flag.String("bootstrap", "", "адреса доверенных Worker'ов через запятую — старт без Master'а")
	trusted := flag.String("trusted", "", "файл со списком доверенных NodeID (JSON-массив)")
	capacity := flag.Float64("capacity-mbps", 0, "ёмкость канала узла по замеру (speedtest), Мбит/с; 0 — неизвестна")
	reserve := flag.Float64("reserve", 0.3, "доля канала, которую держать свободной")
	maxPeers := flag.Int("max-peers", 8, "сколько соседей держать")
	maxHops := flag.Int("max-hops", 4, "радиус топологии вокруг узла")
	admin := flag.String("admin", "127.0.0.1:7401", "админка: /metrics, /mesh/status (только localhost!)")
	cfURL := flag.String("cloudflared-metrics", "", "адрес /metrics cloudflared, если узел за туннелем (например http://127.0.0.1:60123/metrics)")
	printID := flag.Bool("print-id", false, "напечатать NodeID и открытый ключ и выйти")
	flag.Parse()
	log.SetPrefix("[mesh]   ")

	id, err := mesh.LoadOrCreateIdentity(*keyPath)
	if err != nil {
		log.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(id.Pub)
	if *printID {
		os.Stdout.WriteString(string(id.ID) + " " + pubB64 + "\n")
		return
	}

	cfg := mesh.Config{Role: mesh.Role(*role), Identity: id, Listen: *listen, Advertise: *advertise,
		MasterAddr: *masterAddr, JoinToken: os.Getenv("KONTAKT_JOIN_TOKEN"),
		MaxPeers: *maxPeers, MaxHops: *maxHops, CapacityMbps: *capacity, Reserve: *reserve}
	if *bootstrap != "" {
		cfg.Bootstrap = strings.Split(*bootstrap, ",")
	}
	if *masterKey != "" {
		b, err := base64.StdEncoding.DecodeString(*masterKey)
		if err != nil || len(b) != ed25519.PublicKeySize {
			log.Fatalf("-master-key: не открытый ключ ed25519 в base64")
		}
		cfg.MasterPub = b
	}
	switch cfg.Role {
	case mesh.RoleMaster, mesh.RoleWorker, mesh.RoleStandalone:
	default:
		log.Fatalf("-role: master | worker | standalone (edge — это браузер)")
	}
	if cfg.Role == mesh.RoleMaster && cfg.JoinToken == "" {
		log.Fatal("Master без KONTAKT_JOIN_TOKEN примет кого угодно: задайте токен")
	}
	if cfg.MasterAddr != "" && cfg.MasterPub == nil {
		log.Fatal("-master без -master-key: ответ Master'а нечем проверить")
	}

	n, err := mesh.NewNode(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if *trusted != "" {
		if err := n.Trust.LoadTrustedFile(*trusted); err != nil {
			log.Fatal(err)
		}
	}
	if err := n.Start(); err != nil {
		log.Fatal(err)
	}

	var cf *mesh.Cloudflared
	if *cfURL != "" {
		cf = &mesh.Cloudflared{URL: *cfURL}
		go func() {
			for ; ; time.Sleep(15 * time.Second) {
				cf.Poll()
			}
		}()
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", n.MetricsRegistry(cf).Handler())
	mux.HandleFunc("/mesh/status", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, n.Status()) })
	mux.HandleFunc("/mesh/route", func(w http.ResponseWriter, r *http.Request) {
		a, alts := n.Route(mesh.NodeID(r.URL.Query().Get("dst")))
		writeJSON(w, map[string]any{"active": a, "alternatives": alts})
	})
	srv := &http.Server{Addr: *admin, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { log.Fatal(srv.ListenAndServe()) }()

	log.Printf("%s %s: mesh %s, админка %s", cfg.Role, id.ID, n.Addr(), *admin)
	if cfg.Role == mesh.RoleMaster {
		log.Printf("ключ Master'а для Worker'ов: -master-key %s", pubB64)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	n.Close()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	e.Encode(v)
}
