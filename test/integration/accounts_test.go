//go:build integration

package integration

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
)

// The success path end to end (E-02): a validated operator registers,
// its admin signs in, creates a client, the client gets a token whose
// aud is this host, and every step is an events row.
func TestIntegrationOperatorClientTokenFlow(t *testing.T) {
	s := newStack(t, newClock(), &logBuffer{})
	opID, session, _ := s.operator(accounts.RegistryValid)
	op := s.call("GET", "/v1/accounts/operators/"+opID, nil, bearer(session))
	if op.status != 200 || op.str("status") != accounts.OperatorActive || op.str("validation_status") != accounts.RegistryValid {
		t.Fatalf("operator: %d %s", op.status, op.raw)
	}
	id, secret := s.client(opID, session, auth.ScopeTelemetry, auth.ScopeIntents)
	tok := s.token(id, secret)
	if tok.status != 200 || tok.str("scope") != "ussp.intents ussp.telemetry" {
		t.Fatalf("token: %d %s", tok.status, tok.raw)
	}
	claims, err := s.verifier.Verify(context.Background(), tok.str("access_token"))
	if err != nil || claims.Subject != id || claims.Audience != testHost {
		t.Fatalf("claims %+v %v", claims, err)
	}
	for _, e := range []struct{ entity, id, event string }{
		{"operator", opID, accounts.EventOperatorRegistered},
		{"oauth_client", id, accounts.EventClientCreated},
		{"oauth_client", id, accounts.EventTokenIssued},
	} {
		if n := events(t, e.entity, e.id, e.event); n != 1 {
			t.Errorf("%s events: %d", e.event, n)
		}
	}
	// The PATCH of an active operator changes the contact only.
	p := s.call("PATCH", "/v1/accounts/operators/"+opID, map[string]any{"display_name": "Renamed"}, bearer(session))
	if p.status != 200 || p.str("display_name") != "Renamed" || p.str("status") != accounts.OperatorActive {
		t.Fatalf("patch: %d %s", p.status, p.raw)
	}
}

// E-01: an operator the registry does not know is pending and gets no
// client; once the registry says valid (checked again on PATCH) it does.
// A refused registration is not stored; a second account for one
// registration number is refused.
func TestIntegrationOperatorValidationPairs(t *testing.T) {
	s := newStack(t, newClock(), &logBuffer{})
	opID, session, _ := s.operator(accounts.RegistryUnknown)
	r := s.call("POST", "/v1/accounts/operators/"+opID+"/clients", map[string]any{"scopes": []string{auth.ScopeGeo}}, bearer(session))
	if r.status != 403 || r.slug() != "operator_not_validated" {
		t.Fatalf("pending operator: %d %s", r.status, r.raw)
	}
	s.registry.set(accounts.RegistryValid)
	p := s.call("PATCH", "/v1/accounts/operators/"+opID, map[string]any{"contact_email": "new@example.test"}, bearer(session))
	if p.status != 200 || p.str("status") != accounts.OperatorActive || events(t, "operator", opID, accounts.EventOperatorValidated) != 1 {
		t.Fatalf("revalidation: %d %s", p.status, p.raw)
	}
	s.client(opID, session, auth.ScopeGeo)

	s.registry.set("suspended")
	u := unique()
	reg := map[string]any{"registration_number": "GEO-TEST-" + u, "display_name": "x", "contact_email": "x" + u + "@example.test",
		"admin_username": "a" + u, "admin_password": "a-long-password-" + u}
	if r := s.call("POST", "/v1/accounts/operators", reg, nil); r.status != 422 || r.slug() != "registry_refused" {
		t.Fatalf("refused registration: %d %s", r.status, r.raw)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM operator_accounts WHERE authority_registration_number = $1", "GEO-TEST-"+u); n != 0 {
		t.Fatal("a refused registration was stored")
	}
	s.registry.set(accounts.RegistryUnknown)
	if r := s.call("POST", "/v1/accounts/operators", reg, nil); r.status != 201 {
		t.Fatalf("first registration: %d %s", r.status, r.raw)
	}
	reg["admin_username"] = "b" + u
	if r := s.call("POST", "/v1/accounts/operators", reg, nil); r.status != 409 || r.slug() != "operator_exists" {
		t.Fatalf("second account: %d %s", r.status, r.raw)
	}
	// Another operator's admin may not read this operator.
	other, otherSession, _ := s.operator(accounts.RegistryValid)
	if r := s.call("GET", "/v1/accounts/operators/"+opID, nil, bearer(otherSession)); r.status != 403 {
		t.Fatalf("another operator: %d", r.status)
	}
	if r := s.call("GET", "/v1/accounts/operators/"+other, nil, bearer(otherSession)); r.status != 200 {
		t.Fatalf("own operator: %d", r.status)
	}
	if r := s.call("GET", "/v1/accounts/operators/not-a-uuid", nil, bearer(otherSession)); r.status != 400 {
		t.Fatalf("malformed id: %d %s", r.status, r.raw)
	}
}

