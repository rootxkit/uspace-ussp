package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// The accesses of the routes the brief names, as their work packages
// will declare them: /v1/intents takes an operator token or a portal
// session, /v1/admin/* a console session.
var (
	intentsAccess = httpx.Access{Scopes: []string{ScopeIntents}, Sessions: []httpx.SessionAccess{{Realm: RealmPortal}}}
	adminAccess   = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: RealmConsole, Roles: []string{RoleSupervisor, RoleSupport, RoleAdmin}}}}
	recordsAccess = httpx.Access{Scopes: []string{"ussp.records"}}
)

type fakeSessions struct {
	mu      sync.Mutex
	refused map[string]bool
	fail    error
	checked int
}

func (f *fakeSessions) CheckSession(_ context.Context, jti, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked++
	if f.fail != nil {
		return f.fail
	}
	if f.refused[jti] {
		return ErrSessionRefused
	}
	return nil
}

type fakeAudit struct {
	mu       sync.Mutex
	refusals []Refusal
	fail     error
}

func (f *fakeAudit) AuthRefused(_ context.Context, r Refusal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals = append(f.refusals, r)
	return f.fail
}

func (f *fakeAudit) last(t *testing.T) Refusal {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refusals) == 0 {
		t.Fatal("no refusal audited")
	}
	return f.refusals[len(f.refusals)-1]
}

type guardFixture struct {
	c        *clock
	own      *Issuer
	eco      *ecosystem
	v        *Verifier
	sessions *fakeSessions
	audit    *fakeAudit
	counters *core.Counters
	guard    *Guard
}

func newGuardFixture(t testing.TB) *guardFixture {
	t.Helper()
	f := &guardFixture{c: newClock(t0()), own: newOwnIssuer(t, false), eco: newEcosystem(t), sessions: &fakeSessions{refused: map[string]bool{}},
		audit: &fakeAudit{}, counters: &core.Counters{}}
	f.v = newVerifier(t, f.own, f.eco, f.c)
	mustNoErr(t, f.v.BuildEcosystem(context.Background()))
	f.guard = &Guard{Verifier: f.v, OwnIssuer: ownIssuer, Sessions: f.sessions, Audit: f.audit, Counters: f.counters}
	return f
}

func (f *guardFixture) operator(t *testing.T, scopes ...string) string {
	t.Helper()
	got, err := f.own.IssueOperator("op-client-1", scopes, time.Hour, f.c.Now())
	mustNoErr(t, err)
	return got.Token
}

func (f *guardFixture) session(t *testing.T, realm, role, jti string) string {
	t.Helper()
	tok, _, err := f.own.IssueSession("acc-1", realm, jti, []string{role}, time.Hour, f.c.Now())
	mustNoErr(t, err)
	return tok
}

type result struct {
	status    int
	body      map[string]any
	principal *Principal
	header    http.Header
}

func (f *guardFixture) do(t *testing.T, a httpx.Access, method string, set func(r *http.Request)) result {
	t.Helper()
	var seen *Principal
	h := f.guard.Require(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			t.Fatal("no principal behind the guard")
		}
		seen = &p
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(method, "/v1/x", nil)
	if set != nil {
		set(r)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	res := result{status: rec.Code, principal: seen, header: rec.Header()}
	if rec.Code != http.StatusOK {
		if ct := rec.Header().Get("Content-Type"); ct != httpx.ProblemContentType {
			t.Fatalf("refusal content type %q", ct)
		}
		mustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &res.body))
		checkProblemShape(t, rec.Code, res.body)
	}
	return res
}

