package occurrence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/rootxkit/uspace-ussp/internal/occurrence/authclient"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

// countingTokens is a TokenSource that counts what it was asked.
type countingTokens struct {
	n      atomic.Int32
	scopes []string
	base   string
}

func (c *countingTokens) Token(_ context.Context, baseURL string, scopes ...string) (string, error) {
	c.n.Add(1)
	c.base, c.scopes = baseURL, scopes
	return authority.Token, nil
}

func builtBody(t *testing.T) []byte {
	t.Helper()
	r := Report{Kind: KindAirprox, Channel: ChannelMandatory, FlaggedBy: "system", SourceKind: "alert", SourceRef: "pair:p@1",
		Reporter: ReporterSystem, OccurredAt: t0.Add(-time.Minute), AwareAt: t0, FlightIDs: []string{flightA}}
	h, v := 20.0, 5.0
	if err := Build(&r, "USSP-DEV", []Aircraft{{Serial: "S1", OperatorReg: ptr("GEO87astrdge12k8-xyz"), FlightID: flightA}},
		[]Manned{{ICAO24: "4ca1f0"}}, &Separation{HM: &h, VM: &v, At: t0}, "seen", []string{"https://ussp.test/v1/records/flights/" + flightA}); err != nil {
		t.Fatal(err)
	}
	return r.Body
}

