package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Operator statuses (operator_accounts.status).
const (
	OperatorPending = "pending_validation"
	OperatorActive  = "active"
	OperatorRefused = "refused"
)

// Account statuses (portal_users, staff_accounts, oauth_clients).
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Registry answers (WP-5's F8 check). Anything else refuses.
const (
	RegistryValid   = "valid"
	RegistryUnknown = "unknown"
)

// Event types written by this package.
const (
	EventOperatorRegistered = "operator_registered"
	EventOperatorUpdated    = "operator_updated"
	EventOperatorValidated  = "operator_validation_changed"
	EventClientCreated      = "client_created"
	EventClientRotated      = "client_secret_rotated"
	EventSerialBound        = "serial_bound"
	EventSerialUnbound      = "serial_unbound"
	EventLoginSucceeded     = "login_succeeded"
	EventLoginRefused       = "login_refused"
	EventLoginLocked        = "login_locked"
	EventLogout             = "logout"
	EventSessionIdleEnded   = "session_idle_ended"
	EventTokenIssued        = "token_issued"
	EventTokenRefused       = "token_refused"
	EventAuthRefused        = "auth_refused"
	EventStaffCreated       = "staff_created"
)

// Counter names of the service.
const (
	CounterLoginSucceeded  = "login_succeeded"
	CounterMFAChallenged   = "login_mfa_challenged"
	CounterLoginRefused    = "login_refused"
	CounterLoginLocked     = "login_locked"
	CounterLoginLockedOut  = "login_refused_locked"
	CounterSessionsSwept   = "sessions_swept"
	CounterLockoutsSwept   = "login_lockouts_swept"
	CounterRegistryFailed  = "registry_check_failed"
	CounterRefusalNotSaved = "login_refusal_not_audited"
	CounterBindingRefused  = "serial_binding_refused"
	// CounterSessionProjectionFailed counts a session use whose new idle
	// end could not be written to sessions_live.
	CounterSessionProjectionFailed = "session_projection_failed"
	// CounterSessionOperatorUnread counts a portal session use whose
	// operator could not be read: sessions_live keeps the earlier
	// projection rather than one without the operator.
	CounterSessionOperatorUnread = "session_operator_unread"
)

// RegistryChecker asks the authority's registry (F8) about an operator
// registration number. WP-5's registry.Cache implements it; UnknownRegistry
// answers "unknown" and every operator stays pending_validation. An
// error is treated as "unknown": the operator stays pending, never
// passes by default (CLAUDE.md rule 4).
type RegistryChecker interface {
	OperatorStatus(ctx context.Context, registrationNumber string) (string, error)
}

// UnknownRegistry is a RegistryChecker that knows nothing (tests).
type UnknownRegistry struct{}

// OperatorStatus answers unknown.
func (UnknownRegistry) OperatorStatus(context.Context, string) (string, error) {
	return RegistryUnknown, nil
}

// Config are the account and session settings (USSP_SESSION_*,
// USSP_LOGIN_*).
type Config struct {
	SessionTTL   time.Duration
	SessionIdle  time.Duration
	LockoutAfter int
	LockoutFor   time.Duration
	// TOTPIssuer names this USSP in an authenticator app.
	TOTPIssuer string
	Now        func() time.Time
	// BeforeSessionTx, tests only, runs after the credentials were
	// checked and before the transaction that starts the session: the
	// window in which another sign-in can spend the same TOTP code.
	BeforeSessionTx func()
}

// Service is operator accounts, their clients and serial bindings,
// staff accounts, sign-in and sessions. It implements auth.ClientStore,
// auth.TokenAuditor, auth.SessionChecker and auth.Auditor on the
// relational database.
type Service struct {
	Store    *store.Store
	Hasher   *auth.Hasher
	Issuer   *auth.Issuer // nil: no session can start
	Registry RegistryChecker
	Bindings auth.BindingsProjector
	// LiveSessions projects every session started and ended to
	// sessions_live, inside the session's transaction, for the processes
	// that cannot read the session rows (traffic-ws; audit B2). nil
	// projects nothing (a process with no reader of it).
	LiveSessions auth.SessionsProjector
	// Policy is the current policy (client_secret_overlap_s).
	Policy func() policy.Values
	// MFA seals staff TOTP secrets; nil: a staff admin cannot sign in.
	MFA *Sealer
	// LoginLimiter bounds sign-in and self-registration attempts per
	// client address (per process; the per-username lock is in the DB).
	LoginLimiter *httpx.RateLimiter
	Counters     *core.Counters
	Logger       *slog.Logger
	Config       Config
}

func (s *Service) now() time.Time {
	if s.Config.Now != nil {
		return s.Config.Now()
	}
	return time.Now()
}

func (s *Service) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

// Error is a refusal of this service that is its own problem.
type Error struct {
	Status     int
	Slug       string
	Detail     string
	Field      *core.FieldError
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Slug + ": " + e.Detail }

// HTTPStatus implements httpx.StatusError.
func (e *Error) HTTPStatus() int { return e.Status }

// ProblemSlug implements httpx.StatusError.
func (e *Error) ProblemSlug() string { return e.Slug }

// ProblemDetail implements httpx.StatusError.
func (e *Error) ProblemDetail() string { return e.Detail }

func refuse(status int, slug, detail string) *Error {
	return &Error{Status: status, Slug: slug, Detail: detail}
}

var (
	errNotFound = refuse(http.StatusNotFound, httpx.SlugNotFound, "no such resource")
	errNotYours = refuse(http.StatusForbidden, httpx.SlugForbidden, "the resource belongs to another operator")
)

// fieldText validates a free-text field: present, valid UTF-8 without
// control characters, at most maxLen runes.
func fieldText(name, v string, maxLen int) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", core.Fieldf(name, "required")
	case !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl):
		return "", core.Fieldf(name, "contains control characters or is not UTF-8")
	case utf8.RuneCountInString(v) > maxLen:
		return "", core.Fieldf(name, "longer than %d characters", maxLen)
	}
	return v, nil
}

