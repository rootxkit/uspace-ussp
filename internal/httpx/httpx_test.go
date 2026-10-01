package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-core/core"
)

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) ProblemBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Fatalf("Content-Type = %q", ct)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	// The lab's problem/v1 requires these four and "errors" is an array,
	// never null, even when empty.
	for _, k := range []string{"type", "title", "status", "errors"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("problem lacks %q: %s", k, rec.Body.String())
		}
	}
	if _, ok := raw["errors"].([]any); !ok {
		t.Fatalf("errors is not an array: %s", rec.Body.String())
	}
	var p ProblemBody
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if !regexp.MustCompile(`^https://schemas\.uspace\.ge/problems/[a-z][a-z0-9_]*$`).MatchString(p.Type) {
		t.Fatalf("type %q does not match the schema pattern", p.Type)
	}
	if p.Status != rec.Code {
		t.Fatalf("status %d != HTTP %d", p.Status, rec.Code)
	}
	return p
}

func TestProblemCapsErrorsAt100AndSaysTruncated(t *testing.T) {
	errs := make([]*core.FieldError, 0, 150)
	for i := range 150 {
		errs = append(errs, core.Fieldf(fmt.Sprintf("features[%d]", i), "bad"))
	}
	p := NewProblem(http.StatusBadRequest, SlugValidation, "", "", errs...)
	if len(p.Errors) != MaxProblemErrors || !p.Truncated {
		t.Fatalf("len=%d truncated=%v", len(p.Errors), p.Truncated)
	}
	p = NewProblem(http.StatusBadRequest, SlugValidation, "", "", errs[:100]...)
	if len(p.Errors) != 100 || p.Truncated {
		t.Fatalf("exactly 100: len=%d truncated=%v", len(p.Errors), p.Truncated)
	}
}

func TestProblemSlugAndTitle(t *testing.T) {
	p := NewProblem(http.StatusForbidden, "Not-A-Slug", "", "")
	if p.Slug() != SlugInternal || p.Title != "Forbidden" {
		t.Fatalf("got %+v", p)
	}
	p = NewProblem(http.StatusForbidden, "forbidden", "Not allowed", "")
	if p.Slug() != "forbidden" || p.Title != "Not allowed" {
		t.Fatalf("got %+v", p)
	}
}

func TestWriteErrorMapsFieldErrorsTooLargeAndInternal(t *testing.T) {
	cases := []struct {
		err    error
		status int
		slug   string
		fields []string
	}{
		{errors.Join(core.Fieldf("serial", "too long"), &core.FieldError{Field: "class", Reason: "unknown"}), 400, SlugValidation, []string{"serial", "class"}},
		{fmt.Errorf("decode: %w", &core.FieldError{Field: "lat_deg", Reason: "out of range"}), 400, SlugValidation, []string{"lat_deg"}},
		{&http.MaxBytesError{Limit: 10}, 413, SlugBodyTooLarge, []string{"body"}},
		{errors.New("pq: password authentication failed for user secret"), 500, SlugInternal, nil},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		WriteError(rec, httptest.NewRequest(http.MethodPost, "/v1/x", nil), c.err)
		p := decodeProblem(t, rec)
		if rec.Code != c.status || p.Slug() != c.slug || p.Instance != "/v1/x" {
			t.Errorf("%v: got %d %s %q", c.err, rec.Code, p.Slug(), p.Instance)
		}
		var fields []string
		for _, e := range p.Errors {
			fields = append(fields, e.Field)
		}
		if strings.Join(fields, ",") != strings.Join(c.fields, ",") {
			t.Errorf("%v: fields %v, want %v", c.err, fields, c.fields)
		}
		if strings.Contains(rec.Body.String(), "password") {
			t.Errorf("an internal error leaked its text: %s", rec.Body.String())
		}
	}
}

