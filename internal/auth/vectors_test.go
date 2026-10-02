package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/vectors"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

type jwtFixtures struct {
	Issuer   string          `json:"issuer"`
	Audience string          `json:"audience"`
	MaxSkewS float64         `json:"max_skew_s"`
	JWKS     json.RawMessage `json:"jwks"`
}

type jwtInput struct {
	Token        string `json:"token"`
	NowS         int64  `json:"now_s"`
	RequireScope string `json:"require_scope"`
}

type jwtExpected struct {
	Accepted       bool     `json:"accepted"`
	Reason         string   `json:"reason"`
	Claim          string   `json:"claim"`
	Subject        string   `json:"subject"`
	Scopes         []string `json:"scopes"`
	RequireScopeOK *bool    `json:"require_scope_ok"`
}

// jwt_verify.json, the cases owned by ussp, through this repository's
// middleware (brief WP-2): the vector's issuer is an allow-listed
// ecosystem issuer whose JWKS a local server publishes, its audience is
// USSP_AUDIENCES, its skew the verifier's, its now the clock. Each
// token is presented as a bearer to a route that requires the case's
// scope (rid.read when the case names none); the status, the problem
// type, the claim at fault and core's counter are asserted. No judgement
// is re-implemented: the verdicts are core's, the plumbing is ours.
func TestVectorsJWTVerifyThroughTheMiddleware(t *testing.T) {
	f := vectors.Load(t, "jwt_verify.json")
	var fx jwtFixtures
	vectors.Unmarshal(t, f.Fixtures, &fx)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fx.JWKS) }))
	defer jwks.Close()

	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var in jwtInput
		var exp jwtExpected
		c.Decode(t, &in, &exp)
		now := time.Unix(in.NowS, 0).UTC()
		v, err := NewVerifier(context.Background(), VerifierConfig{Ecosystem: coreauth.Config{
			Issuers:             map[string]coreauth.IssuerConfig{fx.Issuer: {JWKSURL: jwks.URL}},
			Audiences:           []string{fx.Audience},
			StrictSessionClaims: true,
			MaxSkew:             time.Duration(fx.MaxSkewS * float64(time.Second)),
			Now:                 func() time.Time { return now },
		}})
		mustNoErr(t, err)
		mustNoErr(t, v.BuildEcosystem(context.Background()))
		scope := in.RequireScope
		if scope == "" {
			scope = "rid.read"
		}
		g := &Guard{Verifier: v, Counters: &core.Counters{}}
		var seen *Principal
		h := g.Require(httpx.Access{Scopes: []string{scope}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := PrincipalFrom(r.Context())
			seen = &p
			w.WriteHeader(http.StatusOK)
		}))
		r := httptest.NewRequest(http.MethodGet, "/v1/records/daily/2026-10-02", nil)
		r.Header.Set("Authorization", "Bearer "+in.Token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		eco := v.CounterSets()[CounterSetEcosystem]
		if !exp.Accepted {
			var p httpx.ProblemBody
			mustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &p))
			if rec.Code != http.StatusUnauthorized || p.Slug() != exp.Reason || len(p.Errors) != 1 || p.Errors[0].Field != exp.Claim {
				t.Fatalf("got %d %s, want 401 %s on %s", rec.Code, rec.Body.String(), exp.Reason, exp.Claim)
			}
			if eco.Get(exp.Reason) != 1 || g.Counters.Get(CounterTokenRefused) != 1 {
				t.Fatalf("counter %s not counted: %v %v", exp.Reason, eco.Snapshot(), g.Counters.Snapshot())
			}
			if strings.Contains(rec.Body.String(), in.Token) {
				t.Fatal("the refusal echoes the token")
			}
			return
		}
		if exp.RequireScopeOK != nil && !*exp.RequireScopeOK {
			if rec.Code != http.StatusForbidden || g.Counters.Get(CounterScopeRefused) != 1 {
				t.Fatalf("got %d, want 403 for the missing scope %s", rec.Code, scope)
			}
			return
		}
		if rec.Code != http.StatusOK || seen == nil || eco.Get(coreauth.CounterAccepted) != 1 {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
		if seen.Claims.Subject != exp.Subject || (exp.Scopes != nil && !slices.Equal(seen.Claims.Scopes, exp.Scopes)) {
			t.Fatalf("principal %+v, want %s %v", seen.Claims, exp.Subject, exp.Scopes)
		}
	})
}
