package testif

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// fakeVerifier admits "tok-<scope> <scope>..." with those scopes; any
// other token is refused as expired, "jwks" as the issuer keys missing.
type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, tok string) (coreauth.Claims, error) {
	switch {
	case tok == "jwks":
		return coreauth.Claims{}, &coreauth.TokenError{Counter: auth.CounterJWKSUnavailable, Claim: "kid", Reason: "keys not read"}
	case strings.HasPrefix(tok, "tok-"):
		return coreauth.Claims{Subject: "lab-01", Scopes: strings.Split(strings.TrimPrefix(tok, "tok-"), ",")}, nil
	}
	return coreauth.Claims{}, &coreauth.TokenError{Counter: "rejected_expired", Claim: "exp", Reason: "expired"}
}

func guarded(t *testing.T, counters *core.Counters, access map[string]httpx.Access) http.Handler {
	t.Helper()
	s := &Server{Guard: &Guard{Verifier: fakeVerifier{}, Counters: counters}}
	s.interfaces = append(s.interfaces, iface{name: "probe", access: access, register: func(g *httpx.GuardedMux) {
		for p := range access {
			g.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
				if _, ok := ClaimsFrom(r.Context()); !ok {
					t.Error("admitted without claims")
				}
				w.WriteHeader(http.StatusNoContent)
			})
		}
	}})
	mux := http.NewServeMux()
	if err := s.Mount(mux); err != nil {
		t.Fatal(err)
	}
	return mux
}

func call(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Without a credential 401, a refused token 401 typed by its reason,
// another scope 403, the keys not read 503, each with the problem body
// and its counter; the file's scope is admitted (E-01).
func TestGuard(t *testing.T) {
	counters := &core.Counters{}
	h := guarded(t, counters, FlightPlanningAccess())
	status := BaseFlightPlanning + "/status"
	cases := []struct {
		token   string
		code    int
		slug    string
		counter string
	}{
		{"", 401, httpx.SlugUnauthenticated, CounterRefusedNoToken},
		{"garbage", 401, "rejected_expired", CounterRefusedToken},
		{"tok-" + ScopePlan, 403, httpx.SlugForbidden, CounterRefusedScope},
		{"tok-rid.service_provider,session", 403, httpx.SlugForbidden, CounterRefusedScope},
		{"jwks", 503, auth.SlugAuthUnavailable, CounterRefusedUnavailable},
		{"tok-" + ScopeDirectAutomatedTest, 204, "", CounterAccepted},
	}
	for _, c := range cases {
		before := counters.Snapshot()[c.counter]
		w := call(h, http.MethodGet, status, c.token)
		if w.Code != c.code {
			t.Errorf("%q: %d, want %d", c.token, w.Code, c.code)
		}
		if c.slug != "" {
			var p httpx.ProblemBody
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Slug() != c.slug || w.Header().Get("Content-Type") != httpx.ProblemContentType {
				t.Errorf("%q: problem %+v (%v), want %s", c.token, p, err, c.slug)
			}
		}
		if counters.Snapshot()[c.counter] != before+1 {
			t.Errorf("%q: %s not counted", c.token, c.counter)
		}
	}
	// The plan scope is the upsert's, not the status's: per operation.
	if w := call(h, http.MethodPut, BaseFlightPlanning+"/flight_plans/x", "tok-"+ScopePlan); w.Code != 204 {
		t.Errorf("upsert with the plan scope: %d", w.Code)
	}
	// Nothing outside the table is served.
	if w := call(h, http.MethodGet, "/v1/intents", "tok-"+ScopePlan); w.Code != 404 {
		t.Errorf("/v1/intents on the test listener: %d", w.Code)
	}
}

// Mount refuses an interface whose entry names a catalogue scope, and an
// entry without a route: the start fails closed.
func TestMountFailsClosed(t *testing.T) {
	s := &Server{Guard: &Guard{Verifier: fakeVerifier{}, Counters: &core.Counters{}}}
	s.interfaces = []iface{{name: "bad", access: map[string]httpx.Access{
		"GET /test/x": {Scopes: []string{auth.ScopeIntents}},
		"GET /test/y": {Scopes: []string{ScopePlan}},
	}, register: func(g *httpx.GuardedMux) {
		g.HandleFunc("GET /test/x", func(http.ResponseWriter, *http.Request) {})
	}}}
	err := s.Mount(http.NewServeMux())
	if err == nil || !strings.Contains(err.Error(), "GET /test/x") || !strings.Contains(err.Error(), "GET /test/y") {
		t.Fatalf("%v", err)
	}
}
