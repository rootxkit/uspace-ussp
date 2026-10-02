package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Cookie and header names of the session contract (M21).
const (
	CookieSession = "uspace_session"
	CookieCSRF    = "uspace_csrf"
	HeaderCSRF    = "X-CSRF-Token"
)

// Counter names of the guard; each refusal has its own (E-09).
//
//nolint:gosec // G101: counter names, not credentials
const (
	CounterNoCredential   = "auth_no_credential"
	CounterTokenRefused   = "auth_token_refused"
	CounterCSRFRefused    = "auth_csrf_refused"
	CounterScopeRefused   = "auth_scope_refused"
	CounterRealmRefused   = "auth_realm_refused"
	CounterSessionRefused = "auth_session_refused"
	CounterUnavailable    = "auth_unavailable"
	CounterAccepted       = "auth_accepted"
	CounterNotAudited     = "auth_refusal_not_audited"
)

// Problem slugs of the guard beside core's TokenError counters, which
// type the refusal of a token (rejected_expired, rejected_audience, ...).
const (
	SlugCSRF            = "csrf"
	SlugSessionRefused  = "session_refused"
	SlugAuthUnavailable = "auth_unavailable"
)

// Actor types of a refusal's audit row.
const (
	ActorAnonymous  = "anonymous"          // no credential
	ActorUnverified = "unverified_subject" // the sub of a token that did not verify
	ActorClient     = "client"             // a verified machine token's sub
	ActorPortalUser = "operator_user"      // a verified portal session's sub
	ActorStaff      = "staff"              // a verified console session's sub
)

// TokenVerifier verifies a token; *Verifier implements it.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (coreauth.Claims, error)
}

// ErrSessionRefused is a session token whose session is not live:
// unknown, ended, expired or idle.
var ErrSessionRefused = errors.New("session refused")

// SessionChecker is the stateful half of a session's verification (the
// session row: revoked, idle). An error wrapping ErrSessionRefused
// refuses the session; any other error is the store being unavailable.
type SessionChecker interface {
	CheckSession(ctx context.Context, jti, sub string) error
}

// Refusal is one refused request, for the audit log.
type Refusal struct {
	ActorType string
	ActorID   string
	Route     string
	Reason    string // the counter or token refusal reason
	Status    int
	RemoteIP  string
	RequestID string
}

// Auditor records refusals (an events row each). It must not fail the
// refusal: an error is counted and logged by the guard.
type Auditor interface {
	AuthRefused(ctx context.Context, r Refusal) error
}

// Principal is the authenticated caller of a request.
type Principal struct {
	Claims coreauth.Claims
	// Session is true for a session token (scope "session"); Realm and
	// Roles are then the token's.
	Session   bool
	ViaCookie bool
}

type principalKey struct{}

// PrincipalFrom returns the caller the guard admitted.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// WithPrincipal puts p on ctx (tests of handlers behind the guard).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Guard enforces an httpx.Access on a request (it is the httpx.Guard
// of the national router). It verifies the bearer token, or the
// uspace_session cookie with its CSRF double submit, through Verifier;
// admits a machine token that grants one of the scopes, operator scopes
// only from this USSP's issuer and ecosystem scopes only from the
// ecosystem's; admits a session of this USSP's issuer whose realm and
// roles match, after Sessions says it is live; refuses everything else.
// Every refusal is a problem, a counter and an Auditor call; the token
// never appears in any of them.
type Guard struct {
	Verifier  TokenVerifier
	OwnIssuer string
	// Sessions checks the session row; nil verifies sessions statelessly
	// (a process without the relational database).
	Sessions SessionChecker
	// Audit records refusals; nil counts and logs them only.
	Audit    Auditor
	Counters *core.Counters
	Logger   *slog.Logger
}

// refusal is a guard decision against the request.
type refusal struct {
	status  int
	slug    string
	counter string
	detail  string
	field   *core.FieldError
	actor   string
	actorID string
	tokErr  error
}

