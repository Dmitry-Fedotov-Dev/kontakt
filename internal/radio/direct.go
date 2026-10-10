package radio

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// Прямой HTTPS для сокетов эфира: радио само шифрует /radio/ws/* на отдельном порту
// (DirectPort, обычно 8443), и звук не идёт через Caddy — на проде тот тратил на звук больше
// процессора, чем само радио. Сертификат — тот же, что Caddy получает от Let's Encrypt: его копию
// кладёт служба kontakt-tls-sync, радио перечитывает файлы, когда они меняются. Страница узнаёт
// порт из <meta name="air-port"> и, если порт недоступен (закрыт в сети), уходит на путь через Caddy.

// certReloader отдаёт сертификат из файлов и перечитывает их не чаще раза в минуту, если они
// поменялись (продление раз в ~60 дней).
type certReloader struct {
	certPath, keyPath string
	mu                sync.Mutex
	cert              *tls.Certificate
	mod               time.Time
	checked           time.Time
}

func (c *certReloader) load() error {
	st, err := os.Stat(c.certPath)
	if err != nil {
		return err
	}
	if c.cert != nil && !st.ModTime().After(c.mod) {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(c.certPath, c.keyPath)
	if err != nil {
		return err
	}
	c.cert, c.mod = &cert, st.ModTime()
	return nil
}

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) > time.Minute {
		c.checked = time.Now()
		if err := c.load(); err != nil && c.cert == nil {
			return nil, err
		} else if err != nil {
			log.Printf("прямой HTTPS: сертификат не перечитался, работаю со старым: %v", err)
		}
	}
	return c.cert, nil
}

// ServeDirect — прямой HTTPS на addr с сертификатом из файлов; блокирует.
func ServeDirect(addr, certPath, keyPath string, handler http.Handler) error {
	c := &certReloader{certPath: certPath, keyPath: keyPath}
	if err := c.load(); err != nil {
		return fmt.Errorf("прямой HTTPS: %w", err)
	}
	c.checked = time.Now()
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.get},
		// Порт смотрит в интернет напрямую, и сканеры сыплют неудачными рукопожатиями: net/http пишет
		// их в журнал вместе с IP («TLS handshake error from …»). IP в журнал не пишем — молчим.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	return srv.ListenAndServeTLS("", "")
}

// airPortMeta — порт прямого эфира для страницы (пусто — его нет, сокеты через web/Caddy); заодно —
// можно ли ведущему ставить логотип самому: "1" — всем, "bot" — по доступу из бота вещателей, без
// метатега ячейка логотипа скрыта.
func (h *Hub) airPortMeta() string {
	m := ""
	if h.opt.DirectPort != 0 {
		m = fmt.Sprintf(`<meta name="air-port" content="%d">`+"\n", h.opt.DirectPort)
	}
	if mode := h.logoMode(); mode != "" {
		m += `<meta name="host-logos" content="` + mode + `">` + "\n"
	}
	return m
}