func email(v string) (string, error) {
	v, err := fieldText("contact_email", v, 254)
	if err != nil {
		return "", err
	}
	a, err := mail.ParseAddress(v)
	if err != nil || a.Address != v || a.Name != "" {
		return "", core.Fieldf("contact_email", "not an email address")
	}
	return v, nil
}

// NormalizeUsername is the stored and compared form of a username.
func NormalizeUsername(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func username(field, v string) (string, error) {
	v = NormalizeUsername(v)
	if len(v) < 3 || len(v) > 64 {
		return "", core.Fieldf(field, "3 to 64 characters")
	}
	for _, r := range v {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("._@-", r) {
			return "", core.Fieldf(field, "letters, digits and . _ @ - only")
		}
	}
	return v, nil
}

// MinPasswordLen is the shortest password accepted (NIST SP 800-63B
// 5.1.1.2: at least 8; 12 here, as the console of the authority).
const MinPasswordLen = 12

func password(field, v string) error {
	switch {
	case utf8.RuneCountInString(v) < MinPasswordLen:
		return core.Fieldf(field, "at least %d characters", MinPasswordLen)
	case len(v) > auth.MaxSecretBytes:
		return core.Fieldf(field, "at most %d bytes", auth.MaxSecretBytes)
	}
	return nil
}

func (s *Service) audit(ctx context.Context, q *relational.Queries, e store.Event) error {
	_, err := store.Audit(ctx, q, e)
	return err
}

// auditOwnTx writes e in a transaction of its own (a refusal: nothing
// else commits); a failure is counted and logged and the refusal stands.
func (s *Service) auditOwnTx(ctx context.Context, e store.Event) error {
	err := s.Store.Tx(ctx, func(q *relational.Queries) error { return s.audit(ctx, q, e) })
	if err != nil {
		s.count(CounterRefusalNotSaved)
		obs.Error(ctx, s.logger(), "audit row not written", err, slog.String("event_type", e.EventType))
	}
	return err
}

// ---- operators ----

// Registration is a self-registration request.
type Registration struct {
	RegistrationNumber string
	DisplayName        string
	ContactEmail       string
	AdminUsername      string
	AdminPassword      string
}

// Operator is an operator record.
type Operator struct {
	ID                 string
	RegistrationNumber string
	DisplayName        string
	ContactEmail       string
	Status             string
	ValidationStatus   string
	ValidatedAt        *time.Time
	CreatedAt          time.Time
}

func operatorFrom(r relational.OperatorAccount) Operator {
	o := Operator{ID: store.UUIDText(r.ID), RegistrationNumber: r.AuthorityRegistrationNumber, DisplayName: r.DisplayName,
		ContactEmail: r.ContactEmail, Status: r.Status, ValidatedAt: r.ValidatedAt, CreatedAt: r.CreatedAt}
	if r.ValidationStatus != nil {
		o.ValidationStatus = *r.ValidationStatus
	}
	return o
}

// registrationNumber is the stored form: trimmed, the EU secret part
// removed (spec 06 §5: only the public part is registered) and upper-
// cased, as uspace-core regnum compares it.
func registrationNumber(v string) (string, error) {
	v, err := fieldText("registration_number", v, 32)
	if err != nil {
		return "", err
	}
	if strings.ContainsFunc(v, unicode.IsSpace) {
		return "", core.Fieldf("registration_number", "contains white space")
	}
	return regnum.CompareKey(v), nil
}

// judge turns a registry answer into the operator's status.
func judge(answer string) string {
	switch answer {
	case RegistryValid:
		return OperatorActive
	case RegistryUnknown:
		return OperatorPending
	default:
		return OperatorRefused
	}
}

// askRegistry asks the registry; an error or an empty answer is
// "unknown" (pending), counted and logged.
func (s *Service) askRegistry(ctx context.Context, number string) string {
	ans, err := s.Registry.OperatorStatus(ctx, number)
	if err != nil || ans == "" {
		s.count(CounterRegistryFailed)
		if err != nil {
			obs.Error(ctx, s.logger(), "registry check failed; the operator stays pending", err)
		}
		return RegistryUnknown
	}
	return ans
}

// RegisterOperator creates the operator record and its first portal
// user (operator_admin) after asking the registry; a registration the
// registry refuses is not stored.
func (s *Service) RegisterOperator(ctx context.Context, in Registration, ip string) (Operator, error) {
	if s.LoginLimiter != nil {
		if ok, wait := s.LoginLimiter.Allow("ip:" + ip); !ok {
			return Operator{}, &Error{Status: http.StatusTooManyRequests, Slug: httpx.SlugRateLimited,
				Detail: "too many attempts from this address; wait and try again", RetryAfter: wait}
		}
	}
	number, err1 := registrationNumber(in.RegistrationNumber)
	name, err2 := fieldText("display_name", in.DisplayName, 200)
	mailAddr, err3 := email(in.ContactEmail)
	user, err4 := username("admin_username", in.AdminUsername)
	err5 := password("admin_password", in.AdminPassword)
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		return Operator{}, err
	}
	answer := s.askRegistry(ctx, number)
	status := judge(answer)
	if status == OperatorRefused {
		return Operator{}, &Error{Status: http.StatusUnprocessableEntity, Slug: "registry_refused",
			Detail: "the registry does not hold this operator registration number as valid",
			Field:  &core.FieldError{Field: "registration_number", Reason: "registry answered " + clip(answer)}}
	}
	hash, err := s.Hasher.Hash(in.AdminPassword)
	if err != nil {
		return Operator{}, err
	}
	var out Operator
	now := s.now()
	err = s.Store.Tx(ctx, func(q *relational.Queries) error {
		var validatedAt *time.Time
		if status == OperatorActive {
			validatedAt = &now
		}
		row, err := q.InsertOperator(ctx, relational.InsertOperatorParams{
			RegistrationNumber: number, DisplayName: name, ContactEmail: mailAddr, Status: status,
			ValidationStatus: &answer, ValidatedAt: validatedAt,
		})
		if err != nil {
			if store.SQLState(err) == store.StateUniqueViolation {
				return refuse(http.StatusConflict, "operator_exists", "this registration number has an account")
			}
			return err
		}
		u, err := q.InsertPortalUser(ctx, relational.InsertPortalUserParams{
			OperatorID: row.ID, Username: user, PasswordHash: hash, Role: auth.RoleOperatorAdmin, Status: StatusActive,
		})
		if err != nil {
			if store.SQLState(err) == store.StateUniqueViolation {
				return &Error{Status: http.StatusConflict, Slug: "username_taken", Detail: "the username is taken",
					Field: &core.FieldError{Field: "admin_username", Reason: "taken"}}
			}
			return err
		}
		out = operatorFrom(row)
		return s.audit(ctx, q, store.Event{
			ActorType: auth.ActorPortalUser, ActorID: store.UUIDText(u.ID), EntityType: "operator", EntityID: out.ID,
			EventType: EventOperatorRegistered,
			Payload:   map[string]any{"registration_number": number, "status": status, "registry": answer, "admin_username": user, "remote_ip": ip},
		})
	})
	return out, err
}

