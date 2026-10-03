package mesh

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// TrustState — ступень доверия к узлу (§8). Обнаружить узел не значит доверять ему.
type TrustState int

const (
	Unknown       TrustState = iota // о узле только слышали
	Authenticated                   // подпись рукопожатия проверена: ключ у него
	Known                           // есть в реестре Master'а или в локальном списке
	Authorized                      // допуск подписан Master'ом или узел в списке доверенных
	Connected                       // авторизован и связь установлена
)

func (s TrustState) String() string {
	return [...]string{"UNKNOWN", "AUTHENTICATED", "KNOWN", "AUTHORIZED", "CONNECTED"}[s]
}

// Role — роль узла (§5).
type Role string

const (
	RoleMaster     Role = "master"
	RoleWorker     Role = "worker"
	RoleStandalone Role = "standalone"
	RoleEdge       Role = "edge"
)

// Admission — допуск в mesh, подписанный Master'ом. Узел предъявляет его соседям при
// рукопожатии; сосед проверяет подпись закреплённым ключом Master'а — Master для
// этого не нужен, и его отказ не рвёт связи (§6).
type Admission struct {
	Node    NodeID            `json:"node"`
	Pub     ed25519.PublicKey `json:"pub"`
	Role    Role              `json:"role"`
	Expires int64             `json:"exp"` // Unix с
	Master  NodeID            `json:"master"`
	Sig     []byte            `json:"sig"`
}

func (a *Admission) signedBytes() []byte {
	b, _ := json.Marshal([]any{a.Node, a.Pub, a.Role, a.Expires, a.Master})
	return b
}

// IssueAdmission — Master выписывает допуск.
func IssueAdmission(master *Identity, node NodeID, pub ed25519.PublicKey, role Role, ttl time.Duration) Admission {
	a := Admission{Node: node, Pub: pub, Role: role, Expires: time.Now().Add(ttl).Unix(), Master: master.ID}
	a.Sig = master.Sign(a.signedBytes())
	return a
}

var ErrAdmission = errors.New("допуск не действителен")

// Check проверяет допуск закреплённым ключом Master'а.
func (a *Admission) Check(masterPub ed25519.PublicKey, now time.Time) error {
	switch {
	case len(masterPub) != ed25519.PublicKeySize || IDFromPublicKey(masterPub) != a.Master:
		return fmt.Errorf("%w: выписан не нашим Master'ом", ErrAdmission)
	case IDFromPublicKey(a.Pub) != a.Node:
		return fmt.Errorf("%w: ключ не соответствует узлу", ErrAdmission)
	case now.Unix() > a.Expires:
		return fmt.Errorf("%w: истёк", ErrAdmission)
	case !ed25519.Verify(masterPub, a.signedBytes(), a.Sig):
		return fmt.Errorf("%w: подпись", ErrAdmission)
	}
	return nil
}

// Trust — локальное хранилище доверия узла: закреплённый ключ Master'а и список
// доверенных узлов, который переживает отказ Master'а (§8).
type Trust struct {
	MasterPub ed25519.PublicKey

	mu      sync.Mutex
	state   map[NodeID]TrustState
	trusted map[NodeID]bool // локальный список доверенных (файл или конфиг)
	known   map[NodeID]bool // узлы из реестра Master'а
}

func NewTrust(masterPub ed25519.PublicKey) *Trust {
	return &Trust{MasterPub: masterPub, state: map[NodeID]TrustState{},
		trusted: map[NodeID]bool{}, known: map[NodeID]bool{}}
}

// AddTrusted — узел в локальном списке доверенных.
func (t *Trust) AddTrusted(ids ...NodeID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		t.trusted[id] = true
	}
}

// MarkKnown — узел встретился в реестре Master'а.
func (t *Trust) MarkKnown(id NodeID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.known[id] = true
	if t.state[id] < Known && t.state[id] >= Authenticated {
		t.state[id] = Known
	}
}

// Evaluate — ступень доверия после рукопожатия: ключ подтверждён (подпись hello уже
// проверена) и, возможно, предъявлен допуск.
func (t *Trust) Evaluate(id NodeID, adm *Admission, now time.Time) TrustState {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := Authenticated
	if t.known[id] || t.trusted[id] {
		st = Known
	}
	if t.trusted[id] {
		st = Authorized
	} else if adm != nil && adm.Node == id && t.MasterPub != nil && adm.Check(t.MasterPub, now) == nil {
		st = Authorized
	}
	// Повторная оценка (второе соединение того же узла) не понижает CONNECTED, пока
	// узел авторизован: иначе дубль, который сейчас закроют, «отключил» бы живую связь.
	if st == Authorized && t.state[id] == Connected {
		return Connected
	}
	t.state[id] = st
	return st
}

// SetConnected — связь с авторизованным узлом установлена (или разорвана).
func (t *Trust) SetConnected(id NodeID, up bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case up && t.state[id] == Authorized:
		t.state[id] = Connected
	case !up && t.state[id] == Connected:
		t.state[id] = Authorized
	}
}

func (t *Trust) State(id NodeID) TrustState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state[id]
}

// LoadTrustedFile читает список доверенных NodeID (JSON-массив) — его ведёт оператор.
func (t *Trust) LoadTrustedFile(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ids []NodeID
	if err := json.Unmarshal(b, &ids); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	t.AddTrusted(ids...)
	return nil
}