// checkProblemShape asserts the members the lab's problem/v1 requires
// (the full schema check is national's, against the pinned copy).
func checkProblemShape(t *testing.T, status int, body map[string]any) {
	t.Helper()
	if !regexp.MustCompile(`^https://schemas\.uspace\.ge/problems/[a-z_]+$`).MatchString(body["type"].(string)) ||
		body["status"] != float64(status) || body["title"] == "" {
		t.Fatalf("problem %v", body)
	}
	if _, ok := body["errors"].([]any); !ok {
		t.Fatalf("errors is not an array: %v", body)
	}
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func slug(res result) string {
	return strings.TrimPrefix(res.body["type"].(string), httpx.ProblemTypeBase)
}

// E-01: a token with the scope is admitted, the same token without it
// refused 403.
func TestGuardScopePair(t *testing.T) {
	f := newGuardFixture(t)
	res := f.do(t, intentsAccess, http.MethodPost, bearer(f.operator(t, ScopeIntents)))
	if res.status != 200 || res.principal.Session || res.principal.Claims.Subject != "op-client-1" {
		t.Fatalf("with the scope: %+v", res)
	}
	res = f.do(t, intentsAccess, http.MethodPost, bearer(f.operator(t, ScopeGeo)))
	if res.status != 403 || slug(res) != httpx.SlugForbidden || f.counters.Get(CounterScopeRefused) != 1 {
		t.Fatalf("without: %+v", res)
	}
	if r := f.audit.last(t); r.ActorType != ActorClient || r.ActorID != "op-client-1" || r.Reason != CounterScopeRefused || r.Status != 403 {
		t.Fatalf("audit %+v", r)
	}
}

// E-01: expired is 401 typed rejected_expired and counted; inside its
// life the same token is admitted.
func TestGuardExpiredPair(t *testing.T) {
	f := newGuardFixture(t)
	tok := f.operator(t, ScopeIntents)
	if res := f.do(t, intentsAccess, http.MethodGet, bearer(tok)); res.status != 200 {
		t.Fatalf("fresh: %d", res.status)
	}
	f.c.Add(2 * time.Hour)
	res := f.do(t, intentsAccess, http.MethodGet, bearer(tok))
	if res.status != 401 || slug(res) != coreauth.CounterRejectedExpired || f.v.CounterSets()[CounterSetOwn].Get(coreauth.CounterRejectedExpired) != 1 {
		t.Fatalf("expired: %d %v", res.status, res.body)
	}
	if res.header.Get("WWW-Authenticate") == "" {
		t.Fatal("no WWW-Authenticate")
	}
	errs := res.body["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["field"] != "exp" {
		t.Fatalf("errors %v", errs)
	}
	if r := f.audit.last(t); r.ActorType != ActorUnverified || r.ActorID != "op-client-1" || r.Reason != coreauth.CounterRejectedExpired {
		t.Fatalf("audit %+v", r)
	}
}

// E-01: aud = the lab alias is accepted, aud = another system's host
// refused 401 rejected_audience (M18).
func TestGuardAudiencePair(t *testing.T) {
	f := newGuardFixture(t)
	access := httpx.Access{Scopes: []string{"ussp.records"}}
	if res := f.do(t, access, http.MethodGet, bearer(f.eco.token(t, "authority-01", labAlias, []string{"ussp.records"}, f.c.Now()))); res.status != 200 {
		t.Fatalf("lab alias: %d %v", res.status, res.body)
	}
	res := f.do(t, access, http.MethodGet, bearer(f.eco.token(t, "authority-01", "uspace-cisp.test", []string{"ussp.records"}, f.c.Now())))
	if res.status != 401 || slug(res) != coreauth.CounterRejectedAudience {
		t.Fatalf("another host: %d %v", res.status, res.body)
	}
}

// E-01: a console session is admitted on /v1/admin/* and refused on
// /v1/intents; a portal session the reverse (M20).
func TestGuardRealmPairs(t *testing.T) {
	f := newGuardFixture(t)
	console := f.session(t, RealmConsole, RoleSupervisor, "s-console")
	portal := f.session(t, RealmPortal, RoleOperatorAdmin, "s-portal")
	cases := []struct {
		name   string
		access httpx.Access
		token  string
		want   int
	}{
		{"console on admin", adminAccess, console, 200},
		{"console on intents", intentsAccess, console, 403},
		{"portal on intents", intentsAccess, portal, 200},
		{"portal on admin", adminAccess, portal, 403},
		{"portal on records (scope only)", recordsAccess, portal, 403},
	}
	for _, c := range cases {
		res := f.do(t, c.access, http.MethodGet, bearer(c.token))
		if res.status != c.want {
			t.Errorf("%s: %d %v", c.name, res.status, res.body)
			continue
		}
		if c.want == 200 && (!res.principal.Session || res.principal.Claims.Realm == "") {
			t.Errorf("%s: principal %+v", c.name, res.principal)
		}
		if c.want == 403 && slug(res) != httpx.SlugForbidden {
			t.Errorf("%s: slug %s", c.name, slug(res))
		}
	}
	if f.counters.Get(CounterRealmRefused) != 3 {
		t.Fatalf("realm refusals %d", f.counters.Get(CounterRealmRefused))
	}
	if r := f.audit.last(t); r.ActorType != ActorPortalUser || r.ActorID != "acc-1" {
		t.Fatalf("audit %+v", r)
	}
}

// A role the entry does not list is refused; one it lists admitted.
func TestGuardRolePair(t *testing.T) {
	f := newGuardFixture(t)
	opAdmin := httpx.Access{Sessions: []httpx.SessionAccess{{Realm: RealmPortal, Roles: []string{RoleOperatorAdmin}}}}
	if res := f.do(t, opAdmin, http.MethodGet, bearer(f.session(t, RealmPortal, RoleOperatorAdmin, "s1"))); res.status != 200 {
		t.Fatalf("operator_admin: %d", res.status)
	}
	if res := f.do(t, opAdmin, http.MethodGet, bearer(f.session(t, RealmPortal, RoleViewer, "s2"))); res.status != 403 {
		t.Fatalf("viewer: %d", res.status)
	}
}

// E-01: the session row decides: a live session is admitted, an ended
// one is 401 session_refused, and a store that cannot answer is 503.
func TestGuardSessionStatePairs(t *testing.T) {
	f := newGuardFixture(t)
	tok := f.session(t, RealmConsole, RoleAdmin, "s-live")
	if res := f.do(t, adminAccess, http.MethodGet, bearer(tok)); res.status != 200 || f.sessions.checked != 1 {
		t.Fatalf("live: %d", res.status)
	}
	f.sessions.refused["s-live"] = true
	res := f.do(t, adminAccess, http.MethodGet, bearer(tok))
	if res.status != 401 || slug(res) != SlugSessionRefused || f.counters.Get(CounterSessionRefused) != 1 {
		t.Fatalf("ended: %d %v", res.status, res.body)
	}
	f.sessions.fail = errors.New("db down")
	res = f.do(t, adminAccess, http.MethodGet, bearer(tok))
	if res.status != 503 || slug(res) != SlugAuthUnavailable || f.counters.Get(CounterUnavailable) != 1 {
		t.Fatalf("store down: %d %v", res.status, res.body)
	}
	// Without a session checker (a process without the database) the
	// token alone decides.
	f.guard.Sessions = nil
	if res := f.do(t, adminAccess, http.MethodGet, bearer(tok)); res.status != 200 {
		t.Fatalf("stateless: %d", res.status)
	}
}

// E-01: the cookie with the double-submit token is admitted on a POST;
// without the header, or with another value, refused 403 csrf; a GET
// needs no token; a bearer needs none.
func TestGuardCookieAndCSRFPairs(t *testing.T) {
	f := newGuardFixture(t)
	tok := f.session(t, RealmPortal, RoleOperatorAdmin, "s-cookie")
	cookie := func(csrfHeader string) func(*http.Request) {
		return func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: CookieSession, Value: tok})
			r.AddCookie(&http.Cookie{Name: CookieCSRF, Value: "the-csrf"})
			if csrfHeader != "" {
				r.Header.Set(HeaderCSRF, csrfHeader)
			}
		}
	}
	if res := f.do(t, intentsAccess, http.MethodPost, cookie("the-csrf")); res.status != 200 || !res.principal.ViaCookie {
		t.Fatalf("cookie with csrf: %d", res.status)
	}
	for name, h := range map[string]string{"no header": "", "other value": "forged"} {
		res := f.do(t, intentsAccess, http.MethodPost, cookie(h))
		if res.status != 403 || slug(res) != SlugCSRF {
			t.Errorf("%s: %d %v", name, res.status, res.body)
		}
	}
	if res := f.do(t, intentsAccess, http.MethodGet, cookie("")); res.status != 200 {
		t.Fatalf("GET with the cookie: %d", res.status)
	}
	if res := f.do(t, intentsAccess, http.MethodPost, bearer(tok)); res.status != 200 || res.principal.ViaCookie {
		t.Fatalf("bearer: %d", res.status)
	}
	if f.counters.Get(CounterCSRFRefused) != 2 {
		t.Fatalf("csrf refusals %d", f.counters.Get(CounterCSRFRefused))
	}
}

