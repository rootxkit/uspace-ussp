package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// RFC 6749 §5.2 error codes (and RFC 8707's invalid_target) this
// endpoint answers; each is also the problem type's slug.
const (
	OAuthInvalidRequest       = "invalid_request"
	OAuthInvalidClient        = "invalid_client"
	OAuthInvalidScope         = "invalid_scope"
	OAuthInvalidTarget        = "invalid_target"
	OAuthUnauthorizedClient   = "unauthorized_client" //nolint:misspell // RFC 6749 §5.2 spells it so
	OAuthUnsupportedGrantType = "unsupported_grant_type"
	OAuthUnavailable          = "temporarily_unavailable"
)

// Counter names of the token endpoint.
const (
	CounterTokenIssued      = "token_issued"
	CounterTokenRefusedBase = "token_refused_" // + the reason
	CounterTokenNotAudited  = "token_refusal_not_audited"
)

// ErrClientNotFound is a client id with no client.
var ErrClientNotFound = errors.New("client not found")

// ClientRecord is what the token endpoint needs of an operator client.
type ClientRecord struct {
	ClientID   string
	OperatorID string
	SecretHash string
	// PreviousHash is the secret before the last rotation, valid until
	// PreviousValidUntil (policy.ClientSecretOverlapS after it).
	PreviousHash       string
	PreviousValidUntil *time.Time
	Scopes             []string
	// Active is true while the client is active and its operator is
	// active (validated by the registry).
	Active bool
}

// ClientStore reads clients (internal/accounts implements it).
type ClientStore interface {
	ClientForToken(ctx context.Context, clientID string) (ClientRecord, error)
}

// TokenEvent is one issuance or refusal, for the audit log. It never
// carries the token or the secret.
type TokenEvent struct {
	ClientID  string
	Reason    string
	JTI       string
	KID       string
	Audience  string
	Scopes    []string
	ExpiresAt time.Time
	RemoteIP  string
}

// TokenAuditor records issuances and refusals (internal/accounts
// implements it on the events table).
type TokenAuditor interface {
	TokenIssued(ctx context.Context, e TokenEvent) error
	TokenRefused(ctx context.Context, e TokenEvent) error
}

// TokenEndpoint is POST /oauth/token (RFC 6749 §4.4 client credentials)
// of this USSP's issuer for operator clients.
type TokenEndpoint struct {
	// Issuer signs; nil answers 503 temporarily_unavailable (no key).
	Issuer  *Issuer
	Clients ClientStore
	Hasher  *Hasher
	Audit   TokenAuditor
	// TTL is policy.OperatorTokenTTLS of the current policy version.
	TTL           func() time.Duration
	ClientLimiter *httpx.RateLimiter
	IPLimiter     *httpx.RateLimiter
	Counters      *core.Counters
	Logger        *slog.Logger
	Now           func() time.Time
}

type oauthProblem struct {
	*httpx.ProblemBody
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// tokenRefusal is one refusal: the RFC 6749 code, the HTTP status, the
// audit reason and a description safe to send.
type tokenRefusal struct {
	code   string
	status int
	reason string
	desc   string
	wait   time.Duration
}

func (e *TokenEndpoint) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *TokenEndpoint) count(name string) {
	if e.Counters != nil {
		e.Counters.Inc(name)
	}
}

// ServeHTTP answers a token request.
func (e *TokenEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := httpx.RemoteIP(r)
	clientID, issued, ref := e.serve(r, ip)
	if ref != nil {
		e.refuse(w, r, clientID, ip, ref)
		return
	}
	e.count(CounterTokenIssued)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	//nolint:gosec // G117: the RFC 6749 §5.1 answer carries the token to the client that authenticated for it
	_ = json.NewEncoder(w).Encode(struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}{issued.Token, "Bearer", int64(issued.ExpiresAt.Sub(e.now().Truncate(time.Second)) / time.Second), strings.Join(issued.Scopes, " ")})
}

func bad(code, reason, desc string) *tokenRefusal {
	status := http.StatusBadRequest
	if code == OAuthInvalidClient {
		status = http.StatusUnauthorized
	}
	return &tokenRefusal{code: code, status: status, reason: reason, desc: desc}
}

