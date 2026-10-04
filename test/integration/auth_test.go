//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

const (
	testHost   = "ussp.test"
	testAlias  = "ussp-api"
	testIssuer = "https://ussp.test"
)

var (
	keyOnce  sync.Once
	ownKey   *rsa.PrivateKey
	ecoKey   *rsa.PrivateKey
	keyPEM   []byte
	keyError error
)

func testKeys(t *testing.T) (own, eco *rsa.PrivateKey, pemBytes []byte) {
	t.Helper()
	keyOnce.Do(func() {
		if ownKey, keyError = rsa.GenerateKey(rand.Reader, 2048); keyError != nil {
			return
		}
		if ecoKey, keyError = rsa.GenerateKey(rand.Reader, 2048); keyError != nil {
			return
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(ownKey)})
	})
	if keyError != nil {
		t.Fatal(keyError)
	}
	return ownKey, ecoKey, keyPEM
}

// fakeAuthority publishes the JWKS of an ecosystem issuer until Down.
type fakeAuthority struct {
	iss    *coreauth.Issuer
	url    string
	jwks   string
	down   atomic.Bool
	server *httptest.Server
}

func newFakeAuthority(t *testing.T) *fakeAuthority {
	t.Helper()
	_, eco, _ := testKeys(t)
	a := &fakeAuthority{url: "https://authority.test"}
	sk, err := auth.NewSigningKey(eco)
	if err != nil {
		t.Fatal(err)
	}
	if a.iss, err = coreauth.NewIssuer(a.url, eco, sk.KID); err != nil {
		t.Fatal(err)
	}
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/oauth/token" {
			// The token service for this USSP's outgoing calls: the fake
			// CISP's bearer, whatever the audience.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": cisp.Token, "token_type": "Bearer", "expires_in": 3600})
			return
		}
		_ = json.NewEncoder(w).Encode(a.iss.JWKS())
	}))
	t.Cleanup(a.server.Close)
	a.jwks = a.server.URL + "/.well-known/jwks.json"
	return a
}

// withAuth adds this USSP's issuer key, its audiences, the fake
// authority as the allow-listed issuer and token service, a fake CISP
// (withCIS), a fake F8 registry (withRegistry) and a fake DSS (withDSS)
// to a process's variables.
func withAuth(t *testing.T, vars map[string]string, a *fakeAuthority) map[string]string {
	t.Helper()
	_, _, p := testKeys(t)
	file := filepath.Join(t.TempDir(), "issuer-key.pem")
	if err := os.WriteFile(file, p, 0o600); err != nil {
		t.Fatal(err)
	}
	vars["USSP_ISSUER_KEY_FILE"] = file
	vars["USSP_AUDIENCES"] = testHost + "," + testAlias
	vars["USSP_TOKEN_ISSUERS"] = a.url + "=" + a.jwks
	withCIS(t, vars)
	withRegistry(t, vars)
	withGeoid(t, vars)
	withDSS(t, vars)
	return vars
}

// withDSS points USSP_DSS_BASE_URL at a fake DSS (internal/dss/fakedss)
// and returns it.
func withDSS(t *testing.T, vars map[string]string) *fakedss.DSS {
	t.Helper()
	d := fakedss.New()
	t.Cleanup(d.Close)
	vars["USSP_DSS_BASE_URL"] = d.URL()
	return d
}

type clock struct{ ns atomic.Int64 }

func newClock() *clock               { c := &clock{}; c.ns.Store(time.Now().UnixNano()); return c }
func (c *clock) Now() time.Time      { return time.Unix(0, c.ns.Load()) }
func (c *clock) Add(d time.Duration) { c.ns.Add(int64(d)) }

type fixedRegistry struct {
	mu     sync.Mutex
	answer string
}

func (r *fixedRegistry) OperatorStatus(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answer, nil
}

func (r *fixedRegistry) set(a string) { r.mu.Lock(); r.answer = a; r.mu.Unlock() }

// stack is the api process's auth and accounts wiring (internal/app/api
// routes) on the real database, with a test clock, a registry answer of
// the test's choosing, cheap argon2 parameters and a captured log. Two
// stacks on one database are two api replicas.
type stack struct {
	t        *testing.T
	url      string
	clock    *clock
	svc      *accounts.Service
	issuer   *auth.Issuer
	verifier *auth.Verifier
	bindings *auth.MemoryBindings
	sessions *auth.MemorySessions
	registry *fixedRegistry
	sealer   *accounts.Sealer
	log      *logBuffer
	counters *core.Counters
}

func newStack(t *testing.T, c *clock, logs *logBuffer) *stack {
	t.Helper()
	return newStackWith(t, c, logs, nil)
}

