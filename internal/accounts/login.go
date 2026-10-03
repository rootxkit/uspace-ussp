package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Problem slugs of sign-in.
const (
	SlugInvalidCredentials = "invalid_credentials" //nolint:gosec // a problem slug, not a credential
	SlugMFARequired        = "mfa_required"
	SlugAccountLocked      = "account_locked"
	SlugMFAUnavailable     = "mfa_unavailable"
	SlugSessionUnavailable = "session_unavailable"
)

// LoginInput is a sign-in request.
type LoginInput struct {
	Realm    string
	Username string
	Password string
	TOTPCode string
}

// SessionResult is a started session: the token for the
// uspace_session cookie (and the BFF's bearer) and the CSRF value.
type SessionResult struct {
	Token         string
	CSRF          string
	AccountID     string
	Realm         string
	Roles         []string
	OperatorID    string
	ExpiresAt     time.Time
	IdleExpiresAt time.Time
}

// account is the realm-independent view of a user that signs in.
type account struct {
	id, username, hash, role, status string
	operatorID                       string
	mfaRef                           *string
	mfaLastStep                      int64
}

func (s *Service) lookupAccount(ctx context.Context, q *relational.Queries, realm, user string) (account, error) {
	switch realm {
	case auth.RealmPortal:
		u, err := q.PortalUserByUsername(ctx, user)
		if err != nil {
			return account{}, err
		}
		return account{id: store.UUIDText(u.ID), username: u.Username, hash: u.PasswordHash, role: u.Role, status: u.Status,
			operatorID: store.UUIDText(u.OperatorID)}, nil
	default:
		u, err := q.StaffByUsername(ctx, user)
		if err != nil {
			return account{}, err
		}
		return account{id: store.UUIDText(u.ID), username: u.Username, hash: u.PasswordHash, role: u.Role, status: u.Status,
			mfaRef: u.MfaSecretRef, mfaLastStep: u.MfaLastStep}, nil
	}
}

func invalidCredentials() *Error {
	return refuse(http.StatusUnauthorized, SlugInvalidCredentials, "the username, the password or the code is wrong")
}

// Login checks a username and password in a realm (and a TOTP code for
// a staff admin) and starts a session. An unknown user, a disabled
// user, a wrong password and a wrong code are one answer after one
// argon2id verification each, and each is a failure of the username in
// login_lockouts: LockoutAfter consecutive failures lock it for
// LockoutFor, on every replica. A locked username is refused with
// Retry-After before any verification. Every attempt is an events row.
func (s *Service) Login(ctx context.Context, in LoginInput, ip string) (SessionResult, error) {
	if s.LoginLimiter != nil {
		if ok, wait := s.LoginLimiter.Allow("ip:" + ip); !ok {
			s.loginRefused(ctx, in.Realm, auditName(in.Username), "", "rate_limited_address", ip)
			return SessionResult{}, &Error{Status: http.StatusTooManyRequests, Slug: httpx.SlugRateLimited,
				Detail: "too many sign-in attempts from this address; wait and try again", RetryAfter: wait}
		}
	}
	if _, ok := auth.RealmRoles[in.Realm]; !ok {
		return SessionResult{}, core.Fieldf("realm", "must be portal or console")
	}
	if len(in.Username) > 128 || len(in.Password) > auth.MaxSecretBytes || len(in.TOTPCode) > 16 {
		return SessionResult{}, core.Fieldf("body", "a field is too long")
	}
	// A username no account can have (empty, or outside the stored
	// form) is refused here, before the lookup and the lockout: it must
	// not be counted against, or lock, an account under another key
	// (clip would have turned "" into the username "unknown").
	user, err := username("username", in.Username)
	if err != nil {
		s.loginRefused(ctx, in.Realm, invalidUsername, "", "invalid_username", ip)
		return SessionResult{}, err
	}
	if s.Issuer == nil {
		return SessionResult{}, refuse(http.StatusServiceUnavailable, SlugSessionUnavailable, "this USSP has no issuer key: no session can start")
	}
	now := s.now()
	q := s.Store.Queries()
	if lock, err := q.LockoutByUsername(ctx, relational.LockoutByUsernameParams{Realm: in.Realm, Username: user}); err == nil &&
		lock.LockedUntil != nil && now.Before(*lock.LockedUntil) {
		s.count(CounterLoginLockedOut)
		s.loginRefused(ctx, in.Realm, user, "", "locked", ip)
		return SessionResult{}, &Error{Status: http.StatusTooManyRequests, Slug: SlugAccountLocked,
			Detail: "too many failed sign-ins: this username is locked", RetryAfter: lock.LockedUntil.Sub(now)}
	} else if err != nil && !store.IsNoRows(err) {
		return SessionResult{}, err
	}

	acc, err := s.lookupAccount(ctx, q, in.Realm, user)
	if err != nil {
		s.Hasher.VerifyDummy(in.Password)
		if !store.IsNoRows(err) {
			return SessionResult{}, err
		}
		return SessionResult{}, s.failure(ctx, in.Realm, user, "", "unknown_user", ip)
	}
	if ok, _ := s.Hasher.Verify(in.Password, acc.hash); !ok {
		return SessionResult{}, s.failure(ctx, in.Realm, user, acc.id, "wrong_password", ip)
	}
	if acc.status != StatusActive || !roleOK(in.Realm, acc.role) {
		return SessionResult{}, s.failure(ctx, in.Realm, user, acc.id, "account_disabled", ip)
	}
	var mfaStep int64
	if in.Realm == auth.RealmConsole && acc.role == auth.RoleAdmin {
		if s.MFA == nil || acc.mfaRef == nil {
			s.loginRefused(ctx, in.Realm, user, acc.id, "mfa_unavailable", ip)
			return SessionResult{}, refuse(http.StatusServiceUnavailable, SlugMFAUnavailable, "the admin's second factor cannot be checked: no MFA key or no enrolment")
		}
		if in.TOTPCode == "" {
			s.loginRefused(ctx, in.Realm, user, acc.id, "mfa_required", ip)
			return SessionResult{}, &Error{Status: http.StatusUnauthorized, Slug: SlugMFARequired, Detail: "a staff admin also sends a TOTP code",
				Field: &core.FieldError{Field: "totp_code", Reason: "required"}}
		}
		secret, err := s.MFA.Open(*acc.mfaRef, acc.username)
		if err != nil {
			obs.Error(ctx, s.logger(), "MFA secret does not open", err)
			return SessionResult{}, refuse(http.StatusServiceUnavailable, SlugMFAUnavailable, "the admin's second factor cannot be checked")
		}
		step, ok := VerifyTOTP(secret, in.TOTPCode, now, acc.mfaLastStep)
		if !ok {
			return SessionResult{}, s.failure(ctx, in.Realm, user, acc.id, "wrong_code", ip)
		}
		mfaStep = step
	}
	return s.startSession(ctx, in.Realm, acc, mfaStep, ip, now)
}

