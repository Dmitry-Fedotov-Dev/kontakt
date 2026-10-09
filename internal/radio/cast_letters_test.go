package radio

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Студия видит то же, что ведущий в браузере: слушателей, лайки, письма; «не принимать письма»
// работает — следующее письмо от того же слушателя до станции не доходит.
func TestCastLettersAndLikes(t *testing.T) {
	old := letterEvery
	letterEvery = 0
	defer func() { letterEvery = old }()
	h, srv, _, api := castsForTest(t)
	call(t, api, "POST", "/stations/api/station", []byte(`{"f":"93.4","name":"X"}`))
	call(t, api, "POST", "/stations/api/upload?f=934&name=a.mp3", []byte("audio"))
	call(t, api, "POST", "/stations/api/on?f=934", nil)
	waitStations(t, h, func(s []Station) bool { return len(s) == 1 })

	l, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/listen", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() { // читаем, чтобы сокет жил
		for {
			if _, _, err := l.ReadMessage(); err != nil {
				return
			}
		}
	}()
	l.WriteJSON(map[string]int{"tune": 934})
	time.Sleep(100 * time.Millisecond)
	l.WriteJSON(map[string]bool{"like": true})
	l.WriteJSON(map[string]string{"letter": "привет из теста"})

	var in castInfo
	get := func() {
		in = castInfo{}
		json.Unmarshal(call(t, api, "GET", "/stations/api/letters?f=934", nil).Body.Bytes(), &in)
	}
	deadline := time.Now().Add(5 * time.Second)
	for get(); len(in.Letters) == 0 || in.Likes == 0 || in.Listeners == 0; get() {
		if time.Now().After(deadline) {
			t.Fatalf("студия не видит: %+v", in)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if in.Letters[0].Text != "привет из теста" || in.Likes != 1 || in.Listeners != 1 {
		t.Fatalf("студия: %+v", in)
	}
	if w := call(t, api, "POST", "/stations/api/letter?f=934&op=block&id="+jsonNum(in.Letters[0].ID), nil); w.Code != 200 {
		t.Fatalf("block: %d %s", w.Code, w.Body)
	}
	time.Sleep(200 * time.Millisecond) // действие уходит радио со следующим кадром
	l.WriteJSON(map[string]string{"letter": "второе"})
	time.Sleep(300 * time.Millisecond)
	get()
	if len(in.Letters) != 1 || !in.Letters[0].Blocked {
		t.Fatalf("после «не принимать» пришло: %+v", in.Letters)
	}
}

func jsonNum(v uint64) string { b, _ := json.Marshal(v); return string(b) }