// portalAdmin returns the portal user behind p if it is an active
// operator_admin of operatorID.
func (s *Service) portalAdmin(ctx context.Context, q *relational.Queries, p auth.Principal, operatorID string) (relational.PortalUser, error) {
	if !p.Session || p.Claims.Realm != auth.RealmPortal {
		return relational.PortalUser{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "an operator portal session is required")
	}
	uid, err := store.UUID("sub", p.Claims.Subject)
	if err != nil {
		return relational.PortalUser{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "the session names no portal user")
	}
	u, err := q.PortalUserByID(ctx, uid)
	if store.IsNoRows(err) {
		return relational.PortalUser{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "the session names no portal user")
	}
	if err != nil {
		return relational.PortalUser{}, err
	}
	if u.Status != StatusActive || u.Role != auth.RoleOperatorAdmin {
		return relational.PortalUser{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "an active operator_admin is required")
	}
	if store.UUIDText(u.OperatorID) != strings.ToLower(operatorID) {
		return relational.PortalUser{}, errNotYours
	}
	return u, nil
}

func (s *Service) operatorRow(ctx context.Context, q *relational.Queries, id string) (relational.OperatorAccount, error) {
	uid, err := store.UUID("operator_id", id)
	if err != nil {
		return relational.OperatorAccount{}, errNotFound
	}
	r, err := q.OperatorByID(ctx, uid)
	if store.IsNoRows(err) {
		return relational.OperatorAccount{}, errNotFound
	}
	return r, err
}