// failure counts one failed sign-in of (realm, username) in its own
// transaction, locks the username at LockoutAfter, audits both, and
// returns the one answer every failure gets.
func (s *Service) failure(ctx context.Context, realm, user, accountID, reason, ip string) error {
	s.count(CounterLoginRefused)
	now := s.now()
	err := s.Store.Tx(ctx, func(q *relational.Queries) error {
		key := relational.EnsureLockoutParams{Realm: realm, Username: user}
		if err := q.EnsureLockout(ctx, key); err != nil {
			return err
		}
		l, err := q.LockoutForUpdate(ctx, relational.LockoutForUpdateParams(key))
		if err != nil {
			return err
		}
		failures := l.Failures + 1
		if l.LockedUntil != nil && !now.Before(*l.LockedUntil) {
			failures = 1 // the last lock is over: a new count
		}
		var until *time.Time
		if s.Config.LockoutAfter > 0 && int(failures) >= s.Config.LockoutAfter {
			u := now.Add(s.Config.LockoutFor)
			until = &u
		}
		if err := q.SetLockout(ctx, relational.SetLockoutParams{Failures: failures, LockedUntil: until, At: now, Realm: realm, Username: user}); err != nil {
			return err
		}
		if err := s.audit(ctx, q, loginEvent(realm, user, accountID, EventLoginRefused, map[string]any{"reason": reason, "failures": failures, "remote_ip": ip})); err != nil {
			return err
		}
		if until == nil {
			return nil
		}
		s.count(CounterLoginLocked)
		return s.audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "login-lockout", EntityType: "login", EntityID: realm + ":" + user,
			EventType: EventLoginLocked, Payload: map[string]any{"failures": failures, "locked_until": until, "remote_ip": ip}})
	})
	if err != nil {
		s.count(CounterRefusalNotSaved)
		obs.Error(ctx, s.logger(), "sign-in failure not recorded", err, slog.String("reason", reason))
	}
	return invalidCredentials()
}

