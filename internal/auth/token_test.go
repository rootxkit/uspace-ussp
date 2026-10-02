package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// cheapHasher: test parameters inside the bounds a stored hash may have
// (production uses the OWASP minimum).
func cheapHasher(t testing.TB) *Hasher {
	t.Helper()
	h, err := NewHasherWithParams(HashParams{MemoryKiB: 64, Time: 1, Threads: 1})
	mustNoErr(t, err)
	return h
}

type fakeClients struct {
	mu      sync.Mutex
	clients map[string]ClientRecord
	fail    error
}

func (f *fakeClients) ClientForToken(_ context.Context, id string) (ClientRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return ClientRecord{}, f.fail
	}
	c, ok := f.clients[id]
	if !ok {
		return ClientRecord{}, ErrClientNotFound
	}
	return c, nil
}

type fakeTokenAudit struct {
	mu      sync.Mutex
	issued  []TokenEvent
	refused []TokenEvent
	failIss error
	failRef error
}

func (f *fakeTokenAudit) TokenIssued(_ context.Context, e TokenEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIss != nil {
		return f.failIss
	}
	f.issued = append(f.issued, e)
	return nil
}

func (f *fakeTokenAudit) TokenRefused(_ context.Context, e TokenEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused = append(f.refused, e)
	return f.failRef
}

type tokenFixture struct {
	c        *clock
	own      *Issuer
	hasher   *Hasher
	clients  *fakeClients
	audit    *fakeTokenAudit
	counters *core.Counters
	ep       *TokenEndpoint
	v        *Verifier
	log      *logBuffer
}

const (
	secretA   = "the-secret-of-client-a-0123456789"
	secretOld = "the-old-secret-of-client-a-987654"
)

func newTokenFixture(t testing.TB) *tokenFixture {
	t.Helper()
	f := &tokenFixture{c: newClock(t0()), own: newOwnIssuer(t, false), hasher: cheapHasher(t), audit: &fakeTokenAudit{},
		counters: &core.Counters{}, log: &logBuffer{}}
	hash, err := f.hasher.Hash(secretA)
	mustNoErr(t, err)
	f.clients = &fakeClients{clients: map[string]ClientRecord{
		"op-a": {ClientID: "op-a", OperatorID: "o1", SecretHash: hash, Scopes: []string{ScopeIntents, ScopeTelemetry}, Active: true},
	}}
	f.ep = &TokenEndpoint{Issuer: f.own, Clients: f.clients, Hasher: f.hasher, Audit: f.audit,
		TTL: func() time.Duration { return 30 * time.Minute }, Counters: f.counters, Logger: f.log.logger(), Now: f.c.fn()}
	f.v = newVerifier(t, f.own, nil, f.c)
	return f
}

type tokenResult struct {
	status int
	header http.Header
	body   map[string]any
}

func (f *tokenFixture) post(t testing.TB, form url.Values, set func(*http.Request)) tokenResult {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if set != nil {
		set(r)
	}
	rec := httptest.NewRecorder()
	httpx.RealIP(nil)(f.ep).ServeHTTP(rec, r)
	res := tokenResult{status: rec.Code, header: rec.Header()}
	mustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &res.body))
	return res
}

func creds(id, secret string) url.Values {
	return url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
}

func (r tokenResult) oauthError() string { s, _ := r.body["error"].(string); return s }