// E-01, brief WP-2: a rotated secret works inside the overlap and not
// after it; the new one works throughout.
func TestIntegrationSecretRotationOverlap(t *testing.T) {
	s := newStack(t, newClock(), &logBuffer{})
	opID, session, _ := s.operator(accounts.RegistryValid)
	id, old := s.client(opID, session, auth.ScopeGeo)
	r := s.call("POST", "/v1/accounts/operators/"+opID+"/clients/"+id+"/rotate", nil, bearer(session))
	fresh := r.str("client_secret")
	if r.status != 200 || fresh == "" || fresh == old || r.str("previous_valid_until") == "" {
		t.Fatalf("rotate: %d %s", r.status, r.raw)
	}
	if r := s.call("POST", "/v1/accounts/operators/"+opID+"/clients/op-nobody/rotate", nil, bearer(session)); r.status != 404 {
		t.Fatalf("unknown client: %d", r.status)
	}
	if t1 := s.token(id, old); t1.status != 200 {
		t.Fatalf("old secret inside the overlap: %d %s", t1.status, t1.raw)
	}
	s.clock.Add(time.Hour + time.Second)
	if t2 := s.token(id, old); t2.status != 401 || t2.str("error") != auth.OAuthInvalidClient {
		t.Fatalf("old secret after the overlap: %d %s", t2.status, t2.raw)
	}
	if t3 := s.token(id, fresh); t3.status != 200 {
		t.Fatalf("new secret: %d %s", t3.status, t3.raw)
	}
}

// E-01, 06 T3: a serial bound to client A is allowed for A and refused
// for B; binding it to B is refused; unbinding ends A's; a binding whose
// projection fails leaves nothing (B-09).
func TestIntegrationSerialBindings(t *testing.T) {
	s := newStack(t, newClock(), &logBuffer{})
	opID, session, _ := s.operator(accounts.RegistryValid)
	a, _ := s.client(opID, session, auth.ScopeTelemetry)
	b, _ := s.client(opID, session, auth.ScopeTelemetry)
	sn := "TEST" + unique()
	bind := func(clientID, serial string, class any) resp {
		body := map[string]any{"serial": serial}
		if class != nil {
			body["class_label"] = class
		}
		return s.call("POST", "/v1/accounts/operators/"+opID+"/clients/"+clientID+"/serials", body, bearer(session))
	}
	if r := bind(a, strings.ToLower(sn), nil); r.status != 201 || r.str("serial_fold") != sn {
		t.Fatalf("bind to A: %d %s", r.status, r.raw)
	}
	if !s.bindings.Allowed(a, sn) || s.bindings.Allowed(b, sn) {
		t.Fatal("the projection does not say A yes, B no")
	}
	if r := bind(b, sn, nil); r.status != 409 || r.slug() != "serial_bound_elsewhere" {
		t.Fatalf("bind to B: %d %s", r.status, r.raw)
	}
	if r := bind(a, sn, nil); r.status != 201 {
		t.Fatalf("bind to A again: %d", r.status)
	}
	if r := bind(a, "DJI-0042", "C1"); r.status != 400 {
		t.Fatalf("a non-CTA serial for C1: %d %s", r.status, r.raw)
	}
	del := s.call("DELETE", "/v1/accounts/operators/"+opID+"/clients/"+a+"/serials/"+url.PathEscape(sn), nil, bearer(session))
	if del.status != 204 || s.bindings.Allowed(a, sn) {
		t.Fatalf("unbind: %d", del.status)
	}
	if again := s.call("DELETE", "/v1/accounts/operators/"+opID+"/clients/"+a+"/serials/"+sn, nil, bearer(session)); again.status != 404 {
		t.Fatalf("unbind twice: %d", again.status)
	}
	if r := bind(b, sn, nil); r.status != 201 || !s.bindings.Allowed(b, sn) {
		t.Fatalf("bind to B after A let go: %d", r.status)
	}
	s.bindings.Fail = errors.New("kv down")
	other := "TEST" + unique()
	if r := bind(a, other, nil); r.status != 503 || r.slug() != "projection_unavailable" {
		t.Fatalf("projection down: %d %s", r.status, r.raw)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM client_serial_bindings WHERE serial_fold = $1", other); n != 0 {
		t.Fatal("a binding whose projection failed was stored")
	}
}