func loginEvent(realm, user, accountID, eventType string, payload map[string]any) store.Event {
	actorType, actorID := auth.ActorAnonymous, "unknown"
	if accountID != "" {
		actorType, actorID = auth.ActorPortalUser, accountID
		if realm == auth.RealmConsole {
			actorType = auth.ActorStaff
		}
	}
	payload["username"] = user
	payload["realm"] = realm
	return store.Event{ActorType: actorType, ActorID: actorID, EntityType: "login", EntityID: realm + ":" + user, EventType: eventType, Payload: payload}
}

// loginRefused audits a refusal that is not a failure of the username
// (a limit, a missing second factor).
// invalidUsername labels, in the audit log only, a sign-in whose
// username no account can have; the parentheses keep it apart from
// every real username.
const invalidUsername = "(invalid)"

// auditName is the username as the audit log names it.
func auditName(raw string) string {
	if u, err := username("username", raw); err == nil {
		return u
	}
	return invalidUsername
}

func (s *Service) loginRefused(ctx context.Context, realm, user, accountID, reason, ip string) {
	s.count(CounterLoginRefused)
	_ = s.auditOwnTx(ctx, loginEvent(realm, clip(user), accountID, EventLoginRefused, map[string]any{"reason": reason, "remote_ip": ip}))
}

func (s *Service) startSession(ctx context.Context, realm string, acc account, mfaStep int64, ip string, now time.Time) (SessionResult, error) {
	jti, err := auth.RandomSecret(24)
	if err != nil {
		return SessionResult{}, err
	}
	csrf, err := auth.RandomSecret(32)
	if err != nil {
		return SessionResult{}, err
	}
	roles := []string{acc.role}
	token, exp, err := s.Issuer.IssueSession(acc.id, realm, jti, roles, s.Config.SessionTTL, now)
	if err != nil {
		return SessionResult{}, err
	}
	accountID, err := store.UUID("sub", acc.id)
	if err != nil {
		return SessionResult{}, err
	}
	if s.Config.BeforeSessionTx != nil {
		s.Config.BeforeSessionTx()
	}
	err = s.Store.Tx(ctx, func(q *relational.Queries) error {
		if mfaStep > 0 {
			// The row lock makes two sign-ins with one code race to one
			// winner: the step stored is the last accepted.
			st, err := q.StaffForUpdate(ctx, accountID)
			if err != nil {
				return err
			}
			if st.MfaLastStep >= mfaStep {
				return errCodeReplayed
			}
			if err := q.SetStaffMFAStep(ctx, relational.SetStaffMFAStepParams{Step: mfaStep, ID: accountID}); err != nil {
				return err
			}
		}
		if err := q.ClearLockout(ctx, relational.ClearLockoutParams{Realm: realm, Username: acc.username}); err != nil {
			return err
		}
		if err := q.InsertSession(ctx, relational.InsertSessionParams{Jti: jti, Realm: realm, AccountID: accountID, Roles: roles,
			IssuedAt: now.Truncate(time.Second), ExpiresAt: exp, RemoteIp: ip}); err != nil {
			return err
		}
		if err := s.projectSession(ctx, jti, auth.LiveSession{Subject: acc.id, Realm: realm, ExpiresAt: exp,
			IdleUntil: now.Add(s.Config.SessionIdle)}); err != nil {
			return err
		}
		return s.audit(ctx, q, loginEvent(realm, acc.username, acc.id, EventLoginSucceeded, map[string]any{
			"session": jti, "roles": roles, "exp": exp, "kid": s.Issuer.Keys().Current.KID, "mfa": mfaStep > 0, "remote_ip": ip}))
	})
	if errors.Is(err, errCodeReplayed) {
		// The code was spent between the check and the row lock (another
		// sign-in, maybe on another replica): a failure like any wrong
		// code, counted against the username and audited.
		return SessionResult{}, s.failure(ctx, realm, acc.username, acc.id, "code_replayed", ip)
	}
	if err != nil {
		return SessionResult{}, fmt.Errorf("start session: %w", err)
	}
	s.count(CounterLoginSucceeded)
	return SessionResult{Token: token, CSRF: csrf, AccountID: acc.id, Realm: realm, Roles: roles, OperatorID: acc.operatorID,
		ExpiresAt: exp, IdleExpiresAt: earlier(exp, now.Add(s.Config.SessionIdle))}, nil
}

// projectSession writes the session to sessions_live; a failure is a
// *policy.ProjectionError, which rolls the transaction back (B-09).
func (s *Service) projectSession(ctx context.Context, jti string, ls auth.LiveSession) error {
	if s.LiveSessions == nil {
		return nil
	}
	if err := s.LiveSessions.ProjectSession(ctx, jti, ls); err != nil {
		return &policy.ProjectionError{Bucket: auth.BucketSessionsLive, Err: err}
	}
	return nil
}