// No credential, a non-bearer Authorization, a token that does not
// parse: 401, audited with actor unknown (the sub cannot be read).
func TestGuardRefusesMissingAndMalformedCredentials(t *testing.T) {
	f := newGuardFixture(t)
	for name, set := range map[string]func(*http.Request){
		"none":      nil,
		"basic":     func(r *http.Request) { r.Header.Set("Authorization", "Basic YTpi") },
		"garbage":   bearer("not.a.jwt"),
		"empty jar": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieSession, Value: ""}) },
	} {
		res := f.do(t, intentsAccess, http.MethodGet, set)
		if res.status != 401 {
			t.Errorf("%s: %d", name, res.status)
			continue
		}
		if r := f.audit.last(t); r.ActorID != "unknown" {
			t.Errorf("%s: actor %q", name, r.ActorID)
		}
	}
}

// Operator scopes are honoured only from this issuer and ecosystem
// scopes only from the ecosystem: an authority token that claims
// ussp.intents is refused, as is a session of another issuer.
func TestGuardKeepsScopesToTheirIssuers(t *testing.T) {
	f := newGuardFixture(t)
	if res := f.do(t, intentsAccess, http.MethodPost, bearer(f.eco.token(t, "authority-01", ownHost, []string{ScopeIntents}, f.c.Now()))); res.status != 403 {
		t.Fatalf("ecosystem token with an operator scope: %d", res.status)
	}
	if res := f.do(t, recordsAccess, http.MethodGet, bearer(f.eco.token(t, "authority-01", ownHost, []string{"ussp.records"}, f.c.Now()))); res.status != 200 {
		t.Fatalf("ecosystem token with its scope: %d", res.status)
	}
	// An ecosystem token that calls itself a session.
	res := f.do(t, adminAccess, http.MethodGet, bearer(f.eco.token(t, "x", ownHost, []string{SessionScope}, f.c.Now())))
	if res.status != 403 {
		t.Fatalf("a session of another issuer: %d", res.status)
	}
	// A session token without realm or roles (only possible from a
	// mis-issued token) is refused.
	f.guard.OwnIssuer = f.eco.URL
	res = f.do(t, adminAccess, http.MethodGet, bearer(f.eco.token(t, "x", ownHost, []string{SessionScope}, f.c.Now())))
	if res.status != 403 {
		t.Fatalf("a session without realm: %d", res.status)
	}
}

