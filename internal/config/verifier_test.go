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

// The CIS notification receiver's configuration both ways: issuers and
// audiences given, it carries them (E-01); either missing, or an issuer
// without its JWKS URL, is refused naming the variable.
func TestCompactConfig(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"USSP_AUDIENCES":          "ussp.example,ussp.lab",
		"USSP_CIS_NOTIFY_ISSUERS": "https://cisp.example=https://cisp.example/.well-known/jwks.json,https://ansp.example=https://ansp.example/.well-known/jwks.json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	cc, err := c.CompactConfig()
	if err != nil || len(cc.Issuers) != 2 || cc.Issuers["https://ansp.example"].JWKSURL != "https://ansp.example/.well-known/jwks.json" ||
		!slices.Equal(cc.Audiences, []string{"ussp.example", "ussp.lab"}) {
		t.Fatalf("got %+v %v", cc, err)
	}
	c, _ = LoadFrom(env(map[string]string{"USSP_AUDIENCES": "ussp.example"}))
	if _, err := c.CompactConfig(); !slices.Equal(fieldNames(err), []string{"USSP_CIS_NOTIFY_ISSUERS"}) {
		t.Fatalf("no issuers: %v", err)
	}
	c, _ = LoadFrom(env(map[string]string{"USSP_CIS_NOTIFY_ISSUERS": "https://cisp.example=https://cisp.example/jwks"}))
	if _, err := c.CompactConfig(); !slices.Equal(fieldNames(err), []string{"USSP_AUDIENCES"}) {
		t.Fatalf("no audience: %v", err)
	}
	c = Config{Audiences: []string{"ussp.example"}, CISNotifyIssuers: []string{"https://cisp.example"}}
	if _, err := c.CompactConfig(); !slices.Equal(fieldNames(err), []string{"USSP_CIS_NOTIFY_ISSUERS"}) {
		t.Fatalf("no JWKS URL: %v", err)
	}
}

// The CIS publishers' configuration both ways: the authority and the
// ANSP with their JWKS URLs and the configured max age are carried (the
// default is 366 days, not core's 5 min); a missing list, a publisher
// that is neither, one named twice or one without its JWKS URL is
// refused naming the variable.
func TestPublisherConfig(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"USSP_CIS_PUBLISHER_KEYS": "authority=https://auth.example/.well-known/jwks.json, ansp=https://ansp.example/.well-known/jwks.json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	pc, err := c.PublisherConfig()
	if err != nil || len(pc.Publishers) != 2 || pc.Publishers[PublisherANSP].JWKSURL != "https://ansp.example/.well-known/jwks.json" ||
		pc.Publishers[PublisherAuthority].JWKSURL != "https://auth.example/.well-known/jwks.json" || pc.MaxAge != 366*24*time.Hour {
		t.Fatalf("got %+v %v", pc, err)
	}
	c, err = LoadFrom(env(map[string]string{
		"USSP_CIS_PUBLISHER_KEYS": "authority=https://auth.example/jwks", "USSP_CIS_PUBLISHER_SIG_MAX_AGE_S": "3600",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if pc, err := c.PublisherConfig(); err != nil || pc.MaxAge != time.Hour || len(pc.Publishers) != 1 {
		t.Fatalf("max age: %+v %v", pc, err)
	}
	if _, err := LoadFrom(env(map[string]string{"USSP_CIS_PUBLISHER_SIG_MAX_AGE_S": "60"})); !slices.Equal(fieldNames(err), []string{"USSP_CIS_PUBLISHER_SIG_MAX_AGE_S"}) {
		t.Fatalf("below the minimum: %v", err)
	}
	for name, keys := range map[string][]string{
		"none":         nil,
		"not one":      {"cisp=https://cisp.example/jwks"},
		"named twice":  {"ansp=https://a.example/jwks", "ansp=https://b.example/jwks"},
		"without JWKS": {"authority"},
	} {
		c := Config{CISPublisherKeys: keys, CISPublisherSigMaxAgeS: 3600}
		if _, err := c.PublisherConfig(); !slices.Equal(fieldNames(err), []string{"USSP_CIS_PUBLISHER_KEYS"}) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c = Config{CISPublisherKeys: []string{"ansp=https://a.example/jwks"}}
	if _, err := c.PublisherConfig(); !slices.Equal(fieldNames(err), []string{"USSP_CIS_PUBLISHER_SIG_MAX_AGE_S"}) {
		t.Fatalf("no max age: %v", err)
	}
}
