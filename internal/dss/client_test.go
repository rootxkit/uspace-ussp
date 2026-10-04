package dss

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3548"
	"go.yaml.in/yaml/v3"
)

// stub is a DSS or a peer that answers every request with status and
// body, counting the requests.
func stub(t *testing.T, status int, body string, header map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, &n
}

func stubClient(url string) *Client {
	return &Client{DSSBaseURL: url, Tokens: &tokens{sub: ourManager}, Backoff: time.Millisecond, CallTimeout: time.Second}
}

var aoi = volume(41.7, 44.8)

// A read is retried on a 5xx, a 429 or no answer, up to its attempts,
// and the DSS marked down after the last; a 4xx is never retried; a
// write is retried only on a 429 or a 503 (the answers that say it was
// not made), never on a 500.
func TestClientRetries(t *testing.T) {
	ctx := context.Background()
	s, n := stub(t, http.StatusInternalServerError, `{}`, nil)
	c := stubClient(s.URL)
	if _, err := c.QueryOperationalIntents(ctx, aoi); !errors.Is(err, ErrDSSDown) || n.Load() != DefaultAttempts {
		t.Fatalf("%v after %d attempts", err, n.Load())
	}
	if r := c.Reach(); !r.Known || r.Up {
		t.Fatalf("%+v", r)
	}
	n.Store(0)
	if _, err := c.PutOperationalIntent(ctx, oursID, "", f3548.PutOperationalIntentReferenceParameters{}); !errors.Is(err, ErrDSSDown) || n.Load() != 1 {
		t.Fatalf("a write retried on 500: %v after %d", err, n.Load())
	}
	s2, n2 := stub(t, http.StatusServiceUnavailable, `{}`, nil)
	c2 := stubClient(s2.URL)
	if _, err := c2.DeleteOperationalIntent(ctx, oursID, "o"); !errors.Is(err, ErrDSSDown) || n2.Load() != DefaultAttempts {
		t.Fatalf("a write not retried on 503: %v after %d", err, n2.Load())
	}
	s3, n3 := stub(t, http.StatusBadRequest, `{"message":"bad"}`, nil)
	c3 := stubClient(s3.URL)
	var re *RefusedError
	if _, err := c3.QueryConstraints(ctx, aoi); !errors.As(err, &re) || re.Status != 400 || n3.Load() != 1 {
		t.Fatalf("a 4xx retried or not refused: %v after %d", err, n3.Load())
	}
	if r := c3.Reach(); !r.Up {
		t.Fatalf("a 4xx is an answer: %+v", r)
	}
	// No answer at all.
	c4 := stubClient("http://127.0.0.1:1")
	if _, err := c4.Availability(ctx, ourManager); !errors.Is(err, ErrDSSDown) {
		t.Fatal(err)
	}
	// A cancelled context ends the retries.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.QueryOperationalIntents(cctx, aoi); !errors.Is(err, ErrDSSDown) {
		t.Fatal(err)
	}
}