// An ecosystem token before its issuer's keys were ever fetched is 503,
// not 401: the fault is ours.
func TestGuardAnswers503BeforeTheJWKS(t *testing.T) {
	f := newGuardFixture(t)
	v := newVerifier(t, f.own, f.eco, f.c) // not built
	f.guard.Verifier = v
	res := f.do(t, recordsAccess, http.MethodGet, bearer(f.eco.token(t, "authority-01", ownHost, []string{"ussp.records"}, f.c.Now())))
	if res.status != 503 || slug(res) != SlugAuthUnavailable {
		t.Fatalf("got %d %v", res.status, res.body)
	}
}

// An audit row that cannot be written is counted and the refusal
// stands; without an auditor the refusal is still counted.
func TestGuardRefusalStandsWithoutItsAuditRow(t *testing.T) {
	f := newGuardFixture(t)
	f.audit.fail = errors.New("db down")
	if res := f.do(t, intentsAccess, http.MethodGet, nil); res.status != 401 || f.counters.Get(CounterNotAudited) != 1 {
		t.Fatalf("got %d, not audited %d", res.status, f.counters.Get(CounterNotAudited))
	}
	f.guard.Audit = nil
	if res := f.do(t, intentsAccess, http.MethodGet, nil); res.status != 401 || f.counters.Get(CounterNoCredential) != 2 {
		t.Fatal("refusal without an auditor")
	}
}

func TestClipBoundsAndSanitises(t *testing.T) {
	long := strings.Repeat("é", 100)
	if got := clip(long); len(got) > 128 || !strings.HasPrefix(long, got) {
		t.Fatalf("%q", got)
	}
	if got := clip("a\nb\x00c\xff"); got != "a?b?c?" {
		t.Fatalf("%q", got)
	}
}

func FuzzGuard(f *testing.F) {
	fx := newGuardFixture(f)
	f.Add("Bearer x.y.z", "", "")
	f.Add("", "eyJhbGciOiJSUzI1NiJ9.e30.x", "csrf")
	f.Add("Bearer "+strings.Repeat("a", 9000), "", "")
	f.Fuzz(func(t *testing.T, authz, cookie, csrf string) {
		h := fx.guard.Require(intentsAccess)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("an unsigned credential was admitted")
		}))
		r := httptest.NewRequest(http.MethodPost, "/v1/intents", nil)
		if authz != "" {
			r.Header["Authorization"] = []string{authz}
		}
		if cookie != "" {
			r.Header["Cookie"] = []string{CookieSession + "=" + cookie + "; " + CookieCSRF + "=" + csrf}
			r.Header.Set(HeaderCSRF, csrf)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 401 && rec.Code != 403 {
			t.Fatalf("status %d", rec.Code)
		}
		if authz != "" && len(authz) > 12 && strings.Contains(rec.Body.String(), authz[7:]) {
			t.Fatal("the refusal echoes the token")
		}
	})
}
