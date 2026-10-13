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
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

type statusSvc struct {
	n       gen.StatusNotice
	created bool
	err     error
	list    []gen.StatusNotice
	listErr error
	got     []string
}

func (s *statusSvc) Request(_ context.Context, staff, kind string) (gen.StatusNotice, bool, error) {
	s.got = append(s.got, staff+"|"+kind)
	return s.n, s.created, s.err
}

func (s *statusSvc) List(context.Context) ([]gen.StatusNotice, error) { return s.list, s.listErr }

func notice() gen.StatusNotice {
	return gen.StatusNotice{Kind: "start", At: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), CertificateId: "0123456789abcdef0123456789abcdef",
		Reference: "DEV01:status:start:1", RequestedBy: "staff-1", State: "pending"}
}

// GET and POST /v1/admin/status: the list with the certificate id; 201
// for a new notice, 200 for the stored one, 400 for one not admitted,
// 503 when nothing is configured or the store fails.
func TestStatusRoutes(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Claims: coreauth.Claims{Subject: "staff-1"}})
	get := func(s *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.ListStatusNotices(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil))
		return rec
	}
	post := func(s *Server, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/status", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		s.RequestStatusNotice(rec, req)
		return rec
	}
	svc := &statusSvc{n: notice(), created: true, list: []gen.StatusNotice{notice()}}
	s := &Server{Status: svc, CertificateID: "0123456789abcdef0123456789abcdef"}
	rec := get(s)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["certificate_id"] != s.CertificateID || len(body["notices"].([]any)) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := get(&Server{Status: &statusSvc{}}); !strings.Contains(rec.Body.String(), `"certificate_id":null`) || !strings.Contains(rec.Body.String(), `"notices":[]`) {
		t.Fatalf("no certificate: %s", rec.Body)
	}
	if rec := post(s, `{"kind":"start"}`); rec.Code != 201 || svc.got[0] != "staff-1|start" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for name, c := range map[string]struct {
		s    *Server
		body string
		code int
	}{
		"stored":       {&Server{Status: &statusSvc{n: notice()}}, `{"kind":"start"}`, 200},
		"not admitted": {&Server{Status: &statusSvc{err: &RefusedError{Reason: "a restart follows a cease"}}}, `{"kind":"restart"}`, 400},
		"db":           {&Server{Status: &statusSvc{err: errors.New("db")}}, `{"kind":"cease"}`, 503},
		"bad body":     {&Server{Status: &statusSvc{}}, `{"kind":"start","extra":1}`, 400},
		"none":         {&Server{}, `{"kind":"start"}`, 503},
	} {
		if rec := post(c.s, c.body); rec.Code != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := get(&Server{}); rec.Code != 503 {
		t.Errorf("list none: %d", rec.Code)
	}
	if rec := get(&Server{Status: &statusSvc{listErr: errors.New("db")}}); rec.Code != 503 {
		t.Errorf("list db: %d", rec.Code)
	}
}