func TestRequestIDAcceptsWellFormedAndReplacesOthers(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = RequestIDFrom(r.Context()) }))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(RequestIDHeader, "caddy-abc.123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if seen != "caddy-abc.123" || rec.Header().Get(RequestIDHeader) != seen {
		t.Errorf("well-formed id not kept: %q", seen)
	}
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(RequestIDHeader, "bad id\nwith newline")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !regexp.MustCompile(`^[0-9a-f]{24}$`).MatchString(seen) || rec.Header().Get(RequestIDHeader) != seen {
		t.Errorf("malformed id not replaced: %q", seen)
	}
	if RequestIDFrom(context.Background()) != "" {
		t.Error("id from an empty context")
	}
}

func TestAccessLogWritesOneStructuredLinePerRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/x/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})
	// Baseline copies the request (context values), so the pattern must
	// survive the copy.
	h := Baseline(mux, logger, BaselineDeps{Counters: &core.Counters{}})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/x/42", nil))
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["route"] != "GET /v1/x/{id}" || m["status"] != float64(201) || m["bytes_out"] != float64(5) || m["request_id"] == "" {
		t.Errorf("access line: %v", m)
	}
}

func TestRecoverTurnsAPanicIntoACountedProblem(t *testing.T) {
	c := &core.Counters{}
	h := Recover(slog.New(slog.DiscardHandler), c)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if p := decodeProblem(t, rec); rec.Code != 500 || p.Slug() != SlugInternal {
		t.Fatalf("got %d %+v", rec.Code, p)
	}
	if c.Get(CounterHandlerPanics) != 1 {
		t.Error("panic not counted")
	}
	// A handler that does not panic passes through untouched.
	ok := Recover(slog.New(slog.DiscardHandler), c)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	rec = httptest.NewRecorder()
	ok.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 204 {
		t.Errorf("got %d", rec.Code)
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	h := Recover(slog.New(slog.DiscardHandler), &core.Counters{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // identity, as net/http compares it
			t.Fatalf("recovered %v, want ErrAbortHandler re-raised", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func readAll(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	_, _ = fmt.Fprintf(w, "%d", len(b))
}

func TestBodyCapAcceptsAtTheCapAndRefusesOneByteMore(t *testing.T) {
	c := &core.Counters{}
	h := BodyCap(10, c)(http.HandlerFunc(readAll))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789")))
	if rec.Code != 200 || rec.Body.String() != "10" {
		t.Fatalf("at the cap: %d %s", rec.Code, rec.Body.String())
	}
	if c.Get(CounterBodyTooLarge) != 0 {
		t.Fatal("a body at the cap was counted as too large")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789A")))
	if p := decodeProblem(t, rec); rec.Code != 413 || p.Slug() != SlugBodyTooLarge {
		t.Fatalf("over the cap: %d %+v", rec.Code, p)
	}
	if c.Get(CounterBodyTooLarge) != 1 {
		t.Fatal("too large not counted")
	}
}

func TestBodyLimitOverridesTheDefaultPerRoute(t *testing.T) {
	c := &core.Counters{}
	mux := http.NewServeMux()
	mux.Handle("POST /big", BodyLimit(20, c, http.HandlerFunc(readAll)))
	mux.Handle("POST /small", BodyLimit(3, c, http.HandlerFunc(readAll)))
	mux.HandleFunc("POST /default", readAll)
	h := BodyCap(10, c)(mux)
	body := strings.Repeat("x", 15)
	for path, want := range map[string]int{"/big": 200, "/small": 413, "/default": 413} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.ContentLength = -1 // unknown length: the cap applies while reading
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%s: got %d, want %d", path, rec.Code, want)
		}
	}
	// A declared Content-Length over the route cap is refused before the
	// handler reads anything.
	called := false
	pre := BodyLimit(3, c, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	pre.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abcd")))
	if rec.Code != 413 || called {
		t.Fatalf("declared length: %d called=%v", rec.Code, called)
	}
	// Outside BodyCap, BodyLimit applies its own cap.
	alone := BodyLimit(3, c, http.HandlerFunc(readAll))
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abcd"))
	r.ContentLength = -1
	rec = httptest.NewRecorder()
	alone.ServeHTTP(rec, r)
	if rec.Code != 413 {
		t.Fatalf("standalone: %d", rec.Code)
	}
}