// endSession removes the session from sessions_live; a failure is a
// *policy.ProjectionError, which rolls the transaction back (B-09).
func (s *Service) endSession(ctx context.Context, jti string) error {
	if s.LiveSessions == nil {
		return nil
	}
	if err := s.LiveSessions.EndSession(ctx, jti); err != nil {
		return &policy.ProjectionError{Bucket: auth.BucketSessionsLive, Err: err}
	}
	return nil
}

// errCodeReplayed is a TOTP step already accepted, found under the row
// lock.
var errCodeReplayed = errors.New("the TOTP code was already used")

// touchEvery bounds the writes of last_seen_at to one a minute per
// session.
const touchEvery = time.Minute

// CheckSession implements auth.SessionChecker: the session row exists
// for that account, is not revoked, has not expired and was used within
// the idle timeout; an idle session is ended so that it stays ended.
func (s *Service) CheckSession(ctx context.Context, jti, sub string) error {
	now := s.now()
	sess, err := s.Store.Queries().SessionByJTI(ctx, jti)
	if store.IsNoRows(err) {
		return fmt.Errorf("%w: unknown session", auth.ErrSessionRefused)
	}
	if err != nil {
		return err
	}
	switch {
	case store.UUIDText(sess.AccountID) != sub:
		return fmt.Errorf("%w: the session is not this account's", auth.ErrSessionRefused)
	case sess.RevokedAt != nil:
		return fmt.Errorf("%w: the session was ended", auth.ErrSessionRefused)
	case !now.Before(sess.ExpiresAt):
		return fmt.Errorf("%w: the session expired", auth.ErrSessionRefused)
	case now.Sub(sess.LastSeenAt) > s.Config.SessionIdle:
		err := s.Store.Tx(ctx, func(q *relational.Queries) error {
			if _, err := q.RevokeSession(ctx, relational.RevokeSessionParams{At: &now, Reason: strPtr("idle"), Jti: jti}); err != nil {
				return err
			}
			if err := s.endSession(ctx, jti); err != nil {
				return err
			}
			return s.audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "session-idle", EntityType: "session", EntityID: jti,
				EventType: EventSessionIdleEnded, Payload: map[string]any{"account_id": sub, "idle_s": int64(s.Config.SessionIdle / time.Second)}})
		})
		if err != nil {
			obs.Error(ctx, s.logger(), "idle session not ended", err)
		}
		return fmt.Errorf("%w: the session was idle longer than %s", auth.ErrSessionRefused, s.Config.SessionIdle)
	}
	if now.Sub(sess.LastSeenAt) >= touchEvery {
		if err := s.Store.Queries().TouchSession(ctx, relational.TouchSessionParams{At: now, Jti: jti}); err != nil {
			return err
		}
		// The use moves the idle end in sessions_live too. A failure
		// leaves the earlier end there: traffic-ws may then close the
		// session's sockets early (sign in again), never late.
		if err := s.projectSession(ctx, jti, auth.LiveSession{Subject: sub, Realm: sess.Realm, ExpiresAt: sess.ExpiresAt,
			IdleUntil: now.Add(s.Config.SessionIdle)}); err != nil {
			s.count(CounterSessionProjectionFailed)
			obs.Error(ctx, s.logger(), "session use not projected; its idle end in sessions_live is the earlier one", err)
		}
	}
	return nil
}

func strPtr(s string) *string { return &s }

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Logout ends the caller's session.
func (s *Service) Logout(ctx context.Context, p auth.Principal) error {
	if !p.Session {
		return refuse(http.StatusForbidden, httpx.SlugForbidden, "only a session can be ended")
	}
	now := s.now()
	return s.Store.Tx(ctx, func(q *relational.Queries) error {
		if _, err := q.RevokeSession(ctx, relational.RevokeSessionParams{At: &now, Reason: strPtr("logout"), Jti: p.Claims.JTI}); err != nil {
			return err
		}
		if err := s.endSession(ctx, p.Claims.JTI); err != nil {
			return err
		}
		actor := auth.ActorPortalUser
		if p.Claims.Realm == auth.RealmConsole {
			actor = auth.ActorStaff
		}
		return s.audit(ctx, q, store.Event{ActorType: actor, ActorID: p.Claims.Subject, EntityType: "session", EntityID: p.Claims.JTI, EventType: EventLogout})
	})
}

// Me is the account behind a session.
type Me struct {
	AccountID        string
	Username         string
	Realm            string
	Roles            []string
	OperatorID       string
	SessionExpiresAt time.Time
}

