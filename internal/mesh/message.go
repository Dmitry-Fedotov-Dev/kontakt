package mesh

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Типы управляющих сообщений.
const (
	MsgHello     = "hello"     // рукопожатие: кто я, мой допуск
	MsgRegister  = "register"  // Worker → Master
	MsgAdmission = "admission" // Master → Worker: допуск и соседи
	MsgHeartbeat = "heartbeat" // Worker → Master
	MsgPeers     = "peers"     // запрос/ответ соседей
	MsgPing      = "ping"      // замер ребра
	MsgPong      = "pong"
	MsgTopology  = "topology" // объявление рёбер узла (ограничено max_hops)
	MsgData      = "data"     // полезная нагрузка поверх mesh (фаза 2)
)

// Envelope — подписанное управляющее сообщение. Подпись покрывает тип, отправителя,
// время, номер и тело; ключ отправителя едет в конверте, а NodeID обязан ему
// соответствовать — подставить чужое имя нельзя.
type Envelope struct {
	Type string            `json:"t"`
	From NodeID            `json:"f"`
	Pub  ed25519.PublicKey `json:"k"`
	TS   int64             `json:"ts"` // Unix нс
	Seq  uint64            `json:"n"`
	Body json.RawMessage   `json:"b,omitempty"`
	Sig  []byte            `json:"s"`
}

func (e *Envelope) signedBytes() []byte {
	var b bytes.Buffer
	b.WriteString(e.Type)
	b.WriteByte(0)
	b.WriteString(string(e.From))
	b.WriteByte(0)
	var n [16]byte
	binary.BigEndian.PutUint64(n[:8], uint64(e.TS))
	binary.BigEndian.PutUint64(n[8:], e.Seq)
	b.Write(n[:])
	b.Write(e.Body)
	return b.Bytes()
}

// Signer выпускает подписанные конверты с растущим номером.
type Signer struct {
	id  *Identity
	mu  sync.Mutex
	seq uint64
	now func() time.Time
}

// NewSigner — номер начинается с текущего времени в нс, а не с нуля: иначе после
// перезапуска узла соседи отвергали бы его сообщения как повтор.
func NewSigner(id *Identity) *Signer {
	return &Signer{id: id, now: time.Now, seq: uint64(time.Now().UnixNano())}
}

// Seal упаковывает тело в подписанный конверт.
func (s *Signer) Seal(typ string, body any) (*Envelope, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	e := &Envelope{Type: typ, From: s.id.ID, Pub: s.id.Pub, TS: s.now().UnixNano(), Seq: seq, Body: raw}
	e.Sig = s.id.Sign(e.signedBytes())
	return e, nil
}

// Ошибки проверки.
var (
	ErrBadSignature = errors.New("подпись не сходится")
	ErrIDMismatch   = errors.New("NodeID не соответствует ключу")
	ErrStale        = errors.New("сообщение вне окна по времени")
	ErrReplay       = errors.New("повтор сообщения")
)

// Verifier проверяет конверты: подпись, имя, окно по времени и повтор.
type Verifier struct {
	Window time.Duration // допустимое расхождение часов, по умолчанию 30 с
	now    func() time.Time
	mu     sync.Mutex
	last   map[NodeID]*replayWindow
}

// replayWindow — окно защиты от повтора, как в IPsec (RFC 4303): номер принимается,
// если он новее максимума или внутри окна и ещё не встречался. Строгое «номер обязан
// расти» не годится: на одно соединение пишут несколько горутин (пробы, объявления,
// пересылка), и Seal(5), Seal(6) уходят на провод как 6, 5 — пятый отвергался бы как
// повтор, и пробы молча «терялись».
type replayWindow struct {
	max  uint64
	bits [replayBits / 64]uint64 // бит i — номер max-i уже был
}

const replayBits = 1024

func (w *replayWindow) accept(seq uint64) bool {
	switch {
	case seq > w.max:
		shift := seq - w.max
		if shift >= replayBits {
			w.bits = [replayBits / 64]uint64{}
		} else {
			for ; shift > 0; shift-- { // сдвиг на 1 бит за шаг: окно маленькое, шаги редки
				carry := uint64(0)
				for i := range w.bits {
					next := w.bits[i] >> 63
					w.bits[i] = w.bits[i]<<1 | carry
					carry = next
				}
			}
		}
		w.max = seq
		w.bits[0] |= 1
		return true
	case w.max-seq >= replayBits:
		return false // слишком старый: за окном
	default:
		i := w.max - seq
		if w.bits[i/64]&(1<<(i%64)) != 0 {
			return false
		}
		w.bits[i/64] |= 1 << (i % 64)
		return true
	}
}

func NewVerifier() *Verifier {
	return &Verifier{Window: 30 * time.Second, now: time.Now, last: map[NodeID]*replayWindow{}}
}

// VerifySig — подпись, имя и окно по времени, без проверки повтора. Для
// пересылаемых объявлений: их дубликаты гасятся по версии, а не по номеру, иначе
// свежий ping от того же узла «состарил» бы его объявление, идущее другим путём.
func (v *Verifier) VerifySig(e *Envelope) error {
	if len(e.Pub) != ed25519.PublicKeySize || IDFromPublicKey(e.Pub) != e.From {
		return ErrIDMismatch
	}
	if !ed25519.Verify(e.Pub, e.signedBytes(), e.Sig) {
		return ErrBadSignature
	}
	if d := v.now().Sub(time.Unix(0, e.TS)); d > v.Window || d < -v.Window {
		return fmt.Errorf("%w: %v", ErrStale, d.Round(time.Second))
	}
	return nil
}

// Verify проверяет конверт целиком; повтор того же сообщения (перехваченного и
// отправленного снова) отвергается, перестановка соседних — нет (replayWindow).
func (v *Verifier) Verify(e *Envelope) error {
	if err := v.VerifySig(e); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	w := v.last[e.From]
	if w == nil {
		w = &replayWindow{}
		v.last[e.From] = w
	}
	if !w.accept(e.Seq) {
		return ErrReplay
	}
	return nil
}

// Decode разбирает тело конверта.
func (e *Envelope) Decode(v any) error { return json.Unmarshal(e.Body, v) }
