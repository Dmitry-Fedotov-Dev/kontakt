package netutil

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// LocalOnly — защита админ-порта (signal, радио, бот): он слушает только localhost, но браузер
// владельца с запущенным стендом или SSH-туннелем могла бы попросить о запросе любая открытая
// страница. Host — только localhost (иначе DNS rebinding: чужой домен, указывающий на 127.0.0.1,
// читал бы ответы), запрос с Origin — только со своей страницы (иначе CSRF: бан, разбан, жалобы).
// curl, Prometheus и сервисы друг другу Origin не шлют — им можно.
//
// GET /metrics и /healthz — с любым Host: Prometheus в Docker Desktop приходит как
// host.docker.internal. В метриках нет ничего о людях (только счётчики), чужая страница с Origin
// их всё равно не получит.
func LocalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		open := r.Method == http.MethodGet && (r.URL.Path == "/metrics" || r.URL.Path == "/healthz")
		if !open && !isLocalHost(r.Host) {
			http.Error(w, "только через localhost", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "чужая страница", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLocalHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

// SameSite — для CheckOrigin сокетов: браузер может открыть сокет только со своей страницы, иначе
// чужой сайт встроит рулетку или эфир к себе (CSWSH). Без Origin — не браузер (софтфоны, xk6-sip,
// приложения, генератор нагрузки), им можно. Сравнивается имя хоста без порта: страница и сокет
// бывают на разных портах (прямой эфир на 8443).
func SameSite(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(u.Hostname(), host)
}
