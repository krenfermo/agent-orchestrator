// Package secretbox encrypts the small number of credentials AO must be able
// to read back rather than merely verify.
//
// Everywhere AO only has to CHECK a secret it stores a bcrypt hash and never
// the secret (users.password_hash, auth_sessions.token_hash). An SMTP password
// is different in kind: AO has to present the original string to the mail
// server on every send, so a hash cannot do the job. Encryption at rest with a
// key held outside the database is the honest alternative — the DB file alone
// (a backup, a copied ~/.ao, a support bundle) does not yield the password.
//
// The key is a 32-byte random value in a 0600 file under the AO data dir,
// created on first use. Deliberately not the OS keychain: the daemon runs
// unattended and headless, on Linux and in containers where no keychain exists,
// and a send that silently stops working because a keychain is locked would be
// worse than this. AES-256-GCM gives confidentiality and tamper detection; a
// fresh random nonce is prepended to every ciphertext.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// KeyFileName is the key's name inside the AO data dir.
const KeyFileName = "secret.key"

const keySize = 32

// ErrNotEncrypted reports a stored value that is not a well-formed ciphertext
// this box produced.
var ErrNotEncrypted = errors.New("secretbox: value is not valid ciphertext")

// Box seals and opens secrets with one key, loaded lazily on first use so that
// merely constructing a Box never touches the filesystem.
type Box struct {
	keyPath string

	mu   sync.Mutex
	aead cipher.AEAD
}

// New returns a Box whose key lives at <dir>/secret.key.
func New(dir string) *Box {
	return &Box{keyPath: filepath.Join(dir, KeyFileName)}
}

// NewWithKey returns a Box using an explicit 32-byte key, for tests and for
// callers that manage key material themselves.
func NewWithKey(key []byte) (*Box, error) {
	aead, err := aeadFrom(key)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and returns a base64 string safe to store in SQLite.
// Empty plaintext seals to the empty string: "no password set" is a state the
// caller must be able to represent without a ciphertext that decrypts to "".
func (b *Box) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	aead, err := b.cipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secretbox: nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal. The empty string opens to the empty
// string, mirroring Seal.
func (b *Box) Open(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", ErrNotEncrypted
	}
	aead, err := b.cipher()
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize() {
		return "", ErrNotEncrypted
	}
	nonce, body := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", ErrNotEncrypted
	}
	return string(plaintext), nil
}

func (b *Box) cipher() (cipher.AEAD, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.aead != nil {
		return b.aead, nil
	}
	key, err := loadOrCreateKey(b.keyPath)
	if err != nil {
		return nil, err
	}
	aead, err := aeadFrom(key)
	if err != nil {
		return nil, err
	}
	b.aead = aead
	return aead, nil
}

func aeadFrom(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("secretbox: key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: gcm: %w", err)
	}
	return aead, nil
}

// loadOrCreateKey reads the key file, generating one on first use. The file is
// written 0600 and its directory 0700: the key is the only thing standing
// between a copied database and a readable password.
func loadOrCreateKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("secretbox: key path is required")
	}
	if raw, err := os.ReadFile(path); err == nil {
		key, decodeErr := base64.StdEncoding.DecodeString(string(raw))
		if decodeErr != nil || len(key) != keySize {
			return nil, fmt.Errorf("secretbox: key file %s is corrupt", path)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secretbox: read key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("secretbox: key dir: %w", err)
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("secretbox: generate key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("secretbox: write key: %w", err)
	}
	return key, nil
}
