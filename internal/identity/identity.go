// Package identity — вечная кука, по которой «Контакт» и радио узнают человека (и банят).
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
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

// Ensure ставит куку, если её нет, и возвращает идентификатор. Кука — на весь домен (Path=/):
// рулетка и радио под /radio/ видят одного и того же человека.
func Ensure(w http.ResponseWriter, r *http.Request) string {
	if id := FromRequest(r); id != "" {
		return id
	}
	id := New()
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: id, Path: "/",
		MaxAge: 10 * 365 * 24 * 3600, Expires: time.Now().AddDate(10, 0, 0),
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	r.AddCookie(&http.Cookie{Name: CookieName, Value: id}) // чтобы и этот запрос уже был «с кукой»
	return id
}