// Answers that cannot be used are refusals naming why, never accepted:
// another reference, no ovn, a state that is not F3548's, too many
// subscribers or references, a body over the bound, not the shape.
func TestClientRefusesAnswers(t *testing.T) {
	ctx := context.Background()
	change := func(id, ovn, state string, subs int) string {
		ss := make([]f3548.SubscriberToNotify, subs)
		for i := range ss {
			ss[i] = f3548.SubscriberToNotify{UssBaseUrl: "https://peer.example/x", Subscriptions: []f3548.SubscriptionState{{SubscriptionId: peerID}}}
		}
		r := f3548.ChangeOperationalIntentReferenceResponse{OperationalIntentReference: f3548.OperationalIntentReference{Id: id, State: f3548.OperationalIntentState(state)}, Subscribers: ss}
		if ovn != "" {
			r.OperationalIntentReference.Ovn = &ovn
		}
		b, _ := json.Marshal(r)
		return string(b)
	}
	for _, c := range []struct {
		name string
		body string
	}{
		{"another reference", change(peerID, "o", "Accepted", 0)},
		{"no ovn", change(oursID, "", "Accepted", 0)},
		{"local state", change(oursID, "o", "pending_dss", 0)},
		{"too many subscribers", change(oursID, "o", "Accepted", MaxSubscribers+1)},
		{"not json", "{"},
	} {
		s, _ := stub(t, http.StatusOK, c.body, nil)
		var re *RefusedError
		if _, err := stubClient(s.URL).PutOperationalIntent(ctx, oursID, "o", f3548.PutOperationalIntentReferenceParameters{}); !errors.As(err, &re) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	bad := `{"subscribers":[{"uss_base_url":"not a url","subscriptions":[]}]}`
	if err := checkSubscribers(func() []f3548.SubscriberToNotify {
		var r f3548.ChangeOperationalIntentReferenceResponse
		_ = json.Unmarshal([]byte(bad), &r)
		return r.Subscribers
	}()); err == nil {
		t.Error("a subscriber that is not a URL")
	}
	many := make([]f3548.OperationalIntentReference, MaxReferences+1)
	b, _ := json.Marshal(f3548.QueryOperationalIntentReferenceResponse{OperationalIntentReferences: many})
	s, _ := stub(t, http.StatusOK, string(b), nil)
	if _, err := stubClient(s.URL).QueryOperationalIntents(ctx, aoi); err == nil {
		t.Error("more references than the bound")
	}
	cs := make([]f3548.ConstraintReference, MaxReferences+1)
	b, _ = json.Marshal(f3548.QueryConstraintReferencesResponse{ConstraintReferences: cs})
	s, _ = stub(t, http.StatusOK, string(b), nil)
	if _, err := stubClient(s.URL).QueryConstraints(ctx, aoi); err == nil {
		t.Error("more constraints than the bound")
	}
	huge := `{"x":"` + strings.Repeat("a", MaxAnswerBytes) + `"}`
	s, _ = stub(t, http.StatusOK, huge, nil)
	if _, err := stubClient(s.URL).QueryOperationalIntents(ctx, aoi); err == nil {
		t.Error("an answer over the bound")
	}
	s, _ = stub(t, http.StatusOK, `{"status":{"uss":"x","availability":"Sideways"},"version":"1"}`, nil)
	if _, err := stubClient(s.URL).Availability(ctx, "x"); err == nil {
		t.Error("an availability that is not F3548's")
	}
	s, _ = stub(t, http.StatusOK, `{"subscription":{"id":"other","version":"v"}}`, nil)
	if _, err := stubClient(s.URL).PutSubscription(ctx, peerID, "", f3548.PutSubscriptionParameters{}); err == nil {
		t.Error("a subscription answer about another one")
	}
	s, _ = stub(t, http.StatusOK, `{"operational_intent_reference":{"id":"other"}}`, nil)
	if _, err := stubClient(s.URL).GetOperationalIntent(ctx, oursID); err == nil {
		t.Error("a reference answer about another one")
	}
}

// A 409 AirspaceConflictResponse names the references missing; one that
// is not one is a conflict with no members; one naming more than the
// bound is refused.
func TestClientConflicts(t *testing.T) {
	ctx := context.Background()
	b, _ := json.Marshal(f3548.AirspaceConflictResponse{Message: ptrS("missing"),
		MissingOperationalIntents: &[]f3548.OperationalIntentReference{{Id: peerID}}, MissingConstraints: &[]f3548.ConstraintReference{{Id: peer2ID}}})
	s, _ := stub(t, http.StatusConflict, string(b), nil)
	var ce *ConflictError
	_, err := stubClient(s.URL).PutOperationalIntent(ctx, oursID, "", f3548.PutOperationalIntentReferenceParameters{})
	if !errors.As(err, &ce) || len(ce.MissingOperationalIntents) != 1 || len(ce.MissingConstraints) != 1 || !strings.Contains(err.Error(), peerID) {
		t.Fatalf("%v", err)
	}
	s, _ = stub(t, http.StatusConflict, `not json`, nil)
	if _, err := stubClient(s.URL).DeleteOperationalIntent(ctx, oursID, "o"); !errors.As(err, &ce) || len(ce.MissingOperationalIntents) != 0 {
		t.Fatalf("%v", err)
	}
	many := make([]f3548.OperationalIntentReference, MaxReferences+1)
	b, _ = json.Marshal(f3548.AirspaceConflictResponse{MissingOperationalIntents: &many})
	s, _ = stub(t, http.StatusConflict, string(b), nil)
	var re *RefusedError
	if _, err := stubClient(s.URL).PutOperationalIntent(ctx, oursID, "", f3548.PutOperationalIntentReferenceParameters{}); !errors.As(err, &re) {
		t.Fatalf("%v", err)
	}
	s, _ = stub(t, http.StatusNotFound, `{}`, nil)
	if err := stubClient(s.URL).DeleteSubscription(ctx, peerID, "v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

// The DSS's Date header gives the clock drift; a drift beyond
// TimeSyncMaxDifferentialSeconds makes the probe degraded (E-02: and a
// small one leaves it up).
func TestClientDrift(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		skew time.Duration
		want string
	}{{0, "up"}, {time.Minute, "degraded"}} {
		date := time.Now().Add(c.skew).UTC().Format(http.TimeFormat)
		s, _ := stub(t, http.StatusOK, `{"operational_intent_references":[]}`, map[string]string{"Date": date})
		cl := stubClient(s.URL)
		if _, err := cl.QueryOperationalIntents(ctx, aoi); err != nil {
			t.Fatal(err)
		}
		r := cl.Reach()
		if !r.DriftKnown {
			t.Fatal("no drift read")
		}
		st, detail := Probe(cl, nil, nil)(ctx)
		if string(st) != c.want {
			t.Fatalf("skew %s: %s %s", c.skew, st, detail)
		}
	}
}

// Peer calls: the details read through core's UnmarshalOperationalIntent,
// another intent or no ovn refused; a constraint without volume refused;
// a notification's 5xx is a peer down, its 4xx a refusal; a base URL
// that is not one is refused before any call; a token that cannot be
// had fails the call.
func TestClientPeerCalls(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	if _, err := g.c.PeerDetails(ctx, g.peer.URL(), peerID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	for _, body := range []string{`{}`, `{"operational_intent":{"reference":{}}}`, `{"operational_intent":` + oiJSON(t, peer2ID, "o") + `}`,
		`{"operational_intent":` + oiJSON(t, peerID, "") + `}`} {
		s, _ := stub(t, http.StatusOK, body, nil)
		var re *RefusedError
		if _, err := stubClient(s.URL).PeerDetails(ctx, s.URL, peerID); !errors.As(err, &re) {
			t.Errorf("%s: %v", body, err)
		}
	}
	s, _ := stub(t, http.StatusOK, `{"operational_intent":`+oiJSON(t, peerID, "o")+`}`, nil)
	if oi, err := stubClient(s.URL).PeerDetails(ctx, s.URL, peerID); err != nil || *oi.Reference.Ovn != "o" {
		t.Fatalf("%v", err)
	}
	s, _ = stub(t, http.StatusOK, `{"constraint":{"reference":{"id":"`+peerID+`","ovn":"o"},"details":{"volumes":[]}}}`, nil)
	if _, err := stubClient(s.URL).PeerConstraint(ctx, s.URL, peerID); err == nil {
		t.Fatal("a constraint without volume")
	}
	if _, err := g.c.PeerConstraint(ctx, "not a url", peerID); err == nil {
		t.Fatal("a base URL that is not one")
	}
	for status, want := range map[int]bool{http.StatusBadGateway: true, http.StatusBadRequest: false} {
		s, _ := stub(t, status, `{}`, nil)
		err := stubClient(s.URL).Notify(ctx, s.URL, f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: oursID})
		if errors.Is(err, ErrDSSDown) != want || err == nil {
			t.Errorf("%d: %v", status, err)
		}
	}
	if err := g.c.Notify(ctx, "http://127.0.0.1:1", f3548.PutOperationalIntentDetailsParameters{}); !errors.Is(err, ErrDSSDown) {
		t.Fatal(err)
	}
	c := stubClient(g.dss.URL())
	c.Tokens = &tokens{fail: errBoom}
	if _, err := c.QueryOperationalIntents(ctx, aoi); err == nil {
		t.Fatal("a call without a token")
	}
	c.Tokens = nil
	if _, err := c.QueryOperationalIntents(ctx, aoi); err == nil {
		t.Fatal("a call without a token client")
	}
	if _, err := (&Client{}).QueryOperationalIntents(ctx, aoi); err == nil {
		t.Fatal("a client without a DSS")
	}
	// Every call names the target's host as its audience (M18).
	tk := &tokens{sub: ourManager}
	c = &Client{DSSBaseURL: g.dss.URL(), Tokens: tk}
	_, _ = c.QueryOperationalIntents(ctx, aoi)
	_, _ = c.PeerDetails(ctx, g.peer.URL(), peerID)
	if len(tk.auds) != 2 || tk.auds[0] != g.dss.URL() || tk.auds[1] != g.peer.URL() {
		t.Fatalf("audiences %v", tk.auds)
	}
}

// oiJSON is an operational intent of id with ovn.
func oiJSON(t *testing.T, id, ovn string) string {
	t.Helper()
	v := []f3548.Volume4D{volume(41.7, 44.8)}
	off := []f3548.Volume4D{}
	ref := f3548.OperationalIntentReference{Id: id, Manager: peerManager, State: f3548.Accepted, Version: 1, UssAvailability: f3548.Normal,
		UssBaseUrl: "https://peer.example", SubscriptionId: peer2ID, TimeStart: *v[0].TimeStart, TimeEnd: *v[0].TimeEnd}
	if ovn != "" {
		ref.Ovn = &ovn
	}
	b, _ := json.Marshal(f3548.OperationalIntent{Reference: ref, Details: f3548.OperationalIntentDetails{Volumes: &v, OffNominalVolumes: &off}})
	return string(b)
}

// FuzzConflictAnswer: whatever a DSS answers to a write, outcome never
// panics and a 409 is a conflict or a refusal, never a success.
func FuzzConflictAnswer(f *testing.F) {
	f.Add(409, []byte(`{"message":"m","missing_operational_intents":[{"id":"x"}]}`))
	f.Add(409, []byte(`{"missing_constraints":[{}]}`))
	f.Add(200, []byte(`{}`))
	f.Add(404, []byte(``))
	f.Fuzz(func(t *testing.T, status int, body []byte) {
		if status < 100 || status > 599 {
			return
		}
		err := outcome(answer{status: status, body: body})
		if status == http.StatusOK && err != nil {
			t.Fatal(err)
		}
		if status == http.StatusConflict {
			var ce *ConflictError
			var re *RefusedError
			if !errors.As(err, &ce) && !errors.As(err, &re) {
				t.Fatalf("409 read as %v", err)
			}
		}
	})
}

// Every call the client makes carries a token whose scopes meet one of
// the requirements the pinned F3548 file's `security` names for the
// operation (E-03: read from the file, not from memory; the lab DSS
// refused a constraint query made with utm.strategic_coordination).
func TestClientScopesMeetTheStandard(t *testing.T) {
	raw, err := os.ReadFile("../../api/standards/f3548-v21.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	type seen struct{ method, path, scope string }
	var mu sync.Mutex
	var calls []seen
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		var c struct {
			Scope string `json:"scope"`
		}
		if len(parts) == 3 {
			b, _ := base64.RawURLEncoding.DecodeString(parts[1])
			_ = json.Unmarshal(b, &c)
		}
		mu.Lock()
		calls = append(calls, seen{r.Method, r.URL.Path, c.Scope})
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer s.Close()
	c := stubClient(s.URL)
	ctx := context.Background()
	_, _ = c.QueryOperationalIntents(ctx, aoi)
	_, _ = c.QueryConstraints(ctx, aoi)
	_, _ = c.GetOperationalIntent(ctx, oursID)
	for _, st := range f3548.DSSStates {
		_, _ = c.PutOperationalIntent(ctx, oursID, "", f3548.PutOperationalIntentReferenceParameters{State: st})
		_, _ = c.PutOperationalIntent(ctx, oursID, "o", f3548.PutOperationalIntentReferenceParameters{State: st})
	}
	_, _ = c.DeleteOperationalIntent(ctx, oursID, "o")
	_, _ = c.PutSubscription(ctx, peerID, "", f3548.PutSubscriptionParameters{})
	_, _ = c.PutSubscription(ctx, peerID, "v", f3548.PutSubscriptionParameters{})
	_, _ = c.GetSubscription(ctx, peerID)
	_ = c.DeleteSubscription(ctx, peerID, "v")
	_, _ = c.Availability(ctx, ourManager)
	_, _ = c.PeerDetails(ctx, s.URL, peerID)
	_, _ = c.PeerConstraint(ctx, s.URL, peerID)
	_ = c.Notify(ctx, s.URL, f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: oursID})
	if len(calls) < 18 {
		t.Fatalf("%d calls", len(calls))
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, call := range calls {
		var reqs []map[string][]string
		found := false
		for tmpl, ops := range doc.Paths {
			re := regexp.MustCompile("^" + param.ReplaceAllString(tmpl, "[^/]+") + "$")
			node, ok := ops[strings.ToLower(call.method)]
			if !ok || !re.MatchString(call.path) {
				continue
			}
			var op struct {
				Security []map[string][]string `yaml:"security"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatal(err)
			}
			reqs, found = op.Security, true
		}
		if !found {
			t.Errorf("%s %s is no operation of the file", call.method, call.path)
			continue
		}
		have := strings.Fields(call.scope)
		ok := false
		for _, r := range reqs {
			if !slices.ContainsFunc(r["Authority"], func(s string) bool { return !slices.Contains(have, s) }) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s %s with %q meets none of %v", call.method, call.path, call.scope, reqs)
		}
	}
}
