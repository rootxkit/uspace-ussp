package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

// Fixture numbers and serials (CLAUDE.md: GEO-TEST-* numbers, TEST*
// serials); GEOTESTOP0001 fits the default registration pattern.
const (
	opNumber = "GEOTESTOP0001"
	opSecret = "GEOTESTOP0001-abc"
	snA      = "TEST-SN-A"
	pilotA   = "GEO-TEST-PILOT-1"
)

// recordingTokens records the audience base URL and scopes asked.
type recordingTokens struct {
	mu    sync.Mutex
	bases []string
	scope []string
}

func (r *recordingTokens) Token(_ context.Context, base string, scopes ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bases = append(r.bases, base)
	r.scope = append(r.scope, scopes...)
	return authority.Token, nil
}

func newFake(t *testing.T) (*authority.Fake, *Client) {
	t.Helper()
	f := authority.New()
	t.Cleanup(f.Close)
	c, err := NewClient(ClientConfig{BaseURL: f.URL(), Tokens: authority.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func mustNorm(t *testing.T, qs ...Query) []normalised {
	t.Helper()
	ns, err := checkQueries(qs, PurposeAuthorisation)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func TestNewClientRefusesBadBase(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "/relative", "https://"} {
		if _, err := NewClient(ClientConfig{BaseURL: u}); err == nil {
			t.Fatalf("%q accepted", u)
		}
	}
	if _, err := NewClient(ClientConfig{BaseURL: "https://authority.test"}); err != nil {
		t.Fatal(err)
	}
}

func TestClientWithoutTokensSendsNothing(t *testing.T) {
	f := authority.New()
	defer f.Close()
	c, err := NewClient(ClientConfig{BaseURL: f.URL()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Operator: opNumber})); !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("got %v", err)
	}
	if f.Requests("validate") != 0 {
		t.Fatal("an unauthenticated request was sent")
	}
}

// One lookup is a GET carrying the purpose as the query parameter the
// contract names, with a registry.validate token for the authority's
// base URL (the token client derives aud from its host, M18); several
// are one POST batch. The secret part of a number is never sent.
func TestClientValidateGetAndBatch(t *testing.T) {
	f := authority.New()
	defer f.Close()
	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f.SetOperator(opNumber, "active", &until)
	f.SetUAS(snA, "suspended", "C1", "under_900g")
	f.SetPilot(pilotA, "active", authority.Competency{Competency: "A1/A3", ValidUntil: until})
	tok := &recordingTokens{}
	c, err := NewClient(ClientConfig{BaseURL: f.URL(), Tokens: tok})
	if err != nil {
		t.Fatal(err)
	}
	vs, err := c.Validate(t.Context(), PurposeIdentification, mustNorm(t, Query{Operator: opSecret, Serial: snA, Pilot: pilotA}))
	if err != nil {
		t.Fatal(err)
	}
	v := vs[0]
	if v.Operator.Status != "valid" || v.Operator.RegistrationNumber != opNumber || v.Uas.Status != "suspended" || *v.Uas.ClassLabel != "C1" ||
		v.Pilot.Status != "valid" || len(v.Pilot.Competencies) != 1 {
		t.Fatalf("answer %+v %+v %+v", v.Operator, v.Uas, v.Pilot)
	}
	if f.Requests("validate") != 1 || f.Requests("validate_batch") != 0 || f.Purposes()[0] != "identification" {
		t.Fatalf("requests %d/%d purposes %v", f.Requests("validate"), f.Requests("validate_batch"), f.Purposes())
	}
	if len(tok.bases) != 1 || tok.bases[0] != f.URL() || tok.scope[0] != ScopeRegistryValidate {
		t.Fatalf("token asked for %v %v", tok.bases, tok.scope)
	}
	vs, err = c.Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Operator: opNumber}, Query{Serial: "test-sn-a"}, Query{Pilot: "nobody"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 || vs[0].Operator.Status != "valid" || vs[1].Uas.Status != "suspended" || vs[2].Pilot.Status != "unknown" {
		t.Fatalf("batch %+v", vs)
	}
	if f.Requests("validate_batch") != 1 {
		t.Fatal("several queries were not one batch")
	}
}

// A field F8 does not define (a name) is refused as personal data, and
// the field's name, never its value, is reported (E-01: the answer
// without it is accepted, above and here).
func TestClientRefusesAFieldF8DoesNotDefine(t *testing.T) {
	f, c := newFake(t)
	f.SetOperator(opNumber, "active", nil)
	if _, err := c.Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Operator: opNumber})); err != nil {
		t.Fatalf("the clean answer was refused: %v", err)
	}
	f.Misbehave("name", "Test Person")
	_, err := c.Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Operator: opNumber}))
	var re *RefusedError
	if !errors.As(err, &re) || re.Counter != CounterPIIRefused || !strings.Contains(re.Detail, `"name"`) || strings.Contains(err.Error(), "Test Person") {
		t.Fatalf("got %v", err)
	}
}