// Brief WP-2: the lockout proven both ways. Nine failures then the right
// password signs in; ten failures and the right password is refused
// 429 with Retry-After, on this replica and on another, until the lock
// ends.
func TestIntegrationLoginLockout(t *testing.T) {
	c := newClock()
	s := newStack(t, c, &logBuffer{})
	replica := newStack(t, c, &logBuffer{})
	u := unique()
	user, pass := "pilot."+u, "the-right-password-"+u
	if r := s.call("POST", "/v1/accounts/operators", map[string]any{"registration_number": "GEO-TEST-" + u, "display_name": "x",
		"contact_email": "l" + u + "@example.test", "admin_username": user, "admin_password": pass}, nil); r.status != 201 {
		t.Fatal(r.raw)
	}
	fail := func(n int) {
		for range n {
			if r := s.login(auth.RealmPortal, user, "wrong-password", ""); r.status != 401 || r.slug() != accounts.SlugInvalidCredentials {
				t.Fatalf("failure: %d %s", r.status, r.raw)
			}
		}
	}
	fail(9)
	if r := s.login(auth.RealmPortal, user, pass, ""); r.status != 200 {
		t.Fatalf("after 9 failures: %d %s", r.status, r.raw)
	}
	fail(10)
	r := s.login(auth.RealmPortal, user, pass, "")
	if r.status != 429 || r.slug() != accounts.SlugAccountLocked {
		t.Fatalf("after 10 failures: %d %s", r.status, r.raw)
	}
	if ra, err := strconv.Atoi(r.header.Get("Retry-After")); err != nil || ra < 899 || ra > 900 {
		t.Fatalf("Retry-After %q", r.header.Get("Retry-After"))
	}
	if r := replica.login(auth.RealmPortal, user, pass, ""); r.status != 429 {
		t.Fatalf("another replica: %d (the lock must hold across replicas)", r.status)
	}
	if n := events(t, "login", "portal:"+user, accounts.EventLoginLocked); n != 1 {
		t.Fatalf("login_locked events %d", n)
	}
	c.Add(15*time.Minute + time.Second)
	if r := replica.login(auth.RealmPortal, user, pass, ""); r.status != 200 {
		t.Fatalf("after the lock: %d %s", r.status, r.raw)
	}
	// An unknown username is counted the same way, so a lock does not
	// tell it from a known one.
	ghost := "ghost." + unique()
	for range 10 {
		s.login(auth.RealmPortal, ghost, "x-password-x", "")
	}
	if r := s.login(auth.RealmPortal, ghost, "x-password-x", ""); r.status != 429 {
		t.Fatalf("unknown username after 10: %d", r.status)
	}
	// An empty username is refused before the lockout: eleven of them
	// neither create nor move a lockout row (in particular not that of
	// a user called "unknown").
	before := count(t, relOwner(t), "SELECT count(*) FROM login_lockouts WHERE username IN ('', 'unknown', '(invalid)')")
	for range 11 {
		if r := s.login(auth.RealmPortal, "", "x-password-x", ""); r.status != 400 {
			t.Fatalf("empty username: %d %s", r.status, r.raw)
		}
	}
	if after := count(t, relOwner(t), "SELECT count(*) FROM login_lockouts WHERE username IN ('', 'unknown', '(invalid)')"); after != before {
		t.Fatalf("lockout rows for an empty username: %d -> %d", before, after)
	}
}

