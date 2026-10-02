package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
// the handshake status and the close code (-1 when none).
func dial(t *testing.T, url string, h http.Header) (string, int, websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	_, msg, err := conn.Read(ctx)
	if err != nil {
		return "", resp.StatusCode, websocket.CloseStatus(err)
	}
	return string(msg), resp.StatusCode, -1
}

// E-01 (M22): the cookie on an upgrade from an allowed Origin is 101 and
// served; without the cookie, or from another origin, the socket is
// closed with 4401 (re-login).
func TestWSUpgradePairs(t *testing.T) {
	f := newGuardFixture(t)
	srv := wsServer(t, f)
	sess := f.session(t, RealmConsole, RoleSupervisor, "s-ws")
	cookie := CookieSession + "=" + sess

	msg, status, code := dial(t, srv.URL, http.Header{"Cookie": {cookie}, "Origin": {consoleOrigin}})
	if status != http.StatusSwitchingProtocols || msg != "hello console" || code != -1 {
		t.Fatalf("cookie, allowed origin: %d %q %d", status, msg, code)
	}
	for name, h := range map[string]http.Header{
		"no cookie":       {"Origin": {consoleOrigin}},
		"another origin":  {"Cookie": {cookie}, "Origin": {"https://evil.test"}},
		"no origin":       {"Cookie": {cookie}},
		"portal session":  {"Cookie": {CookieSession + "=" + f.session(t, RealmPortal, RoleViewer, "s-p")}, "Origin": {consoleOrigin}},
		"bearer, browser": {"Authorization": {"Bearer " + sess}, "Origin": {"https://evil.test"}},
	} {
		_, status, code := dial(t, srv.URL, h)
		if status != http.StatusSwitchingProtocols || code != CloseRelogin {
			t.Errorf("%s: status %d close %d", name, status, code)
		}
	}
	if f.counters.Get(CounterWSRefused) != 5 || f.counters.Get(CounterWSAccepted) != 1 || f.counters.Get(CounterWSOriginRefused) != 3 {
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

// A request that is not an upgrade gets the library's refusal and no
// connection.
func TestWSNotAnUpgrade(t *testing.T) {
	f := newGuardFixture(t)
	ws := &WSAuth{Guard: f.guard, AllowedOrigins: []string{consoleOrigin}}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/traffic", nil)
	conn, _, err := ws.AcceptWS(rec, r, adminAccess)
	if conn != nil || err == nil || errors.Is(err, ErrWSRefused) {
		t.Fatalf("conn %v err %v", conn, err)
	}
}
