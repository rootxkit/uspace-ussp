package national

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

type health struct{}

func (health) GetHealthz(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }
func (health) GetReadyz(w http.ResponseWriter, _ *http.Request)  { w.WriteHeader(200) }

type noClients struct{}

func (noClients) ClientForToken(context.Context, string) (auth.ClientRecord, error) {
	return auth.ClientRecord{}, auth.ErrClientNotFound
}

type noAudit struct{}

func (noAudit) TokenIssued(context.Context, auth.TokenEvent) error  { return nil }
func (noAudit) TokenRefused(context.Context, auth.TokenEvent) error { return nil }

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

// router is the national router of the api process without a database:
// every refusal before a service call can be exercised.
func router(t *testing.T) (http.Handler, *auth.Issuer) {
	t.Helper()
	return routerWith(t, nil)
}

// routerWith is router with reg as the registry lookup.
func routerWith(t *testing.T, reg RegistryValidator, with ...func(*Server)) (http.Handler, *auth.Issuer) {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if testKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	sk, err := auth.NewSigningKey(testKey)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewIssuerKeys(sk, nil)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := auth.NewIssuer("https://ussp.test", "ussp.test", keys)
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(context.Background(), auth.VerifierConfig{
		Ecosystem: coreauth.Config{Audiences: []string{"ussp.test"}, StrictSessionClaims: true}, Own: iss})
	if err != nil {
		t.Fatal(err)
	}
	hasher, err := auth.NewHasherWithParams(auth.HashParams{MemoryKiB: 64, Time: 1, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	guard := &auth.Guard{Verifier: v, OwnIssuer: iss.URL, Counters: &core.Counters{}}
	token := &auth.TokenEndpoint{Issuer: iss, Clients: noClients{}, Hasher: hasher, Audit: noAudit{}, TTL: func() time.Duration { return time.Hour }}
	mux := http.NewServeMux()
	srv := &Server{Health: health{}, Token: token, Issuer: iss, Registry: reg}
	for _, f := range with {
		f(srv)
	}
	if err := Register(mux, srv, guard.Require); err != nil {
		t.Fatalf("the access table does not cover the routes: %v", err)
	}
	mux.HandleFunc("/", httpx.NotFound)
	return httpx.Baseline(mux, obs.Discard(), httpx.BaselineDeps{Counters: &core.Counters{}}), iss
}

// The start refuses when one generated route has no access entry: the
// real generated router against the table minus one entry (fail
// closed), and the complete table is accepted (E-01 pair).
func TestAccessTableFailsClosedOnTheGeneratedRoutes(t *testing.T) {
	router(t) // the complete table: Register returned nil
	for pattern := range AccessTable() {
		table := AccessTable()
		delete(table, pattern)
		g := httpx.NewGuardedMux(http.NewServeMux(), table, func(httpx.Access) func(http.Handler) http.Handler {
			return func(h http.Handler) http.Handler { return h }
		}, auth.ValidateAccess)
		gen.HandlerWithOptions(&Server{}, gen.StdHTTPServerOptions{BaseRouter: g})
		if err := g.Err(); err == nil || !strings.Contains(err.Error(), pattern) {
			t.Errorf("without %s: %v", pattern, err)
		}
	}
}

func compileProblemSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("testdata/problem-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	const id = "https://schemas.uspace.ge/problem/v1.json"
	if err := c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Brief WP-2: the problem bodies of this work package validate against
// the pinned schemas/common/problem/v1 copy: the guard's refusals (no
// credential, a refused token typed by core's counter, a session on a
// scope route), the token endpoint's RFC 6749 problems, the body
// decoder's and the router's 404.
func TestProblemBodiesValidateAgainstTheLabSchema(t *testing.T) {
	schema := compileProblemSchema(t)
	h, iss := router(t)
	sess, _, err := iss.IssueSession("acc-1", auth.RealmConsole, "s1", []string{auth.RoleAdmin}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	expired, err := iss.IssueOperator("op-1", []string{auth.ScopeGeo}, time.Minute, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, method, path, body, ctype, authz string
		status                                 int
		slug                                   string
	}{
		{"no credential", "GET", "/v1/accounts/me", "", "", "", 401, "unauthenticated"},
		{"expired token", "GET", "/v1/accounts/me", "", "", "Bearer " + expired.Token, 401, "rejected_expired"},
		{"console on portal route", "GET", "/v1/accounts/operators/0b7e5a1e-0000-4000-8000-000000000001", "", "", "Bearer " + sess, 403, "forbidden"},
		{"machine token on a session route", "POST", "/v1/accounts/logout", "", "", "Bearer " + func() string {
			op, _ := iss.IssueOperator("op-1", []string{auth.ScopeGeo}, time.Hour, time.Now())
			return op.Token
		}(), 403, "forbidden"},
		{"token grant", "POST", "/oauth/token", "grant_type=password", "application/x-www-form-urlencoded", "", 400, "unsupported_grant_type"},
		{"token client", "POST", "/oauth/token", "grant_type=client_credentials&client_id=x&client_secret=y", "application/x-www-form-urlencoded", "", 401, "invalid_client"},
		{"login not json", "POST", "/v1/accounts/login", "{", "application/json", "", 400, "validation"},
		{"login unknown field", "POST", "/v1/accounts/login", `{"realm":"portal","username":"a","password":"b","x":1}`, "application/json", "", 400, "validation"},
		{"login wrong content type", "POST", "/v1/accounts/login", `{}`, "text/plain", "", 400, "validation"},
		{"registration trailing data", "POST", "/v1/accounts/operators", `{} {}`, "application/json", "", 400, "validation"},
		{"no route", "GET", "/v1/nowhere", "", "", "", 404, "not_found"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		if c.authz != "" {
			r.Header.Set("Authorization", c.authz)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.status {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
			continue
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if err := schema.Validate(inst); err != nil {
			t.Errorf("%s: the body does not validate against problem/v1: %v\n%s", c.name, err, rec.Body.String())
		}
		var p struct{ Type string }
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		if p.Type != httpx.ProblemTypeBase+c.slug {
			t.Errorf("%s: type %s, want slug %s", c.name, p.Type, c.slug)
		}
	}
	// The schema rejects what it must (the check is not vacuous).
	bad, _ := jsonschema.UnmarshalJSON(strings.NewReader(`{"type":"https://schemas.uspace.ge/problems/x1","title":"t","status":400,"errors":[]}`))
	if schema.Validate(bad) == nil {
		t.Fatal("the pinned schema accepted a slug with a digit")
	}
}

// The success branch of the public routes without a database (E-02).
func TestJWKSAndHealthAreServed(t *testing.T) {
	h, iss := router(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	var set gen.JWKS
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &set) != nil || len(set.Keys) != 1 || set.Keys[0]["kid"] != iss.Keys().Current.KID {
		t.Fatalf("jwks %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "max-age") {
		t.Fatal("jwks not cacheable")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz %d", rec.Code)
	}
	// Without a key the JWKS is a 503, not an empty set.
	rec = httptest.NewRecorder()
	(&Server{}).GetJWKS(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if rec.Code != 503 {
		t.Fatalf("no key: %d", rec.Code)
	}
}

func TestParamErrorNamesTheParameter(t *testing.T) {
	rec := httptest.NewRecorder()
	paramError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), &gen.InvalidParamFormatError{ParamName: "operator_id"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"operator_id"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(`{"realm":"portal","username":"u","password":"p"}`)
	f.Add(`{"realm":1}`)
	f.Add(`[]`)
	f.Fuzz(func(t *testing.T, body string) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		var in gen.LoginRequest
		if err := decode(r, &in); err != nil {
			if p := httpx.ProblemFromError(err); p.Status != 400 && p.Status != 413 {
				t.Fatalf("status %d for %q", p.Status, body)
			}
		}
	})
}

// Conformance C6 on every route: a request without a credential is 401
// unauthenticated whatever else is wrong with it (every path parameter
// malformed, no query, a body that is not JSON), on every operation of
// the access table that is not public. The pair: the same malformed
// requests with a credential the operation admits reach validation and
// are 400, so the 401 is the guard's and not a router's that refuses
// everything.
func TestEveryRouteAuthenticatesBeforeItValidates(t *testing.T) {
	h, iss := router(t)
	malformed := func(pattern string) (string, string) {
		method, path, _ := strings.Cut(pattern, " ")
		for {
			i := strings.Index(path, "{")
			if i < 0 {
				break
			}
			j := strings.Index(path[i:], "}")
			path = path[:i] + "not-valid" + path[i+j+1:]
		}
		return method, path
	}
	send := func(method, path, authz string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("{"))
		r.Header.Set("Content-Type", "application/json")
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	n := 0
	for pattern, a := range AccessTable() {
		if a.Public {
			continue
		}
		n++
		method, path := malformed(pattern)
		rec := send(method, path, "")
		var p struct{ Type string }
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		if rec.Code != http.StatusUnauthorized || p.Type != httpx.ProblemTypeBase+httpx.SlugUnauthenticated {
			t.Errorf("%s as %s %s without credential: %d %s, want 401 unauthenticated", pattern, method, path, rec.Code, p.Type)
		}
	}
	if n < 40 {
		t.Fatalf("only %d guarded routes judged", n)
	}
	op, err := iss.IssueOperator("op-1", []string{auth.ScopeIntents, auth.ScopeGeo}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := iss.IssueSession("acc-1", auth.RealmPortal, "s-c6", []string{auth.RoleOperatorAdmin}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ pattern, token string }{
		{"GET /v1/intents/{intent_id}", op.Token},
		{"PATCH /v1/intents/{intent_id}", op.Token},
		{"GET /v1/geo/intents/{intent_id}", op.Token},
		{"GET /v1/accounts/operators/{operator_id}", admin},
	} {
		method, path := malformed(c.pattern)
		if rec := send(method, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credential: %d", c.pattern, rec.Code)
		}
		if rec := send(method, path, "Bearer "+c.token); rec.Code != http.StatusBadRequest {
			t.Errorf("%s with credential: %d %s, want 400", c.pattern, rec.Code, rec.Body.String())
		}
	}
}
