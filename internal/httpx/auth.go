package httpx

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
)

// Counter names of the scope middleware. A refusal by the verifier is
// also counted under the verifier's own TokenError counter.
//
//nolint:gosec // G101: counter names, not credentials
const (
	CounterNoBearer      = "auth_no_bearer"
	CounterTokenRefused  = "auth_token_refused"
	CounterScopeRefused  = "auth_scope_refused"
	CounterTokenAccepted = "auth_token_accepted"
)

// TokenVerifier verifies a bearer token. *auth.Verifier of uspace-core
// implements it; WP-2 wires one built from config.VerifierConfig, which
// has StrictSessionClaims on.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (auth.Claims, error)
}

const claimsKey ctxKey = 200

// Bearer returns the token of an "Authorization: Bearer <token>" header,
// or "" when there is none. The scheme is case-insensitive (RFC 9110).
func Bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// ClaimsFrom returns the claims RequireScope verified, and whether there
// are any.
func ClaimsFrom(ctx context.Context) (auth.Claims, bool) {
	c, ok := ctx.Value(claimsKey).(auth.Claims)
	return c, ok
}

// RequireScope admits a request whose bearer token v accepts and which
// grants scope: no token or a refused one is a 401 unauthenticated
// problem naming the claim at fault (never the token), a token without
// the scope is a 403 forbidden problem. The verified claims are in the
// request context (ClaimsFrom). Each outcome is counted.
func RequireScope(v TokenVerifier, scope string, counters *core.Counters) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := Bearer(r)
			if token == "" {
				counters.Inc(CounterNoBearer)
				w.Header().Set("WWW-Authenticate", `Bearer`)
				NewProblem(http.StatusUnauthorized, SlugUnauthenticated, "", "a bearer token is required").Write(w, r)
				return
			}
			claims, err := v.Verify(r.Context(), token)
			if err != nil {
				counters.Inc(CounterTokenRefused)
				var te *auth.TokenError
				detail := "the token was refused"
				var errs []*core.FieldError
				if errors.As(err, &te) {
					errs = append(errs, &core.FieldError{Field: te.Claim, Reason: te.Reason})
				}
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				NewProblem(http.StatusUnauthorized, SlugUnauthenticated, "", detail, errs...).Write(w, r)
				return
			}
			if err := auth.RequireScope(claims, scope); err != nil {
				counters.Inc(CounterScopeRefused)
				NewProblem(http.StatusForbidden, SlugForbidden, "", "the token does not grant "+scope,
					&core.FieldError{Field: "scope", Reason: "missing " + scope}).Write(w, r)
				return
			}
			counters.Inc(CounterTokenAccepted)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
		})
	}
}