// Operator returns the operator of the caller (an operator_admin).
func (s *Service) Operator(ctx context.Context, p auth.Principal, id string) (Operator, error) {
	q := s.Store.Queries()
	if _, err := s.portalAdmin(ctx, q, p, id); err != nil {
		return Operator{}, err
	}
	r, err := s.operatorRow(ctx, q, id)
	if err != nil {
		return Operator{}, err
	}
	return operatorFrom(r), nil
}

// OperatorUpdate is the changeable part of an operator record.
type OperatorUpdate struct {
	DisplayName  *string
	ContactEmail *string
}

// UpdateOperator changes the record; an operator still pending is
// checked with the registry again.
func (s *Service) UpdateOperator(ctx context.Context, p auth.Principal, id string, in OperatorUpdate) (Operator, error) {
	var errs []error
	if in.DisplayName != nil {
		v, err := fieldText("display_name", *in.DisplayName, 200)
		in.DisplayName, errs = &v, append(errs, err)
	}
	if in.ContactEmail != nil {
		v, err := email(*in.ContactEmail)
		in.ContactEmail, errs = &v, append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return Operator{}, err
	}
	var out Operator
	err := s.Store.Tx(ctx, func(q *relational.Queries) error {
		u, err := s.portalAdmin(ctx, q, p, id)
		if err != nil {
			return err
		}
		row, err := q.UpdateOperatorContact(ctx, relational.UpdateOperatorContactParams{ID: u.OperatorID, DisplayName: in.DisplayName, ContactEmail: in.ContactEmail})
		if err != nil {
			return err
		}
		if row.Status == OperatorPending {
			answer := s.askRegistry(ctx, row.AuthorityRegistrationNumber)
			if st := judge(answer); st != OperatorPending {
				now := s.now()
				var at *time.Time
				if st == OperatorActive {
					at = &now
				}
				if row, err = q.SetOperatorValidation(ctx, relational.SetOperatorValidationParams{ID: row.ID, Status: st, ValidationStatus: &answer, ValidatedAt: at}); err != nil {
					return err
				}
				if err := s.audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "registry-check", EntityType: "operator",
					EntityID: store.UUIDText(row.ID), EventType: EventOperatorValidated, Payload: map[string]any{"status": st, "registry": answer}}); err != nil {
					return err
				}
			}
		}
		out = operatorFrom(row)
		return s.audit(ctx, q, store.Event{ActorType: auth.ActorPortalUser, ActorID: p.Claims.Subject, EntityType: "operator",
			EntityID: out.ID, EventType: EventOperatorUpdated, Payload: map[string]any{"display_name": in.DisplayName != nil, "contact_email": in.ContactEmail != nil}})
	})
	return out, err
}

