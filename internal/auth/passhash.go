package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters of new hashes: the OWASP Password Storage Cheat
// Sheet minimum (m = 19 MiB, t = 2, p = 1), a 16-byte salt and a 32-byte
// tag, as the authority uses. A stored hash carries its own parameters.
const (
	HashMemoryKiB = 19456
	HashTime      = 2
	HashThreads   = 1
	saltBytes     = 16
	tagBytes      = 32
	// MaxSecretBytes bounds what is hashed: a longer input is refused
	// before any work, so a request cannot make the server hash megabytes.
	MaxSecretBytes = 1024
	// Bounds of a stored hash's parameters: a row outside them is
	// refused rather than run.
	maxMemoryKiB = 4 << 20
	maxTime      = 100
	maxThreads   = 64
)

// ErrMalformedHash is a stored hash that is not an argon2id PHC string
// within the bounds above.
var ErrMalformedHash = errors.New("not an argon2id hash string")

// HashParams are argon2id parameters.
type HashParams struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

func (p HashParams) valid() bool {
	return p.MemoryKiB >= 8*uint32(p.Threads) && p.MemoryKiB <= maxMemoryKiB && p.Time >= 1 && p.Time <= maxTime &&
		p.Threads >= 1 && p.Threads <= maxThreads
}

// Hasher hashes passwords and client secrets with argon2id in the PHC
// string format and verifies them in constant time. It holds a dummy
// hash of the same cost so that a lookup that finds nothing spends the
// same time as one that finds a wrong secret (no account or client
// enumeration by timing).
type Hasher struct {
	p     HashParams
	dummy string
}

// NewHasher returns a Hasher with the OWASP parameters.
func NewHasher() (*Hasher, error) {
	return NewHasherWithParams(HashParams{MemoryKiB: HashMemoryKiB, Time: HashTime, Threads: HashThreads})
}

// NewHasherWithParams returns a Hasher with p, which must be within the
// bounds a stored hash may have; tests use cheap parameters.
func NewHasherWithParams(p HashParams) (*Hasher, error) {
	if !p.valid() {
		return nil, fmt.Errorf("argon2id parameters m=%d t=%d p=%d are out of bounds", p.MemoryKiB, p.Time, p.Threads)
	}
	h := &Hasher{p: p}
	junk, err := RandomSecret(32)
	if err != nil {
		return nil, err
	}
	if h.dummy, err = h.Hash(junk); err != nil {
		return nil, err
	}
	return h, nil
}

// Hash returns the PHC string of secret with a random salt.
func (h *Hasher) Hash(secret string) (string, error) {
	if len(secret) > MaxSecretBytes {
		return "", fmt.Errorf("secret longer than %d bytes", MaxSecretBytes)
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	tag := argon2.IDKey([]byte(secret), salt, h.p.Time, h.p.MemoryKiB, h.p.Threads, tagBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, h.p.MemoryKiB, h.p.Time, h.p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(tag)), nil
}

// Verify reports whether secret matches encoded. A secret longer than
// MaxSecretBytes is a mismatch after the same work as any other, and a
// malformed hash is an error after the work of the dummy.
func (h *Hasher) Verify(secret, encoded string) (bool, error) {
	ok, err := verifyHash(secret, encoded)
	if err != nil {
		h.VerifyDummy(secret)
		return false, err
	}
	return ok && len(secret) <= MaxSecretBytes, nil
}

// VerifyDummy spends the work of one verification and reports nothing:
// the path taken when there is no stored hash to compare with.
func (h *Hasher) VerifyDummy(secret string) { _, _ = verifyHash(secret, h.dummy) }

func verifyHash(secret, encoded string) (bool, error) {
	p, salt, tag, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if len(secret) > MaxSecretBytes {
		secret = secret[:MaxSecretBytes]
	}
	got := argon2.IDKey([]byte(secret), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(tag)))
	return subtle.ConstantTimeCompare(got, tag) == 1, nil
}

// decodeHash parses $argon2id$v=19$m=..,t=..,p=..$salt$tag.
func decodeHash(encoded string) (HashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return HashParams{}, nil, nil, ErrMalformedHash
	}
	var p HashParams
	kvs := strings.Split(parts[3], ",")
	if len(kvs) != 3 {
		return HashParams{}, nil, nil, ErrMalformedHash
	}
	for i, kv := range kvs {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || name != [...]string{"m", "t", "p"}[i] {
			return HashParams{}, nil, nil, ErrMalformedHash
		}
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return HashParams{}, nil, nil, ErrMalformedHash
		}
		switch i {
		case 0:
			p.MemoryKiB = uint32(n)
		case 1:
			p.Time = uint32(n)
		default:
			if n > 255 {
				return HashParams{}, nil, nil, ErrMalformedHash
			}
			p.Threads = uint8(n)
		}
	}
	if !p.valid() {
		return HashParams{}, nil, nil, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return HashParams{}, nil, nil, ErrMalformedHash
	}
	tag, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(tag) < 16 || len(tag) > 64 {
		return HashParams{}, nil, nil, ErrMalformedHash
	}
	return p, salt, tag, nil
}

// RandomSecret is n random bytes, base64url without padding: client
// secrets, CSRF tokens, session ids.
func RandomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
