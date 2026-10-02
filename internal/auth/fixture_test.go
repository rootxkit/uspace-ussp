package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
)

// The keys of the tests: generated once per run (2048-bit RSA takes a
// while), never written anywhere.
var (
	keysOnce sync.Once
	keyA     *rsa.PrivateKey
	keyB     *rsa.PrivateKey
	keyEco   *rsa.PrivateKey
)

func testKeys(t testing.TB) (a, b, eco *rsa.PrivateKey) {
	t.Helper()
	keysOnce.Do(func() {
		for _, k := range []**rsa.PrivateKey{&keyA, &keyB, &keyEco} {
			var err error
			if *k, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
				panic(err)
			}
		}
	})
	return keyA, keyB, keyEco
}

const (
	ownHost   = "ussp.test"
	labAlias  = "ussp-api"
	ownIssuer = "https://ussp.test"
)

// clock is a settable test clock.
type clock struct{ t atomic.Int64 }

func newClock(at time.Time) *clock {
	c := &clock{}
	c.t.Store(at.UnixNano())
	return c
}
func (c *clock) Now() time.Time       { return time.Unix(0, c.t.Load()) }
func (c *clock) Add(d time.Duration)  { c.t.Add(int64(d)) }
func (c *clock) Set(at time.Time)     { c.t.Store(at.UnixNano()) }
func (c *clock) fn() func() time.Time { return c.Now }
func t0() time.Time                   { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
func mustNoErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func signingKey(t testing.TB, k *rsa.PrivateKey) coreauth.SigningKey {
	t.Helper()
	sk, err := NewSigningKey(k)
	mustNoErr(t, err)
	return sk
}

// newOwnIssuer is this USSP's issuer on key A, with key B as previous
// when withPrevious.
func newOwnIssuer(t testing.TB, withPrevious bool) *Issuer {
	t.Helper()
	a, b, _ := testKeys(t)
	var prev *coreauth.SigningKey
	if withPrevious {
		p := signingKey(t, b)
		prev = &p
	}
	keys, err := NewIssuerKeys(signingKey(t, a), prev)
	mustNoErr(t, err)
	iss, err := NewIssuer(ownIssuer, ownHost, keys)
	mustNoErr(t, err)
	return iss
}

// ecosystem is a fake token service: an issuer whose JWKS an httptest
// server publishes until Down is called.
type ecosystem struct {
	URL    string // iss
	JWKS   string // the JWKS URL
	issuer *coreauth.Issuer
	down   atomic.Bool
	srv    *httptest.Server
}

func newEcosystem(t testing.TB) *ecosystem {
	t.Helper()
	_, _, k := testKeys(t)
	e := &ecosystem{}
	sk := signingKey(t, k)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if e.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(e.issuer.JWKS())
	}))
	t.Cleanup(e.srv.Close)
	e.URL = "https://authority.test"
	e.JWKS = e.srv.URL + "/.well-known/jwks.json"
	var err error
	e.issuer, err = coreauth.NewIssuer(e.URL, k, sk.KID)
	mustNoErr(t, err)
	return e
}

func (e *ecosystem) Down() { e.down.Store(true) }
func (e *ecosystem) Up()   { e.down.Store(false) }
func (e *ecosystem) token(t testing.TB, sub, aud string, scopes []string, now time.Time) string {
	t.Helper()
	tok, err := e.issuer.Issue(sub, aud, scopes, 10*time.Minute, now)
	mustNoErr(t, err)
	return tok
}

// newVerifier is the Verifier of the tests: own issuer (with previous
// key), the ecosystem allow-listed, both audiences, on the clock.
func newVerifier(t testing.TB, own *Issuer, eco *ecosystem, c *clock) *Verifier {
	t.Helper()
	cfg := coreauth.Config{Audiences: []string{ownHost, labAlias}, StrictSessionClaims: true, Issuers: map[string]coreauth.IssuerConfig{}}
	if c != nil {
		cfg.Now = c.fn()
	}
	if eco != nil {
		cfg.Issuers[eco.URL] = coreauth.IssuerConfig{JWKSURL: eco.JWKS}
	}
	v, err := NewVerifier(context.Background(), VerifierConfig{Ecosystem: cfg, Own: own, RetryInterval: 10 * time.Millisecond})
	mustNoErr(t, err)
	return v
}

// logBuffer is a JSON logger whose output a test can search.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) logger() *slog.Logger { return slog.New(slog.NewJSONHandler(l, nil)) }
