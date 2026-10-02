package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
)

// RFC 7638 §3.1: the example RSA key and its SHA-256 thumbprint
// NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs. Copied from the RFC
// example as lestrrat-go/jwx v3.3.0 pins it in
// jwk/rsa_thumbprint_test.go (rfc7638RSAModulus and the expected hex),
// not written from memory (E-03).
const (
	rfc7638N        = "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw"
	rfc7638ThumbHex = "3736cbb1787cb8309c77ee8c3705c5e16ffb9e859715901f1e4c59b11182f57b"
)

func TestThumbprintMatchesRFC7638(t *testing.T) {
	n, err := base64.RawURLEncoding.DecodeString(rfc7638N)
	mustNoErr(t, err)
	tp := Thumbprint(&rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537})
	raw, err := base64.RawURLEncoding.DecodeString(tp)
	mustNoErr(t, err)
	if hex.EncodeToString(raw) != rfc7638ThumbHex || tp != "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs" {
		t.Fatalf("thumbprint %s", tp)
	}
}

// The kid is the thumbprint of exactly the n and e that core publishes
// in the JWKS (one representation, derived two ways).
func TestJWKSPublishesBothKeysUnderTheirThumbprints(t *testing.T) {
	iss := newOwnIssuer(t, true)
	raw, err := iss.Keys().JWKSJSON()
	mustNoErr(t, err)
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	mustNoErr(t, json.Unmarshal(raw, &set))
	if len(set.Keys) != 2 {
		t.Fatalf("%d keys", len(set.Keys))
	}
	a, b, _ := testKeys(t)
	want := []string{Thumbprint(&a.PublicKey), Thumbprint(&b.PublicKey)}
	for i, k := range set.Keys {
		n, _ := base64.RawURLEncoding.DecodeString(k["n"].(string))
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537}
		if k["kid"] != want[i] || Thumbprint(pub) != want[i] || k["alg"] != "RS256" || k["use"] != "sig" {
			t.Errorf("key %d: %v", i, k)
		}
		if _, private := k["d"]; private {
			t.Error("the JWKS publishes a private member")
		}
	}
	if iss.Keys().Current.KID != want[0] || iss.Keys().Previous.KID != want[1] {
		t.Fatal("current and previous kids")
	}
}

// The previous key verifies (rotation overlap) but never signs.
func TestPreviousKeyVerifiesAndNeverSigns(t *testing.T) {
	c := newClock(t0())
	_, b, _ := testKeys(t)
	withPrev := newOwnIssuer(t, true)
	// A token signed earlier by the previous key (an issuer on key B).
	keysB, err := NewIssuerKeys(signingKey(t, b), nil)
	mustNoErr(t, err)
	oldIssuer, err := NewIssuer(ownIssuer, ownHost, keysB)
	mustNoErr(t, err)
	old, err := oldIssuer.IssueOperator("op-1", []string{ScopeGeo}, time.Hour, c.Now())
	mustNoErr(t, err)
	v := newVerifier(t, withPrev, nil, c)
	if _, err := v.Verify(context.Background(), old.Token); err != nil {
		t.Fatalf("a token of the previous key is refused during the rotation: %v", err)
	}
	fresh, err := withPrev.IssueOperator("op-1", []string{ScopeGeo}, time.Hour, c.Now())
	mustNoErr(t, err)
	if fresh.KID != withPrev.Keys().Current.KID || peekKID(t, fresh.Token) != withPrev.Keys().Current.KID {
		t.Fatal("not signed by the current key")
	}
	// After the rotation ends (no previous key) the old token is refused.
	if _, err := newVerifier(t, newOwnIssuer(t, false), nil, c).Verify(context.Background(), old.Token); err == nil {
		t.Fatal("a token of a key no longer published verified")
	}
}

func peekKID(t *testing.T, tok string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[0])
	mustNoErr(t, err)
	var h map[string]string
	mustNoErr(t, json.Unmarshal(raw, &h))
	return h["kid"]
}

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	mustNoErr(t, os.WriteFile(p, b, 0o600))
	return p
}