// Me returns the caller's account.
func (s *Service) Me(ctx context.Context, p auth.Principal) (Me, error) {
	if !p.Session {
		return Me{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "a session is required")
	}
	id, err := store.UUID("sub", p.Claims.Subject)
	if err != nil {
		return Me{}, errNotFound
	}
	q := s.Store.Queries()
	out := Me{AccountID: p.Claims.Subject, Realm: p.Claims.Realm, Roles: p.Claims.Roles, SessionExpiresAt: p.Claims.ExpiresAt}
	if p.Claims.Realm == auth.RealmPortal {
		u, err := q.PortalUserByID(ctx, id)
		if err != nil {
			return Me{}, notFoundOr(err)
		}
		out.Username, out.OperatorID = u.Username, store.UUIDText(u.OperatorID)
		return out, nil
	}
	u, err := q.StaffByID(ctx, id)
	if err != nil {
		return Me{}, notFoundOr(err)
	}
	out.Username = u.Username
	return out, nil
}

func notFoundOr(err error) error {
	if store.IsNoRows(err) {
		return errNotFound
	}
	return err
}

// StaffCreated is a new staff account; an admin's TOTP enrolment is
// shown once.
type StaffCreated struct {
	ID         string
	TOTPSecret string
	TOTPURI    string
}

// CreateStaff creates a console account (the bootstrap path: the
// console's user management is WP-18's). An admin gets a TOTP secret,
// sealed under the MFA key, which is returned once.
func (s *Service) CreateStaff(ctx context.Context, user, pass, role, actor string) (StaffCreated, error) {
	u, err1 := username("username", user)
	err2 := password("password", pass)
	var err3 error
	if !roleOK(auth.RealmConsole, role) {
		err3 = core.Fieldf("role", "must be supervisor, support or admin")
	}
	if err := errors.Join(err1, err2, err3); err != nil {
		return StaffCreated{}, err
	}
	hash, err := s.Hasher.Hash(pass)
	if err != nil {
		return StaffCreated{}, err
	}
	var out StaffCreated
	var ref *string
	if role == auth.RoleAdmin {
		if s.MFA == nil {
			return StaffCreated{}, core.Fieldf("USSP_MFA_KEY_FILE", "an admin needs a TOTP secret, sealed under the MFA key")
		}
		secret, uri, err := NewTOTPSecret(s.Config.TOTPIssuer, u)
		if err != nil {
			return StaffCreated{}, err
		}
		sealed, err := s.MFA.Seal(secret, u)
		if err != nil {
			return StaffCreated{}, err
		}
		out.TOTPSecret, out.TOTPURI, ref = secret, uri, &sealed
	}
	err = s.Store.Tx(ctx, func(q *relational.Queries) error {
		row, err := q.InsertStaff(ctx, relational.InsertStaffParams{Username: u, PasswordHash: hash, Role: role, MfaSecretRef: ref, Status: StatusActive})
		if err != nil {
			if store.SQLState(err) == store.StateUniqueViolation {
				return refuse(http.StatusConflict, "username_taken", "the username is taken")
			}
			return err
		}
		out.ID = store.UUIDText(row.ID)
		return s.audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: clip(actor), EntityType: "staff", EntityID: out.ID,
			EventType: EventStaffCreated, Payload: map[string]any{"username": u, "role": role, "mfa": ref != nil}})
	})
	return out, err
}

// SweepRetention keeps an expired session row, and an untouched lockout
// row, this long before the sweep deletes it (the events rows stay).
const SweepRetention = 24 * time.Hour

// Sweep deletes sessions expired more than SweepRetention ago and
// lockout rows untouched for as long that hold no lock: both tables stay
// bounded by the sign-in rate times their lifetimes (E-10).
func (s *Service) Sweep(ctx context.Context) (sessions, lockouts int64, err error) {
	now := s.now()
	err = s.Store.Tx(ctx, func(q *relational.Queries) error {
		if sessions, err = q.SweepSessions(ctx, now.Add(-SweepRetention)); err != nil {
			return err
		}
		lockouts, err = q.SweepLockouts(ctx, relational.SweepLockoutsParams{Before: now.Add(-SweepRetention), Now: &now})
		return err
	})
	if err == nil {
		for range sessions {
			s.count(CounterSessionsSwept)
		}
		for range lockouts {
			s.count(CounterLockoutsSwept)
		}
	}
	return sessions, lockouts, err
}

// RunSweep sweeps every interval until ctx ends.
func (s *Service) RunSweep(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				obs.Error(ctx, s.logger(), "session and lockout sweep failed; it runs again next interval", err)
			}
		}
	}
}
