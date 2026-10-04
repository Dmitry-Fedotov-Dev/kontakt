// Package notify — Telegram-бот владельца: тревоги Prometheus, события модерации и команды
// модерации из одного чата.
//
// Пишет только в чат владельца (TELEGRAM_CHAT_ID) и слушает только его: сообщения из других
// чатов молча пропускаются. В сообщениях — ключи записей модерации (хеш куки), зоны и
// названия тревог; ни кук, ни IP, ни названий станций.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Bot — минимальный клиент Bot API: отправка, правка, кнопки и long polling.
type Bot struct {
	API   string // https://api.telegram.org (в тестах — подделка)
	Token string
	Chat  int64
	c     *http.Client
}

func NewBot(api, token string, chat int64) *Bot {
	return &Bot{API: api, Token: token, Chat: chat, c: &http.Client{Timeout: 40 * time.Second}}
}

// Button — кнопка под сообщением; Data приходит обратно в callback_query.
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

type Update struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
	Callback *struct {
		ID      string `json:"id"`
		Data    string `json:"data"`
		Message *struct {
			ID   int64  `json:"message_id"`
			Text string `json:"text"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

func (b *Bot) call(method string, params, out any) error {
	body, _ := json.Marshal(params)
	resp, err := b.c.Post(b.API+"/bot"+b.Token+"/"+method, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: нет связи", method) // в тексте ошибки net/http был бы URL с токеном
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: %s", method, resp.Status)
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func keyboard(rows [][]Button) any {
	if len(rows) == 0 {
		return nil
	}
	return map[string]any{"inline_keyboard": rows}
}

// Send — сообщение в чат владельца (HTML: экранирует вызывающий).
func (b *Bot) Send(html string, rows ...[]Button) error {
	p := map[string]any{"chat_id": b.Chat, "text": html, "parse_mode": "HTML", "disable_web_page_preview": true}
	if k := keyboard(rows); k != nil {
		p["reply_markup"] = k
	}
	return b.call("sendMessage", p, nil)
}

// Edit заменяет текст сообщения и убирает кнопки.
func (b *Bot) Edit(msgID int64, html string) error {
	return b.call("editMessageText", map[string]any{"chat_id": b.Chat, "message_id": msgID, "text": html,
		"parse_mode": "HTML", "disable_web_page_preview": true}, nil)
}

func (b *Bot) Answer(callbackID, text string) error {
	return b.call("answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text}, nil)
}

// Updates — long polling: ждёт до wait новых сообщений и нажатий начиная с offset.
func (b *Bot) Updates(offset int64, wait time.Duration) ([]Update, error) {
	var u []Update
	err := b.call("getUpdates", map[string]any{"offset": offset, "timeout": int(wait.Seconds()),
		"allowed_updates": []string{"message", "callback_query"}}, &u)
	return u, err
}
