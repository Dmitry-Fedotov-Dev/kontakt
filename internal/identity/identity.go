// Package identity — вечная кука, по которой «Контакт» узнаёт человека (и банит).
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

const CookieName = "kontakt_id"

// FromRequest достаёт идентификатор из куки; принимаются только 32 hex-символа.
func FromRequest(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil || len(c.Value) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(c.Value); err != nil {
		return ""
	}
	return c.Value
}

// New — случайные 128 бит.
func New() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
