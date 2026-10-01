package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
)

// fakeVerifier accepts the token "good" with the given scopes and
// refuses every other one the way core's verifier does.
type fakeVerifier struct{ scopes []string }

func (f fakeVerifier) Verify(_ context.Context, token string) (auth.Claims, error) {
	if token != "good" {
		return auth.Claims{}, &auth.TokenError{Counter: auth.CounterRejectedSignature, Claim: "signature", Reason: "does not verify"}
	}
	return auth.Claims{Subject: "client-1", Scopes: f.scopes}, nil
}

func TestBearer(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc":   "abc",
		"bearer  abc ": "abc",
		"Basic abc":    "",
		"Bearer":       "",
		"":             "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := Bearer(r); got != want {
			t.Errorf("%q: got %q, want %q", header, got, want)
		}
	}
}

// Every refusal of RequireScope beside the request it admits (E-01).
func TestRequireScope(t *testing.T) {
	var seen auth.Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = ClaimsFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		name    string
		header  string
		scopes  []string
		status  int
		slug    string
		counter string
	}{
		{"no token", "", nil, 401, SlugUnauthenticated, CounterNoBearer},
		{"refused token", "Bearer forged", []string{"ussp.intents"}, 401, SlugUnauthenticated, CounterTokenRefused},
		{"scope missing", "Bearer good", []string{"ussp.geo"}, 403, SlugForbidden, CounterScopeRefused},
		{"admitted", "Bearer good", []string{"ussp.geo", "ussp.intents"}, 204, "", CounterTokenAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen = auth.Claims{}
			c := &core.Counters{}
			h := RequireScope(fakeVerifier{scopes: tc.scopes}, "ussp.intents", c)(next)
			r := httptest.NewRequest(http.MethodPost, "/v1/intents", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tc.status || c.Get(tc.counter) != 1 {
				t.Fatalf("status %d counter %s=%d", rec.Code, tc.counter, c.Get(tc.counter))
			}
			if tc.status == 204 {
				if seen.Subject != "client-1" {
					t.Fatalf("claims not in the context: %+v", seen)
				}
				return
			}
			if seen.Subject != "" {
				t.Fatal("a refused request reached the handler")
			}
			p := decodeProblem(t, rec)
			if p.Slug() != tc.slug {
				t.Fatalf("slug %s, want %s", p.Slug(), tc.slug)
			}
			if tc.counter == CounterTokenRefused && (len(p.Errors) != 1 || p.Errors[0].Field != "signature") {
				t.Fatalf("the refused claim is not named: %+v", p.Errors)
			}
		})
	}
}

func TestProblemWritesTheSharedShape(t *testing.T) {
	rec := httptest.NewRecorder()
	Problem(rec, http.StatusServiceUnavailable, "cis_stale", "the CIS is 900 s old",
		&Extra{Instance: "/v1/geo", Errors: []*core.FieldError{{Field: "cis_age_s", Reason: "above 600"}}})
	p := decodeProblem(t, rec)
	if p.Slug() != "cis_stale" || p.Instance != "/v1/geo" || p.Title != "Service Unavailable" || len(p.Errors) != 1 {
		t.Fatalf("got %+v", p)
	}
	rec = httptest.NewRecorder()
	Problem(rec, http.StatusNotFound, SlugNotFound, "", nil)
	if p := decodeProblem(t, rec); p.Slug() != SlugNotFound || len(p.Errors) != 0 {
		t.Fatalf("got %+v", p)
	}
}
