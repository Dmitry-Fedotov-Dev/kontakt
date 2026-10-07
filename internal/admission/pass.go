package admission

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Пропуск: кука kontakt_pass = «уровень.срок.подпись», подпись — HMAC-SHA256 от (кука kontakt_id,
// уровень, срок) ключом сервера. Выдаётся заранее, при обычном заходе на страницу (/api/me), и
// живёт PassTTL — поэтому во время атаки свои проходят в резерв без похода в базу модерации и без
// «входа», который сам мог бы лечь под нагрузкой. Подделать нельзя без ключа; к чужой куке не
// подходит; отзывается баном (уровень пересчитывается при следующей выдаче) или сменой ключа.

const (
	PassCookie = "kontakt_pass"
	PassTTL    = 7 * 24 * time.Hour
)

// Passes — выдача и проверка пропусков; без ключа (Key == nil) пропусков нет, уровень — только из
// базы модерации.
type Passes struct {
	Key []byte
}

// LoadPasses читает ключ из файла (сырые байты, ≥ 32); пустой путь или нет файла — без пропусков.
func LoadPasses(path string) (*Passes, error) {
	if path == "" {
		return &Passes{}, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Passes{}, nil
	}
	if err != nil {
		return &Passes{}, err
	}
	if len(b) < 32 {
		return &Passes{}, fmt.Errorf("ключ пропусков %s короче 32 байт", path)
	}
	return &Passes{Key: b}, nil
}

func (p *Passes) sign(id string, lv Level, exp int64) string {
	m := hmac.New(sha256.New, p.Key)
	fmt.Fprintf(m, "%s|%d|%d", id, lv, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// Value — значение куки для id с уровнем lv.
func (p *Passes) Value(id string, lv Level, now time.Time) string {
	exp := now.Add(PassTTL).Unix()
	return fmt.Sprintf("%d.%d.%s", lv, exp, p.sign(id, lv, exp))
}

// Check — уровень по пропуску из куки (Anon — нет, чужой, просрочен или поддельный).
func (p *Passes) Check(r *http.Request, id string) Level {
	if p == nil || p.Key == nil || id == "" {
		return Anon
	}
	c, err := r.Cookie(PassCookie)
	if err != nil {
		return Anon
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return Anon
	}
	lv, err1 := strconv.Atoi(parts[0])
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || lv < int(Anon) || lv > int(Owner) || time.Now().Unix() > exp {
		return Anon
	}
	if !hmac.Equal([]byte(parts[2]), []byte(p.sign(id, Level(lv), exp))) {
		return Anon
	}
	return Level(lv)
}

// Issue ставит пропуск уровня lv (или снимает, если lv == Anon). Кука — на весь домен, как
// kontakt_id: рулетка и радио под /radio/ видят один пропуск.
func (p *Passes) Issue(w http.ResponseWriter, r *http.Request, id string, lv Level) {
	if p == nil || p.Key == nil || id == "" {
		return
	}
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	if lv == Anon {
		if _, err := r.Cookie(PassCookie); err == nil {
			http.SetCookie(w, &http.Cookie{Name: PassCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
		}
		return
	}
	if p.Check(r, id) == lv { // уже есть действующий — не трогаем (срок продлится при следующей смене)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: PassCookie, Value: p.Value(id, lv, time.Now()), Path: "/",
		MaxAge: int(PassTTL.Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}
