// Package signer is the compact-JWS signer the fake CISP and the fake
// ANSP sign their change notifications with: uspace-core's
// KeyRing.SignCompact, as the real ones do (CISP WP-6, ANSP WP-8), with
// a fresh RSA key per fake. Tests only.
package signer

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
)

// Signer signs deliveries as one issuer.
type Signer struct {
	Issuer string
	ring   *coreauth.KeyRing
}

// New returns a signer for iss with a new 2048-bit key under kid.
func New(iss, kid string) (*Signer, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	ring, err := coreauth.NewKeyRing(coreauth.SigningKey{KID: kid, Key: k})
	if err != nil {
		return nil, err
	}
	return &Signer{Issuer: iss, ring: ring}, nil
}

// IssuerConfig is the static key set a verifier needs for this issuer.
func (s *Signer) IssuerConfig() coreauth.IssuerConfig {
	return coreauth.IssuerConfig{Keys: s.ring.JWKS()}
}

// JWKSHandler serves the issuer's public keys.
func (s *Signer) JWKSHandler(w http.ResponseWriter, _ *http.Request) {
	b, err := json.Marshal(s.ring.JWKS())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// Sign signs body for the receiver at callback: aud is the callback's
// host without its port (M19), sub the subscription, jti the delivery
// id.
func (s *Signer) Sign(callback, sub, jti string, body json.RawMessage, now time.Time) (string, error) {
	u, err := url.Parse(callback)
	if err != nil {
		return "", err
	}
	return s.SignFor(u.Hostname(), sub, jti, body, now)
}

// SignFor signs body with an explicit audience.
func (s *Signer) SignFor(aud, sub, jti string, body json.RawMessage, now time.Time) (string, error) {
	return s.ring.SignCompact(coreauth.CompactClaims{Issuer: s.Issuer, Audience: aud, Subject: sub, JTI: jti}, body, now)
}

// NewID is a random delivery id.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