// form reads the request's form body: the content type must be
// application/x-www-form-urlencoded, nothing may come in the query
// string, and no parameter may repeat (RFC 6749 §3.2).
func form(r *http.Request) (url.Values, *tokenRefusal) {
	if r.URL.RawQuery != "" {
		return nil, bad(OAuthInvalidRequest, "query_parameters", "parameters in the query string are refused")
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, bad(OAuthInvalidRequest, "content_type", "the body must be application/x-www-form-urlencoded")
	}
	if err := r.ParseForm(); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &tokenRefusal{code: OAuthInvalidRequest, status: http.StatusRequestEntityTooLarge, reason: "body_too_large", desc: "the body is too large"}
		}
		return nil, bad(OAuthInvalidRequest, "malformed_body", "the body is not a form")
	}
	for k, v := range r.PostForm {
		if len(v) > 1 {
			return nil, bad(OAuthInvalidRequest, "repeated_parameter", "parameter "+quote(k)+" is repeated")
		}
	}
	return r.PostForm, nil
}

// clientCredentials is the client id and secret from HTTP Basic
// (client_secret_basic, the parts form-encoded per RFC 6749 §2.3.1) or
// from the body (client_secret_post); never both.
func clientCredentials(r *http.Request, f url.Values) (id, secret string, ref *tokenRefusal) {
	bid, bsecret, basic := r.BasicAuth()
	_, bodyID := f["client_id"]
	_, bodySecret := f["client_secret"]
	switch {
	case basic && (bodyID || bodySecret):
		return "", "", bad(OAuthInvalidRequest, "two_client_authentications", "authenticate with HTTP Basic or in the body, not both")
	case basic:
		var err1, err2 error
		id, err1 = url.QueryUnescape(bid)
		secret, err2 = url.QueryUnescape(bsecret)
		if err1 != nil || err2 != nil {
			return "", "", bad(OAuthInvalidClient, "malformed_basic", "the Basic credentials are not form-encoded")
		}
	default:
		id, secret = f.Get("client_id"), f.Get("client_secret")
	}
	if id == "" || secret == "" {
		return "", "", bad(OAuthInvalidClient, "no_client_authentication", "the client must authenticate")
	}
	if len(id) > 64 || len(secret) > MaxSecretBytes {
		return "", "", bad(OAuthInvalidClient, "malformed_client", "the client credentials are too long")
	}
	return id, secret, nil
}

func (e *TokenEndpoint) serve(r *http.Request, ip string) (string, Issued, *tokenRefusal) {
	if e.IPLimiter != nil {
		if ok, wait := e.IPLimiter.Allow("ip:" + ip); !ok {
			return "", Issued{}, &tokenRefusal{code: OAuthUnavailable, status: http.StatusTooManyRequests, reason: "rate_limited_address",
				desc: "too many token requests from this address", wait: wait}
		}
	}
	if e.Issuer == nil {
		return "", Issued{}, &tokenRefusal{code: OAuthUnavailable, status: http.StatusServiceUnavailable, reason: "no_issuer_key",
			desc: "this USSP has no issuer key configured"}
	}
	f, ref := form(r)
	if ref != nil {
		return "", Issued{}, ref
	}
	if gt := f.Get("grant_type"); gt != "client_credentials" {
		return "", Issued{}, bad(OAuthUnsupportedGrantType, "grant_type", "only client_credentials is supported")
	}
	id, secret, ref := clientCredentials(r, f)
	if ref != nil {
		return "", Issued{}, ref
	}
	id = clip(id)
	if e.ClientLimiter != nil {
		if ok, wait := e.ClientLimiter.Allow("client:" + id); !ok {
			return id, Issued{}, &tokenRefusal{code: OAuthUnavailable, status: http.StatusTooManyRequests, reason: "rate_limited_client",
				desc: "too many token requests for this client", wait: wait}
		}
	}
	requested, err := ParseOperatorScopes(f.Get("scope"))
	if err != nil {
		return id, Issued{}, bad(OAuthInvalidScope, "unknown_scope", err.Error())
	}
	if aud, ok := f["audience"]; ok && !e.ownAudience(aud[0]) {
		return id, Issued{}, bad(OAuthInvalidTarget, "foreign_audience", "this issuer issues tokens for "+e.Issuer.Audience+" only")
	}
	c, ref := e.authenticateClient(r.Context(), id, secret)
	if ref != nil {
		return id, Issued{}, ref
	}
	granted := c.Scopes
	if len(requested) > 0 {
		granted = slices.DeleteFunc(slices.Clone(requested), func(s string) bool { return !slices.Contains(c.Scopes, s) })
	}
	if len(granted) == 0 {
		return id, Issued{}, bad(OAuthInvalidScope, "scope_not_held", "the client holds none of the requested scopes")
	}
	now := e.now()
	issued, err := e.Issuer.IssueOperator(c.ClientID, granted, e.TTL(), now)
	if err != nil {
		obs.Error(r.Context(), e.logger(), "token not signed", err, obs.ClientID(c.ClientID))
		return id, Issued{}, &tokenRefusal{code: OAuthUnavailable, status: http.StatusServiceUnavailable, reason: "sign_failed", desc: "the token could not be signed"}
	}
	// No audit row, no token: an issuance the record does not show must
	// not exist.
	if err := e.Audit.TokenIssued(r.Context(), TokenEvent{
		ClientID: c.ClientID, JTI: issued.JTI, KID: issued.KID, Audience: e.Issuer.Audience, Scopes: granted, ExpiresAt: issued.ExpiresAt, RemoteIP: ip,
	}); err != nil {
		obs.Error(r.Context(), e.logger(), "token issuance not audited; refused", err, obs.ClientID(c.ClientID))
		return id, Issued{}, &tokenRefusal{code: OAuthUnavailable, status: http.StatusServiceUnavailable, reason: "audit_failed", desc: "the issuance could not be recorded; retry later"}
	}
	return id, issued, nil
}