func TestLoadKeyFileAcceptsPKCS1AndPKCS8(t *testing.T) {
	a, _, _ := testKeys(t)
	p1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(a)})
	der, err := x509.MarshalPKCS8PrivateKey(a)
	mustNoErr(t, err)
	p8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, b := range map[string][]byte{"pkcs1": p1, "pkcs8": p8} {
		k, err := LoadKeyFile("USSP_ISSUER_KEY_FILE", writeFile(t, name, b))
		if err != nil || k.KID != Thumbprint(&a.PublicKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Every refusal of LoadKeyFile names the variable and never the key.
func TestLoadKeyFileRefusals(t *testing.T) {
	a, _, _ := testKeys(t)
	p1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(a)})
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mustNoErr(t, err)
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	mustNoErr(t, err)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	mustNoErr(t, err)
	cases := map[string][]byte{
		"not pem":         []byte("hello"),
		"two blocks":      append(append([]byte{}, p1...), p1...),
		"encrypted":       pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: []byte{1}}),
		"bad pkcs1":       pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1, 2}}),
		"bad pkcs8":       pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2}}),
		"ec key":          pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}),
		"public key":      pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1}}),
		"1024 bits":       pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(small)}),
		"larger than cap": append(p1, []byte(strings.Repeat(" ", MaxKeyFileBytes))...),
	}
	for name, b := range cases {
		_, err := LoadKeyFile("USSP_ISSUER_KEY_FILE", writeFile(t, "k.pem", b))
		if err == nil || !strings.Contains(err.Error(), "USSP_ISSUER_KEY_FILE") {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "PRIVATE KEY-----") {
			t.Errorf("%s: the error quotes the key", name)
		}
	}
	if _, err := LoadKeyFile("USSP_ISSUER_KEY_FILE", filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Error("a missing file loaded")
	}
	if _, err := NewSigningKey(nil); err == nil {
		t.Error("nil key accepted")
	}
	bad := *a
	bad.Primes = []*big.Int{big.NewInt(3), big.NewInt(5)}
	if _, err := NewSigningKey(&bad); err == nil {
		t.Error("a key that does not validate accepted")
	}
}

func TestIssuerKeysRefuseTheSameKeyTwice(t *testing.T) {
	a, _, _ := testKeys(t)
	k := signingKey(t, a)
	if _, err := NewIssuerKeys(k, &k); err == nil || !strings.Contains(err.Error(), "USSP_ISSUER_PREVIOUS_KEY_FILE") {
		t.Fatalf("got %v", err)
	}
	if _, err := NewIssuerKeys(coreauth.SigningKey{KID: "x"}, nil); err == nil {
		t.Fatal("a ring without a key built")
	}
}

