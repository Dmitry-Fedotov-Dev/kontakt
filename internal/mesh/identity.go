// Package mesh — ядро Kontakt Mesh: идентичность узлов, доверие, транспорт,
// измерения, топология, маршруты и failover. Ни одной константы ёмкости сети:
// всё, что касается полосы и качества, берётся из замеров (docs/MESH.md).
package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NodeID — имя узла: хеш открытого ключа. Подделать NodeID, не владея ключом, нельзя.
type NodeID string

// IDFromPublicKey выводит NodeID из открытого ключа.
func IDFromPublicKey(pub ed25519.PublicKey) NodeID {
	h := sha256.Sum256(pub)
	return NodeID(strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h[:15])))
}

// Identity — постоянная криптографическая идентичность узла. Закрытый ключ из
// структуры не отдаётся: подписывать умеет только Sign.
type Identity struct {
	ID   NodeID
	Pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

// NewIdentity — новая идентичность (для эфемерных узлов Edge и тестов).
func NewIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{ID: IDFromPublicKey(pub), Pub: pub, priv: priv}, nil
}

const pemType = "KONTAKT NODE PRIVATE KEY"

// LoadOrCreateIdentity читает ключ из path или создаёт новый и сохраняет с правами
// 0600. Идентичность переживает перезапуск (§7).
func LoadOrCreateIdentity(path string) (*Identity, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id, err := NewIdentity()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		blk := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: id.priv.Seed()})
		// O_EXCL: два процесса, стартовавшие разом, не затрут ключ друг друга
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return LoadOrCreateIdentity(path)
			}
			return nil, err
		}
		if _, err := f.Write(blk); err != nil {
			f.Close()
			return nil, err
		}
		return id, f.Close()
	}
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != pemType || len(blk.Bytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: не ключ узла Kontakt", path)
	}
	priv := ed25519.NewKeyFromSeed(blk.Bytes)
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{ID: IDFromPublicKey(pub), Pub: pub, priv: priv}, nil
}

// Sign подписывает данные закрытым ключом узла.
func (id *Identity) Sign(data []byte) []byte { return ed25519.Sign(id.priv, data) }