// ownAudience accepts this USSP's host, bare or as a URL.
func (e *TokenEndpoint) ownAudience(v string) bool {
	if strings.Contains(v, "://") {
		host, err := AudienceOf(v)
		return err == nil && strings.EqualFold(host, e.Issuer.Audience)
	}
	return strings.EqualFold(v, e.Issuer.Audience)
}

// authenticateClient checks the secret in constant time: an unknown
// client spends a dummy verification; during a rotation the previous
// secret is accepted until its overlap ends, never after.
func (e *TokenEndpoint) authenticateClient(ctx context.Context, id, secret string) (ClientRecord, *tokenRefusal) {
	c, err := e.Clients.ClientForToken(ctx, id)
	if err != nil {
		e.Hasher.VerifyDummy(secret)
		if !errors.Is(err, ErrClientNotFound) {
			obs.Error(ctx, e.logger(), "client lookup failed", err)
			return ClientRecord{}, &tokenRefusal{code: OAuthUnavailable, status: http.StatusServiceUnavailable, reason: "store_unavailable", desc: "the client cannot be checked now"}
		}
		return ClientRecord{}, bad(OAuthInvalidClient, "unknown_client", "the client authentication failed")
	}
	ok, _ := e.Hasher.Verify(secret, c.SecretHash)
	if !ok && c.PreviousHash != "" {
		if prevOK, _ := e.Hasher.Verify(secret, c.PreviousHash); prevOK {
			if c.PreviousValidUntil != nil && e.now().Before(*c.PreviousValidUntil) {
				ok = true
			} else {
				return ClientRecord{}, bad(OAuthInvalidClient, "previous_secret_expired", "the client authentication failed")
			}
		}
	}
	if !ok {
		return ClientRecord{}, bad(OAuthInvalidClient, "wrong_secret", "the client authentication failed")
	}
	if !c.Active {
		return ClientRecord{}, bad(OAuthUnauthorizedClient, "client_not_active", "the client or its operator is not active")
	}
	return c, nil
}

func (e *TokenEndpoint) logger() *slog.Logger {
	if e.Logger == nil {
		return obs.Discard()
	}
	return e.Logger
}

func (e *TokenEndpoint) refuse(w http.ResponseWriter, r *http.Request, clientID, ip string, ref *tokenRefusal) {
	e.count(CounterTokenRefusedBase + ref.reason)
	actor := clientID
	if actor == "" {
		actor = "unknown"
	}
	if e.Audit != nil {
		if err := e.Audit.TokenRefused(r.Context(), TokenEvent{ClientID: actor, Reason: ref.reason, RemoteIP: ip}); err != nil {
			e.count(CounterTokenNotAudited)
			obs.Error(r.Context(), e.logger(), "token refusal not audited", err, slog.String("reason", ref.reason))
		}
	}
	if ref.status == http.StatusTooManyRequests {
		httpx.RetryAfter(w, ref.wait)
	}
	if ref.code == OAuthInvalidClient {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
	}
	p := oauthProblem{ProblemBody: httpx.NewProblem(ref.status, ref.code, "", ref.desc), Error: ref.code, ErrorDescription: ref.desc}
	p.Instance = r.URL.Path
	w.Header().Set("Content-Type", httpx.ProblemContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(ref.status)
	_ = json.NewEncoder(w).Encode(p)
}