func totpNow(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return accounts.TOTPCode(key, at)
}

// A staff admin signs in with a TOTP code (brief WP-2): without one
// 401 mfa_required, a wrong one 401, the right one 200, the same one a
// second time refused (on another replica too: the step is in the
// database). A supervisor needs no code.
func TestIntegrationStaffAdminTOTP(t *testing.T) {
	c := newClock()
	s := newStack(t, c, &logBuffer{})
	replica := newStack(t, c, &logBuffer{})
	u := unique()
	created, err := s.svc.CreateStaff(context.Background(), "admin."+u, "staff-password-"+u, auth.RoleAdmin, "test")
	if err != nil || created.TOTPSecret == "" || !strings.HasPrefix(created.TOTPURI, "otpauth://totp/") {
		t.Fatalf("%+v %v", created, err)
	}
	ref := ""
	if err := relOwner(t).QueryRow(context.Background(), "SELECT mfa_secret_ref FROM staff_accounts WHERE username = $1", "admin."+u).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ref, created.TOTPSecret) {
		t.Fatal("the TOTP secret is stored in clear")
	}
	if r := s.login(auth.RealmConsole, "admin."+u, "staff-password-"+u, ""); r.status != 401 || r.slug() != accounts.SlugMFARequired {
		t.Fatalf("no code: %d %s", r.status, r.raw)
	}
	if r := s.login(auth.RealmConsole, "admin."+u, "staff-password-"+u, "000000"); r.status != 401 || r.slug() != accounts.SlugInvalidCredentials {
		t.Fatalf("wrong code: %d %s", r.status, r.raw)
	}
	code := totpNow(t, created.TOTPSecret, c.Now())
	r := s.login(auth.RealmConsole, "admin."+u, "staff-password-"+u, code)
	if r.status != 200 || r.str("realm") != auth.RealmConsole {
		t.Fatalf("right code: %d %s", r.status, r.raw)
	}
	if r := replica.login(auth.RealmConsole, "admin."+u, "staff-password-"+u, code); r.status != 401 {
		t.Fatalf("the same code again on another replica: %d", r.status)
	}
	key := "console:admin." + u
	failures := func() int64 {
		return count(t, relOwner(t), "SELECT coalesce(max(failures), 0) FROM login_lockouts WHERE realm || ':' || username = $1", key)
	}
	refusals := func() int64 { return events(t, "login", key, accounts.EventLoginRefused) }
	// Review fix: the replay the row lock catches (the code spent by
	// another sign-in between the check and the lock) is a failure like
	// the one caught before: counted against the username and audited.
	c.Add(30 * time.Second)
	next := totpNow(t, created.TOTPSecret, c.Now())
	f0, r0 := failures(), refusals()
	s.svc.Config.BeforeSessionTx = func() {
		step := c.Now().Unix() / 30
		if _, err := relOwner(t).Exec(context.Background(), "UPDATE staff_accounts SET mfa_last_step = $1 WHERE username = $2", step, "admin."+u); err != nil {
			t.Error(err)
		}
	}
	r = s.login(auth.RealmConsole, "admin."+u, "staff-password-"+u, next)
	s.svc.Config.BeforeSessionTx = nil
	if r.status != 401 || r.slug() != accounts.SlugInvalidCredentials {
		t.Fatalf("replay under the lock: %d %s", r.status, r.raw)
	}
	if failures() != f0+1 || refusals() != r0+1 {
		t.Fatalf("the replay under the lock was not counted and audited: failures %d -> %d, refusals %d -> %d", f0, failures(), r0, refusals())
	}
	// The presence twin: the next code without interference signs in.
	c.Add(30 * time.Second)
	if r := s.login(auth.RealmConsole, "admin."+u, "staff-password-"+u, totpNow(t, created.TOTPSecret, c.Now())); r.status != 200 {
		t.Fatalf("a fresh code: %d %s", r.status, r.raw)
	}
	if _, err := s.svc.CreateStaff(context.Background(), "super."+u, "staff-password-"+u, auth.RoleSupervisor, "test"); err != nil {
		t.Fatal(err)
	}
	if r := s.login(auth.RealmConsole, "super."+u, "staff-password-"+u, ""); r.status != 200 {
		t.Fatalf("supervisor: %d %s", r.status, r.raw)
	}
	// A portal user cannot sign in to the console with the same name.
	if r := s.login(auth.RealmPortal, "super."+u, "staff-password-"+u, ""); r.status != 401 {
		t.Fatalf("staff in the portal realm: %d", r.status)
	}
}

