package radio

// Фоновый приёмник: /stream/{freq} — волна обычным аудиопотоком (MP3 64 кбит/с) для <audio src>.
// Страница слушает через Web Audio, а его Android Chrome глушит, как только гаснет экран:
// воспроизведение из Web Audio (и <audio> с MediaStream) он медиаплеером не считает. Поток по
// HTTP — как у любого интернет-радио: играет с погашенным экраном и показывает плеер на экране
// блокировки. Страница переключается на него, когда её прячут, и обратно, когда показывают.
//
// Почему MP3, а не WAV, который не надо кодировать: Chrome начинает играть поток, только накопив
// данные, — для MP3 это ~32 КБ (64 кбит/с — 4 с), для WAV ~230 КБ (14 с при 16 бит 8 кГц; замер
// 11.10.2026 в Chrome 141). Подложить тишину в начало не помогает: ждёт тех же секунд звука.
// Кодирует ffmpeg (он уже нужен станциям с сервера) — свой процесс на каждый фоновый приёмник:
// lame на 8 кГц почти не ест процессора (0,3 % ядра), но ~14 МБ своей памяти на процесс, а у службы
// радио на всё MemoryMax=450M (само радио — ~30 МБ, замер 11.10.2026). Отсюда streamMax.
//
// Поток — такой же приёмник, как сокет: место в воротах слушателей, станция видит его в числе
// слушателей. Шума между станциями нет: станция молчит или ушла — идёт тишина, вернулась — звук.

import (
	"io"
	"net/http"
	"os/exec"
	"sync/atomic"
	"time"

	"kontakt/internal/identity"
)

const (
	streamRate = 8000
	streamMax  = 20 // фоновых приёмников (процессов ffmpeg) одновременно, см. выше
	// streamGap — сколько звука может не хватать, прежде чем в кодер пойдёт тишина: станция
	// молчит или её нет. Меньше — тишина вклинивается в паузы живого эфира (пачки по 100 мс,
	// рывки сети ведущего); больше — браузер успевает остановиться «за нехваткой данных».
	streamGap  = 500 * time.Millisecond
	streamTick = 100 * time.Millisecond
	ulawSilent = 0xff
)

var streamsLive atomic.Int32

func (h *Hub) serveStream(w http.ResponseWriter, r *http.Request) {
	f, ok := ParseFreq(r.PathValue("freq"))
	if !ok {
		http.Error(w, "frequency out of range 87.5–108.0", http.StatusNotFound)
		return
	}
	if h.ffmpeg == "" {
		http.Error(w, "фонового приёма нет: на сервере не установлен ffmpeg", http.StatusServiceUnavailable)
		return
	}
	if streamsLive.Add(1) > streamMax {
		streamsLive.Add(-1)
		http.Error(w, "фоновых приёмников слишком много — попробуйте позже", http.StatusServiceUnavailable)
		return
	}
	defer streamsLive.Add(-1)
	id := identity.FromRequest(r)
	tk, res := h.admit(r, id, &h.listenGate)
	h.listenAdmit.Inc(string(res))
	if tk == nil {
		h.rejected.Inc("listeners_full")
		http.Error(w, "приёмников слишком много — попробуйте позже", http.StatusServiceUnavailable)
		return
	}
	defer tk.Release()

	cmd := exec.CommandContext(r.Context(), h.ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "mulaw", "-ar", "8000", "-ac", "1", "-i", "pipe:0",
		"-c:a", "libmp3lame", "-b:a", "64k", "-write_xing", "0", "-id3v2_version", "0", "-flush_packets", "1",
		"-f", "mp3", "pipe:1")
	in, err1 := cmd.StdinPipe()
	out, err2 := cmd.StdoutPipe()
	if err1 != nil || err2 != nil || cmd.Start() != nil {
		http.Error(w, "кодер не запустился", http.StatusServiceUnavailable)
		return
	}
	defer cmd.Wait()
	defer in.Close()

	l := &listener{id: id, out: make(chan []byte, listenerBuf), ctl: make(chan []byte, 1)}
	h.mu.Lock()
	h.ls[l] = struct{}{}
	h.sessions.Inc()
	h.mu.Unlock()
	h.streams.Inc()
	h.tune(l, f)
	defer func() {
		h.mu.Lock()
		if s := h.st[l.tuned]; s != nil {
			delete(s.subs, l)
		}
		delete(h.ls, l)
		h.seatLeftLocked(id)
		h.mu.Unlock()
	}()
	go feedEncoder(r, l, in)

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	buf := make([]byte, 4096)
	for {
		n, err := out.Read(buf)
		if n > 0 {
			rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, werr := w.Write(buf[:n]); werr != nil || rc.Flush() != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// feedEncoder — звук станции в кодер; звука не хватает дольше streamGap — тишина до отставания
// в 250 мс: иначе кодер замолкает, а браузер останавливается и ждёт.
func feedEncoder(r *http.Request, l *listener, in io.WriteCloser) {
	defer in.Close()
	start, samples := time.Now(), 0
	tick := time.NewTicker(streamTick)
	defer tick.Stop()
	silence := make([]byte, streamRate)
	for i := range silence {
		silence[i] = ulawSilent
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-l.out:
			samples += len(b)
			if _, err := in.Write(b); err != nil {
				return
			}
		case <-tick.C:
			behind := int(time.Since(start).Seconds()*streamRate) - samples
			if behind > int(streamGap.Seconds()*streamRate) {
				n := min(behind-streamRate/4, len(silence))
				samples += n
				if _, err := in.Write(silence[:n]); err != nil {
					return
				}
			}
		}
	}
}
