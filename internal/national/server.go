// Package national is the glue between the generated server of
// api/openapi.yaml (internal/national/gen) and the services: it decodes
// the contract's request types, calls the service, and encodes the
// contract's response types. Every operation is registered through an
// httpx.GuardedMux with its entry in AccessTable, so a route without an
// access entry is not served and the api process refuses to start.
package national

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/registry"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Health serves the two health operations (the process supplies them).
type Health interface {
	GetHealthz(w http.ResponseWriter, r *http.Request)
	GetReadyz(w http.ResponseWriter, r *http.Request)
}

// Server implements gen.ServerInterface for the api process.
type Server struct {
	Health
	// Token is POST /oauth/token.
	Token http.Handler
	// Issuer publishes the JWKS; nil answers 503.
	Issuer   *auth.Issuer
	Accounts *accounts.Service
	// CIS is POST /v1/cis/notifications (internal/cis.Receiver); nil
	// answers 503 cis_unavailable.
	CIS http.Handler
	// Registry answers GET /v1/registry/validate (internal/registry's
	// Cache); nil answers 503 registry_unavailable.
	Registry RegistryValidator
	Logger   *slog.Logger
}

// RegistryValidator is the cached, audited F8 lookup
// (internal/registry.Cache).
type RegistryValidator interface {
	ValidateAudited(ctx context.Context, actorType, actorID string, qs []registry.Query, p registry.Purpose) ([]registry.Result, error)
}

var _ gen.ServerInterface = (*Server)(nil)

var portalAdmin = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmPortal, Roles: []string{auth.RoleOperatorAdmin}}}}

var anySession = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmPortal}, {Realm: auth.RealmConsole}}}

// AccessTable is the access entry of every operation of
// api/openapi.yaml this process serves, by ServeMux pattern. The
// GuardedMux refuses a route that is missing here and an entry that
// matches no route.
func AccessTable() map[string]httpx.Access {
	public := httpx.Access{Public: true}
	return map[string]httpx.Access{
		"GET /healthz":                                      public,
		"GET /readyz":                                       public,
		"POST /oauth/token":                                 public, // the client authenticates in the request (RFC 6749 §2.3)
		"GET /.well-known/jwks.json":                        public,
		"POST /v1/accounts/login":                           public, // limited per address and per username
		"POST /v1/accounts/operators":                       public, // self-registration, limited per address
		"POST /v1/cis/notifications":                        public, // no bearer: the compact JWS in the body is verified by internal/cis.Receiver (issuer allow-list, aud, iat, jti)
		"GET /v1/registry/validate":                         {Scopes: []string{auth.ScopeIntents}},
		"POST /v1/accounts/logout":                          anySession,
		"GET /v1/accounts/me":                               anySession,
		"GET /v1/accounts/operators/{operator_id}":          portalAdmin,
		"PATCH /v1/accounts/operators/{operator_id}":        portalAdmin,
		"POST /v1/accounts/operators/{operator_id}/clients": portalAdmin,
		"POST /v1/accounts/operators/{operator_id}/clients/{client_id}/rotate":             portalAdmin,
		"POST /v1/accounts/operators/{operator_id}/clients/{client_id}/serials":            portalAdmin,
		"DELETE /v1/accounts/operators/{operator_id}/clients/{client_id}/serials/{serial}": portalAdmin,
	}
}

// Register registers every operation on mux behind guard; the error
// lists every route without a valid access entry and every entry
// without a route.
func Register(mux *http.ServeMux, s *Server, guard httpx.Guard) error {
	g := httpx.NewGuardedMux(mux, AccessTable(), guard, auth.ValidateAccess)
	gen.HandlerWithOptions(s, gen.StdHTTPServerOptions{BaseRouter: g, ErrorHandlerFunc: paramError})
	return g.Err()
}

// paramError answers a path or query parameter the generated router
// could not bind.
func paramError(w http.ResponseWriter, r *http.Request, err error) {
	field := "parameter"
	var ip *gen.InvalidParamFormatError
	if errors.As(err, &ip) {
		field = ip.ParamName
	}
	httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "Invalid request", "", core.Fieldf(field, "malformed")).Write(w, r)
}

// MaxJSONBytes caps a JSON request body of this package's operations.
const MaxJSONBytes = 64 << 10

