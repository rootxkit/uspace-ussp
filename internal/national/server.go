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
	"slices"
	"strings"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"

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
	// Portal resolves portal sessions on the intents, geo and alert
	// operations (brief WP-17); nil answers them 503 portal_unavailable.
	Portal PortalMembers
	// CIS is POST /v1/cis/notifications (internal/cis.Receiver); nil
	// answers 503 cis_unavailable.
	CIS http.Handler
	// Registry answers GET /v1/registry/validate (internal/registry's
	// Cache); nil answers 503 registry_unavailable.
	Registry RegistryValidator
	// RegistryScope says which keys are the calling client's own: those
	// are answered in full, every other key status only (audit S6). nil
	// answers every key status only.
	RegistryScope RegistryScoper
	// RegistryLimiter bounds the lookups per client (nil: unbounded).
	RegistryLimiter *httpx.RateLimiter
	// Intents serves /v1/intents (internal/intent.Service); nil answers
	// 503 intents_unavailable.
	Intents Intents
	// Alerts records acknowledgements (internal/alerts.Service); nil
	// answers 503 alerts_unavailable.
	Alerts AlertAcker
	// Geo answers /v1/geo* from the CIS cache; nil answers 503
	// cis_unavailable.
	Geo *Geo
	// Coordination lists the Annex V notices for the console
	// (internal/coordination); nil answers 503 coordination_unavailable.
	Coordination CoordinationLister
	// Records serves /v1/records; nil answers 503 records_unavailable.
	Records *Records
	// Occurrences serves /v1/admin/occurrences; nil answers 503
	// occurrences_unavailable.
	Occurrences *Occurrences
	// Status serves /v1/admin/status (CertificateID is
	// USSP_CERTIFICATE_ID); nil answers 503 status_unavailable.
	Status        StatusNotices
	CertificateID string
	// Weather answers GET /v1/weather (internal/weather.Service); nil
	// answers 503 weather_unavailable with reason not_configured.
	Weather WeatherAnswerer
	// Admin serves the console's /v1/admin/* of WP-18; nil answers 503
	// admin_unavailable.
	Admin  Admin
	Logger *slog.Logger
}

// RegistryValidator is the cached, audited F8 lookup
// (internal/registry.Cache).
type RegistryValidator interface {
	ValidateAudited(ctx context.Context, actorType, actorID string, qs []registry.Query, p registry.Purpose) ([]registry.Result, error)
}

// RegistryScoper reads a client's own registry keys
// (internal/accounts.Service).
type RegistryScoper interface {
	RegistryScope(ctx context.Context, clientID string) (accounts.RegistryScope, error)
}

var _ gen.ServerInterface = (*Server)(nil)

var portalAdmin = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmPortal, Roles: []string{auth.RoleOperatorAdmin}}}}

// operatorOrPortal admits an operator token granting scope or any
// portal session (brief WP-17; the handlers refuse a viewer's writes).
func operatorOrPortal(scope string) httpx.Access {
	return httpx.Access{Scopes: []string{scope}, Sessions: []httpx.SessionAccess{{Realm: auth.RealmPortal}}}
}

var portalAny = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmPortal}}}

var anySession = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmPortal}, {Realm: auth.RealmConsole}}}

var consoleStaff = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmConsole, Roles: []string{auth.RoleSupervisor, auth.RoleSupport}}}}

var consoleSupervisor = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmConsole, Roles: []string{auth.RoleSupervisor}}}}

var consoleAny = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmConsole, Roles: []string{auth.RoleSupervisor, auth.RoleSupport, auth.RoleAdmin}}}}