func TestNewIssuerRefusesAnIncompleteConfiguration(t *testing.T) {
	keys := newOwnIssuer(t, false).Keys()
	for name, f := range map[string]func() (*Issuer, error){
		"no url":      func() (*Issuer, error) { return NewIssuer("", ownHost, keys) },
		"no audience": func() (*Issuer, error) { return NewIssuer(ownIssuer, "", keys) },
		"no keys":     func() (*Issuer, error) { return NewIssuer(ownIssuer, ownHost, nil) },
	} {
		if _, err := f(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Operator tokens: core's issuer, aud = this host, the operator scopes,
// TTL clipped to one hour; verified by the verifier like any token.
func TestIssueOperatorIsVerifiedByCore(t *testing.T) {
	c := newClock(t0())
	iss := newOwnIssuer(t, false)
	v := newVerifier(t, iss, nil, c)
	got, err := iss.IssueOperator("op-1", []string{ScopeIntents, ScopeTelemetry}, 3*time.Hour, c.Now())
	mustNoErr(t, err)
	claims, err := v.Verify(context.Background(), got.Token)
	mustNoErr(t, err)
	if claims.Subject != "op-1" || claims.Audience != ownHost || claims.Issuer != ownIssuer || claims.JTI != got.JTI || got.JTI == "" ||
		!slices.Equal(claims.Scopes, []string{ScopeIntents, ScopeTelemetry}) || !claims.ExpiresAt.Equal(c.Now().Add(time.Hour)) ||
		!got.ExpiresAt.Equal(claims.ExpiresAt) {
		t.Fatalf("claims %+v issued %+v", claims, got)
	}
	if _, err := iss.IssueOperator("op-1", []string{"cis.read"}, time.Hour, c.Now()); err == nil {
		t.Error("an ecosystem scope issued")
	}
	if _, err := iss.IssueOperator("op-1", nil, time.Hour, c.Now()); err == nil {
		t.Error("a token without scope issued")
	}
	if _, err := iss.IssueOperator("", []string{ScopeGeo}, time.Hour, c.Now()); err == nil {
		t.Error("a token without sub issued")
	}
}

// Sessions: scope "session", roles, realm, jti, aud = this host, exp at
// most 12 h; core's verifier reads roles and realm with
// StrictSessionClaims.
func TestIssueSessionIsVerifiedByCore(t *testing.T) {
	c := newClock(t0())
	iss := newOwnIssuer(t, false)
	v := newVerifier(t, iss, nil, c)
	tok, exp, err := iss.IssueSession("acc-1", RealmConsole, "sess-1", []string{RoleSupervisor}, 24*time.Hour, c.Now())
	mustNoErr(t, err)
	claims, err := v.Verify(context.Background(), tok)
	mustNoErr(t, err)
	if claims.Subject != "acc-1" || claims.Realm != RealmConsole || !slices.Equal(claims.Roles, []string{RoleSupervisor}) ||
		claims.JTI != "sess-1" || !slices.Equal(claims.Scopes, []string{SessionScope}) || claims.Audience != ownHost ||
		!exp.Equal(c.Now().Add(MaxSessionTTL)) || !claims.ExpiresAt.Equal(exp) || claims.KeyID != iss.Keys().Current.KID {
		t.Fatalf("claims %+v exp %s", claims, exp)
	}
	if _, _, err := iss.IssueSession("acc-1", "police", "s", []string{"x"}, time.Hour, c.Now()); err == nil {
		t.Error("an unknown realm signed")
	}
	k := iss.Keys().Current
	for name, sc := range map[string]SessionClaims{
		"no sub":   {Issuer: ownIssuer, Audience: ownHost, JTI: "j", Realm: RealmPortal, Roles: []string{"r"}, IssuedAt: c.Now(), ExpiresAt: c.Now().Add(time.Hour)},
		"no roles": {Issuer: ownIssuer, Audience: ownHost, Subject: "s", JTI: "j", Realm: RealmPortal, IssuedAt: c.Now(), ExpiresAt: c.Now().Add(time.Hour)},
		"no life":  {Issuer: ownIssuer, Audience: ownHost, Subject: "s", JTI: "j", Realm: RealmPortal, Roles: []string{"r"}, IssuedAt: c.Now(), ExpiresAt: c.Now()},
	} {
		if _, err := signSession(k, sc); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
}

func TestPeekReadsOnlyWhatParses(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for tok, want := range map[string]peeked{
		"a." + enc(`{"iss":"i","sub":"s","jti":"j"}`) + ".c": {Iss: "i", Sub: "s", JTI: "j"},
		"a." + enc(`{"iss":1,"sub":["x"]}`) + ".c":           {},
		"a." + enc(`not json`) + ".c":                        {},
		"a.!!!.c":                                            {},
		"a.b":                                                {},
		strings.Repeat("a", MaxPeekBytes+1):                  {},
	} {
		if got := peek(tok); got != want {
			t.Errorf("%.40q: %+v", tok, got)
		}
	}
}

func FuzzPeek(f *testing.F) {
	f.Add("a.eyJzdWIiOiJ4In0.c")
	f.Add("..")
	f.Fuzz(func(t *testing.T, tok string) {
		p := peek(tok)
		if len(tok) > MaxPeekBytes && p != (peeked{}) {
			t.Fatal("peeked past the bound")
		}
		_ = subjectOf(tok)
	})
}
