package auth

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
)

// MaxKeyFileBytes bounds a PEM file read at start.
const MaxKeyFileBytes = 64 << 10

// LoadKeyFile reads path, a PEM file holding one RSA private key
// (PKCS #1 "RSA PRIVATE KEY" or PKCS #8 "PRIVATE KEY", unencrypted), and
// returns it under its kid, the RFC 7638 thumbprint. field names the
// configuration variable in errors; the key itself never appears in
// one.
func LoadKeyFile(field, path string) (coreauth.SigningKey, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the operator's configuration
	if err != nil {
		return coreauth.SigningKey{}, core.Fieldf(field, "%s cannot be read: %v", quote(path), errors.Unwrap(err))
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, MaxKeyFileBytes+1))
	if err != nil {
		return coreauth.SigningKey{}, core.Fieldf(field, "%s cannot be read", quote(path))
	}
	if len(raw) > MaxKeyFileBytes {
		return coreauth.SigningKey{}, core.Fieldf(field, "%s is larger than %d bytes", quote(path), MaxKeyFileBytes)
	}
	key, err := ParsePrivateKeyPEM(raw)
	if err != nil {
		return coreauth.SigningKey{}, core.Fieldf(field, "%s: %v", quote(path), err)
	}
	sk, err := NewSigningKey(key)
	if err != nil {
		return coreauth.SigningKey{}, core.Fieldf(field, "%s: %v", quote(path), err)
	}
	return sk, nil
}

// NewSigningKey checks key as core's issuer does (at least
// auth.MinRSABits, rsa Validate) and names it by its thumbprint.
func NewSigningKey(key *rsa.PrivateKey) (coreauth.SigningKey, error) {
	if key == nil {
		return coreauth.SigningKey{}, errors.New("no key")
	}
	if bits := key.N.BitLen(); bits < coreauth.MinRSABits {
		return coreauth.SigningKey{}, fmt.Errorf("the key is %d bits, shorter than %d", bits, coreauth.MinRSABits)
	}
	if err := key.Validate(); err != nil {
		return coreauth.SigningKey{}, errors.New("the RSA key does not validate")
	}
	return coreauth.SigningKey{KID: Thumbprint(&key.PublicKey), Key: key}, nil
}

// ParsePrivateKeyPEM parses exactly one PEM block holding an RSA private
// key. Encrypted PEM (a Proc-Type header) is refused.
func ParsePrivateKeyPEM(raw []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("more than one PEM block")
	}
	if _, enc := block.Headers["Proc-Type"]; enc {
		return nil, errors.New("encrypted PEM is not supported")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("not a PKCS #1 RSA private key")
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("not a PKCS #8 private key")
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("a %T, not an RSA key (RS256 only)", k)
		}
		return rk, nil
	default:
		return nil, fmt.Errorf("PEM block %s is not a private key", quote(block.Type))
	}
}

// Thumbprint is the RFC 7638 §3 SHA-256 thumbprint of pub, base64url
// without padding: the hash of the JSON object with exactly the
// required RSA members in lexicographic order, {"e","kty","n"}, the
// integers as unsigned big-endian bytes without leading zeros
// (RFC 7518 §6.3.1). The kid of every key of this issuer.
func Thumbprint(pub *rsa.PublicKey) string {
	b64 := base64.RawURLEncoding
	e := b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	n := b64.EncodeToString(pub.N.Bytes())
	// The members are base64url strings and fixed names, so this is
	// already the canonical form RFC 7638 §3.3 asks for (no white space,
	// no escaping).
	sum := sha256.Sum256([]byte(`{"e":"` + e + `","kty":"RSA","n":"` + n + `"}`))
	return b64.EncodeToString(sum[:])
}

// IssuerKeys are this USSP's issuer keys: Current signs every operator
// and session token; Previous, during a rotation, is published and
// verifies but never signs (brief WP-2, USSP_ISSUER_PREVIOUS_KEY_FILE).
type IssuerKeys struct {
	Current  coreauth.SigningKey
	Previous *coreauth.SigningKey
	ring     *coreauth.KeyRing
}

// NewIssuerKeys builds the ring of current and previous; the two must
// be different keys.
func NewIssuerKeys(current coreauth.SigningKey, previous *coreauth.SigningKey) (*IssuerKeys, error) {
	var retired []coreauth.SigningKey
	if previous != nil {
		if previous.KID == current.KID {
			return nil, core.Fieldf("USSP_ISSUER_PREVIOUS_KEY_FILE", "holds the same key as USSP_ISSUER_KEY_FILE")
		}
		retired = append(retired, *previous)
	}
	ring, err := coreauth.NewKeyRing(current, retired...)
	if err != nil {
		return nil, err
	}
	return &IssuerKeys{Current: current, Previous: previous, ring: ring}, nil
}

// Ring is the core key ring (the current key active, the previous
// retired).
func (k *IssuerKeys) Ring() *coreauth.KeyRing { return k.ring }

// JWKSJSON is the JWKS to publish: every key of the ring, public parts
// only, as core renders them.
func (k *IssuerKeys) JWKSJSON() ([]byte, error) {
	return json.Marshal(k.ring.JWKS())
}
