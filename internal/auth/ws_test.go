package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const consoleOrigin = "https://ussp.test"

// wsServer upgrades through WSAuth with the admin access and, once in,
// sends "hello" and closes normally.
func wsServer(t *testing.T, f *guardFixture) *httptest.Server {
	t.Helper()
	ws := &WSAuth{Guard: f.guard, AllowedOrigins: []string{consoleOrigin}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, p, err := ws.AcceptWS(w, r, adminAccess)
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte("hello "+p.Claims.Realm))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dial opens the socket and reads one message; it returns the message,
// the handshake status and the close code (-1 when none). A handshake
// that is not upgraded returns its status and close code -1.
func dial(t *testing.T, url string, h http.Header) (string, int, websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp == nil {
			t.Fatalf("dial: %v", err)
		}
		return "", resp.StatusCode, -1
	}
	defer func() { _ = conn.CloseNow() }()
	_, msg, err := conn.Read(ctx)
	if err != nil {
		return "", resp.StatusCode, websocket.CloseStatus(err)
	}
	return string(msg), resp.StatusCode, -1
}

// E-01 (M22, conformance C4 and C8): the cookie on an upgrade from an
// allowed Origin is 101 and served, and so is a machine client's bearer
// without an Origin. Every other refusal is answered before any upgrade
// with its status, never 101: no credential (with or without an allowed
// Origin) and a refused bearer 401, another Origin or a cookie without
// one 403. Only a browser's session from an allowed Origin that the
// guard refuses is upgraded and closed with 4401, which a browser can
// read.
func TestWSUpgradePairs(t *testing.T) {
	f := newGuardFixture(t)
	srv := wsServer(t, f)
	sess := f.session(t, RealmConsole, RoleSupervisor, "s-ws")
	cookie := CookieSession + "=" + sess

	msg, status, code := dial(t, srv.URL, http.Header{"Cookie": {cookie}, "Origin": {consoleOrigin}})
	if status != http.StatusSwitchingProtocols || msg != "hello console" || code != -1 {
		t.Fatalf("cookie, allowed origin: %d %q %d", status, msg, code)
	}
	for name, c := range map[string]struct {
		h      http.Header
		status int
	}{
		"nothing":         {http.Header{}, http.StatusUnauthorized},
		"no cookie":       {http.Header{"Origin": {consoleOrigin}}, http.StatusUnauthorized},
		"bad bearer":      {http.Header{"Authorization": {"Bearer not-a-token"}}, http.StatusUnauthorized},
		"another origin":  {http.Header{"Cookie": {cookie}, "Origin": {"https://evil.test"}}, http.StatusForbidden},
		"no origin":       {http.Header{"Cookie": {cookie}}, http.StatusForbidden},
		"bearer, browser": {http.Header{"Authorization": {"Bearer " + sess}, "Origin": {"https://evil.test"}}, http.StatusForbidden},
	} {
		_, status, code := dial(t, srv.URL, c.h)
		if status != c.status || code != -1 {
			t.Errorf("%s: status %d close %d, want %d before any upgrade", name, status, code, c.status)
		}
	}
	_, status, code = dial(t, srv.URL, http.Header{"Cookie": {CookieSession + "=" + f.session(t, RealmPortal, RoleViewer, "s-p")}, "Origin": {consoleOrigin}})
	if status != http.StatusSwitchingProtocols || code != CloseRelogin {
		t.Errorf("portal session from an allowed origin: status %d close %d, want 101 and 4401", status, code)
	}
	if f.counters.Get(CounterWSRefused) != 7 || f.counters.Get(CounterWSAccepted) != 1 || f.counters.Get(CounterWSOriginRefused) != 3 {
		t.Fatalf("counters %v", f.counters.Snapshot())
	}
	if r := f.audit.last(t); r.Status == 0 || r.Reason == "" {
		t.Fatalf("refusal not audited: %+v", r)
	}
	// A machine client with a bearer and no browser Origin is served.
	msg, _, code = dial(t, srv.URL, http.Header{"Authorization": {"Bearer " + sess}})
	if msg != "hello console" || code != -1 {
		t.Fatalf("bearer: %q %d", msg, code)
	}
}

// Require judges the upgrade before the handler behind it runs (C6):
// without a credential the handler is never reached and the answer is
// 401; with one the handler gets the caller.
func TestWSRequireRunsBeforeTheHandler(t *testing.T) {
	f := newGuardFixture(t)
	ws := &WSAuth{Guard: f.guard, AllowedOrigins: []string{consoleOrigin}}
	var reached int
	h := ws.Require(adminAccess)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		if _, ok := PrincipalFrom(r.Context()); !ok {
			t.Error("no principal behind Require")
		}
		w.WriteHeader(http.StatusBadRequest) // the parameters a router would refuse
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/alerts", nil))
	if rec.Code != http.StatusUnauthorized || reached != 0 {
		t.Fatalf("no credential: %d, handler reached %d times", rec.Code, reached)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/alerts", nil)
	r.Header.Set("Authorization", "Bearer "+f.session(t, RealmConsole, RoleSupervisor, "s-req"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest || reached != 1 {
		t.Fatalf("credential: %d, handler reached %d times", rec.Code, reached)
	}
}

// A request that is not an upgrade, with a valid credential, gets the
// library's refusal and no connection; without one it is 401.
func TestWSNotAnUpgrade(t *testing.T) {
	f := newGuardFixture(t)
	ws := &WSAuth{Guard: f.guard, AllowedOrigins: []string{consoleOrigin}}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/traffic", nil)
	r.Header.Set("Authorization", "Bearer "+f.session(t, RealmConsole, RoleSupervisor, "s-plain"))
	conn, _, err := ws.AcceptWS(rec, r, adminAccess)
	if conn != nil || err == nil || errors.Is(err, ErrWSRefused) {
		t.Fatalf("conn %v err %v", conn, err)
	}
	rec = httptest.NewRecorder()
	conn, _, err = ws.AcceptWS(rec, httptest.NewRequest(http.MethodGet, "/v1/traffic", nil), adminAccess)
	if conn != nil || !errors.Is(err, ErrWSRefused) || rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d conn %v err %v", rec.Code, conn, err)
	}
}

// E-01 pair (B-10): an authenticated caller Admit turns away gets Admit's
// answer (503 with Retry-After) and no socket; the same caller admitted
// is upgraded and served.
func TestWSAdmitAnswersBeforeTheUpgrade(t *testing.T) {
	f := newGuardFixture(t)
	var admit atomic.Bool
	ws := &WSAuth{Guard: f.guard, AllowedOrigins: []string{consoleOrigin}, Admit: func(w http.ResponseWriter, _ *http.Request, p Principal) bool {
		if admit.Load() {
			return true
		}
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		return p.Claims.Subject == "never"
	}}
	errs := make(chan error, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := ws.AcceptWS(w, r, adminAccess)
		errs <- err
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte("in"))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	sess := f.session(t, RealmConsole, RoleSupervisor, "s-admit")
	h := http.Header{"Authorization": {"Bearer " + sess}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{HTTPHeader: h})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("not admitted: %v %+v", err, resp)
	}
	if err := <-errs; !errors.Is(err, ErrWSNotAdmitted) || f.counters.Get(CounterWSAccepted) != 0 {
		t.Fatalf("err %v counters %v", err, f.counters.Snapshot())
	}
	admit.Store(true)
	if msg, status, code := dial(t, srv.URL, h); msg != "in" || status != http.StatusSwitchingProtocols || code != -1 {
		t.Fatalf("admitted: %q %d %d", msg, status, code)
	}
}