// ---- clients ----

// ClientSecret is a client with its secret, shown once.
type ClientSecret struct {
	ClientID           string
	Secret             string
	Scopes             []string
	Status             string
	PreviousValidUntil *time.Time
}

// newClientID is "op-" and 16 random characters: never derived from
// anything an attacker can guess.
func newClientID() (string, error) {
	r, err := auth.RandomSecret(12)
	if err != nil {
		return "", err
	}
	return "op-" + strings.ToLower(strings.NewReplacer("-", "x", "_", "y").Replace(r)), nil
}

// CreateClient creates a machine client of an active operator with the
// given operator scopes; the secret is returned once and stored as an
// argon2id hash. The client_created event is the operator's
// notification for now (06 T3).
func (s *Service) CreateClient(ctx context.Context, p auth.Principal, operatorID string, scopes []string) (ClientSecret, error) {
	scopes, err := auth.ParseOperatorScopes(strings.Join(scopes, " "))
	if err != nil {
		return ClientSecret{}, core.Fieldf("scopes", "%v", err)
	}
	if len(scopes) == 0 {
		return ClientSecret{}, core.Fieldf("scopes", "at least one operator scope")
	}
	secret, err := auth.RandomSecret(32)
	if err != nil {
		return ClientSecret{}, err
	}
	hash, err := s.Hasher.Hash(secret)
	if err != nil {
		return ClientSecret{}, err
	}
	id, err := newClientID()
	if err != nil {
		return ClientSecret{}, err
	}
	err = s.Store.Tx(ctx, func(q *relational.Queries) error {
		u, err := s.portalAdmin(ctx, q, p, operatorID)
		if err != nil {
			return err
		}
		op, err := q.OperatorByID(ctx, u.OperatorID)
		if err != nil {
			return err
		}
		if op.Status != OperatorActive {
			return refuse(http.StatusForbidden, "operator_not_validated", "the operator is "+op.Status+": no client before the registry says valid")
		}
		if _, err := q.InsertClient(ctx, relational.InsertClientParams{ClientID: id, OperatorID: op.ID, SecretHash: hash, Scopes: scopes, Status: StatusActive}); err != nil {
			return err
		}
		return s.audit(ctx, q, store.Event{ActorType: auth.ActorPortalUser, ActorID: p.Claims.Subject, EntityType: "oauth_client",
			EntityID: id, EventType: EventClientCreated, Payload: map[string]any{"operator_id": operatorID, "scopes": scopes}})
	})
	if err != nil {
		return ClientSecret{}, err
	}
	return ClientSecret{ClientID: id, Secret: secret, Scopes: scopes, Status: StatusActive}, nil
}

