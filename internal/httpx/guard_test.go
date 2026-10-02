package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// refuseAll refuses every request with 401 so a test can tell a guarded
// route from a public one.
func refuseAll(Access) func(http.Handler) http.Handler {
	return func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}
}

func ok204(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func status(h http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

// The route with an entry is served behind its guard, the public one
// without; a route without an entry is not served at all and is an
// error, as is an entry no route uses (fail closed both ways).
func TestGuardedMuxFailsClosed(t *testing.T) {
	table := map[string]Access{
		"GET /open":      {Public: true},
		"GET /closed":    {Scopes: []string{"ussp.geo"}},
		"GET /forgotten": {Scopes: []string{"ussp.geo"}},
	}
	g := NewGuardedMux(http.NewServeMux(), table, refuseAll, nil)
	g.HandleFunc("GET /open", ok204)
	g.HandleFunc("GET /closed", ok204)
	g.HandleFunc("GET /unlisted", ok204)
	if status(g, http.MethodGet, "/open") != 204 {
		t.Fatal("public route not served")
	}
	if status(g, http.MethodGet, "/closed") != 401 {
		t.Fatal("restricted route served without its guard")
	}
	if status(g, http.MethodGet, "/unlisted") != 404 {
		t.Fatal("a route without an access entry was served")
	}
	err := g.Err()
	if err == nil || !strings.Contains(err.Error(), "GET /unlisted has no access entry") ||
		!strings.Contains(err.Error(), "GET /forgotten matches no route") {
		t.Fatalf("err %v", err)
	}
}

// The success branch: a complete table is no error (E-02).
func TestGuardedMuxAcceptsACompleteTable(t *testing.T) {
	g := NewGuardedMux(http.NewServeMux(), map[string]Access{"GET /a": {Public: true}, "POST /b": {Sessions: []SessionAccess{{Realm: "console"}}}}, refuseAll, nil)
	g.HandleFunc("GET /a", ok204)
	g.HandleFunc("POST /b", ok204)
	if err := g.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestGuardedMuxRefusesInvalidEntries(t *testing.T) {
	for name, a := range map[string]Access{
		"empty":               {},
		"both":                {Public: true, Scopes: []string{"ussp.geo"}},
		"empty scope":         {Scopes: []string{""}},
		"empty of all":        {AllScopes: []string{"ussp.geo", ""}},
		"public and all":      {Public: true, AllScopes: []string{"ussp.geo"}},
		"realmless entry":     {Sessions: []SessionAccess{{Roles: []string{"admin"}}}},
		"public websocket":    {Public: true, WebSocket: true},
		"websocket alone":     {WebSocket: true},
		"refused by validate": {Scopes: []string{"made.up"}},
	} {
		validate := func(a Access) error {
			if len(a.Scopes) == 1 && a.Scopes[0] == "made.up" {
				return errors.New("unknown scope made.up")
			}
			return nil
		}
		g := NewGuardedMux(http.NewServeMux(), map[string]Access{"GET /x": a}, refuseAll, validate)
		g.HandleFunc("GET /x", ok204)
		if g.Err() == nil {
			t.Errorf("%s: accepted", name)
		}
		if status(g, http.MethodGet, "/x") != 404 {
			t.Errorf("%s: served", name)
		}
	}
}

// An entry restricted only by AllScopes is valid (E-01 twin of "empty").
func TestAccessAllScopesIsRestricted(t *testing.T) {
	if err := (Access{AllScopes: []string{"a.b", "c.d"}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAccessString(t *testing.T) {
	for want, a := range map[string]Access{
		"public":                            {Public: true},
		"scope:ussp.geo or session:portal":  {Scopes: []string{"ussp.geo"}, Sessions: []SessionAccess{{Realm: "portal"}}},
		"session:console/supervisor|admin":  {Sessions: []SessionAccess{{Realm: "console", Roles: []string{"supervisor", "admin"}}}},
		"scopes:a.b+c.d":                    {AllScopes: []string{"a.b", "c.d"}},
		"websocket or scope:ussp.telemetry": {WebSocket: true, Scopes: []string{"ussp.telemetry"}},
	} {
		if got := a.String(); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

// E-01 pair: a WebSocket entry is served without the mux's guard (its
// upgrade authenticates itself, M22), a plain entry of the same scope
// behind it.
func TestGuardedMuxLeavesWebSocketsToTheirUpgrade(t *testing.T) {
	g := NewGuardedMux(http.NewServeMux(), map[string]Access{
		"GET /ws":    {WebSocket: true, Scopes: []string{"ussp.telemetry"}},
		"POST /data": {Scopes: []string{"ussp.telemetry"}},
	}, refuseAll, nil)
	g.HandleFunc("GET /ws", ok204)
	g.HandleFunc("POST /data", ok204)
	if err := g.Err(); err != nil {
		t.Fatal(err)
	}
	if status(g, http.MethodGet, "/ws") != 204 {
		t.Fatal("the WebSocket route was guarded by the mux")
	}
	if status(g, http.MethodPost, "/data") != 401 {
		t.Fatal("the plain route was served without its guard")
	}
}