// The session lifecycle (M20, M21): the cookie authenticates a GET; a
// POST with the cookie needs the CSRF header; logout ends the session
// for every replica; an idle session ends.
func TestIntegrationSessionLifecycle(t *testing.T) {
	c := newClock()
	s := newStack(t, c, &logBuffer{})
	replica := newStack(t, c, &logBuffer{})
	_, token, _ := s.operator(accounts.RegistryValid)
	csrf := ""
	r := s.call("GET", "/v1/accounts/me", nil, bearer(token))
	if r.status != 200 || r.str("realm") != auth.RealmPortal {
		t.Fatalf("me: %d %s", r.status, r.raw)
	}
	// The login answer sets both cookies with the contract's attributes.
	u := unique()
	s.call("POST", "/v1/accounts/operators", map[string]any{"registration_number": "GEO-TEST-" + u, "display_name": "x",
		"contact_email": "c" + u + "@example.test", "admin_username": "cookie." + u, "admin_password": "cookie-password-" + u}, nil)
	l := s.login(auth.RealmPortal, "cookie."+u, "cookie-password-"+u, "")
	var cookies []*http.Cookie
	for _, sc := range l.header.Values("Set-Cookie") {
		ck, err := http.ParseSetCookie(sc)
		if err != nil {
			t.Fatal(err)
		}
		if !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode {
			t.Fatalf("cookie attributes %s", sc)
		}
		cookies = append(cookies, ck)
		if ck.Name == auth.CookieCSRF {
			csrf = ck.Value
		}
	}
	if len(cookies) != 2 || csrf != l.str("csrf_token") {
		t.Fatalf("cookies %v", l.header.Values("Set-Cookie"))
	}
	withCookies := func(header string) func(*http.Request) {
		return func(req *http.Request) {
			for _, ck := range cookies {
				req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
			}
			if header != "" {
				req.Header.Set(auth.HeaderCSRF, header)
			}
		}
	}
	if r := s.call("GET", "/v1/accounts/me", nil, withCookies("")); r.status != 200 {
		t.Fatalf("GET with the cookie: %d %s", r.status, r.raw)
	}
	if r := s.call("POST", "/v1/accounts/logout", nil, withCookies("")); r.status != 403 || r.slug() != auth.SlugCSRF {
		t.Fatalf("POST without CSRF: %d %s", r.status, r.raw)
	}
	if r := s.call("POST", "/v1/accounts/logout", nil, withCookies(csrf)); r.status != 204 {
		t.Fatalf("logout: %d %s", r.status, r.raw)
	}
	if r := replica.call("GET", "/v1/accounts/me", nil, withCookies("")); r.status != 401 || r.slug() != auth.SlugSessionRefused {
		t.Fatalf("after logout, on another replica: %d %s", r.status, r.raw)
	}
	// Idle: used, then 31 minutes of nothing.
	if r := s.call("GET", "/v1/accounts/me", nil, bearer(token)); r.status != 200 {
		t.Fatal(r.raw)
	}
	c.Add(31 * time.Minute)
	if r := s.call("GET", "/v1/accounts/me", nil, bearer(token)); r.status != 401 || r.slug() != auth.SlugSessionRefused {
		t.Fatalf("idle: %d %s", r.status, r.raw)
	}
	c.Add(-31 * time.Minute)
	if r := s.call("GET", "/v1/accounts/me", nil, bearer(token)); r.status != 401 {
		t.Fatalf("an idle-ended session came back: %d", r.status)
	}
}

