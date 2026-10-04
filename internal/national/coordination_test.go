package national

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

type lister struct {
	out gen.CoordinationNotices
	err error
}

func (l lister) Open(context.Context) (gen.CoordinationNotices, error) { return l.out, l.err }

func mustUUID(t *testing.T, s string) openapi_types.UUID {
	t.Helper()
	var u openapi_types.UUID
	if err := u.UnmarshalText([]byte(s)); err != nil {
		t.Fatal(err)
	}
	return u
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
	flight, errText := mustUUID(t, "0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d"), "the ANSP answered 503"
	intent := mustUUID(t, "6f1c0d2e-3b4a-4c5d-8e6f-7a8b9c0d1e2f")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	l := lister{out: gen.CoordinationNotices{Truncated: true, Notices: []gen.CoordinationNoticeItem{
		{Id: 1, NoticeRef: "USSP-DEV:i:nonconformance:5", Kind: "nonconformance", IntentId: intent, FlightId: &flight,
			State: "pending", CreatedAt: now, AgeS: 12.5, Attempts: 3, LastError: &errText, NextAt: &now},
		{Id: 2, NoticeRef: "USSP-DEV:i:intent_notice", Kind: "intent_notice", IntentId: intent, State: "failed", CreatedAt: now, FailedAt: &now},
	}}}
	rec := call(&Server{Coordination: l})
	var body struct {
		Notices []map[string]any `json:"notices"`
		Trunc   bool             `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 || len(body.Notices) != 2 || !body.Trunc ||
		body.Notices[0]["flight_id"] != flight.String() || body.Notices[0]["last_error"] != errText || body.Notices[0]["age_s"] != 12.5 ||
		body.Notices[1]["flight_id"] != nil || body.Notices[1]["state"] != "failed" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := call(&Server{Coordination: lister{}}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"notices":[]`) {
		t.Fatalf("none open: %d %s", rec.Code, rec.Body)
	}
	if rec := call(&Server{}); rec.Code != 503 {
		t.Fatalf("no lister: %d", rec.Code)
	}
	if rec := call(&Server{Coordination: lister{err: errors.New("db down")}}); rec.Code != 503 {
		t.Fatalf("lister down: %d", rec.Code)
	}
}
