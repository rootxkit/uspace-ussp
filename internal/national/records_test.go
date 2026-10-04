package national

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/auth"
)

type recBuilder struct {
	rec any
	err error
}

func (b recBuilder) Flight(context.Context, string) (any, error) { return b.rec, b.err }

type recDaily struct {
	path string
	b    RecordBundle
	err  error
}

func (d recDaily) Open(context.Context, time.Time) (RecordBundle, *os.File, error) {
	if d.err != nil {
		return RecordBundle{}, nil, d.err
	}
	f, err := os.Open(d.path)
	return d.b, f, err
}

type recAudit struct {
	err  error
	seen *[]string
}

func (a recAudit) AuditRead(_ context.Context, actor, entity, id string) error {
	if a.seen != nil {
		*a.seen = append(*a.seen, actor+"|"+entity+"|"+id)
	}
	return a.err
}

func authorityCtx() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Claims: coreauth.Claims{Subject: "authority-01", Scopes: []string{ScopeRecords}}})
}

// GET /v1/records/flights/{id}: the record, audited with the caller
// before the body; 404 for a flight not held, 503 when it cannot be read
// or audited (nothing is served unaudited) or nothing is configured.
func TestGetFlightRecord(t *testing.T) {
	var id openapi_types.UUID
	if err := id.UnmarshalText([]byte("0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d")); err != nil {
		t.Fatal(err)
	}
	call := func(s *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.GetFlightRecord(rec, httptest.NewRequest(http.MethodGet, "/v1/records/flights/"+id.String(), nil).WithContext(authorityCtx()), id)
		return rec
	}
	var seen []string
	ok := &Records{Builder: recBuilder{rec: map[string]string{"schema": "record/flight/v1", "flight_id": id.String()}}, Audit: recAudit{seen: &seen}}
	if rec := call(&Server{Records: ok}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"schema":"record/flight/v1"`) ||
		len(seen) != 1 || seen[0] != "authority-01|flight|"+id.String() {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, seen)
	}
	for name, c := range map[string]struct {
		s    *Server
		code int
	}{
		"not held":     {&Server{Records: &Records{Builder: recBuilder{err: ErrRecordNotFound}, Audit: recAudit{}}}, 404},
		"flights down": {&Server{Records: &Records{Builder: recBuilder{err: errors.New("db")}, Audit: recAudit{}}}, 503},
		"audit down":   {&Server{Records: &Records{Builder: recBuilder{}, Audit: recAudit{err: errors.New("db")}}}, 503},
		"none":         {&Server{}, 503},
	} {
		if rec := call(c.s); rec.Code != c.code {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

// GET /v1/records/daily/{date}: the bytes with the hash header, audited;
// 404 without a bundle, 500 for a changed one, 503 otherwise.
func TestGetDailyRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026-10-03-0123456789ab.jsonl.gz")
	if err := os.WriteFile(path, []byte("gzip bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	day := openapi_types.Date{Time: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}
	call := func(s *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.GetDailyRecords(rec, httptest.NewRequest(http.MethodGet, "/v1/records/daily/2026-10-03", nil).WithContext(authorityCtx()), day)
		return rec
	}
	var seen []string
	b := RecordBundle{Date: day.Time, Hash: strings.Repeat("a", 64), Flights: 3}
	rec := call(&Server{Records: &Records{Daily: recDaily{path: path, b: b}, Audit: recAudit{seen: &seen}}})
	if rec.Code != 200 || rec.Body.String() != "gzip bytes" || rec.Header().Get("X-Content-SHA256") != b.Hash ||
		rec.Header().Get("X-Record-Flights") != "3" || rec.Header().Get("Content-Type") != "application/gzip" || len(seen) != 1 {
		t.Fatalf("%d %q %v %v", rec.Code, rec.Body, rec.Header(), seen)
	}
	for name, c := range map[string]struct {
		s    *Server
		code int
	}{
		"no bundle": {&Server{Records: &Records{Daily: recDaily{err: ErrRecordNotFound}, Audit: recAudit{}}}, 404},
		"changed":   {&Server{Records: &Records{Daily: recDaily{err: ErrBundleCorrupt}, Audit: recAudit{}}}, 500},
		"down":      {&Server{Records: &Records{Daily: recDaily{err: errors.New("db")}, Audit: recAudit{}}}, 503},
		"unaudited": {&Server{Records: &Records{Daily: recDaily{path: path, b: b}, Audit: recAudit{err: errors.New("db")}}}, 503},
		"none":      {&Server{}, 503},
	} {
		if rec := call(c.s); rec.Code != c.code {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}