// decode reads exactly one JSON object into v, refusing unknown fields
// and trailing data; every refusal is a field error.
func decode(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return core.Fieldf("Content-Type", "must be application/json")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxJSONBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxJSONBytes {
		return &http.MaxBytesError{Limit: MaxJSONBytes}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return core.Fieldf(te.Field, "wrong type")
		}
		return core.Fieldf("body", "not the expected JSON object")
	}
	if dec.More() {
		return core.Fieldf("body", "trailing data after the JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail writes err as a problem; an accounts error with a wait carries
// Retry-After. A 500 is logged with its cause, which is never sent.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *accounts.Error
	if errors.As(err, &ae) {
		if ae.RetryAfter > 0 {
			httpx.RetryAfter(w, ae.RetryAfter)
		}
		var errs []*core.FieldError
		if ae.Field != nil {
			errs = append(errs, ae.Field)
		}
		httpx.NewProblem(ae.Status, ae.Slug, "", ae.Detail, errs...).Write(w, r)
		return
	}
	p := httpx.ProblemFromError(err)
	if p.Status == http.StatusInternalServerError {
		obs.Error(r.Context(), s.logger(), "request failed", err, obs.RequestID(httpx.RequestIDFrom(r.Context())))
	}
	p.Write(w, r)
}

func (s *Server) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

func principal(r *http.Request) auth.Principal {
	p, _ := auth.PrincipalFrom(r.Context())
	return p
}

// RequestToken is POST /oauth/token.
func (s *Server) RequestToken(w http.ResponseWriter, r *http.Request) { s.Token.ServeHTTP(w, r) }

// ReceiveCISNotification is POST /v1/cis/notifications.
func (s *Server) ReceiveCISNotification(w http.ResponseWriter, r *http.Request) {
	if s.CIS == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "cis_unavailable", "", "no CIS notification issuer is configured (USSP_CIS_NOTIFY_ISSUERS)").Write(w, r)
		return
	}
	s.CIS.ServeHTTP(w, r)
}

// ValidateRegistry is GET /v1/registry/validate: the cached F8 answer,
// status only, recorded with the client and the purpose.
func (s *Server) ValidateRegistry(w http.ResponseWriter, r *http.Request, params gen.ValidateRegistryParams) {
	if s.Registry == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, registry.ReasonRegistryUnavailable, "", "the registry lookup is not configured on this process").Write(w, r)
		return
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	q := registry.Query{Operator: deref(params.Operator), Serial: deref(params.Serial), Pilot: deref(params.Pilot)}
	rs, err := s.Registry.ValidateAudited(r.Context(), store.ActorClient, principal(r).Claims.Subject, []registry.Query{q}, registry.Purpose(params.Purpose))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.RegistryValidation{Operator: registryAnswer(rs[0].Operator), Uas: registryAnswer(rs[0].UAS), Pilot: registryAnswer(rs[0].Pilot)})
}

func registryAnswer(a *registry.Answer) *gen.RegistryAnswer {
	if a == nil {
		return nil
	}
	out := &gen.RegistryAnswer{Key: a.Key, Status: gen.RegistryStatus(a.Status), ValidUntil: a.ValidUntil, CacheAgeS: a.CacheAgeS}
	if a.Reason != "" {
		reason := gen.RegistryAnswerReason(a.Reason)
		out.Reason = &reason
	}
	if a.ClassLabel != "" {
		out.ClassLabel = &a.ClassLabel
	}
	if a.MTOMBand != "" {
		out.MtomBand = &a.MTOMBand
	}
	if a.Competencies != nil {
		cs := make([]gen.RegistryCompetency, len(a.Competencies))
		for i, c := range a.Competencies {
			cs[i] = gen.RegistryCompetency{Competency: c.Competency, ValidUntil: c.ValidUntil}
		}
		out.Competencies = &cs
	}
	return out
}

// GetJWKS is GET /.well-known/jwks.json.
func (s *Server) GetJWKS(w http.ResponseWriter, r *http.Request) {
	if s.Issuer == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "no_issuer_key", "", "this USSP has no issuer key configured").Write(w, r)
		return
	}
	body, err := s.Issuer.Keys().JWKSJSON()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(body)
}

