// Package config — конфигурация поведения сигнального сервиса с горячей перезагрузкой.
//
// Файл перечитывается при изменении (проверка раз в 500 мс), по SIGHUP или через админ-ручку.
// Новый конфиг сначала проверяется, затем объявляется («через 1 с применю») и ровно через
// apply_delay_ms атомарно подменяется. Идущие разговоры не трогаются, новые звонки сразу
// получают новое поведение.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	// Звук линии, если абонент не выбрал свой.
	DefaultLine string `json:"default_line"`
	// Какие режимы доступны в интерфейсе: "64", "32", "16", "8", "clean".
	AllowedLines []string `json:"allowed_lines"`
	// Бан (вечный) — когда за ban_window_days пожаловались столько РАЗНЫХ людей; до этого —
	// жёлтая карточка. 1 — бан с первой жалобы. Действует и на рулетку, и на эфир радио.
	BanReporters int `json:"ban_reporters"`
	// Сколько дней жалоба идёт в зачёт.
	BanWindowDays int `json:"ban_window_days"`
	// Жалобы новичка не идут в зачёт, пока у него меньше стольких сессий (разговор от 30 с
	// или 5 минут у приёмника); 0 — засчитывать всем.
	TrustTalks int `json:"trust_talks"`
	// Сколько секунд после разговора на собеседника ещё можно пожаловаться.
	ReportWindowSec int `json:"report_window_sec"`
	// Не больше стольких жалоб в час от одного человека (защита от массовых ложных жалоб).
	MaxReportsPerHour int `json:"max_reports_per_hour"`
	// Ограничение длины разговора в минутах, 0 — без ограничения.
	MaxCallMinutes int `json:"max_call_minutes"`
	// Техработы: новые звонки получают 503, идущие продолжаются.
	Maintenance bool `json:"maintenance"`
	// Страница поддержки (Патреон и т.п.) — ссылка «support» в трубке и радио; пусто — ссылки нет.
	DonateURL string `json:"donate_url"`
	// Через сколько миллисекунд после объявления применять новый конфиг.
	ApplyDelayMs int `json:"apply_delay_ms"`
}

func Default() Config {
	return Config{DefaultLine: "32", AllowedLines: []string{"64", "32", "16", "8"}, BanReporters: 2, BanWindowDays: 7,
		TrustTalks: 3, ReportWindowSec: 120, MaxReportsPerHour: 10, ApplyDelayMs: 1000}
}

func (c Config) Validate() error {
	ok := []string{"64", "32", "16", "8", "clean"}
	if !slices.Contains(ok, c.DefaultLine) {
		return fmt.Errorf("default_line: %q, можно %v", c.DefaultLine, ok)
	}
	if len(c.AllowedLines) == 0 {
		return fmt.Errorf("allowed_lines пуст")
	}
	for _, l := range c.AllowedLines {
		if !slices.Contains(ok, l) {
			return fmt.Errorf("allowed_lines: %q, можно %v", l, ok)
		}
	}
	if c.BanReporters < 1 || c.BanWindowDays < 1 {
		return fmt.Errorf("ban_reporters и ban_window_days — от 1")
	}
	if c.DonateURL != "" && !strings.HasPrefix(c.DonateURL, "https://") {
		return fmt.Errorf("donate_url: только https://…, а не %q", c.DonateURL)
	}
	if c.TrustTalks < 0 || c.ReportWindowSec < 0 || c.MaxReportsPerHour < 0 || c.MaxCallMinutes < 0 || c.ApplyDelayMs < 0 {
		return fmt.Errorf("отрицательные числа не допускаются")
	}
	return nil
}

func Parse(b []byte) (Config, error) {
	c := Default()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // опечатка в ключе не должна молча игнорироваться
	if err := dec.Decode(&c); err != nil {
		if strings.Contains(err.Error(), `"ban_policy"`) {
			err = fmt.Errorf(`%w: ban_policy заменён на ban_reporters ("instant" = 1, "yellow" = 2) и ban_window_days`, err)
		}
		return Config{}, err
	}
	return c, c.Validate()
}

// Live хранит текущий конфиг и умеет его подменять.
type Live struct {
	path    string
	cur     atomic.Pointer[Config]
	mu      sync.Mutex
	lastSum [32]byte
	pending *time.Timer
	// OnApply вызывается после подмены (старый, новый).
	OnApply func(old, new Config)
}

// Load читает файл; если path пуст или файла нет — работает на значениях по умолчанию.
func Load(path string) (*Live, error) {
	l := &Live{path: path}
	c := Default()
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if c, err = Parse(b); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			l.lastSum = sha256.Sum256(b)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	l.cur.Store(&c)
	return l, nil
}

func (l *Live) Get() Config { return *l.cur.Load() }

// Watch опрашивает файл и перезагружает его при изменении.
func (l *Live) Watch(stop <-chan struct{}) {
	if l.path == "" {
		return
	}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			l.Reload(false)
		}
	}
}

// Reload перечитывает файл. force — перезагрузить, даже если содержимое не менялось (SIGHUP).
// Возвращает текст для журнала/админки.
func (l *Live) Reload(force bool) (string, error) {
	if l.path == "" {
		return "", fmt.Errorf("конфиг-файл не задан")
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	l.mu.Lock()
	if sum == l.lastSum && !force {
		l.mu.Unlock()
		return "без изменений", nil
	}
	l.lastSum = sum
	l.mu.Unlock()

	c, err := Parse(b)
	if err != nil {
		log.Printf("конфиг: новый файл с ошибкой, оставляю прежний: %v", err)
		return "", err
	}
	return l.Schedule(c), nil
}

// Schedule объявляет новый конфиг и применяет его через ApplyDelayMs.
func (l *Live) Schedule(c Config) string {
	delay := time.Duration(c.ApplyDelayMs) * time.Millisecond
	msg := fmt.Sprintf("конфиг: получен новый, применю через %s: %s", delay, diff(l.Get(), c))
	log.Print(msg)
	l.mu.Lock()
	if l.pending != nil {
		l.pending.Stop()
	}
	l.pending = time.AfterFunc(delay, func() {
		old := l.Get()
		l.cur.Store(&c)
		log.Printf("конфиг: применён")
		if l.OnApply != nil {
			l.OnApply(old, c)
		}
	})
	l.mu.Unlock()
	return msg
}

func diff(a, b Config) string {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var ma, mb map[string]any
	json.Unmarshal(ja, &ma)
	json.Unmarshal(jb, &mb)
	var out []string
	for k, v := range mb {
		if fmt.Sprint(ma[k]) != fmt.Sprint(v) {
			out = append(out, fmt.Sprintf("%s %v → %v", k, ma[k], v))
		}
	}
	if len(out) == 0 {
		return "без изменений по значениям"
	}
	slices.Sort(out)
	return fmt.Sprint(out)
}