// The success branch, read in full: the token verifies with aud = this
// host, the requested scopes the client holds, the policy TTL, and the
// issuance is audited with its jti (E-02).
func TestTokenIssuedAndVerified(t *testing.T) {
	f := newTokenFixture(t)
	form := creds("op-a", secretA)
	form.Set("scope", "ussp.telemetry")
	res := f.post(t, form, nil)
	if res.status != 200 || res.header.Get("Cache-Control") != "no-store" || res.body["token_type"] != "Bearer" ||
		res.body["expires_in"] != float64(1800) || res.body["scope"] != ScopeTelemetry {
		t.Fatalf("got %d %v", res.status, res.body)
	}
	claims, err := f.v.Verify(context.Background(), res.body["access_token"].(string))
	mustNoErr(t, err)
	if claims.Subject != "op-a" || claims.Audience != ownHost || !slices.Equal(claims.Scopes, []string{ScopeTelemetry}) {
		t.Fatalf("claims %+v", claims)
	}
	if len(f.audit.issued) != 1 || f.audit.issued[0].JTI != claims.JTI || f.audit.issued[0].RemoteIP != "192.0.2.1" ||
		f.counters.Get(CounterTokenIssued) != 1 {
		t.Fatalf("audit %+v", f.audit.issued)
	}
	// No scope asked: every scope the client holds.
	res = f.post(t, creds("op-a", secretA), nil)
	if res.status != 200 || res.body["scope"] != "ussp.intents ussp.telemetry" {
		t.Fatalf("all scopes: %v", res.body)
	}
	// HTTP Basic, the parts form-encoded (RFC 6749 §2.3.1); audience as
	// the bare host and as a URL.
	for _, aud := range []string{ownHost, "https://USSP.test/v1"} {
		res = f.post(t, url.Values{"grant_type": {"client_credentials"}, "audience": {aud}}, func(r *http.Request) {
			r.SetBasicAuth(url.QueryEscape("op-a"), url.QueryEscape(secretA))
		})
		if res.status != 200 {
			t.Fatalf("basic, audience %s: %d %v", aud, res.status, res.body)
		}
	}
}

// Every refusal beside the request it spoils (E-01): each answers its
// RFC 6749 code inside a problem, is counted under its reason and
// audited.
func TestTokenRefusals(t *testing.T) {
	type tc struct {
		name, code, reason string
		status             int
		form               url.Values
		set                func(*http.Request)
	}
	withScope := func(v url.Values, k, val string) url.Values { v.Set(k, val); return v }
	cases := []tc{
		{"query string", OAuthInvalidRequest, "query_parameters", 400, creds("op-a", secretA), func(r *http.Request) { r.URL.RawQuery = "x=1" }},
		{"json body", OAuthInvalidRequest, "content_type", 400, creds("op-a", secretA), func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }},
		{"repeated", OAuthInvalidRequest, "repeated_parameter", 400, url.Values{"grant_type": {"client_credentials", "client_credentials"}}, nil},
		{"grant", OAuthUnsupportedGrantType, "grant_type", 400, withScope(creds("op-a", secretA), "grant_type", "password"), nil},
		{"two auths", OAuthInvalidRequest, "two_client_authentications", 400, creds("op-a", secretA), func(r *http.Request) { r.SetBasicAuth("op-a", secretA) }},
		{"no auth", OAuthInvalidClient, "no_client_authentication", 401, url.Values{"grant_type": {"client_credentials"}}, nil},
		{"bad basic", OAuthInvalidClient, "malformed_basic", 401, url.Values{"grant_type": {"client_credentials"}}, func(r *http.Request) { r.SetBasicAuth("%zz", "x") }},
		{"long id", OAuthInvalidClient, "malformed_client", 401, creds(strings.Repeat("a", 65), secretA), nil},
		{"unknown scope", OAuthInvalidScope, "unknown_scope", 400, withScope(creds("op-a", secretA), "scope", "cis.read"), nil},
		{"foreign audience", OAuthInvalidTarget, "foreign_audience", 400, withScope(creds("op-a", secretA), "audience", "uspace-cisp.test"), nil},
		{"unknown client", OAuthInvalidClient, "unknown_client", 401, creds("op-nobody", secretA), nil},
		{"wrong secret", OAuthInvalidClient, "wrong_secret", 401, creds("op-a", "not-the-secret"), nil},
		{"scope not held", OAuthInvalidScope, "scope_not_held", 400, withScope(creds("op-a", secretA), "scope", ScopeGeo), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTokenFixture(t)
			res := f.post(t, c.form, c.set)
			if res.status != c.status || res.oauthError() != c.code || f.counters.Get(CounterTokenRefusedBase+c.reason) != 1 {
				t.Fatalf("got %d %v (counters %v)", res.status, res.body, f.counters.Snapshot())
			}
			if res.body["type"] != httpx.ProblemTypeBase+c.code || res.body["status"] != float64(c.status) || res.body["title"] == "" {
				t.Fatalf("not a problem: %v", res.body)
			}
			if c.status == 401 && res.header.Get("WWW-Authenticate") == "" {
				t.Fatal("no WWW-Authenticate on invalid_client")
			}
			if len(f.audit.refused) != 1 || f.audit.refused[0].Reason != c.reason || len(f.audit.issued) != 0 {
				t.Fatalf("audit %+v", f.audit)
			}
		})
	}
}

