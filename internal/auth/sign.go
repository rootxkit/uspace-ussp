package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
)

// SessionClaims are the claims of a portal or console session token
// (M20, cross-plan Appendix A).
type SessionClaims struct {
	Issuer    string
	Audience  string
	Subject   string // the account id
	Roles     []string
	Realm     string
	JTI       string // the session id
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// signSession signs c as an RS256 compact JWS (RFC 7515 §7.1) with key:
// header {"alg":"RS256","kid","typ":"JWT"}, payload the RFC 7519 claims
// plus scope "session", roles and realm, signature RSASSA-PKCS1-v1_5
// over SHA-256 (RFC 7518 §3.3).
//
// It exists because core's Issuer signs a fixed claim set without roles
// and realm, and this repository may not import a JOSE library
// (depguard). It signs only; every token it makes is verified by core's
// Verifier like any other, and the tests verify each one that way.
func signSession(key coreauth.SigningKey, c SessionClaims) (string, error) {
	switch {
	case c.Issuer == "" || c.Audience == "" || c.Subject == "" || c.JTI == "" || c.Realm == "":
		return "", errors.New("a session needs iss, aud, sub, jti and realm")
	case len(c.Roles) == 0:
		return "", errors.New("a session needs at least one role")
	case !c.ExpiresAt.After(c.IssuedAt):
		return "", errors.New("a session must expire after it is issued")
	}
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": key.KID, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Iss   string   `json:"iss"`
		Sub   string   `json:"sub"`
		Aud   string   `json:"aud"`
		Iat   int64    `json:"iat"`
		Exp   int64    `json:"exp"`
		JTI   string   `json:"jti"`
		Scope string   `json:"scope"`
		Roles []string `json:"roles"`
		Realm string   `json:"realm"`
	}{c.Issuer, c.Subject, c.Audience, c.IssuedAt.Unix(), c.ExpiresAt.Unix(), c.JTI, SessionScope, c.Roles, c.Realm})
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return input + "." + b64.EncodeToString(sig), nil
}

// MaxPeekBytes bounds the token whose payload peek reads.
const MaxPeekBytes = coreauth.DefaultMaxTokenBytes

// peeked are string claims read from an unverified token.
type peeked struct{ Iss, Sub, JTI string }

// peek reads string claims of a compact JWS payload WITHOUT verifying
// it: only to route a token to the verifier of its issuer, to name the
// actor of a refusal in the audit log, and to read back the jti of a
// token this issuer has just signed. Nothing peeked is trusted.
func peek(token string) peeked {
	if len(token) > MaxPeekBytes {
		return peeked{}
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return peeked{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return peeked{}
	}
	var c struct {
		Iss any `json:"iss"`
		Sub any `json:"sub"`
		JTI any `json:"jti"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return peeked{}
	}
	var p peeked
	p.Iss, _ = c.Iss.(string)
	p.Sub, _ = c.Sub.(string)
	p.JTI, _ = c.JTI.(string)
	return p
}