// clientOf returns the client of operatorID locked for update.
func clientOf(ctx context.Context, q *relational.Queries, u relational.PortalUser, clientID string) (relational.OauthClient, error) {
	c, err := q.ClientForUpdate(ctx, clientID)
	if store.IsNoRows(err) {
		return relational.OauthClient{}, errNotFound
	}
	if err != nil {
		return relational.OauthClient{}, err
	}
	if store.UUIDText(c.OperatorID) != store.UUIDText(u.OperatorID) {
		return relational.OauthClient{}, errNotFound
	}
	return c, nil
}

// RotateClient gives the client a new secret; the current one becomes
// the previous and works until policy.ClientSecretOverlapS from now.
func (s *Service) RotateClient(ctx context.Context, p auth.Principal, operatorID, clientID string) (ClientSecret, error) {
	secret, err := auth.RandomSecret(32)
	if err != nil {
		return ClientSecret{}, err
	}
	hash, err := s.Hasher.Hash(secret)
	if err != nil {
		return ClientSecret{}, err
	}
	var out ClientSecret
	err = s.Store.Tx(ctx, func(q *relational.Queries) error {
		u, err := s.portalAdmin(ctx, q, p, operatorID)
		if err != nil {
			return err
		}
		c, err := clientOf(ctx, q, u, clientID)
		if err != nil {
			return err
		}
		now := s.now()
		until := now.Add(time.Duration(s.Policy().ClientSecretOverlapS) * time.Second)
		row, err := q.RotateClientSecret(ctx, relational.RotateClientSecretParams{
			ClientID: c.ClientID, SecretHash: hash, PreviousSecretHash: &c.SecretHash, PreviousValidUntil: &until, RotatedAt: &now,
		})
		if err != nil {
			return err
		}
		out = ClientSecret{ClientID: row.ClientID, Secret: secret, Scopes: row.Scopes, Status: row.Status, PreviousValidUntil: &until}
		return s.audit(ctx, q, store.Event{ActorType: auth.ActorPortalUser, ActorID: p.Claims.Subject, EntityType: "oauth_client",
			EntityID: c.ClientID, EventType: EventClientRotated, Payload: map[string]any{"previous_valid_until": until}})
	})
	return out, err
}

// ClientForToken implements auth.ClientStore.
func (s *Service) ClientForToken(ctx context.Context, clientID string) (auth.ClientRecord, error) {
	r, err := s.Store.Queries().ClientForToken(ctx, clientID)
	if store.IsNoRows(err) {
		return auth.ClientRecord{}, auth.ErrClientNotFound
	}
	if err != nil {
		return auth.ClientRecord{}, err
	}
	rec := auth.ClientRecord{ClientID: r.ClientID, OperatorID: store.UUIDText(r.OperatorID), SecretHash: r.SecretHash,
		PreviousValidUntil: r.PreviousValidUntil, Scopes: r.Scopes,
		Active: r.ClientStatus == StatusActive && r.OperatorStatus == OperatorActive}
	if r.PreviousSecretHash != nil {
		rec.PreviousHash = *r.PreviousSecretHash
	}
	return rec, nil
}

// ---- serial bindings ----

// Binding is one live client-serial binding.
type Binding struct {
	ClientID   string
	Serial     string
	SerialFold string
	BoundAt    time.Time
}