// Require is the middleware enforcing a.
func (g *Guard) Require(a httpx.Access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ref := g.authenticate(r, a)
			if ref != nil {
				g.refuse(w, r, ref)
				return
			}
			g.count(CounterAccepted)
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func (g *Guard) count(name string) {
	if g.Counters != nil {
		g.Counters.Inc(name)
	}
}

// credential returns the bearer token, or the session cookie (viaCookie).
func credential(r *http.Request) (token string, viaCookie bool, malformed bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		tok := httpx.Bearer(r)
		return tok, false, tok == ""
	}
	if c, err := r.Cookie(CookieSession); err == nil && c.Value != "" {
		return c.Value, true, false
	}
	return "", false, false
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// csrfOK is the double-submit check of a cookie-authenticated request
// with a method that changes state: the X-CSRF-Token header equals the
// uspace_csrf cookie, compared in constant time.
func csrfOK(r *http.Request) bool {
	c, err := r.Cookie(CookieCSRF)
	h := r.Header.Get(HeaderCSRF)
	if err != nil || c.Value == "" || h == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(h)) == 1
}

func (g *Guard) authenticate(r *http.Request, a httpx.Access) (Principal, *refusal) {
	token, viaCookie, malformed := credential(r)
	if token == "" {
		reason := "a bearer token or the session cookie is required"
		if malformed {
			reason = "the Authorization header is not a bearer token"
		}
		return Principal{}, &refusal{status: http.StatusUnauthorized, slug: httpx.SlugUnauthenticated, counter: CounterNoCredential,
			detail: reason, actor: ActorAnonymous, actorID: "unknown"}
	}
	if viaCookie && !safeMethod(r.Method) && !csrfOK(r) {
		return Principal{}, &refusal{status: http.StatusForbidden, slug: SlugCSRF, counter: CounterCSRFRefused,
			detail: "a request authenticated by the session cookie must send the uspace_csrf value as X-CSRF-Token",
			field:  &core.FieldError{Field: HeaderCSRF, Reason: "missing or not the uspace_csrf value"},
			actor:  ActorUnverified, actorID: subjectOf(token)}
	}
	claims, err := g.Verifier.Verify(r.Context(), token)
	if err != nil {
		var te *coreauth.TokenError
		if errors.As(err, &te) && te.Counter == CounterJWKSUnavailable {
			return Principal{}, &refusal{status: http.StatusServiceUnavailable, slug: SlugAuthUnavailable, counter: CounterUnavailable,
				detail: "the token's issuer keys are not available yet; retry later", actor: ActorUnverified, actorID: subjectOf(token)}
		}
		return Principal{}, &refusal{status: http.StatusUnauthorized, counter: CounterTokenRefused, tokErr: err,
			actor: ActorUnverified, actorID: subjectOf(token)}
	}
	p := Principal{Claims: claims, ViaCookie: viaCookie, Session: slices.Contains(claims.Scopes, SessionScope)}
	if !p.Session {
		return g.admitMachine(p, a)
	}
	return g.admitSession(r, p, a)
}

func (g *Guard) admitMachine(p Principal, a httpx.Access) (Principal, *refusal) {
	c := p.Claims
	for _, s := range a.Scopes {
		// An operator scope is honoured only on a token of this USSP's
		// issuer, an ecosystem scope only on a token of another.
		if c.HasScope(s) && IsOperatorScope(s) == (c.Issuer == g.OwnIssuer) {
			return p, nil
		}
	}
	want := "a session"
	if len(a.Scopes) > 0 {
		want = "one of " + strings.Join(a.Scopes, ", ")
	}
	return Principal{}, &refusal{status: http.StatusForbidden, slug: httpx.SlugForbidden, counter: CounterScopeRefused,
		detail: "the token does not grant " + want, field: &core.FieldError{Field: "scope", Reason: "missing " + want},
		actor: ActorClient, actorID: clip(c.Subject)}
}

