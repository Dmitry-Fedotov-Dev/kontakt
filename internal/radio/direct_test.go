package radio

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Прямой HTTPS: сертификат из файлов, сокеты под /radio/ws/*, звук доходит, страница видит порт.
func TestDirectTLS(t *testing.T) {
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	kder, _ := x509.MarshalECPrivateKey(key)
	certPath, keyPath := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	_, port, _ := net.SplitHostPort(addr)
	h := NewHub(Options{DirectPort: atoi(port)})
	go ServeDirect(addr, certPath, keyPath, h.Handler())

	d := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	var host *websocket.Conn
	var err error
	for i := 0; i < 50; i++ { // сервер поднимается
		if host, _, err = d.Dial("wss://"+addr+"/radio/ws/host?f=1053&name=X", nil); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })
	l, _, err := d.Dial("wss://"+addr+"/radio/ws/listen", http.Header{"Origin": {"https://127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.WriteJSON(map[string]int{"tune": 1053})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				host.WriteMessage(websocket.BinaryMessage, frame(9))
			}
		}
	}()
	if b := readAudio(t, l); b[0] != 9 {
		t.Fatalf("кадр %d", b[0])
	}
	// страница знает порт прямого эфира
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	r, err := c.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	buf := new(strings.Builder)
	b := make([]byte, 1<<16)
	for {
		n, err := r.Body.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(buf.String(), `<meta name="air-port" content="`+port+`">`) {
		t.Fatal("на странице нет порта прямого эфира")
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
