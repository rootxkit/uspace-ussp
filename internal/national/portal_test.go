package national

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

const (
	portalOp    = "7d1e3c5a-2b4f-4a6e-8c9d-0e1f2a3b4c5d"
	portalAcct  = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	portalIntID = "5f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c"
)

// members is a PortalMembers with one portal user and one binding
// (TEST0001 to op-client-1).
type members struct {
	role string
	err  error
}

func (m members) PortalMember(_ context.Context, p auth.Principal) (accounts.Member, error) {
	if m.err != nil {
		return accounts.Member{}, m.err
	}
	return accounts.Member{AccountID: p.Claims.Subject, OperatorID: portalOp, Role: m.role}, nil
}

func (members) BoundClient(_ context.Context, operatorID, sn string) (string, error) {
	if operatorID == portalOp && sn == "TEST0001" {
		return "op-client-1", nil
	}
	return "", &accounts.Error{Status: 403, Slug: accounts.SlugSerialNotBound, Detail: "not bound"}
}

// intentsFake records which client each call acted as, and the portal
// user on its context.
type intentsFake struct {
	calls []string
}

func (f *intentsFake) note(ctx context.Context, op, client string) {
	f.calls = append(f.calls, op+"|"+client+"|"+intent.PortalUserFrom(ctx))
}

func (f *intentsFake) Submit(ctx context.Context, clientID string, _ []byte) (intent.Decision, bool, error) {
	f.note(ctx, "submit", clientID)
	return intent.Decision{IntentID: portalIntID}, true, nil
}

func (f *intentsFake) Get(ctx context.Context, clientID, id string) (intent.Decision, error) {
	f.note(ctx, "get", clientID)
	return intent.Decision{IntentID: id}, nil
}

func (f *intentsFake) List(ctx context.Context, clientID string, _ intent.ListFilter) ([]intent.Decision, error) {
	f.note(ctx, "list", clientID)
	return nil, nil
}

func (f *intentsFake) Change(ctx context.Context, clientID, id string, _ []byte) (intent.Decision, error) {
	f.note(ctx, "change", clientID)
	return intent.Decision{IntentID: id}, nil
}

func (f *intentsFake) ClientOf(_ context.Context, operatorID, id string) (string, error) {
	if operatorID == portalOp && id == portalIntID {
		return "op-client-1", nil
	}
	return "", &intent.Error{Status: 404, Slug: "not_found", Detail: "no such intent"}
}

func (f *intentsFake) ListOperator(ctx context.Context, operatorID string, _ intent.ListFilter) ([]intent.Decision, error) {
	f.note(ctx, "list-operator", operatorID)
	return []intent.Decision{{IntentID: portalIntID}}, nil
}

func portalCtx(role string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Session: true, ViaCookie: true,
		Claims: coreauth.Claims{Subject: portalAcct, Realm: auth.RealmPortal, Roles: []string{role}, Scopes: []string{auth.SessionScope}}})
}

func requestBody(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../schemas/intent/request/v1/examples/open-a1-c0.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func slugOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return p.Type[strings.LastIndex(p.Type, "/")+1:]
}