// The authority's OccurrenceReport (the pinned api/clients/authority.yaml)
// requires category; the queued body names it so and never kind (H-1:
// the body said kind, and every report would have been a 400).
func TestBuiltBodyNamesTheAuthorityRequiredMembers(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(builtBody(t), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["kind"]; ok {
		t.Error("the queued body names the class kind; the authority's schema calls it category")
	}
	for _, f := range requiredOf(t) {
		if _, ok := m[f]; !ok {
			t.Errorf("the queued body has no %s, which the authority's OccurrenceReport requires", f)
		}
	}
	if m["category"] != KindAirprox {
		t.Errorf("category %v", m["category"])
	}
}

// requiredOf is the required list of OccurrenceReport in the pinned copy:
// the contract read, not written from memory (E-03).
func requiredOf(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../api/clients/authority.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required []string `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var op struct {
		OperationID string `yaml:"operationId"`
		Scope       string `yaml:"x-scope"`
	}
	node := doc.Paths["/v1/occurrences"]["post"]
	if err := node.Decode(&op); err != nil {
		t.Fatal(err)
	}
	if op.OperationID != "createOccurrence" || op.Scope != ScopeOccurrences {
		t.Fatalf("the pinned contract's POST /v1/occurrences: %+v", op)
	}
	req := doc.Components.Schemas["OccurrenceReport"].Required
	if len(req) == 0 {
		t.Fatal("no OccurrenceReport in the pinned contract")
	}
	return req
}

// A report queued before the contract was pinned names its class kind:
// it is sent as category, and the mapping is a function of the bytes, so
// every try sends the same report (the authority's replay, never 409).
func TestWireOfMapsTheQueuedBody(t *testing.T) {
	body := builtBody(t)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["kind"] = m["category"]
	delete(m, "category")
	legacy, _ := json.Marshal(m)
	for name, b := range map[string][]byte{"current": body, "queued before the pin": legacy} {
		w, err := WireOf(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		again, _ := WireOf(b)
		x, _ := json.Marshal(w)
		y, _ := json.Marshal(again)
		if w.Category != authclient.Airprox || w.Schema != authclient.Occurrencev1 || !bytes.Equal(x, y) {
			t.Errorf("%s: %s", name, x)
		}
		if w.Aircraft == nil || len(*w.Aircraft) != 1 || *(*w.Aircraft)[0].OperatorReg != "GEO87astrdge12k8" || (*w.Aircraft)[0].AuthorisationNumber != nil {
			t.Errorf("%s: aircraft %s", name, x)
		}
		if w.MinSeparation == nil || *w.MinSeparation.HM != 20 || w.Reporter == nil || *w.Reporter.PersonRef != ReporterSystem {
			t.Errorf("%s: %s", name, x)
		}
	}
}

// A body that does not map is refused before any side effect: no token
// is asked for and nothing reaches the authority; the same client with a
// body that maps asks for one token of scope occurrences.write and sends
// it (E-01 pair).
func TestBodyRefusedBeforeAnySideEffect(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(500) }))
	t.Cleanup(srv.Close)
	tok := &countingTokens{}
	c, err := NewClient(srv.URL, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"no json":          `{`,
		"another schema":   strings.Replace(string(builtBody(t)), `"occurrence/v1"`, `"occurrence/v2"`, 1),
		"unknown category": strings.Replace(string(builtBody(t)), `"airprox"`, `"violation"`, 1),
		"unknown channel":  strings.Replace(string(builtBody(t)), `"mandatory"`, `"maybe"`, 1),
	}
	for name, b := range bad {
		_, err := c.Submit(context.Background(), []byte(b))
		var perm *PermanentError
		if !errors.As(err, &perm) || perm.Status != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if tok.n.Load() != 0 || hits.Load() != 0 {
		t.Fatalf("a body that does not map asked %d tokens and sent %d requests", tok.n.Load(), hits.Load())
	}
	if _, err := c.Submit(context.Background(), builtBody(t)); err == nil {
		t.Fatal("a 500 is not an error")
	}
	if tok.n.Load() != 1 || hits.Load() != 1 || tok.base != srv.URL || len(tok.scopes) != 1 || tok.scopes[0] != ScopeOccurrences {
		t.Fatalf("a body that maps: %d tokens (%s %v), %d requests", tok.n.Load(), tok.base, tok.scopes, hits.Load())
	}
}

// What each answer of the authority means: 201 and 200 (the replay) are
// delivered with the occurrence id; 409 is permanent and a conflict;
// another 4xx permanent; 408, 429, every 5xx, a receipt that does not
// read and an authority that cannot be reached are tried again.
func TestSubmitVerdicts(t *testing.T) {
	body := builtBody(t)
	var ref string
	{
		var p Payload
		_ = json.Unmarshal(body, &p)
		ref = p.ReportRef
	}
	receipt := func(r string) string {
		b, _ := json.Marshal(map[string]any{"occurrence_id": "01J0000000000000000000000A", "report_ref": r, "received_at": t0,
			"within_72h": true, "state": "received", "replayed": false})
		return string(b)
	}
	cases := map[string]struct {
		status    int
		body      string
		id        string
		permanent bool
		conflict  bool
	}{
		"201":              {201, receipt(ref), "01J0000000000000000000000A", false, false},
		"200 replay":       {200, receipt(ref), "01J0000000000000000000000A", false, false},
		"201 another ref":  {201, receipt("x"), "", false, false},
		"201 no receipt":   {201, `{}`, "", false, false},
		"409":              {409, `{"type":"https://schemas.uspace.ge/problems/report_ref_conflict","detail":"another report"}`, "", true, true},
		"400":              {400, `{"type":"https://schemas.uspace.ge/problems/validation"}`, "", true, false},
		"403":              {403, `{}`, "", true, false},
		"408":              {408, `{}`, "", false, false},
		"429":              {429, `{}`, "", false, false},
		"500":              {500, `{}`, "", false, false},
		"502 after commit": {502, `{}`, "", false, false},
		"503 key":          {503, `{"type":"https://schemas.uspace.ge/problems/occurrence_key_unavailable"}`, "", false, false},
		"302":              {302, ``, "", true, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = io.ReadAll(r.Body)
				if r.URL.Path != "/v1/occurrences" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+authority.Token {
					w.WriteHeader(418)
					return
				}
				if tc.status == 302 {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			c, err := NewClient(srv.URL+"/", authority.Tokens{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			id, err := c.Submit(context.Background(), body)
			var perm *PermanentError
			switch {
			case tc.id != "":
				if err != nil || id != tc.id {
					t.Fatalf("id %q err %v", id, err)
				}
			case tc.permanent:
				if !errors.As(err, &perm) || perm.Status != tc.status || (perm.Status == http.StatusConflict) != tc.conflict {
					t.Fatalf("not permanent: %v", err)
				}
			default:
				if err == nil || errors.As(err, &perm) {
					t.Fatalf("not tried again: %v", err)
				}
			}
			var m map[string]any
			if json.Unmarshal(got, &m) != nil || m["category"] != KindAirprox || m["kind"] != nil {
				t.Fatalf("sent %s", got)
			}
		})
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c, _ := NewClient(srv.URL, authority.Tokens{}, nil)
	var perm *PermanentError
	if _, err := c.Submit(context.Background(), body); err == nil || errors.As(err, &perm) {
		t.Fatalf("unreachable: %v", err)
	}
	for _, bad := range []string{"", "ftp://a", "http://u:p@a", "http://a?q=1", "/rel"} {
		if _, err := NewClient(bad, authority.Tokens{}, nil); err == nil {
			t.Errorf("base URL %q accepted", bad)
		}
	}
	if _, err := NewClient("http://a", nil, nil); err == nil {
		t.Error("no token source accepted")
	}
}

// The answer is read to its bound: one longer is not a receipt, tried
// again (E-10).
func TestSubmitAnswerBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"pad":"`+strings.Repeat("x", MaxAnswerBytes)+`"}`)
	}))
	t.Cleanup(srv.Close)
	c, _ := NewClient(srv.URL, authority.Tokens{}, nil)
	var perm *PermanentError
	if _, err := c.Submit(context.Background(), builtBody(t)); err == nil || errors.As(err, &perm) || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("an answer past the bound: %v", err)
	}
}

