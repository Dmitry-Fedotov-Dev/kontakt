package radio

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kontakt/internal/moderation"
)

// Письма к лайкам: слушатель прикладывает к лайку короткое письмо ведущему. Видит его только
// ведущий, и не знает от кого: сервер отдаёт ему текст и номер письма, а кто отправил, помнит
// только сам — чтобы ведущий мог «не принимать от этого слушателя» или пожаловаться. Писем
// нигде не хранит и не пишет в журнал; номер → отправитель — в памяти, последние letterMemory.
//
// Слушатель: {"like":true,"letter":"…"} → ответ {"letter":"sent"|"too_fast"|…,"wait":секунд}.
// Ведущий получает {"letter":{"id":7,"text":"…"}}, шлёт {"block":7} или {"reportLetter":7}
// (ответ {"letterReport":"yellow"|"banned"|…}).

const (
	letterRunes  = 140
	letterMemory = 200 // столько последних писем станции ведущий может заблокировать или обжаловать
)

// letterEvery — письмо от одного человека не чаще; переменная — для тестов.
var letterEvery = 30 * time.Second

// letterKey — по куке; без куки (без модерации) — по соединению.
func letterKey(l *listener) string {
	if l.id != "" {
		return l.id
	}
	return fmt.Sprintf("conn-%p", l)
}

func (h *Hub) letter(l *listener, text string) (res string, wait int) {
	defer func() { h.lettersSent.Inc(res) }()
	text = clean(text, letterRunes)
	if text == "" {
		return "empty", 0
	}
	// бан в эфир закрывает и письма (слушать можно); проверка — вне замка, Mod может быть удалённым
	if h.opt.Mod != nil && l.id != "" && h.opt.Mod.Status(l.id, moderation.Air).Banned {
		return "banned", 0
	}
	h.mu.Lock()
	s, now, key := l.heard, time.Now(), letterKey(l)
	switch {
	case s == nil || h.st[s.freq] != s:
		h.mu.Unlock()
		return "no_station", 0
	case l.id != "" && s.host == l.id:
		h.mu.Unlock()
		return "self", 0
	}
	if last, ok := h.letterTimes[key]; ok && now.Sub(last) < letterEvery {
		h.mu.Unlock()
		return "too_fast", ceilSec(letterEvery - now.Sub(last))
	}
	h.letterTimes[key] = now
	if len(h.letterTimes) > 10000 { // старые отметки больше не нужны
		for k, t := range h.letterTimes {
			if now.Sub(t) >= letterEvery {
				delete(h.letterTimes, k)
			}
		}
	}
	if s.blocked[key] { // ведущий не принимает от него: тот думает, что отправил
		h.mu.Unlock()
		return "sent", ceilSec(letterEvery)
	}
	h.letterSeq++
	id := h.letterSeq
	if len(s.letterFrom) >= letterMemory {
		s.letterFrom = map[uint64]string{}
	}
	s.letterFrom[id] = key
	h.mu.Unlock()
	s.sayHost(map[string]any{"letter": map[string]any{"id": id, "text": text}})
	return "sent", ceilSec(letterEvery)
}

// ceilSec — секунды с округлением вверх: «подождите 0 с» не бывает.
func ceilSec(d time.Duration) int { return int((d + time.Second - 1) / time.Second) }

// hostLetterAction — ведущий заблокировал отправителя письма или пожаловался на него.
func (h *Hub) hostLetterAction(s *station, data []byte) {
	var m struct {
		Block        uint64 `json:"block"`
		ReportLetter uint64 `json:"reportLetter"`
	}
	if json.Unmarshal(data, &m) != nil || (m.Block == 0 && m.ReportLetter == 0) {
		return
	}
	id := m.Block
	if id == 0 {
		id = m.ReportLetter
	}
	h.mu.Lock()
	from := s.letterFrom[id]
	if from != "" {
		s.blocked[from] = true // и при жалобе тоже: письма от него больше не нужны
	}
	h.mu.Unlock()
	if m.ReportLetter == 0 || from == "" {
		return
	}
	res := "noted"
	if h.opt.Mod != nil && s.host != "" && !strings.HasPrefix(from, "conn-") { // без куки жаловаться не на кого
		r, err := h.opt.Mod.Report(from, s.host, moderation.Air)
		if err != nil && r == "" {
			r = "error"
		}
		res = r
	}
	h.letterReports.Inc(res)
	s.sayHost(map[string]string{"letterReport": res})
}