// E-01, brief WP-2: a rotated secret works inside the overlap and not
// after it; the current secret works throughout.
func TestTokenPreviousSecretInsideTheOverlapOnly(t *testing.T) {
	f := newTokenFixture(t)
	old, err := f.hasher.Hash(secretOld)
	mustNoErr(t, err)
	until := f.c.Now().Add(time.Hour)
	c := f.clients.clients["op-a"]
	c.PreviousHash, c.PreviousValidUntil = old, &until
	f.clients.clients["op-a"] = c
	if res := f.post(t, creds("op-a", secretOld), nil); res.status != 200 {
		t.Fatalf("inside the overlap: %d %v", res.status, res.body)
	}
	f.c.Add(time.Hour)
	res := f.post(t, creds("op-a", secretOld), nil)
	if res.status != 401 || f.counters.Get(CounterTokenRefusedBase+"previous_secret_expired") != 1 {
		t.Fatalf("after the overlap: %d %v", res.status, res.body)
	}
	if res := f.post(t, creds("op-a", secretA), nil); res.status != 200 {
		t.Fatalf("the current secret after the overlap: %d", res.status)
	}
}

// E-01: an inactive client or operator is refused with
// OAuthUnauthorizedClient, after
// the secret was checked (so a wrong secret does not learn the state).
func TestTokenInactiveClientPair(t *testing.T) {
	f := newTokenFixture(t)
	c := f.clients.clients["op-a"]
	c.Active = false
	f.clients.clients["op-a"] = c
	if res := f.post(t, creds("op-a", secretA), nil); res.status != 400 || res.oauthError() != OAuthUnauthorizedClient {
		t.Fatalf("inactive: %d %v", res.status, res.body)
	}
	if res := f.post(t, creds("op-a", "wrong"), nil); res.oauthError() != OAuthInvalidClient {
		t.Fatalf("inactive, wrong secret: %v", res.body)
	}
}

// E-01: the per-address and per-client budgets admit, then refuse 429
// with Retry-After; another address and another client are unaffected.
func TestTokenRateLimits(t *testing.T) {
	f := newTokenFixture(t)
	f.ep.IPLimiter = httpx.NewRateLimiter(0.01, 2, 10, nil)
	f.ep.ClientLimiter = httpx.NewRateLimiter(0.01, 1, 10, nil)
	if res := f.post(t, creds("op-a", secretA), nil); res.status != 200 {
		t.Fatalf("first: %d", res.status)
	}
	res := f.post(t, creds("op-a", secretA), nil)
	if res.status != 429 || res.oauthError() != OAuthUnavailable || res.header.Get("Retry-After") == "" ||
		f.counters.Get(CounterTokenRefusedBase+"rate_limited_client") != 1 {
		t.Fatalf("client budget: %d %v", res.status, res.body)
	}
	res = f.post(t, creds("op-b", "x"), nil)
	if res.status != 429 || f.counters.Get(CounterTokenRefusedBase+"rate_limited_address") != 1 {
		t.Fatalf("address budget: %d %v", res.status, res.body)
	}
	res = f.post(t, creds("op-b", "x"), func(r *http.Request) { r.RemoteAddr = "198.51.100.9:1" })
	if res.status != 401 {
		t.Fatalf("another address: %d", res.status)
	}
}