func (g *Guard) admitSession(r *http.Request, p Principal, a httpx.Access) (Principal, *refusal) {
	c := p.Claims
	actor := ActorPortalUser
	if c.Realm == RealmConsole {
		actor = ActorStaff
	}
	forbidden := func(reason string) *refusal {
		return &refusal{status: http.StatusForbidden, slug: httpx.SlugForbidden, counter: CounterRealmRefused,
			detail: "this session may not use this operation", field: &core.FieldError{Field: "realm", Reason: reason},
			actor: actor, actorID: clip(c.Subject)}
	}
	if g.OwnIssuer == "" || c.Issuer != g.OwnIssuer {
		return Principal{}, forbidden("a session of another issuer")
	}
	if _, ok := RealmRoles[c.Realm]; !ok || len(c.Roles) == 0 {
		return Principal{}, forbidden("no realm or no role")
	}
	if g.Sessions != nil {
		if err := g.Sessions.CheckSession(r.Context(), c.JTI, c.Subject); err != nil {
			if errors.Is(err, ErrSessionRefused) {
				return Principal{}, &refusal{status: http.StatusUnauthorized, slug: SlugSessionRefused, counter: CounterSessionRefused,
					detail: "the session has ended; sign in again", actor: actor, actorID: clip(c.Subject)}
			}
			obs.Error(r.Context(), g.logger(), "session check failed", err)
			return Principal{}, &refusal{status: http.StatusServiceUnavailable, slug: SlugAuthUnavailable, counter: CounterUnavailable,
				detail: "the session cannot be checked now; retry later", actor: actor, actorID: clip(c.Subject)}
		}
	}
	for _, sa := range a.Sessions {
		if sa.Realm != c.Realm {
			continue
		}
		if len(sa.Roles) == 0 || slices.ContainsFunc(c.Roles, func(role string) bool { return slices.Contains(sa.Roles, role) }) {
			return p, nil
		}
		return Principal{}, forbidden("the session's role is not admitted")
	}
	return Principal{}, forbidden("the " + c.Realm + " realm is not admitted")
}

func (g *Guard) logger() *slog.Logger {
	if g.Logger == nil {
		return obs.Discard()
	}
	return g.Logger
}

func (g *Guard) refuse(w http.ResponseWriter, r *http.Request, ref *refusal) {
	g.record(r, ref)
	if ref.tokErr != nil {
		httpx.TokenRefused(w, r, ref.tokErr)
		return
	}
	if ref.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	var errs []*core.FieldError
	if ref.field != nil {
		errs = append(errs, ref.field)
	}
	httpx.NewProblem(ref.status, ref.slug, "", ref.detail, errs...).Write(w, r)
}

// record counts a refusal and writes its audit row.
func (g *Guard) record(r *http.Request, ref *refusal) {
	g.count(ref.counter)
	reason := ref.counter
	if ref.tokErr != nil {
		var te *coreauth.TokenError
		if errors.As(ref.tokErr, &te) {
			reason = te.Counter
		}
	}
	if g.Audit != nil {
		err := g.Audit.AuthRefused(r.Context(), Refusal{
			ActorType: ref.actor, ActorID: ref.actorID, Route: httpx.Route(r), Reason: reason, Status: ref.status,
			RemoteIP: httpx.RemoteIP(r), RequestID: httpx.RequestIDFrom(r.Context()),
		})
		if err != nil {
			g.count(CounterNotAudited)
			obs.Error(r.Context(), g.logger(), "refusal not audited", err, slog.String("reason", reason))
		}
	}
}

// subjectOf is the actor of a refusal before verification: the token's
// sub when it parses, else "unknown" (brief WP-2). Untrusted; clipped.
func subjectOf(token string) string {
	if s := clip(peek(token).Sub); s != "" {
		return s
	}
	return "unknown"
}

// clip bounds a value from a request before it is stored or logged:
// valid UTF-8, no control characters, at most 128 bytes.
func clip(v string) string {
	v = strings.ToValidUTF8(v, "?")
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, v)
	for len(v) > 128 {
		v = v[:128]
		for len(v) > 0 && !utf8.ValidString(v) {
			v = v[:len(v)-1]
		}
	}
	return v
}