// Every refusal of the guard is an events row: actor "unknown" without
// a readable sub, the token's sub otherwise (brief WP-2).
func TestIntegrationRefusalsAreAudited(t *testing.T) {
	s := newStack(t, newClock(), &logBuffer{})
	before := count(t, relOwner(t), "SELECT count(*) FROM events WHERE event_type = 'auth_refused' AND actor_id = 'unknown'")
	if r := s.call("GET", "/v1/accounts/me", nil, nil); r.status != 401 {
		t.Fatal(r.raw)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM events WHERE event_type = 'auth_refused' AND actor_id = 'unknown'"); n != before+1 {
		t.Fatalf("no audit row for the anonymous refusal: %d -> %d", before, n)
	}
	opID, session, _ := s.operator(accounts.RegistryValid)
	id, secret := s.client(opID, session, auth.ScopeGeo)
	tok := s.token(id, secret).str("access_token")
	if r := s.call("GET", "/v1/accounts/me", nil, bearer(tok)); r.status != 403 {
		t.Fatalf("machine token on a session route: %d", r.status)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM events WHERE event_type = 'auth_refused' AND actor_id = $1", id); n != 1 {
		t.Fatalf("refusal of %s audited %d times", id, n)
	}
	if r := s.token(id, "wrong"); r.status != 401 || events(t, "oauth_client", id, accounts.EventTokenRefused) != 1 {
		t.Fatalf("token refusal: %d", r.status)
	}
}

// E-10: expired sessions and untouched lockout rows beyond the
// retention are swept; recent ones stay.
func TestIntegrationSweepBoundsSessionsAndLockouts(t *testing.T) {
	c := newClock()
	s := newStack(t, c, &logBuffer{})
	owner := relOwner(t)
	u := unique()
	old := c.Now().Add(-48 * time.Hour)
	for i, at := range []time.Time{old, c.Now()} {
		jti := fmt.Sprintf("sweep-%s-%d", u, i)
		if _, err := owner.Exec(context.Background(), `INSERT INTO sessions (jti, realm, account_id, roles, issued_at, expires_at, last_seen_at)
			VALUES ($1, 'portal', gen_random_uuid(), '{viewer}', $2, $2, $2)`, jti, at); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(context.Background(), `INSERT INTO login_lockouts (realm, username, failures, updated_at) VALUES ('portal', $1, 3, $2)`,
			jti, at); err != nil {
			t.Fatal(err)
		}
	}
	sessions, lockouts, err := s.svc.Sweep(context.Background())
	if err != nil || sessions < 1 || lockouts < 1 {
		t.Fatalf("swept %d sessions %d lockouts: %v", sessions, lockouts, err)
	}
	if n := count(t, owner, "SELECT count(*) FROM sessions WHERE jti LIKE $1", "sweep-"+u+"-%"); n != 1 {
		t.Fatalf("%d sessions left, want the recent one", n)
	}
	if n := count(t, owner, "SELECT count(*) FROM login_lockouts WHERE username LIKE $1", "sweep-"+u+"-%"); n != 1 {
		t.Fatalf("%d lockouts left, want the recent one", n)
	}
}

