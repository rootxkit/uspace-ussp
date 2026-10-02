package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

func TestHasherPairsAndBounds(t *testing.T) {
	h := cheapHasher(t)
	enc, err := h.Hash("correct horse battery")
	mustNoErr(t, err)
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("encoding %q", enc)
	}
	if ok, err := h.Verify("correct horse battery", enc); !ok || err != nil {
		t.Fatalf("right secret: %v %v", ok, err)
	}
	if ok, err := h.Verify("correct horse batterz", enc); ok || err != nil {
		t.Fatalf("wrong secret: %v %v", ok, err)
	}
	if other, _ := h.Hash("correct horse battery"); other == enc {
		t.Fatal("two hashes share a salt")
	}
	long := strings.Repeat("x", MaxSecretBytes+1)
	if _, err := h.Hash(long); err == nil {
		t.Fatal("an over-long secret hashed")
	}
	same, _ := h.Hash(long[:MaxSecretBytes])
	if ok, _ := h.Verify(long, same); ok {
		t.Fatal("an over-long secret matched its prefix")
	}
	for _, bad := range []string{
		"", "plain", "$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$dGFndGFndGFndGFndGFn",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$dGFndGFndGFndGFndGFn",
		"$argon2id$v=19$m=64,t=1$c2FsdHNhbHQ$dGFndGFndGFndGFndGFn",
		"$argon2id$v=19$t=1,m=64,p=1$c2FsdHNhbHQ$dGFndGFndGFndGFndGFn",
		"$argon2id$v=19$m=x,t=1,p=1$c2FsdHNhbHQ$dGFndGFndGFndGFndGFn",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$dGFndGFndGFndGFndGFn",
		"$argon2id$v=19$m=64,t=1,p=300$c2FsdHNhbHQ$dGFndGFndGFndGFndGFn",
		"$argon2id$v=19$m=64,t=1,p=1$!!$dGFndGFndGFndGFndGFn",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$dGFn",
	} {
		if ok, err := h.Verify("x", bad); ok || !errors.Is(err, ErrMalformedHash) {
			t.Errorf("%q: %v %v", bad, ok, err)
		}
	}
	if _, err := NewHasherWithParams(HashParams{MemoryKiB: 1, Time: 1, Threads: 1}); err == nil {
		t.Fatal("absurd parameters accepted")
	}
	if prod, err := NewHasher(); err != nil || prod.p.MemoryKiB != 19456 || prod.p.Time != 2 || prod.p.Threads != 1 {
		t.Fatalf("production parameters %+v %v", prod, err)
	}
}

func FuzzDecodeHash(f *testing.F) {
	f.Add("$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$dGFndGFndGFndGFndGFn")
	f.Fuzz(func(t *testing.T, s string) {
		p, salt, tag, err := decodeHash(s)
		if err == nil && (!p.valid() || len(salt) < 8 || len(tag) < 16) {
			t.Fatalf("accepted %q", s)
		}
	})
}

func TestParseOperatorScopes(t *testing.T) {
	got, err := ParseOperatorScopes("  ussp.geo ussp.intents ussp.geo\t")
	if err != nil || !slices.Equal(got, []string{ScopeIntents, ScopeGeo}) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := ParseOperatorScopes(""); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"cis.read", "ussp.records", "session", "USSP.GEO"} {
		if _, err := ParseOperatorScopes(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func FuzzParseOperatorScopes(f *testing.F) {
	f.Add("ussp.geo ussp.telemetry")
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseOperatorScopes(s)
		if err != nil {
			return
		}
		for _, sc := range got {
			if !IsOperatorScope(sc) {
				t.Fatalf("accepted %q", sc)
			}
		}
	})
}

// Brief WP-2: an unknown scope, realm or role in an access entry is
// refused at start; the catalogue's are accepted.
func TestValidateAccess(t *testing.T) {
	good := []httpx.Access{
		{Scopes: []string{ScopeIntents, "utm.strategic_coordination", "ussp.records", "dp.observe"}},
		{Sessions: []httpx.SessionAccess{{Realm: RealmConsole, Roles: []string{RoleAdmin}}, {Realm: RealmPortal}}},
		{AllScopes: []string{"utm.strategic_coordination", "utm.availability_arbitration"}},
	}
	for _, a := range good {
		if err := ValidateAccess(a); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	bad := []httpx.Access{
		{Scopes: []string{"rid.observe"}},
		{Scopes: []string{"made.up"}},
		{AllScopes: []string{"utm.strategic_coordination", "made.up"}},
		{Sessions: []httpx.SessionAccess{{Realm: "police"}}},
		{Sessions: []httpx.SessionAccess{{Realm: RealmPortal, Roles: []string{RoleAdmin}}}},
	}
	for _, a := range bad {
		if err := ValidateAccess(a); err == nil {
			t.Errorf("%s accepted", a)
		}
	}
}

// The cookie contract (M21): both cookies HttpOnly, Secure,
// SameSite=Strict, Path=/, expiring with the session; logout expires
// both.
func TestSessionCookies(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookies(rec, "tok", "csrf", time.Now().Add(time.Hour))
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("%d cookies", len(cookies))
	}
	for _, c := range cookies {
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge <= 0 ||
			(c.Name != CookieSession && c.Name != CookieCSRF) {
			t.Errorf("cookie %+v", c)
		}
	}
	rec = httptest.NewRecorder()
	ClearSessionCookies(rec)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge >= 0 || c.Value != "" {
			t.Errorf("not cleared: %+v", c)
		}
	}
}

func TestRandomSecretIsRandom(t *testing.T) {
	a, err := RandomSecret(32)
	mustNoErr(t, err)
	b, _ := RandomSecret(32)
	if a == b || len(a) != 43 {
		t.Fatalf("%q %q", a, b)
	}
}