// The dependencies that can fail answer 503 and issue nothing: no key,
// the store, the signer's audit row.
func TestTokenUnavailablePaths(t *testing.T) {
	f := newTokenFixture(t)
	f.ep.Issuer = nil
	if res := f.post(t, creds("op-a", secretA), nil); res.status != 503 || f.counters.Get(CounterTokenRefusedBase+"no_issuer_key") != 1 {
		t.Fatalf("no key: %d", res.status)
	}
	f = newTokenFixture(t)
	f.clients.fail = errors.New("db down")
	if res := f.post(t, creds("op-a", secretA), nil); res.status != 503 || res.oauthError() != OAuthUnavailable {
		t.Fatalf("store: %d %v", res.status, res.body)
	}
	f = newTokenFixture(t)
	f.audit.failIss = errors.New("db down")
	res := f.post(t, creds("op-a", secretA), nil)
	if res.status != 503 || res.body["access_token"] != nil || f.counters.Get(CounterTokenRefusedBase+"audit_failed") != 1 {
		t.Fatalf("audit down: %d %v", res.status, res.body)
	}
	// A refusal whose audit row fails stands and is counted.
	f = newTokenFixture(t)
	f.audit.failRef = errors.New("db down")
	if res := f.post(t, creds("op-a", "wrong"), nil); res.status != 401 || f.counters.Get(CounterTokenNotAudited) != 1 {
		t.Fatalf("refusal not audited: %d", res.status)
	}
}

func TestTokenBodyTooLarge(t *testing.T) {
	f := newTokenFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader("grant_type="+strings.Repeat("x", 100)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 10)
	rec := httptest.NewRecorder()
	f.ep.ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	r = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader("%zz"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	f.ep.ServeHTTP(rec, r)
	if rec.Code != 400 {
		t.Fatalf("malformed form: %d", rec.Code)
	}
}

// Brief WP-2: no secret in any log line. Every path of the endpoint
// runs with a captured logger, the failing dependencies included; the
// log must not hold the secret, the token or the Basic credentials.
func TestTokenNeverLogsASecretOrAToken(t *testing.T) {
	f := newTokenFixture(t)
	res := f.post(t, creds("op-a", secretA), nil)
	tok, _ := res.body["access_token"].(string)
	if tok == "" {
		t.Fatal("no token")
	}
	f.post(t, creds("op-a", secretA+"x"), nil)
	f.post(t, creds("op-a", secretA), func(r *http.Request) { r.SetBasicAuth("op-a", secretA) })
	f.clients.fail = errors.New("db down")
	f.post(t, creds("op-a", secretA), nil)
	f.clients.fail = nil
	f.audit.failIss = errors.New("db down")
	f.post(t, creds("op-a", secretA), nil)
	f.audit.failRef = errors.New("db down")
	f.post(t, creds("op-a", "wrong-secret-value"), nil)
	logged := f.log.String()
	if !strings.Contains(logged, "not audited") {
		t.Fatalf("the failing paths did not log (the test proves nothing): %s", logged)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("op-a:" + secretA))
	for _, secret := range []string{secretA, tok, basic, "wrong-secret-value"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("the log holds a credential: %s", logged)
		}
	}
}

func FuzzTokenEndpoint(f *testing.F) {
	fx := newTokenFixture(f)
	fx.ep.Audit = &fakeTokenAudit{}
	f.Add("grant_type=client_credentials&client_id=op-a&client_secret=x&scope=ussp.geo", "")
	f.Add("grant_type=client_credentials&audience=%00", "Basic b3AtYTp4")
	f.Add("%%%", "Bearer x")
	f.Fuzz(func(t *testing.T, body, authz string) {
		r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if authz != "" {
			r.Header["Authorization"] = []string{authz}
		}
		rec := httptest.NewRecorder()
		fx.ep.ServeHTTP(rec, r)
		switch rec.Code {
		case 200:
			// Only the right secret of op-a issues.
			id, secret := "", ""
			if v, err := url.ParseQuery(body); err == nil {
				id, secret = v.Get("client_id"), v.Get("client_secret")
			}
			if (id != "op-a" || secret != secretA) && !strings.HasPrefix(authz, "Basic ") {
				t.Fatalf("issued for %q", body)
			}
		case 400, 401, 413, 429, 503:
		default:
			t.Fatalf("status %d", rec.Code)
		}
	})
}
