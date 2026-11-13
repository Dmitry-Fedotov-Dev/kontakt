// kontakt-ring — позвонить и сказать: трубка «Контакта» (телефон, ПК) звонит, качается и вибрирует, на
// дисплее — от кого и текст. Или прямой звонок на SIP-устройство: софтфон, приложение, IP-телефон.
//
//	kontakt-ring -line https://openline.one/?call=<линия> "сборка готова"
//	kontakt-ring -sip sip:100@192.168.1.20 -for 15s "сборка готова"
//	kontakt-ring -hook          # из хука Claude Code: событие — JSON на stdin (Stop, Notification)
//
// Куда звонить — флагами или из окружения: KONTAKT_LINE (ссылка или линия), KONTAKT_URL (станция, если
// в KONTAKT_LINE только линия; по умолчанию https://openline.one), KONTAKT_SIP (через запятую — несколько
// устройств). Заданы оба — звонят оба.
//
// Линия — ваша ссылка из трубки (кнопка «позвонить»): это и есть номер. Звонок слышат трубки, открытые с
// этой линией (вкладка, приложение). С -hook программа всегда выходит с кодом 0 — хук никогда не мешает
// Claude Code; ошибки — в stderr.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"kontakt/internal/ringer"
	"kontakt/internal/signal"
)

func main() {
	line := flag.String("line", os.Getenv("KONTAKT_LINE"), "линия трубки: ссылка https://…/?call=<линия> или сама линия")
	base := flag.String("url", orEnv("KONTAKT_URL", ""), "станция (если -line — не ссылка); по умолчанию https://openline.one")
	sipTo := flag.String("sip", os.Getenv("KONTAKT_SIP"), "SIP-устройства через запятую: sip:100@192.168.1.20[:5060]")
	from := flag.String("from", orEnv("KONTAKT_FROM", ""), "от кого (на дисплее); по умолчанию Kontakt, с -hook — Claude Code")
	ring := flag.Duration("for", 20*time.Second, "сколько звонит SIP-устройство")
	hook := flag.Bool("hook", false, "событие хука Claude Code — JSON на stdin; выход всегда 0")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "kontakt-ring [-line ссылка] [-sip sip:…] [-from имя] [текст]   |   kontakt-ring -hook < событие.json")
		flag.PrintDefaults()
	}
	flag.Parse()

	text := strings.Join(flag.Args(), " ")
	if *hook {
		t, ok := ringer.Hook(os.Stdin)
		if !ok {
			return
		}
		if text == "" {
			text = t
		}
		if *from == "" {
			*from = "Claude Code"
		}
	}
	if *from == "" {
		*from = "Kontakt"
	}
	if *line == "" && *sipTo == "" {
		fail(*hook, "куда звонить? -line (или KONTAKT_LINE) и/или -sip (KONTAKT_SIP)")
		return
	}

	ctx, stop := context.WithTimeout(context.Background(), *ring+15*time.Second)
	defer stop()
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := false
	report := func(err error, format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failed = true
			fmt.Fprintln(os.Stderr, "kontakt-ring:", err)
			return
		}
		if !*hook {
			fmt.Printf(format+"\n", a...)
		}
	}
	if *line != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := *base
			if b == "" {
				b = ringer.BaseOf(*line)
			}
			if b == "" {
				b = "https://openline.one"
			}
			n, err := ringer.Notify(ctx, b, signal.LineOf(*line), *from, text)
			if err == nil && n == 0 {
				err = fmt.Errorf("линию никто не слушает: откройте трубку с этой ссылкой (%s)", b)
			}
			report(err, "звонит трубок: %d", n)
		}()
	}
	for _, t := range strings.Split(*sipTo, ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if !strings.HasPrefix(t, "sip:") {
			t = "sip:" + t
		}
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			r, err := ringer.CallSIP(ctx, t, *from, text, *ring)
			switch {
			case err != nil:
				report(fmt.Errorf("%s: %w", t, err), "")
			case r.Answered:
				report(nil, "%s: ответили — положили", t)
			case r.Status == 486 || r.Status == 600:
				report(nil, "%s: занято", t)
			case r.Status == 603:
				report(nil, "%s: отклонили", t)
			case r.Status >= 300 && r.Status != 487:
				report(fmt.Errorf("%s: отказ %d", t, r.Status), "")
			default:
				report(nil, "%s: прозвонили", t)
			}
		}(t)
	}
	wg.Wait()
	if failed && !*hook {
		os.Exit(1)
	}
}

func fail(hook bool, msg string) {
	fmt.Fprintln(os.Stderr, "kontakt-ring:", msg)
	if !hook {
		os.Exit(2)
	}
}

func orEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