// End to end on the fake authority of the pinned contract, through the
// Service and its queue:
//   - a 5xx the authority answered after its commit is not a refusal: the
//     report stays pending, is tried again and the try is the authority's
//     replay (200, the first id); the authority holds one report;
//   - a 409 (another body under the report_ref) fails the report on its
//     first answer, counted as a conflict and an alarm, never tried again;
//   - the delivered twin of each: nothing failed, nothing in conflict.
func TestDeliveryAgainstTheContract(t *testing.T) {
	fake := authority.New()
	t.Cleanup(fake.Close)
	c, err := NewClient(fake.URL(), authority.Tokens{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	st := newMem()
	st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	s, _ := newService(st, c)
	if err := s.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.FailAfterCommit(1)
	if s.DeliverDue(context.Background()) != 0 {
		t.Fatal("delivered on a 502")
	}
	r := st.report("alert", "pair:pair-1@1")
	if r.state != "pending" || r.attempts != 1 || !strings.Contains(r.lastError, "502") {
		t.Fatalf("after a 502 after the commit: %+v", r)
	}
	if s.Counters.Get(CounterFailed) != 0 || s.Counters.Get(CounterRetried) != 1 {
		t.Fatalf("a 5xx counted as a refusal: %v", s.Counters.Snapshot())
	}
	st.now = st.now.Add(Backoff(1))
	if s.DeliverDue(context.Background()) != 1 {
		t.Fatal("the replay is not delivered")
	}
	r = st.report("alert", "pair:pair-1@1")
	if got := fake.Occurrences(); r.state != "delivered" || len(got) != 1 || r.authRef != got[0].ID {
		t.Fatalf("after the replay: %+v, the authority holds %d", r, len(got))
	}
	if s.Counters.Get(CounterFailed) != 0 || s.Counters.Get(CounterConflict) != 0 || s.Counters.Get(CounterDelivered) != 1 {
		t.Fatalf("counters %v", s.Counters.Snapshot())
	}

	// Another report under the same report_ref: 409, failed at once.
	st2 := newMem()
	st2.events = []AlertEvent{proximity("a1", "", 10.0, 3.0)}
	s2, _ := newService(st2, c)
	s2.MaxAttempts = 5
	_ = s2.Detect(context.Background())
	if s2.DeliverDue(context.Background()) != 0 {
		t.Fatal("a conflicting report delivered")
	}
	r2 := st2.report("alert", "pair:pair-1@1")
	if r2.state != "failed" || r2.attempts != 1 || !strings.Contains(r2.lastError, "409 report_ref_conflict") {
		t.Fatalf("conflict: %+v", r2)
	}
	if s2.Counters.Get(CounterConflict) != 1 || s2.Counters.Get(CounterFailed) != 1 || s2.Counters.Get(CounterRetried) != 0 {
		t.Fatalf("conflict counters %v", s2.Counters.Snapshot())
	}
	st2.now = st2.now.Add(time.Hour)
	if s2.DeliverDue(context.Background()) != 0 || st2.report("alert", "pair:pair-1@1").attempts != 1 {
		t.Fatal("a 409 tried again")
	}
	if len(fake.Occurrences()) != 1 {
		t.Fatal("the conflicting report was stored")
	}
}

// The wait between tries doubles from 5 s up to the configured ceiling
// and never past it (E-10); 0 is the default ceiling.
func TestBackoffCeiling(t *testing.T) {
	if backoff(1, 0) != DefaultBackoffMin || backoff(100, 0) != DefaultBackoffMax {
		t.Fatalf("default: %v %v", backoff(1, 0), backoff(100, 0))
	}
	for n := 1; n < 40; n++ {
		if d := backoff(n, 30*time.Second); d > 30*time.Second || d <= 0 {
			t.Fatalf("try %d waits %v", n, d)
		}
	}
	if backoff(3, 30*time.Second) != 20*time.Second || backoff(4, 30*time.Second) != 30*time.Second {
		t.Fatal(backoff(3, 30*time.Second), backoff(4, 30*time.Second))
	}
	st := newMem()
	st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	s, _ := newService(st, &fakeAuthority{down: true})
	s.BackoffMax = 7 * time.Second
	_ = s.Detect(context.Background())
	for range 3 {
		s.DeliverDue(context.Background())
		r := st.report("alert", "pair:pair-1@1")
		if r.nextAt.Sub(st.now) > 7*time.Second {
			t.Fatalf("next try %v away", r.nextAt.Sub(st.now))
		}
		st.now = r.nextAt
	}
}
