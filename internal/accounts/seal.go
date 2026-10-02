package accounts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/rootxkit/uspace-core/core"
)

// sealedPrefix marks a staff_accounts.mfa_secret_ref that holds the
// TOTP secret sealed under the MFA key: the reference is the sealed
// value itself, opened only by a process holding USSP_MFA_KEY_FILE.
const sealedPrefix = "sealed:v1:"

// Sealer seals staff TOTP secrets with AES-256-GCM under the key of
// USSP_MFA_KEY_FILE, bound to the username (associated data), so a
// sealed secret copied to another account does not open.
type Sealer struct{ aead cipher.AEAD }

// LoadSealer reads the file at path: the base64 of exactly 32 bytes.
func LoadSealer(path string) (*Sealer, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the operator's configuration
	if err != nil {
		return nil, core.Fieldf("USSP_MFA_KEY_FILE", "%q cannot be read", path)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1024))
	if err != nil {
		return nil, core.Fieldf("USSP_MFA_KEY_FILE", "%q cannot be read", path)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return nil, core.Fieldf("USSP_MFA_KEY_FILE", "must hold the base64 of exactly 32 bytes")
	}
	return NewSealer(key)
}

// NewSealer builds a Sealer on a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("the MFA key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal returns the reference of secret for username.
func (s *Sealer) Seal(secret, username string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := s.aead.Seal(nonce, nonce, []byte(secret), []byte("staff_mfa:"+username))
	return sealedPrefix + base64.RawStdEncoding.EncodeToString(ct), nil
}

// ErrSealed is a reference that does not open: another key, another
// account, or not a sealed reference at all.
var ErrSealed = errors.New("the MFA secret reference does not open with this key")

// Open returns the secret behind ref for username.
func (s *Sealer) Open(ref, username string) (string, error) {
	raw, ok := strings.CutPrefix(ref, sealedPrefix)
	if !ok {
		return "", ErrSealed
	}
	ct, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil || len(ct) < s.aead.NonceSize() {
		return "", ErrSealed
	}
	pt, err := s.aead.Open(nil, ct[:s.aead.NonceSize()], ct[s.aead.NonceSize():], []byte("staff_mfa:"+username))
	if err != nil {
		return "", ErrSealed
	}
	return string(pt), nil
}