// projectBindings rewrites the client's client_bindings projection
// inside the transaction; a failure rolls the change back (B-09).
func (s *Service) projectBindings(ctx context.Context, q *relational.Queries, clientID string) error {
	folds, err := q.LiveFoldsOfClient(ctx, clientID)
	if err != nil {
		return err
	}
	if err := s.Bindings.ProjectClientBindings(ctx, clientID, folds); err != nil {
		return &policy.ProjectionError{Bucket: auth.BucketClientBindings, Err: err}
	}
	return nil
}

// BindSerial binds serial to the client after uspace-core
// serial.ValidateForClass with classLabel (empty: unlabelled). A serial
// live-bound to any other client is refused (06 T3).
func (s *Service) BindSerial(ctx context.Context, p auth.Principal, operatorID, clientID, sn, classLabel string) (Binding, error) {
	sn = serial.Normalize(sn) //nolint:misspell // uspace-core's name
	if err := serial.ValidateForClass(sn, classLabel); err != nil {
		return Binding{}, err
	}
	fold := serial.FoldKey(sn)
	var out Binding
	err := s.Store.Tx(ctx, func(q *relational.Queries) error {
		u, err := s.portalAdmin(ctx, q, p, operatorID)
		if err != nil {
			return err
		}
		c, err := clientOf(ctx, q, u, clientID)
		if err != nil {
			return err
		}
		if err := q.LockClientBindings(ctx, c.ClientID); err != nil {
			return err
		}
		if live, err := q.LiveBindingByFold(ctx, fold); err == nil {
			if live.ClientID == c.ClientID {
				out = Binding{ClientID: live.ClientID, Serial: live.Serial, SerialFold: live.SerialFold, BoundAt: live.BoundAt}
				return nil
			}
			s.count(CounterBindingRefused)
			return &Error{Status: http.StatusConflict, Slug: "serial_bound_elsewhere", Detail: "the serial is bound to another client",
				Field: &core.FieldError{Field: "serial", Reason: "bound to another client"}}
		} else if !store.IsNoRows(err) {
			return err
		}
		row, err := q.InsertBinding(ctx, relational.InsertBindingParams{ClientID: c.ClientID, Serial: sn, SerialFold: fold, BoundAt: s.now()})
		if err != nil {
			if store.SQLState(err) == store.StateUniqueViolation {
				s.count(CounterBindingRefused)
				return refuse(http.StatusConflict, "serial_bound_elsewhere", "the serial is bound to another client")
			}
			return err
		}
		out = Binding{ClientID: row.ClientID, Serial: row.Serial, SerialFold: row.SerialFold, BoundAt: row.BoundAt}
		if err := s.audit(ctx, q, store.Event{ActorType: auth.ActorPortalUser, ActorID: p.Claims.Subject, EntityType: "oauth_client",
			EntityID: c.ClientID, EventType: EventSerialBound, Payload: map[string]any{"serial": sn, "serial_fold": fold}}); err != nil {
			return err
		}
		return s.projectBindings(ctx, q, c.ClientID)
	})
	return out, err
}

// UnbindSerial ends the live binding of serial to the client.
func (s *Service) UnbindSerial(ctx context.Context, p auth.Principal, operatorID, clientID, sn string) error {
	fold := serial.FoldKey(sn)
	return s.Store.Tx(ctx, func(q *relational.Queries) error {
		u, err := s.portalAdmin(ctx, q, p, operatorID)
		if err != nil {
			return err
		}
		c, err := clientOf(ctx, q, u, clientID)
		if err != nil {
			return err
		}
		if err := q.LockClientBindings(ctx, c.ClientID); err != nil {
			return err
		}
		now := s.now()
		n, err := q.UnbindSerial(ctx, relational.UnbindSerialParams{At: &now, ClientID: c.ClientID, SerialFold: fold})
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		if err := s.audit(ctx, q, store.Event{ActorType: auth.ActorPortalUser, ActorID: p.Claims.Subject, EntityType: "oauth_client",
			EntityID: c.ClientID, EventType: EventSerialUnbound, Payload: map[string]any{"serial_fold": fold}}); err != nil {
			return err
		}
		return s.projectBindings(ctx, q, c.ClientID)
	})
}

