package testif

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// Counters of the test listener's guard (E-09): each refusal has its own.
const (
	CounterRefusedNoToken     = "testif_refused_no_token"
	CounterRefusedToken       = "testif_refused_token"
	CounterRefusedScope       = "testif_refused_scope"
	CounterRefusedUnavailable = "testif_refused_unavailable"
	CounterAccepted           = "testif_accepted"
)

// Verifier verifies a bearer token: the same uspace-core verifier, issuers
// and USSP_AUDIENCES as every process (M18), built by proc.TokenVerifier.
type Verifier interface {
	Verify(ctx context.Context, token string) (coreauth.Claims, error)
}

// Guard admits a request whose bearer token the verifier accepts and
// which grants one of the entry's scopes (all of them from the closed
// list, ValidateAccess). No credential is 401, a refused token 401 typed
// by the verifier's reason, another scope 403, the issuer's keys not yet
// read 503; each with the problem body, each counted. A session token
// is never admitted: its scope is "session".
type Guard struct {
	Verifier Verifier
	Counters *core.Counters
}

type claimsKey struct{}

// ClaimsFrom returns the verified claims of the request.
func ClaimsFrom(ctx context.Context) (coreauth.Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(coreauth.Claims)
	return c, ok
}

// Require is the httpx.Guard of the test listener.
func (g *Guard) Require(a httpx.Access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := httpx.Bearer(r)
			if token == "" {
				g.Counters.Inc(CounterRefusedNoToken)
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpx.NewProblem(http.StatusUnauthorized, httpx.SlugUnauthenticated, "", "a bearer token is required").Write(w, r)
				return
			}
			claims, err := g.Verifier.Verify(r.Context(), token)
			if err != nil {
				var te *coreauth.TokenError
				if errors.As(err, &te) && te.Counter == auth.CounterJWKSUnavailable {
					g.Counters.Inc(CounterRefusedUnavailable)
					httpx.RetryAfter(w, retryAfter)
					httpx.NewProblem(http.StatusServiceUnavailable, auth.SlugAuthUnavailable, "", "the token's issuer keys are not available yet; retry later").Write(w, r)
					return
				}
				g.Counters.Inc(CounterRefusedToken)
				httpx.TokenRefused(w, r, err)
				return
			}
			if !slices.ContainsFunc(a.Scopes, claims.HasScope) {
				g.Counters.Inc(CounterRefusedScope)
				want := strings.Join(a.Scopes, ", ")
				httpx.NewProblem(http.StatusForbidden, httpx.SlugForbidden, "", "the token does not grant "+want,
					&core.FieldError{Field: "scope", Reason: "missing " + want}).Write(w, r)
				return
			}
			g.Counters.Inc(CounterAccepted)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, claims)))
		})
	}
}