// An operator echoed with a secret part is personal data and refused;
// another operator echoed is not the answer asked.
func TestClientRefusesAWrongOrSecretEcho(t *testing.T) {
	f, c := newFake(t)
	f.SetOperator(opNumber, "active", nil)
	for echo, counter := range map[string]string{opSecret: CounterPIIRefused, "GEOTESTOP0002": CounterAnswerRefused} {
		f.EchoOperatorAs(echo)
		_, err := c.Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Operator: opNumber}))
		var re *RefusedError
		if !errors.As(err, &re) || re.Counter != counter {
			t.Fatalf("echo %s: got %v, want %s", echo, err, counter)
		}
	}
	f.EchoOperatorAs("geotestop0001") // the public part in another case is the same key
	if _, err := c.Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Operator: opNumber})); err != nil {
		t.Fatal(err)
	}
}

// stub serves one fixed answer to every request.
func stub(t *testing.T, status int, body string, header map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientConfig{BaseURL: srv.URL, Tokens: authority.Tokens{}, MaxBodyBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientRefusesAnswersThatAreNotTheAnswerAsked(t *testing.T) {
	q := Query{Operator: opNumber, Serial: snA, Pilot: pilotA}
	op := `"operator":{"registration_number":"` + opNumber + `","status":"valid"}`
	uas := `"uas":{"serial":"` + snA + `","status":"valid"}`
	pilot := `"pilot":{"pilot":"` + pilotA + `","status":"valid","competencies":[]}`
	for name, tc := range map[string]struct {
		body    string
		counter string
	}{
		"missing part":     {`{` + op + `,` + uas + `}`, CounterAnswerRefused},
		"other serial":     {`{` + op + `,"uas":{"serial":"TEST-OTHER","status":"valid"},` + pilot + `}`, CounterAnswerRefused},
		"other pilot":      {`{` + op + `,` + uas + `,"pilot":{"pilot":"x","status":"valid","competencies":[]}}`, CounterAnswerRefused},
		"bad status":       {`{"operator":{"registration_number":"` + opNumber + `","status":"expired"},` + uas + `,` + pilot + `}`, CounterAnswerRefused},
		"bad uas status":   {`{` + op + `,"uas":{"serial":"` + snA + `","status":"active"},` + pilot + `}`, CounterAnswerRefused},
		"bad pilot status": {`{` + op + `,` + uas + `,"pilot":{"pilot":"` + pilotA + `","status":"","competencies":[]}}`, CounterAnswerRefused},
		"long band":        {`{` + op + `,"uas":{"serial":"` + snA + `","status":"valid","mtom_band":"` + strings.Repeat("x", 65) + `"},` + pilot + `}`, CounterAnswerRefused},
		"empty competency": {`{` + op + `,` + uas + `,"pilot":{"pilot":"` + pilotA + `","status":"valid","competencies":[{"competency":"","valid_until":"2030-01-01T00:00:00Z"}]}}`, CounterAnswerRefused},
		"not json":         {`{`, CounterAnswerRefused},
		"trailing":         {`{` + op + `,` + uas + `,` + pilot + `} {}`, CounterAnswerRefused},
		"too long":         {`{` + op + `,` + uas + `,` + pilot + `,"x":"` + strings.Repeat("a", 5000) + `"}`, CounterAnswerRefused},
		"nested name":      {`{"operator":{"registration_number":"` + opNumber + `","status":"valid","address":"x"},` + uas + `,` + pilot + `}`, CounterPIIRefused},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := stub(t, 200, tc.body, nil).Validate(t.Context(), PurposeAuthorisation, mustNorm(t, q))
			var re *RefusedError
			if !errors.As(err, &re) || re.Counter != tc.counter {
				t.Fatalf("got %v, want %s", err, tc.counter)
			}
		})
	}
	// The same answer, complete and correct, is accepted (E-01).
	if _, err := stub(t, 200, `{`+op+`,`+uas+`,`+pilot+`}`, nil).Validate(t.Context(), PurposeAuthorisation, mustNorm(t, q)); err != nil {
		t.Fatal(err)
	}
	// Too many competencies (E-10: one over the bound refused).
	comps := make([]string, MaxCompetencies+1)
	for i := range comps {
		comps[i] = fmt.Sprintf(`{"competency":"c%d","valid_until":"2030-01-01T00:00:00Z"}`, i)
	}
	body := `{"pilot":{"pilot":"` + pilotA + `","status":"valid","competencies":[` + strings.Join(comps, ",") + `]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, Tokens: authority.Tokens{}})
	var re *RefusedError
	if _, err := c.Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Pilot: pilotA})); !errors.As(err, &re) {
		t.Fatalf("%d competencies accepted: %v", MaxCompetencies+1, err)
	}
	// A batch answer with a result missing is refused.
	two := mustNorm(t, Query{Serial: snA}, Query{Serial: "TEST-SN-B"})
	if _, err := stub(t, 200, `{"results":[{`+uas+`}]}`, nil).Validate(t.Context(), PurposeAuthorisation, two); !errors.As(err, &re) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Validate(t.Context(), PurposeAuthorisation, nil); err == nil {
		t.Fatal("an empty lookup was sent")
	}
}

func TestClientStatusErrorAndDeadline(t *testing.T) {
	_, err := stub(t, 503, `{"type":"https://schemas.uspace.ge/problems/unavailable","title":"x","status":503,"errors":[]}`, nil).
		Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Serial: snA}))
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 || se.Slug != "unavailable" || !strings.Contains(se.Error(), "503 unavailable") {
		t.Fatalf("got %v", err)
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, Tokens: authority.Tokens{}, Timeout: 50 * time.Millisecond})
	start := time.Now()
	if _, err := c.Validate(t.Context(), PurposeAuthorisation, mustNorm(t, Query{Serial: snA})); err == nil {
		t.Fatal("a hung authority answered")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the deadline did not hold: %v", d)
	}
}

func TestClientChanges(t *testing.T) {
	f, c := newFake(t)
	f.SetOperator(opNumber, "active", nil)
	f.SetUAS(snA, "active", "", "")
	p, err := c.Changes(t.Context(), 0, 1, "")
	if err != nil || p.NotModified || len(p.Changes) != 1 || p.NextSince != 1 || p.ETag == "" {
		t.Fatalf("page %+v %v", p, err)
	}
	p, err = c.Changes(t.Context(), 1, 10, "")
	if err != nil || len(p.Changes) != 1 || p.Changes[0].PublicKey != snA || p.NextSince != 2 {
		t.Fatalf("page %+v %v", p, err)
	}
	empty, err := c.Changes(t.Context(), 2, 0, "")
	if err != nil || len(empty.Changes) != 0 || empty.NextSince != 2 {
		t.Fatalf("page %+v %v", empty, err)
	}
	nm, err := c.Changes(t.Context(), 2, 10, empty.ETag)
	if err != nil || !nm.NotModified || nm.NextSince != 2 {
		t.Fatalf("304 %+v %v", nm, err)
	}
	f.Down()
	if _, err := c.Changes(t.Context(), 2, 10, ""); err == nil {
		t.Fatal("a down authority answered")
	}
}

func TestClientRefusesAMalformedChangePage(t *testing.T) {
	ch := func(seq int, entity, key string) string {
		return fmt.Sprintf(`{"seq":%d,"entity_type":%q,"entity_id":"x","public_key":%q,"status":"active","at":"2026-01-01T00:00:00Z"}`, seq, entity, key)
	}
	for name, body := range map[string]string{
		"next before since": `{"changes":[],"next_since":4}`,
		"seq at since":      `{"changes":[` + ch(5, "uas", snA) + `],"next_since":5}`,
		"seq past next":     `{"changes":[` + ch(7, "uas", snA) + `],"next_since":6}`,
		"entity":            `{"changes":[` + ch(6, "drone", snA) + `],"next_since":6}`,
		"empty key":         `{"changes":[` + ch(6, "uas", " ") + `],"next_since":6}`,
		"long key":          `{"changes":[` + ch(6, "uas", strings.Repeat("k", MaxPublicKeyLen+1)) + `],"next_since":6}`,
		"too many":          `{"changes":[` + ch(6, "uas", "a") + `,` + ch(7, "uas", "b") + `],"next_since":7}`,
		"unknown field":     `{"changes":[],"next_since":5,"name":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := stub(t, 200, body, nil).Changes(t.Context(), 5, 1, "")
			var re *RefusedError
			if !errors.As(err, &re) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	p, err := stub(t, 200, `{"changes":[`+ch(6, "uas", snA)+`],"next_since":6}`, map[string]string{"ETag": strings.Repeat("e", 200)}).Changes(t.Context(), 5, 1, "")
	if err != nil || len(p.Changes) != 1 || p.ETag != "" {
		t.Fatalf("a well-formed page: %+v %v (an over-long ETag is dropped, never kept)", p, err)
	}
	if _, err := stub(t, 500, `{}`, nil).Changes(t.Context(), 5, 1, ""); err == nil {
		t.Fatal("500 accepted")
	}
}
