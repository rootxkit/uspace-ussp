package stdapi_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"go.yaml.in/yaml/v3"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/stdapi"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
	stdf3548 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3548"
)

const (
	tokenIssuer = "https://tokens.test"
	ownHost     = "ussp.test"
)

func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// recorder is a ServeMux for the generated router that records the
// patterns it registers.
type recorder struct{ patterns []string }

func (r *recorder) HandleFunc(p string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, p)
}
func (r *recorder) ServeHTTP(http.ResponseWriter, *http.Request) {}

func generatedPatterns() (f3411, f3548 []string) {
	r1, r2 := &recorder{}, &recorder{}
	stdf3411.HandlerWithOptions(stdf3411.NewStrictHandler(stdapi.NotImplementedF3411{}, nil), stdf3411.StdHTTPServerOptions{BaseRouter: r1})
	stdf3548.HandlerWithOptions(stdf3548.NewStrictHandler(stdapi.NotImplementedF3548{}, nil), stdf3548.StdHTTPServerOptions{BaseRouter: r2})
	slices.Sort(r1.patterns)
	slices.Sort(r2.patterns)
	return r1.patterns, r2.patterns
}

// planEndpoints reads the table of docs/PLAN.md §6.2 and returns its
// endpoints by process (rid-sp, api), as ServeMux patterns.
func planEndpoints(t *testing.T) map[string][]string {
	t.Helper()
	plan := string(repoFile(t, "docs/PLAN.md"))
	start := strings.Index(plan, "### 6.2 ")
	end := strings.Index(plan, "### 6.3 ")
	if start < 0 || end < start {
		t.Fatal("docs/PLAN.md has no §6.2")
	}
	row := regexp.MustCompile("^\\| `([A-Z]+ [^`?]+)(?:\\?[^`]*)?` \\| ([a-z-]+)")
	out := map[string][]string{}
	for line := range strings.SplitSeq(plan[start:end], "\n") {
		if m := row.FindStringSubmatch(line); m != nil {
			out[m[2]] = append(out[m[2]], m[1])
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

func keys(m map[string]httpx.Access) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Every USS-side operation of PLAN §6.2 is mounted on its process, and
// nothing else is: the generated router's patterns equal the plan's
// list and the access table's keys, in both directions.
func TestMountedOperationsAreThePlans(t *testing.T) {
	plan := planEndpoints(t)
	if len(plan["rid-sp"]) == 0 || len(plan["api"]) == 0 {
		t.Fatalf("the §6.2 table did not parse: %v", plan)
	}
	gen3411, gen3548 := generatedPatterns()
	for name, c := range map[string]struct{ plan, gen, table []string }{
		"F3411 on rid-sp": {plan["rid-sp"], gen3411, keys(stdapi.F3411Access())},
		"F3548 on api":    {plan["api"], gen3548, keys(stdapi.F3548Access())},
	} {
		for _, p := range c.plan {
			if !slices.Contains(c.gen, p) {
				t.Errorf("%s: %s is in the plan but not mounted", name, p)
			}
		}
		for _, p := range c.gen {
			if !slices.Contains(c.plan, p) {
				t.Errorf("%s: %s is mounted but not in the plan", name, p)
			}
		}
		if !slices.Equal(c.gen, c.table) {
			t.Errorf("%s: router %v, access table %v", name, c.gen, c.table)
		}
	}
}

type operation struct {
	OperationID string                 `yaml:"operationId"`
	Security    *[]map[string][]string `yaml:"security"`
}

type openAPI struct {
	Security []map[string][]string           `yaml:"security"`
	Paths    map[string]map[string]yaml.Node `yaml:"paths"`
}

// security returns the requirements of the operation that pattern
// names in the OpenAPI file: its own `security`, else the file's.
func security(t *testing.T, doc openAPI, pattern string) [][]string {
	t.Helper()
	method, path, _ := strings.Cut(pattern, " ")
	node, ok := doc.Paths[path][strings.ToLower(method)]
	if !ok {
		t.Fatalf("%s is not in the OpenAPI file", pattern)
	}
	var op operation
	if err := node.Decode(&op); err != nil {
		t.Fatal(err)
	}
	reqs := doc.Security
	if op.Security != nil {
		reqs = *op.Security
	}
	var out [][]string
	for _, r := range reqs {
		scopes, ok := r["Authority"]
		if len(r) != 1 || !ok {
			t.Fatalf("%s: a requirement other than Authority: %v", pattern, r)
		}
		out = append(out, scopes)
	}
	return out
}

// The scopes of every mounted operation are the ones its `security`
// block in the pinned file names: requirements of one scope each are
// Scopes (any one); a single requirement of several scopes is AllScopes
// (every one). Anything else has no representation and fails.
func TestScopesAreTheOpenAPISecurity(t *testing.T) {
	for file, table := range map[string]map[string]httpx.Access{
		"api/standards/f3411-v22a.yaml": stdapi.F3411Access(),
		"api/standards/f3548-v21.yaml":  stdapi.F3548Access(),
	} {
		var doc openAPI
		if err := yaml.Unmarshal(repoFile(t, file), &doc); err != nil {
			t.Fatal(err)
		}
		for pattern, a := range table {
			reqs := security(t, doc, pattern)
			var want httpx.Access
			switch {
			case len(reqs) == 0:
				t.Errorf("%s: no security in the file", pattern)
				continue
			case len(reqs) == 1 && len(reqs[0]) > 1:
				want.AllScopes = slices.Sorted(slices.Values(reqs[0]))
			default:
				for _, r := range reqs {
					if len(r) != 1 {
						t.Errorf("%s: requirements %v cannot be expressed", pattern, reqs)
					}
					want.Scopes = append(want.Scopes, r...)
				}
				slices.Sort(want.Scopes)
			}
			got := httpx.Access{Scopes: slices.Sorted(slices.Values(a.Scopes)), AllScopes: slices.Sorted(slices.Values(a.AllScopes))}
			if a.Public || len(a.Sessions) > 0 || !slices.Equal(got.Scopes, want.Scopes) || !slices.Equal(got.AllScopes, want.AllScopes) {
				t.Errorf("%s: table %s, file %s", pattern, a, want)
			}
			if err := auth.ValidateAccess(a); err != nil {
				t.Errorf("%s: %v", pattern, err)
			}
		}
	}
}

// ecosystem is a token service the guard trusts, with static keys.
type ecosystem struct {
	iss   *coreauth.Issuer
	guard *auth.Guard
	authC *core.Counters
}

var (
	ecoOnce   sync.Once
	ecoKey    *rsa.PrivateKey
	errEcoKey error
)

func newEcosystem(t *testing.T) *ecosystem {
	t.Helper()
	ecoOnce.Do(func() { ecoKey, errEcoKey = rsa.GenerateKey(rand.Reader, 2048) })
	if errEcoKey != nil {
		t.Fatal(errEcoKey)
	}
	sk, err := auth.NewSigningKey(ecoKey)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewIssuerKeys(sk, nil)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := keys.Ring().Issuer(tokenIssuer)
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(context.Background(), auth.VerifierConfig{Ecosystem: coreauth.Config{
		Audiences: []string{ownHost}, StrictSessionClaims: true,
		Issuers: map[string]coreauth.IssuerConfig{tokenIssuer: {Keys: keys.Ring().JWKS()}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.BuildEcosystem(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := &core.Counters{}
	return &ecosystem{iss: iss, authC: c, guard: &auth.Guard{Verifier: v, Counters: c}}
}

func (e *ecosystem) token(t *testing.T, scopes ...string) string {
	t.Helper()
	tok, err := e.iss.Issue("peer-uss", ownHost, scopes, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type answer struct {
	status int
	slug   string
	body   string
}

func call(t *testing.T, h http.Handler, method, target, token, body string) answer {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	a := answer{status: rec.Code, body: rec.Body.String()}
	if rec.Code >= 400 {
		if ct := rec.Header().Get("Content-Type"); ct != httpx.ProblemContentType {
			t.Fatalf("%s %s: %d with %q: %s", method, target, rec.Code, ct, a.body)
		}
		var p httpx.ProblemBody
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		a.slug = strings.TrimPrefix(p.Type, httpx.ProblemTypeBase)
	}
	return a
}

// target is a request path and a valid body for pattern.
func target(pattern string) (method, path, body string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = regexp.MustCompile(`\{[a-z_]+\}`).ReplaceAllString(path, "2f8343be-6482-4d1b-a474-16847e01af1e")
	if strings.HasPrefix(path, "/uss/flights") && !strings.Contains(path, "details") {
		path += "?view=34.123,-118.456,34.124,-118.455"
	}
	if method == http.MethodPost {
		body = "{}"
	}
	return method, path, body
}

// wrongScope is a catalogue scope the entry does not grant.
func wrongScope(a httpx.Access) string {
	for _, s := range []string{stdapi.ScopeRIDDisplayProvider, stdapi.ScopeRIDServiceProvider, stdapi.ScopeAvailabilityArbitration} {
		if !slices.Contains(a.Scopes, s) && !slices.Contains(a.AllScopes, s) {
			return s
		}
	}
	return "ussp.records"
}

// Every stub answers 501 to a caller with the standard's scope, and 401
// or 403 to everyone else: the guard runs before the stub (brief WP-3
// safety note).
func TestStubsAreGuardedThen501(t *testing.T) {
	e := newEcosystem(t)
	for name, c := range map[string]struct {
		table map[string]httpx.Access
		mount func(*http.ServeMux, stdapi.Options) error
	}{
		"F3411": {stdapi.F3411Access(), func(m *http.ServeMux, o stdapi.Options) error {
			return stdapi.MountF3411(m, stdapi.NotImplementedF3411{}, o)
		}},
		"F3548": {stdapi.F3548Access(), func(m *http.ServeMux, o stdapi.Options) error {
			return stdapi.MountF3548(m, stdapi.NotImplementedF3548{}, o)
		}},
	} {
		mux := http.NewServeMux()
		std := &core.Counters{}
		if err := c.mount(mux, stdapi.Options{Guard: e.guard.Require, Validate: auth.ValidateAccess, Counters: std}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		served := 0
		for pattern, a := range c.table {
			method, path, body := target(pattern)
			if got := call(t, mux, method, path, "", body); got.status != 401 || got.slug != httpx.SlugUnauthenticated {
				t.Errorf("%s without a token: %+v", pattern, got)
			}
			if got := call(t, mux, method, path, e.token(t, wrongScope(a)), body); got.status != 403 || got.slug != httpx.SlugForbidden {
				t.Errorf("%s with %s: %+v", pattern, wrongScope(a), got)
			}
			granted := a.AllScopes
			if len(granted) == 0 {
				granted = a.Scopes[:1]
			}
			if got := call(t, mux, method, path, e.token(t, granted...), body); got.status != 501 || got.slug != stdapi.SlugNotImplemented {
				t.Errorf("%s with %v: %+v", pattern, granted, got)
			}
			served++
		}
		if std.Get(stdapi.CounterNotImplemented) != uint64(served) {
			t.Errorf("%s: %d not_implemented counted for %d", name, std.Get(stdapi.CounterNotImplemented), served)
		}
	}
}

// getLogSet needs every one of the five utm scopes: four are refused,
// all five admitted.
func TestLogSetNeedsEveryScope(t *testing.T) {
	e := newEcosystem(t)
	mux := http.NewServeMux()
	if err := stdapi.MountF3548(mux, stdapi.NotImplementedF3548{}, stdapi.Options{Guard: e.guard.Require}); err != nil {
		t.Fatal(err)
	}
	all := stdapi.F3548Access()["GET /uss/v1/log_sets/{log_set_id}"].AllScopes
	if got := call(t, mux, http.MethodGet, "/uss/v1/log_sets/x", e.token(t, all[:4]...), ""); got.status != 403 {
		t.Fatalf("four scopes: %+v", got)
	}
	if got := call(t, mux, http.MethodGet, "/uss/v1/log_sets/x", e.token(t, all...), ""); got.status != 501 {
		t.Fatalf("five scopes: %+v", got)
	}
}

type failing struct{ stdapi.NotImplementedF3548 }

func (failing) MakeUssReport(context.Context, stdf3548.MakeUssReportRequestObject) (stdf3548.MakeUssReportResponseObject, error) {
	return nil, errors.New("secret cause")
}

// The answers of an admitted request that does not reach a stub: a
// missing parameter and a body that is not JSON are 400 validation, a
// handler failure 500 without its cause; each with its counter. And
// their twins: the same requests put right reach the stub.
func TestRequestRefusalsAndFailures(t *testing.T) {
	e := newEcosystem(t)
	std := &core.Counters{}
	o := stdapi.Options{Guard: e.guard.Require, Counters: std}
	mux := http.NewServeMux()
	if err := stdapi.MountF3411(mux, stdapi.NotImplementedF3411{}, o); err != nil {
		t.Fatal(err)
	}
	dp := e.token(t, stdapi.ScopeRIDDisplayProvider)
	if got := call(t, mux, http.MethodGet, "/uss/flights", dp, ""); got.status != 400 || got.slug != httpx.SlugValidation || !strings.Contains(got.body, `"field":"view"`) {
		t.Fatalf("no view: %+v", got)
	}
	if got := call(t, mux, http.MethodGet, "/uss/flights?view=1,2,3,4&recent_positions_duration=x", dp, ""); got.status != 400 ||
		!strings.Contains(got.body, `"field":"recent_positions_duration"`) {
		t.Fatalf("bad duration: %+v", got)
	}
	if got := call(t, mux, http.MethodGet, "/uss/flights?view=1,2,3,4&recent_positions_duration=20", dp, ""); got.status != 501 {
		t.Fatalf("good parameters: %+v", got)
	}
	sp := e.token(t, stdapi.ScopeRIDServiceProvider)
	if got := call(t, mux, http.MethodPost, "/uss/identification_service_areas/x", sp, "{"); got.status != 400 || got.slug != httpx.SlugValidation {
		t.Fatalf("broken body: %+v", got)
	}
	if got := call(t, mux, http.MethodPost, "/uss/identification_service_areas/x", sp, "{}"); got.status != 501 {
		t.Fatalf("an object: %+v", got)
	}
	if std.Get(stdapi.CounterRequestRefused) != 3 {
		t.Errorf("request_refused %d", std.Get(stdapi.CounterRequestRefused))
	}

	mux = http.NewServeMux()
	if err := stdapi.MountF3548(mux, failing{}, o); err != nil {
		t.Fatal(err)
	}
	got := call(t, mux, http.MethodPost, "/uss/v1/reports", e.token(t, stdapi.ScopeStrategicCoordination), "{}")
	if got.status != 500 || got.slug != httpx.SlugInternal || strings.Contains(got.body, "secret") || std.Get(stdapi.CounterHandlerFailed) != 1 {
		t.Fatalf("failing handler: %+v", got)
	}
	if got := call(t, mux, http.MethodGet, "/uss/v1/constraints/x", e.token(t, stdapi.ScopeConstraintProcessing), ""); got.status != 501 {
		t.Fatalf("a stub beside it: %+v", got)
	}
}

// A mount whose table holds a scope the catalogue refuses fails, so the
// process refuses to start; with the catalogue it mounts.
func TestMountRefusesAnUnknownScope(t *testing.T) {
	e := newEcosystem(t)
	refuse := func(httpx.Access) error { return errors.New("not in the catalogue") }
	if err := stdapi.MountF3411(http.NewServeMux(), stdapi.NotImplementedF3411{}, stdapi.Options{Guard: e.guard.Require, Validate: refuse}); err == nil {
		t.Error("F3411 mounted with refused entries")
	}
	if err := stdapi.MountF3548(http.NewServeMux(), stdapi.NotImplementedF3548{}, stdapi.Options{Guard: e.guard.Require, Validate: refuse}); err == nil {
		t.Error("F3548 mounted with refused entries")
	}
	if err := stdapi.MountF3548(http.NewServeMux(), stdapi.NotImplementedF3548{}, stdapi.Options{Guard: e.guard.Require, Validate: auth.ValidateAccess}); err != nil {
		t.Errorf("F3548 with the catalogue: %v", err)
	}
}

// A mount without a guard is refused naming the field, not a panic at
// the first registration (audit N1); with one it mounts (E-01, above).
func TestMountRefusesANilGuard(t *testing.T) {
	for name, mount := range map[string]func() error{
		"F3411": func() error { return stdapi.MountF3411(http.NewServeMux(), stdapi.NotImplementedF3411{}, stdapi.Options{}) },
		"F3548": func() error { return stdapi.MountF3548(http.NewServeMux(), stdapi.NotImplementedF3548{}, stdapi.Options{}) },
	} {
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s panicked: %v", name, r)
				}
			}()
			err = mount()
		}()
		if err == nil || !strings.Contains(err.Error(), "guard") {
			t.Errorf("%s without a guard: %v", name, err)
		}
	}
}

// A standard-API body over the cap is 413 body_too_large, not a 400 as
// if it were malformed JSON (audit N2); a body under it is decoded (501
// from the stub).
func TestOversizedBodyIs413(t *testing.T) {
	e := newEcosystem(t)
	mux := http.NewServeMux()
	if err := stdapi.MountF3411(mux, stdapi.NotImplementedF3411{}, stdapi.Options{Guard: e.guard.Require}); err != nil {
		t.Fatal(err)
	}
	h := httpx.BodyCap(64, &core.Counters{})(mux)
	sp := e.token(t, stdapi.ScopeRIDServiceProvider)
	big := `{"pad":"` + strings.Repeat("x", 200) + `"}`
	if got := call(t, h, http.MethodPost, "/uss/identification_service_areas/x", sp, big); got.status != 413 || got.slug != httpx.SlugBodyTooLarge {
		t.Fatalf("over the cap: %+v", got)
	}
	if got := call(t, h, http.MethodPost, "/uss/identification_service_areas/x", sp, "{}"); got.status != 501 {
		t.Fatalf("under the cap: %+v", got)
	}
}