// Login is POST /v1/accounts/login.
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var in gen.LoginRequest
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	li := accounts.LoginInput{Realm: string(in.Realm), Username: in.Username, Password: in.Password}
	if in.TotpCode != nil {
		li.TOTPCode = *in.TotpCode
	}
	res, err := s.Accounts.Login(r.Context(), li, httpx.RemoteIP(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	auth.SetSessionCookies(w, res.Token, res.CSRF, res.ExpiresAt)
	out := gen.Session{Token: res.Token, CsrfToken: res.CSRF, AccountId: res.AccountID, Realm: gen.Realm(res.Realm), Roles: res.Roles,
		ExpiresAt: res.ExpiresAt, IdleExpiresAt: res.IdleExpiresAt}
	if res.OperatorID != "" {
		out.OperatorId = &res.OperatorID
	}
	writeJSON(w, http.StatusOK, out)
}

// Logout is POST /v1/accounts/logout.
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	if err := s.Accounts.Logout(r.Context(), principal(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	auth.ClearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// GetMe is GET /v1/accounts/me.
func (s *Server) GetMe(w http.ResponseWriter, r *http.Request) {
	me, err := s.Accounts.Me(r.Context(), principal(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := gen.Me{AccountId: me.AccountID, Username: me.Username, Realm: gen.Realm(me.Realm), Roles: me.Roles, SessionExpiresAt: me.SessionExpiresAt}
	if me.OperatorID != "" {
		out.OperatorId = &me.OperatorID
	}
	writeJSON(w, http.StatusOK, out)
}

func operatorOut(o accounts.Operator) gen.Operator {
	out := gen.Operator{Id: o.ID, RegistrationNumber: o.RegistrationNumber, DisplayName: o.DisplayName, ContactEmail: o.ContactEmail,
		Status: gen.OperatorStatus(o.Status), ValidatedAt: o.ValidatedAt, CreatedAt: o.CreatedAt}
	if o.ValidationStatus != "" {
		v := o.ValidationStatus
		out.ValidationStatus = &v
	}
	return out
}

// RegisterOperator is POST /v1/accounts/operators.
func (s *Server) RegisterOperator(w http.ResponseWriter, r *http.Request) {
	var in gen.OperatorRegistration
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	o, err := s.Accounts.RegisterOperator(r.Context(), accounts.Registration{
		RegistrationNumber: in.RegistrationNumber, DisplayName: in.DisplayName, ContactEmail: in.ContactEmail,
		AdminUsername: in.AdminUsername, AdminPassword: in.AdminPassword,
	}, httpx.RemoteIP(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, operatorOut(o))
}

// GetOperator is GET /v1/accounts/operators/{operator_id}.
func (s *Server) GetOperator(w http.ResponseWriter, r *http.Request, operatorID gen.OperatorID) {
	o, err := s.Accounts.Operator(r.Context(), principal(r), operatorID.String())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, operatorOut(o))
}

// UpdateOperator is PATCH /v1/accounts/operators/{operator_id}.
func (s *Server) UpdateOperator(w http.ResponseWriter, r *http.Request, operatorID gen.OperatorID) {
	var in gen.OperatorUpdate
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	o, err := s.Accounts.UpdateOperator(r.Context(), principal(r), operatorID.String(),
		accounts.OperatorUpdate{DisplayName: in.DisplayName, ContactEmail: in.ContactEmail})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, operatorOut(o))
}

func secretOut(c accounts.ClientSecret) gen.ClientSecret {
	out := gen.ClientSecret{ClientId: c.ClientID, ClientSecret: c.Secret, Status: c.Status, PreviousValidUntil: c.PreviousValidUntil}
	for _, sc := range c.Scopes {
		out.Scopes = append(out.Scopes, gen.OperatorScope(sc))
	}
	return out
}

// CreateClient is POST /v1/accounts/operators/{operator_id}/clients.
func (s *Server) CreateClient(w http.ResponseWriter, r *http.Request, operatorID gen.OperatorID) {
	var in gen.ClientRequest
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	scopes := make([]string, 0, len(in.Scopes))
	for _, sc := range in.Scopes {
		scopes = append(scopes, string(sc))
	}
	c, err := s.Accounts.CreateClient(r.Context(), principal(r), operatorID.String(), scopes)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, secretOut(c))
}

// RotateClientSecret is POST .../clients/{client_id}/rotate.
func (s *Server) RotateClientSecret(w http.ResponseWriter, r *http.Request, operatorID gen.OperatorID, clientID gen.ClientID) {
	c, err := s.Accounts.RotateClient(r.Context(), principal(r), operatorID.String(), clientID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, secretOut(c))
}

// BindSerial is POST .../clients/{client_id}/serials.
func (s *Server) BindSerial(w http.ResponseWriter, r *http.Request, operatorID gen.OperatorID, clientID gen.ClientID) {
	var in gen.SerialBindingRequest
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	class := ""
	if in.ClassLabel != nil {
		class = string(*in.ClassLabel)
	}
	b, err := s.Accounts.BindSerial(r.Context(), principal(r), operatorID.String(), clientID, in.Serial, class)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, gen.SerialBinding{ClientId: b.ClientID, Serial: b.Serial, SerialFold: b.SerialFold, BoundAt: b.BoundAt})
}

// UnbindSerial is DELETE .../clients/{client_id}/serials/{serial}.
func (s *Server) UnbindSerial(w http.ResponseWriter, r *http.Request, operatorID gen.OperatorID, clientID gen.ClientID, serialNumber string) {
	if err := s.Accounts.UnbindSerial(r.Context(), principal(r), operatorID.String(), clientID, serialNumber); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
