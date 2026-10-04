package national

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/coordination"
)

type lister struct {
	items     []coordination.Item
	truncated bool
	err       error
}

func (l lister) Open(context.Context, int) ([]coordination.Item, bool, error) {
	return l.items, l.truncated, l.err
}

// GET /v1/admin/coordination: the open notices with their state and
// age; no lister or one that fails is 503, never an empty list (E-01
// pair with the 200 below).
func TestListCoordinationNotices(t *testing.T) {
	call := func(s *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.ListCoordinationNotices(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/coordination", nil))
		return rec
	}
	flight, errText := "0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d", "the ANSP answered 503"
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	l := lister{truncated: true, items: []coordination.Item{
		{ID: 1, NoticeRef: "USSP-DEV:i:nonconformance:5", Kind: "nonconformance", IntentID: "6f1c0d2e-3b4a-4c5d-8e6f-7a8b9c0d1e2f", FlightID: &flight,
			State: "pending", CreatedAt: now, AgeS: 12.5, Attempts: 3, LastError: &errText, NextAt: &now},
		{ID: 2, NoticeRef: "USSP-DEV:i:intent_notice", Kind: "intent_notice", IntentID: "6f1c0d2e-3b4a-4c5d-8e6f-7a8b9c0d1e2f", State: "failed", CreatedAt: now, FailedAt: &now},
	}}
	rec := call(&Server{Coordination: l})
	var body struct {
		Notices []map[string]any `json:"notices"`
		Trunc   bool             `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 || len(body.Notices) != 2 || !body.Trunc ||
		body.Notices[0]["flight_id"] != flight || body.Notices[0]["last_error"] != errText || body.Notices[0]["age_s"] != 12.5 ||
		body.Notices[1]["flight_id"] != nil || body.Notices[1]["state"] != "failed" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := call(&Server{}); rec.Code != 503 {
		t.Fatalf("no lister: %d", rec.Code)
	}
	if rec := call(&Server{Coordination: lister{err: errors.New("db down")}}); rec.Code != 503 {
		t.Fatalf("lister down: %d", rec.Code)
	}
	bad := lister{items: []coordination.Item{{IntentID: "not-a-uuid"}}}
	if rec := call(&Server{Coordination: bad}); rec.Code < 500 {
		t.Fatalf("malformed row answered %d", rec.Code)
	}
}