// newStackWith is newStack serving /v1/intents from intents (nil: 503);
// each of with changes the national server before it is registered.
func newStackWith(t *testing.T, c *clock, logs *logBuffer, intents national.Intents, with ...func(*national.Server)) *stack {
	t.Helper()
	ensureSchemas(t)
	own, _, _ := testKeys(t)
	sk, err := auth.NewSigningKey(own)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewIssuerKeys(sk, nil)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := auth.NewIssuer(testIssuer, testHost, keys)
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(context.Background(), auth.VerifierConfig{Own: iss,
		Ecosystem: coreauth.Config{Audiences: []string{testHost, testAlias}, StrictSessionClaims: true, Now: c.Now}})
	if err != nil {
		t.Fatal(err)
	}
	hasher, err := auth.NewHasherWithParams(auth.HashParams{MemoryKiB: 64, Time: 1, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := accounts.NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	s := &stack{t: t, clock: c, issuer: iss, verifier: v, bindings: auth.NewMemoryBindings(), sessions: auth.NewMemorySessions(),
		registry: &fixedRegistry{answer: accounts.RegistryValid},
		sealer:   sealer, log: logs, counters: &core.Counters{}}
	pol := policy.Defaults()
	pol.ClientSecretOverlapS = 3600
	s.svc = &accounts.Service{
		Store: appStore(t), Hasher: hasher, Issuer: iss, Registry: s.registry, Bindings: s.bindings, LiveSessions: s.sessions,
		Policy: func() policy.Values { return pol }, MFA: sealer, Counters: s.counters, Logger: logger,
		Config: accounts.Config{SessionTTL: 12 * time.Hour, SessionIdle: 30 * time.Minute, LockoutAfter: 10, LockoutFor: 15 * time.Minute,
			TOTPIssuer: "USSP-TEST", Now: c.Now},
	}
	guard := &auth.Guard{Verifier: v, OwnIssuer: iss.URL, Sessions: s.svc, Audit: s.svc, Counters: s.counters, Logger: logger}
	token := &auth.TokenEndpoint{Issuer: iss, Clients: s.svc, Hasher: hasher, Audit: s.svc, Counters: s.counters, Logger: logger, Now: c.Now,
		TTL: func() time.Duration { return time.Duration(pol.OperatorTokenTTLS) * time.Second }}
	mux := http.NewServeMux()
	ns := &national.Server{Health: noHealth{}, Token: token, Issuer: iss, Accounts: s.svc, Portal: s.svc, Intents: intents, Logger: logger}
	for _, f := range with {
		f(ns)
	}
	if err := national.Register(mux, ns, guard.Require); err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/", httpx.NotFound)
	srv := httptest.NewServer(httpx.Baseline(mux, logger, httpx.BaselineDeps{Counters: s.counters}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

type noHealth struct{}

func (noHealth) GetHealthz(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }
func (noHealth) GetReadyz(w http.ResponseWriter, _ *http.Request)  { w.WriteHeader(200) }

type resp struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (r resp) str(k string) string { s, _ := r.body[k].(string); return s }

func (r resp) slug() string { return strings.TrimPrefix(r.str("type"), httpx.ProblemTypeBase) }

// call sends a request; body is JSON (any) or a form (url-encoded
// string with formCT).
func (s *stack) call(method, path string, body any, set func(*http.Request)) resp {
	s.t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case string:
		rd, ct = strings.NewReader(b), "application/x-www-form-urlencoded"
	default:
		raw, _ := json.Marshal(b)
		rd, ct = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, s.url+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if set != nil {
		set(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, header: res.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

var seq atomic.Int64

// unique is a suffix that keeps the rows of one test apart from every
// other's (the database is shared and not emptied).
func unique() string { return fmt.Sprintf("%d%04d", time.Now().UnixNano()%1_000_000_000, seq.Add(1)) }

// operator registers an operator (the registry answer decides its
// status) and signs its admin in; it returns the operator id and the
// admin's session token.
func (s *stack) operator(answer string) (string, string, string) {
	s.t.Helper()
	s.registry.set(answer)
	u := unique()
	user, pass := "admin."+u, "operator-password-"+u
	r := s.call("POST", "/v1/accounts/operators", map[string]any{
		"registration_number": "GEO-TEST-" + u, "display_name": "Test operator " + u, "contact_email": "ops" + u + "@example.test",
		"admin_username": user, "admin_password": pass,
	}, nil)
	if r.status != 201 {
		s.t.Fatalf("register: %d %s", r.status, r.raw)
	}
	l := s.login(auth.RealmPortal, user, pass, "")
	if l.status != 200 {
		s.t.Fatalf("login: %d %s", l.status, l.raw)
	}
	return r.str("id"), l.str("token"), pass
}

func (s *stack) login(realm, user, pass, code string) resp {
	s.t.Helper()
	body := map[string]any{"realm": realm, "username": user, "password": pass}
	if code != "" {
		body["totp_code"] = code
	}
	return s.call("POST", "/v1/accounts/login", body, nil)
}

func (s *stack) client(opID, session string, scopes ...string) (string, string) {
	s.t.Helper()
	r := s.call("POST", "/v1/accounts/operators/"+opID+"/clients", map[string]any{"scopes": scopes}, bearer(session))
	if r.status != 201 || r.header.Get("Cache-Control") != "no-store" {
		s.t.Fatalf("create client: %d %s", r.status, r.raw)
	}
	return r.str("client_id"), r.str("client_secret")
}

func (s *stack) token(id, secret string) resp {
	s.t.Helper()
	return s.call("POST", "/oauth/token", "grant_type=client_credentials&client_id="+id+"&client_secret="+secret, nil)
}

func events(t *testing.T, entityType, entityID, eventType string) int64 {
	t.Helper()
	return count(t, relOwner(t), "SELECT count(*) FROM events WHERE entity_type = $1 AND entity_id = $2 AND event_type = $3", entityType, entityID, eventType)
}
