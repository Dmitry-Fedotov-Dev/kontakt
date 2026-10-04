// web — входная точка «Контакта». Наружу (в туннель) смотрит только он.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"time"

	"kontakt/internal/netutil"
	"kontakt/internal/webapp"
)

func main() {
	addr := flag.String("http", ":8080", "публичный адрес (его открывать в туннель)")
	signalURL := flag.String("signal", "http://127.0.0.1:8081", "signal: /sip и /api")
	mediaURL := flag.String("media", "http://127.0.0.1:8082", "media: /media")
	radioURL := flag.String("radio", "", "Открытое радио под /radio/, например http://127.0.0.1:27620 (пусто — без радио)")
	httpsAddr := flag.String("https", "", "ещё и HTTPS с самоподписанным сертификатом, например :8443 — браузер даёт микрофон только по https или на localhost")
	flag.Parse()
	log.SetPrefix("[web]    ")
	up := webapp.Upstreams{Signal: mustURL(*signalURL), Media: mustURL(*mediaURL)}
	if *radioURL != "" {
		up.Radio = mustURL(*radioURL)
	}
	h := webapp.Handler(up)
	if *httpsAddr != "" {
		ip := netutil.DetectIP()
		cert, err := selfSigned(ip)
		if err != nil {
			log.Fatal(err)
		}
		srv := &http.Server{Addr: *httpsAddr, Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
		go func() { log.Fatal(srv.ListenAndServeTLS("", "")) }()
		log.Printf("в локальной сети: https://%s%s (браузер предупредит о сертификате — это нормально)", ip, portOf(*httpsAddr))
	}
	log.Printf("Контакт: http://localhost%s", portOf(*addr))
	if up.Radio != nil {
		log.Printf("радио: http://localhost%s%s/", portOf(*addr), webapp.RadioPrefix)
	}
	log.Fatal(http.ListenAndServe(*addr, h))
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		log.Fatal(err)
	}
	return u
}

func portOf(a string) string {
	for i := len(a) - 1; i >= 0; i-- {
		if a[i] == ':' {
			return a[i:]
		}
	}
	return a
}

func selfSigned(ip string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "Kontakt LAN"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(ip)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