// LoadBindings projects every client's live bindings (at start: the
// in-memory projection is empty until WP-6's KV holds it).
func (s *Service) LoadBindings(ctx context.Context, clientIDs []string) error {
	q := s.Store.Queries()
	for _, id := range clientIDs {
		if err := s.projectBindings(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// ---- audit for auth ----

// TokenIssued implements auth.TokenAuditor.
func (s *Service) TokenIssued(ctx context.Context, e auth.TokenEvent) error {
	return s.Store.Tx(ctx, func(q *relational.Queries) error {
		return s.audit(ctx, q, store.Event{ActorType: store.ActorClient, ActorID: e.ClientID, EntityType: "oauth_client", EntityID: e.ClientID,
			EventType: EventTokenIssued, Payload: map[string]any{"jti": e.JTI, "kid": e.KID, "aud": e.Audience, "scopes": e.Scopes,
				"exp": e.ExpiresAt, "remote_ip": e.RemoteIP}})
	})
}

// TokenRefused implements auth.TokenAuditor.
func (s *Service) TokenRefused(ctx context.Context, e auth.TokenEvent) error {
	return s.Store.Tx(ctx, func(q *relational.Queries) error {
		return s.audit(ctx, q, store.Event{ActorType: store.ActorClient, ActorID: clip(e.ClientID), EntityType: "oauth_client",
			EntityID: clip(e.ClientID), EventType: EventTokenRefused, Payload: map[string]any{"reason": e.Reason, "remote_ip": e.RemoteIP}})
	})
}

// AuthRefused implements auth.Auditor.
func (s *Service) AuthRefused(ctx context.Context, r auth.Refusal) error {
	return s.Store.Tx(ctx, func(q *relational.Queries) error {
		return s.audit(ctx, q, store.Event{ActorType: r.ActorType, ActorID: r.ActorID, EntityType: "route", EntityID: r.Route,
			EventType: EventAuthRefused, Payload: map[string]any{"reason": r.Reason, "status": r.Status, "remote_ip": r.RemoteIP, "request_id": r.RequestID}})
	})
}

// clip bounds an untrusted value before it is stored.
func clip(v string) string {
	v = strings.ToValidUTF8(v, "?")
	if len(v) > 128 {
		v = v[:128]
		for !utf8.ValidString(v) {
			v = v[:len(v)-1]
		}
	}
	if v == "" {
		return "unknown"
	}
	return v
}

// roleOK reports whether role is a role of realm.
func roleOK(realm, role string) bool { return slices.Contains(auth.RealmRoles[realm], role) }

// String renders an operator for logs (never the email).
func (o Operator) String() string { return fmt.Sprintf("operator %s (%s)", o.ID, o.Status) }

// ---- what an operator client may see of the registry ----

// RegistryScope is what of the registry is an operator client's own:
// its operator's registration number (the compare key of its public
// part, regnum.Public) and the
// serial fold keys live-bound to it. GET /v1/registry/validate answers
// its own keys in full and every other key status only (audit S6).
// Pilots belong to no operator yet (WP-7), so no pilot is in it.
type RegistryScope struct {
	OperatorKey string
	SerialFolds []string
}

// RegistryScope reads clientID's RegistryScope.
func (s *Service) RegistryScope(ctx context.Context, clientID string) (RegistryScope, error) {
	q := s.Store.Queries()
	c, err := q.ClientByID(ctx, clientID)
	if err != nil {
		return RegistryScope{}, notFoundOr(err)
	}
	op, err := q.OperatorByID(ctx, c.OperatorID)
	if err != nil {
		return RegistryScope{}, notFoundOr(err)
	}
	folds, err := q.LiveFoldsOfClient(ctx, clientID)
	if err != nil {
		return RegistryScope{}, err
	}
	_, key := regnum.Public(op.AuthorityRegistrationNumber)
	return RegistryScope{OperatorKey: key, SerialFolds: folds}, nil
}