// A portal session files under the client its serial is bound to, with
// the portal user on the context for the audit row; reads go through
// the intent's own client; a viewer reads and is refused every write; a
// serial bound to no client of the operator is 403 serial_not_bound;
// another operator's intent is 404 (E-01 pairs).
func TestPortalIntents(t *testing.T) {
	var id gen.IntentID
	if err := id.UnmarshalText([]byte(portalIntID)); err != nil {
		t.Fatal(err)
	}
	actor := auth.ActorPortalUser + ":" + portalAcct
	post := func(s *Server, role string, body []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/intents", bytes.NewReader(body)).WithContext(portalCtx(role))
		r.Header.Set("Content-Type", "application/json")
		s.CreateIntent(rec, r)
		return rec
	}
	f := &intentsFake{}
	s := &Server{Intents: f, Portal: members{role: auth.RoleRemotePilot}}
	if rec := post(s, auth.RoleRemotePilot, requestBody(t)); rec.Code != 201 || f.calls[0] != "submit|op-client-1|"+actor {
		t.Fatalf("pilot files: %d %s %v", rec.Code, rec.Body, f.calls)
	}
	other := bytes.Replace(requestBody(t), []byte(`"TEST0001"`), []byte(`"TEST9999"`), 1)
	if rec := post(s, auth.RoleRemotePilot, other); rec.Code != 403 || slugOf(t, rec) != accounts.SlugSerialNotBound || len(f.calls) != 1 {
		t.Fatalf("unbound serial: %d %s %v", rec.Code, rec.Body, f.calls)
	}
	if rec := post(s, auth.RoleRemotePilot, []byte(`{"nope":1}`)); rec.Code != 400 || len(f.calls) != 1 {
		t.Fatalf("malformed body: %d %s", rec.Code, rec.Body)
	}
	viewer := &Server{Intents: f, Portal: members{role: auth.RoleViewer}}
	if rec := post(viewer, auth.RoleViewer, requestBody(t)); rec.Code != 403 || slugOf(t, rec) != SlugReadOnly || len(f.calls) != 1 {
		t.Fatalf("viewer files: %d %s", rec.Code, rec.Body)
	}
	if rec := post(&Server{Intents: f}, auth.RoleRemotePilot, requestBody(t)); rec.Code != 503 || slugOf(t, rec) != SlugPortalUnavailable {
		t.Fatalf("no portal service: %d %s", rec.Code, rec.Body)
	}

	rec := httptest.NewRecorder()
	viewer.GetIntent(rec, httptest.NewRequest(http.MethodGet, "/v1/intents/"+portalIntID, nil).WithContext(portalCtx(auth.RoleViewer)), id)
	if rec.Code != 200 || f.calls[len(f.calls)-1] != "get|op-client-1|"+actor {
		t.Fatalf("viewer reads: %d %v", rec.Code, f.calls)
	}
	var foreign gen.IntentID
	_ = foreign.UnmarshalText([]byte("9f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c"))
	rec = httptest.NewRecorder()
	viewer.GetIntent(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(portalCtx(auth.RoleViewer)), foreign)
	if rec.Code != 404 {
		t.Fatalf("another operator's intent: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	viewer.ListIntents(rec, httptest.NewRequest(http.MethodGet, "/v1/intents", nil).WithContext(portalCtx(auth.RoleViewer)), gen.ListIntentsParams{})
	if rec.Code != 200 || f.calls[len(f.calls)-1] != "list-operator|"+portalOp+"|" || !strings.Contains(rec.Body.String(), portalIntID) {
		t.Fatalf("list: %d %s %v", rec.Code, rec.Body, f.calls)
	}

	patch := func(s *Server, role string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"action":"end"}`)).WithContext(portalCtx(role))
		r.Header.Set("Content-Type", "application/json")
		s.ChangeIntent(rec, r, id)
		return rec
	}
	n := len(f.calls)
	if rec := patch(viewer, auth.RoleViewer); rec.Code != 403 || len(f.calls) != n {
		t.Fatalf("viewer changes: %d", rec.Code)
	}
	admin := &Server{Intents: f, Portal: members{role: auth.RoleOperatorAdmin}}
	if rec := patch(admin, auth.RoleOperatorAdmin); rec.Code != 200 || f.calls[len(f.calls)-1] != "change|op-client-1|"+actor {
		t.Fatalf("admin changes: %d %v", rec.Code, f.calls)
	}

	// An operator token is unchanged: its own subject is the client.
	tok := auth.WithPrincipal(context.Background(), auth.Principal{Claims: coreauth.Claims{Subject: "op-client-9", Scopes: []string{auth.ScopeIntents}}})
	rec = httptest.NewRecorder()
	admin.GetIntent(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(tok), id)
	if rec.Code != 200 || f.calls[len(f.calls)-1] != "get|op-client-9|" {
		t.Fatalf("token: %d %v", rec.Code, f.calls)
	}
}

func (a *acker) AckForOperator(_ context.Context, alertID, operatorID, actor string) (alerts.AckResult, error) {
	a.seen = append(a.seen, alertID+"|"+operatorID+"|"+actor)
	if a.err != nil {
		return alerts.AckResult{}, a.err
	}
	return alerts.AckResult{AlertID: alertID, AckedAt: time.Date(2026, 10, 3, 12, 0, 10, 0, time.UTC), AckedBy: actor}, nil
}

// A portal operator_admin or remote_pilot acknowledges as
// operator_user:<account>; a viewer is 403 and nothing is recorded.
func TestPortalAckAlert(t *testing.T) {
	var id gen.AlertID
	if err := id.UnmarshalText([]byte(portalIntID)); err != nil {
		t.Fatal(err)
	}
	call := func(s *Server, role string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.AckAlert(rec, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(portalCtx(role)), id)
		return rec
	}
	a := &acker{}
	rec := call(&Server{Alerts: a, Portal: members{role: auth.RoleRemotePilot}}, auth.RoleRemotePilot)
	want := portalIntID + "|" + portalOp + "|" + auth.ActorPortalUser + ":" + portalAcct
	if rec.Code != 200 || len(a.seen) != 1 || a.seen[0] != want || !strings.Contains(rec.Body.String(), auth.ActorPortalUser+":"+portalAcct) {
		t.Fatalf("pilot acks: %d %s %v", rec.Code, rec.Body, a.seen)
	}
	if rec := call(&Server{Alerts: a, Portal: members{role: auth.RoleViewer}}, auth.RoleViewer); rec.Code != 403 || len(a.seen) != 1 {
		t.Fatalf("viewer acks: %d %v", rec.Code, a.seen)
	}
	if rec := call(&Server{Alerts: &acker{err: alerts.ErrNotFound}, Portal: members{role: auth.RoleOperatorAdmin}}, auth.RoleOperatorAdmin); rec.Code != 404 {
		t.Fatalf("another operator's alert: %d", rec.Code)
	}
	if rec := call(&Server{Alerts: a, Portal: members{err: &accounts.Error{Status: 403, Slug: "forbidden"}}}, auth.RoleOperatorAdmin); rec.Code != 403 {
		t.Fatalf("inactive user: %d", rec.Code)
	}
}
