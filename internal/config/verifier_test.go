package config

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/auth"
)

func TestVerifierConfigNeedsIssuersAndAudiences(t *testing.T) {
	c, _ := LoadFrom(env(map[string]string{"USSP_AUDIENCES": "ussp.example"}))
	if _, err := c.VerifierConfig(); !slices.Equal(fieldNames(err), []string{"USSP_TOKEN_ISSUERS"}) {
		t.Fatalf("got %v", err)
	}
	c, _ = LoadFrom(env(map[string]string{"USSP_TOKEN_ISSUERS": "https://auth.example=https://auth.example/jwks"}))
	if _, err := c.VerifierConfig(); !slices.Equal(fieldNames(err), []string{"USSP_AUDIENCES"}) {
		t.Fatalf("got %v", err)
	}
}

const (
	testIss = "https://auth.example"
	testKID = "k1"
	testAud = "ussp.example"
)

// sign builds an RS256 token by hand, so a claim can have a type the
// issuer would never write.
func sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": testKID}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// The verifier configuration has StrictSessionClaims on: a session token
// whose roles is not an array of strings, or whose realm is not a
// string, is refused as rejected_claims; the same token with well-typed
// claims is accepted with its roles and realm (E-01).
func TestVerifierConfigRefusesMalformedSessionClaims(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"USSP_TOKEN_ISSUERS": testIss + "=https://auth.example/.well-known/jwks.json",
		"USSP_AUDIENCES":     testAud + ",ussp-api",
	}))
	if err != nil {
		t.Fatal(err)
	}
	vc, err := c.VerifierConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !vc.StrictSessionClaims || !slices.Equal(vc.Audiences, []string{testAud, "ussp-api"}) || vc.Audience != "" ||
		vc.Issuers[testIss].JWKSURL != "https://auth.example/.well-known/jwks.json" {
		t.Fatalf("verifier config %+v", vc)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := auth.NewIssuer(testIss, key, testKID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	// The test serves the keys statically instead of over HTTPS; every
	// other field is the configuration as built.
	vc.Issuers = map[string]auth.IssuerConfig{testIss: {Keys: iss.JWKS()}}
	vc.Now = func() time.Time { return now }
	v, err := auth.NewVerifier(context.Background(), vc)
	if err != nil {
		t.Fatal(err)
	}
	claims := func(kv ...any) map[string]any {
		m := map[string]any{"iss": testIss, "aud": testAud, "sub": "account-1", "jti": "s-1",
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "scope": "session"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}

	got, err := v.Verify(context.Background(), sign(t, key, claims("roles", []string{"supervisor"}, "realm", "console")))
	if err != nil {
		t.Fatalf("a well-formed session token was refused: %v", err)
	}
	if !slices.Equal(got.Roles, []string{"supervisor"}) || got.Realm != "console" || got.Audience != testAud {
		t.Fatalf("claims %+v", got)
	}
	for _, tc := range []struct {
		name, claim string
		token       string
	}{
		{"roles a string", "roles", sign(t, key, claims("roles", "supervisor", "realm", "console"))},
		{"roles with a number", "roles", sign(t, key, claims("roles", []any{"supervisor", 1}, "realm", "console"))},
		{"realm a number", "realm", sign(t, key, claims("roles", []string{"supervisor"}, "realm", 7))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), tc.token)
			var te *auth.TokenError
			if !errors.As(err, &te) || te.Counter != auth.CounterRejectedClaims || te.Claim != tc.claim {
				t.Fatalf("got %v, want rejected_claims on %s", err, tc.claim)
			}
		})
	}
}
