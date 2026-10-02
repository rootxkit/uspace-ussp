package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	coreauth "github.com/rootxkit/uspace-core/auth"
)

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
