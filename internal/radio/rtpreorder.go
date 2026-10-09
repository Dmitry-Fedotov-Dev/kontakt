package radio

// Звук ведущего по UDP (rtc.go) приходит пакетами RTP: какие-то теряются, какие-то приходят не по
// порядку. rtpReorder выдаёт кадры строго по номерам: ждёт опоздавший не дольше reorderHold
// кадров, на месте потерянного — замена (PLC), повтор с номером назад — выкидывается. Ровный поток
// 50 кадров/с нужен приёмникам: дыра в нём — щелчок и рост их запаса (RX в странице).
//
// Часов у буфера нет: кадр уходит, как только пришёл следующий по номеру, — темп задают часы
// звуковой карты ведущего, как и на пути WebSocket. Иначе пришлось бы подстраивать два тактовых
// генератора (у браузера на стенде было 50,4–51,2 кадра/с).

const (
	// reorderHold — сколько кадров после дыры ждём опоздавший: 3 × 20 мс. Дольше — задержка эфира,
	// а перестановка в интернете почти всегда в пределах пары пакетов.
	reorderHold = 3
	// reorderFill — дыру больше этого (1 с) не заполняем: связь пропадала, начинаем с нового места;
	// приёмники переживут разрыв так же, как разрыв WebSocket.
	reorderFill = 50
	// plcFrames — сколько кадров подряд замена повторяет прошлый звук (тише на 6 дБ каждый раз);
	// дальше — тишина: повтор длиннее 60 мс слышен как «заело».
	plcFrames = 3
)

type rtpReorder struct {
	started bool
	next    uint16            // номер кадра, который уйдёт следующим
	newest  uint16            // самый новый из пришедших
	held    map[uint16][]byte // пришедшие раньше очереди

	prev      []byte // последний ушедший кадр — для замены
	concealed int    // замен подряд

	ok, lost, late uint64 // счётчики с начала: выданы, заменены (потеряны), опоздали или повтор
}

func newRTPReorder() *rtpReorder { return &rtpReorder{held: map[uint16][]byte{}} }

// push — пришёл кадр с номером seq; возвращает кадры, которые можно отдавать в эфир, по порядку.
func (r *rtpReorder) push(seq uint16, frame []byte) (out [][]byte) {
	if !r.started {
		r.started, r.next, r.newest = true, seq, seq
	}
	d := int16(seq - r.next)
	if _, dup := r.held[seq]; dup || d < 0 {
		r.late++
		return nil
	}
	if int(d) > reorderFill { // долгий провал: что было — отдать, дальше — с нового места
		r.lost += uint64(int(d) - len(r.held))
		out = r.drain()
		r.next, r.newest = seq, seq
	}
	r.held[seq] = frame
	if int16(seq-r.newest) > 0 {
		r.newest = seq
	}
	for len(r.held) > 0 {
		if f, ok := r.held[r.next]; ok {
			delete(r.held, r.next)
			out = append(out, f)
			r.prev, r.concealed = f, 0
			r.ok++
			r.next++
			continue
		}
		if int16(r.newest-r.next) < reorderHold { // ждём опоздавший
			break
		}
		out = append(out, r.conceal())
		r.lost++
		r.next++
	}
	return out
}

// drain — все ждущие кадры по порядку, без заполнения дыр между ними.
func (r *rtpReorder) drain() (out [][]byte) {
	for n := 0; len(r.held) > 0 && n <= reorderFill+reorderHold; n++ {
		if f, ok := r.held[r.next]; ok {
			delete(r.held, r.next)
			out = append(out, f)
			r.prev = f
			r.ok++
		}
		r.next++
	}
	clear(r.held)
	return out
}

// conceal — замена потерянного кадра: прошлый звук тише на 6 дБ, после plcFrames — тишина.
func (r *rtpReorder) conceal() []byte {
	r.concealed++
	f := make([]byte, FrameBytes)
	if r.prev == nil || r.concealed > plcFrames {
		for i := range f {
			f[i] = 0xFF // μ-law ноль
		}
		return f
	}
	for i, b := range r.prev {
		if i < FrameBytes {
			f[i] = linearToUlaw(ulawToLinear[b] * 0.5)
		}
	}
	r.prev = f
	return f
}