// Brief WP-2: no secret in any log line. Every flow of the suite runs
// with the log captured, the failing ones included; neither a password,
// a client secret, a token nor a cookie value appears in it.
func TestIntegrationNoSecretInTheLog(t *testing.T) {
	logs := &logBuffer{}
	s := newStack(t, newClock(), logs)
	opID, session, pass := s.operator(accounts.RegistryValid)
	id, secret := s.client(opID, session, auth.ScopeGeo)
	tok := s.token(id, secret).str("access_token")
	s.token(id, secret+"x")
	s.login(auth.RealmPortal, "nobody", pass, "")
	s.call("GET", "/v1/accounts/me", nil, bearer(tok))
	s.call("GET", "/v1/accounts/me", nil, bearer(session+"x"))
	s.bindings.Fail = errors.New("kv down")
	s.call("POST", "/v1/accounts/operators/"+opID+"/clients/"+id+"/serials", map[string]any{"serial": "TEST" + unique()}, bearer(session))
	logged := logs.String()
	if !strings.Contains(logged, `"status":401`) || !strings.Contains(logged, `"status":503`) {
		t.Fatalf("the refusals were not logged (the test proves nothing):\n%s", logged)
	}
	for name, v := range map[string]string{"password": pass, "client secret": secret, "token": tok, "session": session} {
		if strings.Contains(logged, v) {
			t.Fatalf("the log holds the %s", name)
		}
	}
}

// The api process itself, with its issuer key and the fake authority:
// /readyz says issuer and jwks up; an operator registers (pending: the
// fake registry does not hold its number), signs in and reads /me through
// the generated client; when the authority's JWKS goes away, jwks is
// degraded with the age of the cached keys (E-02).
func TestIntegrationAPIProcessAuth(t *testing.T) {
	ensureSchemas(t)
	a := newFakeAuthority(t)
	c := run(t, api.Spec, withAuth(t, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
	}, a))
	code, body := readyz(t, c, client.ReadinessStatusReady)
	if code != 200 || len(body.Degraded) != 0 {
		t.Fatalf("readyz %d %+v", code, body)
	}
	ctx := context.Background()
	u := unique()
	reg, err := c.RegisterOperatorWithResponse(ctx, client.OperatorRegistration{RegistrationNumber: "GEO-TEST-" + u, DisplayName: "P",
		ContactEmail: "p" + u + "@example.test", AdminUsername: "proc." + u, AdminPassword: "proc-password-" + u})
	if err != nil || reg.JSON201 == nil || reg.JSON201.Status != client.PendingValidation {
		t.Fatalf("register: %v %s", err, reg.Body)
	}
	login, err := c.LoginWithResponse(ctx, client.LoginRequest{Realm: client.Portal, Username: "proc." + u, Password: "proc-password-" + u})
	if err != nil || login.JSON200 == nil {
		t.Fatalf("login: %v %s", err, login.Body)
	}
	me, err := c.GetMeWithResponse(ctx, func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+login.JSON200.Token)
		return nil
	})
	if err != nil || me.JSON200 == nil || me.JSON200.Username != "proc."+u || me.JSON200.OperatorId == nil || *me.JSON200.OperatorId != reg.JSON201.Id {
		t.Fatalf("me: %v %s", err, me.Body)
	}
	jwks, err := c.GetJWKSWithResponse(ctx)
	if err != nil || jwks.JSON200 == nil || len(jwks.JSON200.Keys) != 1 {
		t.Fatalf("jwks: %v %s", err, jwks.Body)
	}

	a.down.Store(true)
	code, body = readyz(t, c, client.ReadinessStatusDegraded)
	d := body.Dependencies["jwks"]
	if code != 200 || d.State != client.DependencyStateDegraded || d.Detail == nil || !strings.Contains(*d.Detail, "cached, age") {
		t.Fatalf("authority down: %d %+v", code, d)
	}
}