var consoleAdmin = httpx.Access{Sessions: []httpx.SessionAccess{{Realm: auth.RealmConsole, Roles: []string{auth.RoleAdmin}}}}

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
		"POST /v1/accounts/login/mfa":                       public, // the challenge of the password step authenticates; limited per address and per username
		"POST /v1/accounts/operators":                       public, // self-registration, limited per address
		"POST /v1/cis/notifications":                        public, // no bearer: the compact JWS in the body is verified by internal/cis.Receiver (issuer allow-list, aud, iat, jti)
		"GET /v1/registry/validate":                         {Scopes: []string{auth.ScopeIntents}},
		"POST /v1/intents":                                  operatorOrPortal(auth.ScopeIntents),
		"GET /v1/intents":                                   operatorOrPortal(auth.ScopeIntents),
		"GET /v1/intents/{intent_id}":                       operatorOrPortal(auth.ScopeIntents),
		"PATCH /v1/intents/{intent_id}":                     operatorOrPortal(auth.ScopeIntents),
		"GET /v1/geo":                                       operatorOrPortal(auth.ScopeGeo),
		"GET /v1/geo/intents/{intent_id}":                   operatorOrPortal(auth.ScopeGeo),
		"GET /v1/weather":                                   operatorOrPortal(auth.ScopeGeo),
		"POST /v1/alerts/{alert_id}/ack":                    operatorOrPortal(auth.ScopeTraffic),
		"GET /v1/admin/coordination":                        consoleStaff,
		"GET /v1/admin/occurrences":                         consoleStaff,
		"POST /v1/admin/occurrences":                        consoleSupervisor,
		"GET /v1/admin/status":                              consoleAny,
		"POST /v1/admin/status":                             consoleAdmin,
		"GET /v1/admin/flights":                             consoleAny,
		"GET /v1/admin/alerts":                              consoleAny,
		"GET /v1/admin/escalations":                         consoleAny,
		"POST /v1/admin/alerts/{alert_id}/escalate":         consoleSupervisor,
		"POST /v1/admin/alerts/{alert_id}/close":            consoleSupervisor,
		"GET /v1/admin/dss":                                 consoleAny,
		"GET /v1/admin/inputs":                              consoleAny,
		"GET /v1/admin/policy":                              consoleAny,
		"PUT /v1/admin/policy":                              consoleAdmin,
		"GET /v1/admin/sources":                             consoleAny,
		"POST /v1/admin/sources":                            consoleAdmin,
		"GET /v1/admin/emergency":                           consoleAny,
		"GET /v1/admin/emergency/{flight_id}":               consoleAny,
		"POST /v1/admin/emergency/{flight_id}":              consoleSupervisor,
		"GET /v1/admin/records/days":                        consoleAny,
		"GET /v1/admin/events":                              consoleAny,
		"GET /v1/records/flights/{flight_id}":               {Scopes: []string{ScopeRecords}},
		"GET /v1/records/daily/{date}":                      {Scopes: []string{ScopeRecords}},
		"POST /v1/accounts/logout":                          anySession,
		"GET /v1/accounts/me":                               anySession,
		"GET /v1/accounts/operators/{operator_id}":          portalAdmin,
		"PATCH /v1/accounts/operators/{operator_id}":        portalAdmin,
		"GET /v1/accounts/operators/{operator_id}/clients":  portalAny,
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
// recorded with the client and the purpose. A key of the caller's own
// (its operator, its bound serials) is answered with its detail; any
// other key status only, so an operator client cannot read another
// operator's registration or a pilot's competencies (audit S6). The
// lookups are bounded per client.
func (s *Server) ValidateRegistry(w http.ResponseWriter, r *http.Request, params gen.ValidateRegistryParams) {
	if s.Registry == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, registry.ReasonRegistryUnavailable, "", "the registry lookup is not configured on this process").Write(w, r)
		return
	}
	client := principal(r).Claims.Subject
	if s.RegistryLimiter != nil {
		if ok, wait := s.RegistryLimiter.Allow("client:" + client); !ok {
			httpx.RetryAfter(w, wait)
			httpx.NewProblem(http.StatusTooManyRequests, httpx.SlugRateLimited, "Too many requests",
				"the registry lookup budget of this client is spent; wait and try again").Write(w, r)
			return
		}
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	q := registry.Query{Operator: deref(params.Operator), Serial: deref(params.Serial), Pilot: deref(params.Pilot)}
	rs, err := s.Registry.ValidateAudited(r.Context(), store.ActorClient, client, []registry.Query{q}, registry.Purpose(params.Purpose))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sc := s.scopeOf(r.Context(), client)
	_, askedOperator := regnum.Public(q.Operator)
	ownOperator := q.Operator != "" && sc.OperatorKey != "" && askedOperator == sc.OperatorKey
	ownSerial := q.Serial != "" && slices.Contains(sc.SerialFolds, serial.FoldKey(q.Serial))
	writeJSON(w, http.StatusOK, gen.RegistryValidation{
		Operator: registryAnswer(rs[0].Operator, ownOperator), Uas: registryAnswer(rs[0].UAS, ownSerial), Pilot: registryAnswer(rs[0].Pilot, false)})
}

// scopeOf is the client's own keys; none when they cannot be read (the
// answer is then status only, never more).
func (s *Server) scopeOf(ctx context.Context, client string) accounts.RegistryScope {
	if s.RegistryScope == nil {
		return accounts.RegistryScope{}
	}
	sc, err := s.RegistryScope.RegistryScope(ctx, client)
	if err != nil {
		obs.Error(ctx, s.logger(), "the client's own registry keys could not be read; answering status only", err)
		return accounts.RegistryScope{}
	}
	return sc
}

// registryAnswer is a's wire form: with its detail (validity, class,
// band, competencies) when full, its status alone otherwise.
func registryAnswer(a *registry.Answer, full bool) *gen.RegistryAnswer {
	if a == nil {
		return nil
	}
	out := &gen.RegistryAnswer{Key: a.Key, Status: gen.RegistryStatus(a.Status), CacheAgeS: a.CacheAgeS}
	if a.Reason != "" {
		reason := gen.RegistryAnswerReason(a.Reason)
		out.Reason = &reason
	}
	if !full {
		return out
	}
	out.ValidUntil = a.ValidUntil
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
	if res.Challenge != nil {
		writeJSON(w, http.StatusAccepted, gen.MFAChallenge{MfaToken: res.Challenge.Token, ExpiresAt: res.Challenge.ExpiresAt})
		return
	}
	s.session(w, res)
}

// LoginMFA is POST /v1/accounts/login/mfa: the second step of a staff
// admin's sign-in.
func (s *Server) LoginMFA(w http.ResponseWriter, r *http.Request) {
	var in gen.MFAStep
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.Accounts.LoginMFA(r.Context(), in.MfaToken, in.Code, httpx.RemoteIP(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.session(w, res)
}

// session sets the session cookies and answers the session.
func (s *Server) session(w http.ResponseWriter, res accounts.SessionResult) {
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
