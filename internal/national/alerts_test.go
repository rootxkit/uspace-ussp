package national

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

type acker struct {
	err  error
	seen []string
}

func (a *acker) Ack(_ context.Context, alertID, clientID string) (alerts.AckResult, error) {
	a.seen = append(a.seen, alertID+"|"+clientID)
	if a.err != nil {
		return alerts.AckResult{}, a.err
	}
	return alerts.AckResult{AlertID: alertID, AckedAt: time.Date(2026, 10, 3, 12, 0, 10, 0, time.UTC), AckedBy: clientID}, nil
}

// POST /v1/alerts/{alert_id}/ack: the caller's client acknowledges;
// another operator's alert is 404 (never 403); no service is 503; a
// store failure is a problem, never a 200.
func TestAckAlert(t *testing.T) {
	var id gen.AlertID
	if err := id.UnmarshalText([]byte("5f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c")); err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Claims: coreauth.Claims{Subject: "client-a", Scopes: []string{auth.ScopeTraffic}}})
	call := func(s *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.AckAlert(rec, httptest.NewRequest(http.MethodPost, "/v1/alerts/"+id.String()+"/ack", nil).WithContext(ctx), id)
		return rec
	}
	a := &acker{}
	rec := call(&Server{Alerts: a})
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["acked_by"] != "client-a" || body["alert_id"] != id.String() || a.seen[0] != id.String()+"|client-a" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := call(&Server{Alerts: &acker{err: alerts.ErrNotFound}}); rec.Code != 404 {
		t.Fatalf("not found: %d", rec.Code)
	}
	if rec := call(&Server{}); rec.Code != 503 {
		t.Fatalf("no service: %d", rec.Code)
	}
	if rec := call(&Server{Alerts: &acker{err: errors.New("db down")}}); rec.Code < 500 {
		t.Fatalf("store failure: %d", rec.Code)
	}
}
