// Package netutil — мелочи, общие для сервисов.
package netutil

import (
	"net"
	"strconv"
	"strings"
)

// DetectIP возвращает адрес, с которого машина ходит наружу (пакеты не отправляются).
func DetectIP() string {
	if c, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "127.0.0.1"
}

// ParseRange: "10000-10200" → 10000, 10200.
func ParseRange(s string, defMin, defMax int) (int, int) {
	p := strings.SplitN(s, "-", 2)
	if len(p) != 2 {
		return defMin, defMax
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(p[0]))
	b, err2 := strconv.Atoi(strings.TrimSpace(p[1]))
	if err1 != nil || err2 != nil || a <= 0 || b < a {
		return defMin, defMax
	}
	return a, b
}