// sessionOf reads a session token's claims (signature unchecked; the
// stack issued it).
func sessionOf(t *testing.T, token string) coreauth.Claims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Sub string `json:"sub"`
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return coreauth.Claims{Subject: c.Sub, JTI: c.JTI}
}

// sessions_live follows api's session rows (audit B2): a sign-in
// projects the session, so traffic-ws's LiveSessions admits it; a
// logout and an idle end remove it, so traffic-ws refuses it; a sign-in
// whose projection fails is refused (503) and starts nothing (B-09).
func TestIntegrationSessionsLiveFollowsTheRows(t *testing.T) {
	c := newClock()
	s := newStack(t, c, &logBuffer{})
	live := auth.LiveSessions{Get: s.sessions.Get, Now: c.Now}
	ctx := context.Background()
	_, token, _ := s.operator(accounts.RegistryValid)
	sc := sessionOf(t, token)
	if err := live.CheckSession(ctx, sc.JTI, sc.Subject); err != nil {
		t.Fatalf("a session just started is not live on traffic-ws: %v", err)
	}
	if r := s.call("POST", "/v1/accounts/logout", nil, bearer(token)); r.status != 204 {
		t.Fatalf("logout: %d %s", r.status, r.raw)
	}
	if err := live.CheckSession(ctx, sc.JTI, sc.Subject); !errors.Is(err, auth.ErrSessionRefused) {
		t.Fatalf("a signed-out session is live on traffic-ws: %v", err)
	}

	// Idle: api ends the session at its next use; traffic-ws has
	// already passed its idle end and refuses it too.
	_, token, _ = s.operator(accounts.RegistryValid)
	sc = sessionOf(t, token)
	c.Add(31 * time.Minute)
	if err := live.CheckSession(ctx, sc.JTI, sc.Subject); !errors.Is(err, auth.ErrSessionRefused) {
		t.Fatalf("an idle session is live on traffic-ws: %v", err)
	}
	if r := s.call("GET", "/v1/accounts/me", nil, bearer(token)); r.status != 401 {
		t.Fatalf("idle in api: %d", r.status)
	}
	if _, found, _ := s.sessions.Get(sc.JTI); found {
		t.Fatal("an idle-ended session is still in sessions_live")
	}
	c.Add(-31 * time.Minute)

	// A use moves the idle end on traffic-ws as in api.
	_, token, _ = s.operator(accounts.RegistryValid)
	sc = sessionOf(t, token)
	c.Add(20 * time.Minute)
	if r := s.call("GET", "/v1/accounts/me", nil, bearer(token)); r.status != 200 {
		t.Fatalf("me: %d", r.status)
	}
	c.Add(20 * time.Minute)
	if err := live.CheckSession(ctx, sc.JTI, sc.Subject); err != nil {
		t.Fatalf("a session used 20 min ago is not live on traffic-ws: %v", err)
	}
	c.Add(-40 * time.Minute)

	// B-09: the projection fails, the sign-in is refused and no session
	// row exists for it.
	u := unique()
	s.call("POST", "/v1/accounts/operators", map[string]any{"registration_number": "GEO-TEST-" + u, "display_name": "x",
		"contact_email": "c" + u + "@example.test", "admin_username": "proj." + u, "admin_password": "proj-password-" + u}, nil)
	before := s.sessions.Len()
	s.sessions.Fail = errors.New("sessions_live unavailable")
	r := s.login(auth.RealmPortal, "proj."+u, "proj-password-"+u, "")
	s.sessions.Fail = nil
	if r.status != 503 || s.sessions.Len() != before {
		t.Fatalf("sign-in with sessions_live down: %d %s", r.status, r.raw)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM sessions s JOIN portal_users u ON u.id = s.account_id WHERE u.username = 'proj."+u+"'"); n != 0 {
		t.Fatalf("%d session rows for a refused sign-in", n)
	}
	if r := s.login(auth.RealmPortal, "proj."+u, "proj-password-"+u, ""); r.status != 200 {
		t.Fatalf("sign-in with sessions_live back: %d %s", r.status, r.raw)
	}
}
