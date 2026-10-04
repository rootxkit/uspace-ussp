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

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/occurrence"
)

type occSvc struct {
	item    occurrence.Item
	created bool
	err     error
	got     []string
}

func (o *occSvc) Flag(_ context.Context, staff, alert, kind, narrative string) (occurrence.Item, bool, error) {
	o.got = append(o.got, staff+"|"+alert+"|"+kind+"|"+narrative)
	return o.item, o.created, o.err
}

type occList struct {
	items []occurrence.Item
	err   error
}

func (l occList) Open(context.Context, int) ([]occurrence.Item, bool, error) {
	return l.items, false, l.err
}

func occItem() occurrence.Item {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	return occurrence.Item{ReportRef: "USSP-DEV:occurrence:alert:x", Kind: "airprox", State: "pending", Channel: "mandatory", FlaggedBy: "supervisor",
		BecameAwareAt: now, DeadlineAt: now.Add(72 * time.Hour), TimeToDeadlineS: -5, Critical: true, FlightIDs: []string{"0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d"}}
}

// GET /v1/admin/occurrences: the reports with their deadline and why
// nothing is sent; 503 when the list cannot be read.
func TestListOccurrences(t *testing.T) {
	call := func(s *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.ListOccurrences(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/occurrences", nil))
		return rec
	}
	rec := call(&Server{Occurrences: &Occurrences{List: occList{items: []occurrence.Item{occItem()}}, Delivery: "spec gap"}})
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	reports, _ := body["reports"].([]any)
	if rec.Code != 200 || body["delivery"] != "spec gap" || len(reports) != 1 || reports[0].(map[string]any)["critical"] != true {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := call(&Server{Occurrences: &Occurrences{List: occList{}}}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"delivery":null`) {
		t.Fatalf("delivering: %s", rec.Body)
	}
	for name, s := range map[string]*Server{
		"none": {}, "down": {Occurrences: &Occurrences{List: occList{err: errors.New("db")}}},
		"bad flight": {Occurrences: &Occurrences{List: occList{items: []occurrence.Item{{FlightIDs: []string{"x"}}}}}},
	} {
		if rec := call(s); rec.Code < 500 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

// POST /v1/admin/occurrences: 201 queued under the supervisor's id, 200
// for an event reported before, 404 for an alert not held, 400 for a
// refused flag or body, 503 when it cannot be stored.
func TestFlagOccurrence(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Claims: coreauth.Claims{Subject: "staff-7"}})
	call := func(s *Server, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/occurrences", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		s.FlagOccurrence(rec, req)
		return rec
	}
	const ok = `{"alert_id":"5f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c","kind":"airprox","narrative":"seen"}`
	svc := &occSvc{item: occItem(), created: true}
	if rec := call(&Server{Occurrences: &Occurrences{Service: svc}}, ok); rec.Code != 201 || svc.got[0] != "staff-7|5f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c|airprox|seen" {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, svc.got)
	}
	for name, c := range map[string]struct {
		s    *Server
		body string
		code int
	}{
		"again":     {&Server{Occurrences: &Occurrences{Service: &occSvc{item: occItem()}}}, ok, 200},
		"not held":  {&Server{Occurrences: &Occurrences{Service: &occSvc{err: &occurrence.FlagError{NotFound: true, Reason: "no"}}}}, ok, 404},
		"refused":   {&Server{Occurrences: &Occurrences{Service: &occSvc{err: &occurrence.FlagError{Reason: "kind"}}}}, ok, 400},
		"db":        {&Server{Occurrences: &Occurrences{Service: &occSvc{err: errors.New("db")}}}, ok, 503},
		"none":      {&Server{}, ok, 503},
		"bad body":  {&Server{Occurrences: &Occurrences{Service: &occSvc{}}}, `{"alert_id":"x"}`, 400},
		"long text": {&Server{Occurrences: &Occurrences{Service: &occSvc{}}}, `{"alert_id":"5f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c","narrative":"` + strings.Repeat("n", occurrence.MaxNarrative+1) + `"}`, 400},
	} {
		if rec := call(c.s, c.body); rec.Code != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}
