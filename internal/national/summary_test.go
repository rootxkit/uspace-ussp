package national

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/intent"
)

func fakeSummary() intent.CountSummary {
	counts := map[string]int{}
	for _, st := range intent.States {
		counts[st] = 0
	}
	counts[intent.StateActivated], counts[intent.StateEnded] = 2, 1
	return intent.CountSummary{AsOf: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC), Counts: counts, Total: 3}
}

// summaryRouter is the guarded router with the intents fake and a
// portal member of role.
func summaryRouter(t *testing.T, f *intentsFake, role string) (http.Handler, *auth.Issuer) {
	t.Helper()
	return routerWith(t, nil, func(s *Server) {
		s.Intents = f
		s.Portal = members{role: role}
	})
}

func get(h http.Handler, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// GET /v1/intents/summary: a portal session of any role gets its own
// operator's counts (the operator is the session's), every state in
// the body and nothing else; without a credential it is 401; an
// operator token and a console session are 403 and the service is
// never asked (E-01 pairs).
func TestIntentSummaryIsThePortalSessionsOwn(t *testing.T) {
	for _, role := range []string{auth.RoleViewer, auth.RoleRemotePilot, auth.RoleOperatorAdmin} {
		f := &intentsFake{}
		h, iss := summaryRouter(t, f, role)
		session, _, err := iss.IssueSession(portalAcct, auth.RealmPortal, "s-sum", []string{role}, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		rec := get(h, "/v1/intents/summary", session)
		if rec.Code != 200 || len(f.calls) != 1 || f.calls[0] != "summary-operator|"+portalOp+"|" {
			t.Fatalf("%s: %d %s %v", role, rec.Code, rec.Body, f.calls)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 4 || string(body["total"]) != "3" || string(body["truncated"]) != "false" || string(body["as_of"]) != `"2026-10-15T12:00:00Z"` {
			t.Fatalf("%s: body %s", role, rec.Body)
		}
		var counts map[string]int
		if err := json.Unmarshal(body["counts"], &counts); err != nil || len(counts) != len(intent.States) || counts[intent.StateActivated] != 2 {
			t.Fatalf("%s: counts %s %v", role, body["counts"], err)
		}
	}

	f := &intentsFake{}
	h, iss := summaryRouter(t, f, auth.RoleViewer)
	if rec := get(h, "/v1/intents/summary", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", rec.Code)
	}
	op, err := iss.IssueOperator("op-client-1", []string{auth.ScopeIntents, auth.ScopeGeo, auth.ScopeTraffic}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(h, "/v1/intents/summary", op.Token); rec.Code != http.StatusForbidden {
		t.Fatalf("operator token: %d %s", rec.Code, rec.Body)
	}
	for _, role := range []string{auth.RoleSupervisor, auth.RoleSupport, auth.RoleAdmin} {
		console, _, err := iss.IssueSession("staff-1", auth.RealmConsole, "s-con", []string{role}, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if rec := get(h, "/v1/intents/summary", console); rec.Code != http.StatusForbidden {
			t.Fatalf("console %s: %d %s", role, rec.Code, rec.Body)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused request reached the service: %v", f.calls)
	}
}

// The handler itself refuses what the access table already keeps out
// (defence in depth): a request that is not a portal session is 403; no
// intent service is 503; a member that cannot be resolved is its
// problem; the portal session's call is answered (E-01 pair).
func TestIntentSummaryHandlerFailsClosed(t *testing.T) {
	f := &intentsFake{}
	s := &Server{Intents: f, Portal: members{role: auth.RoleViewer}}
	call := func(s *Server, ctx func() *http.Request) int {
		rec := httptest.NewRecorder()
		s.GetIntentSummary(rec, ctx())
		return rec.Code
	}
	portal := func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/v1/intents/summary", nil).WithContext(portalCtx(auth.RoleViewer))
	}
	if c := call(s, portal); c != 200 {
		t.Fatalf("portal: %d", c)
	}
	if c := call(s, func() *http.Request { return httptest.NewRequest(http.MethodGet, "/v1/intents/summary", nil) }); c != 403 {
		t.Fatalf("no portal session: %d", c)
	}
	if c := call(&Server{Portal: members{role: auth.RoleViewer}}, portal); c != 503 {
		t.Fatalf("no intent service: %d", c)
	}
	if c := call(&Server{Intents: f}, portal); c != 503 {
		t.Fatalf("no portal members: %d", c)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls %v", f.calls)
	}
}

// GET /v1/admin/intents/summary: a console session of a supervisor,
// support or admin gets every operator's counts; without a credential
// it is 401; a portal session of any role and an operator token are
// 403 and the service is never asked (E-01 pairs).
func TestAdminIntentSummaryIsTheConsoles(t *testing.T) {
	f := &intentsFake{}
	h, iss := summaryRouter(t, f, auth.RoleOperatorAdmin)
	for _, role := range []string{auth.RoleSupervisor, auth.RoleSupport, auth.RoleAdmin} {
		console, _, err := iss.IssueSession("staff-"+role, auth.RealmConsole, "s-con", []string{role}, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		rec := get(h, "/v1/admin/intents/summary", console)
		var body struct {
			Counts map[string]int `json:"counts"`
			Total  int            `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != 200 || err != nil || body.Total != 3 || len(body.Counts) != len(intent.States) {
			t.Fatalf("%s: %d %s", role, rec.Code, rec.Body)
		}
	}
	if len(f.calls) != 3 || f.calls[0] != "summary-all||" {
		t.Fatalf("calls %v", f.calls)
	}
	f.calls = nil
	if rec := get(h, "/v1/admin/intents/summary", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", rec.Code)
	}
	for _, role := range []string{auth.RoleOperatorAdmin, auth.RoleRemotePilot, auth.RoleViewer} {
		portal, _, err := iss.IssueSession(portalAcct, auth.RealmPortal, "s-por", []string{role}, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if rec := get(h, "/v1/admin/intents/summary", portal); rec.Code != http.StatusForbidden {
			t.Fatalf("portal %s: %d %s", role, rec.Code, rec.Body)
		}
	}
	op, err := iss.IssueOperator("op-client-1", []string{auth.ScopeIntents, auth.ScopeGeo, auth.ScopeTraffic}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(h, "/v1/admin/intents/summary", op.Token); rec.Code != http.StatusForbidden {
		t.Fatalf("operator token: %d %s", rec.Code, rec.Body)
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused request reached the service: %v", f.calls)
	}
}

// The console handler refuses a request that is not a console session
// and answers 503 without the intent service; a console session is
// answered (E-01 pair).
func TestAdminIntentSummaryHandlerFailsClosed(t *testing.T) {
	f := &intentsFake{}
	call := func(s *Server, ctx context.Context) int {
		rec := httptest.NewRecorder()
		s.GetAdminIntentSummary(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/intents/summary", nil).WithContext(ctx))
		return rec.Code
	}
	console := auth.WithPrincipal(context.Background(), auth.Principal{Session: true, ViaCookie: true,
		Claims: coreauth.Claims{Subject: "staff-1", Realm: auth.RealmConsole, Roles: []string{auth.RoleSupport}, Scopes: []string{auth.SessionScope}}})
	if c := call(&Server{Intents: f}, console); c != 200 {
		t.Fatalf("console: %d", c)
	}
	if c := call(&Server{Intents: f}, portalCtx(auth.RoleOperatorAdmin)); c != 403 {
		t.Fatalf("portal session: %d", c)
	}
	if c := call(&Server{Intents: f}, context.Background()); c != 403 {
		t.Fatalf("no session: %d", c)
	}
	if c := call(&Server{}, console); c != 503 {
		t.Fatalf("no intent service: %d", c)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls %v", f.calls)
	}
}
