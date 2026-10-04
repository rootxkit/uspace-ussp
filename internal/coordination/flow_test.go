package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/ansp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

func controlled(v bool) AirspaceSource {
	return func() (Airspaces, bool) {
		return Airspaces{Version: "3", Controlled: map[string]*bool{"UA1": &v}}, true
	}
}

func newNotifier(st *memStore, as AirspaceSource) *Notifier {
	return &Notifier{Store: st, Airspaces: as, SystemID: testSystem, Counters: &core.Counters{}, Now: func() time.Time { return sentAt }}
}

func transition(stateID int64, state string) Transition {
	d := testDeviation(state, "outside_volume_h")
	d.StateID = stateID
	return Transition{Deviation: *d, Intent: testIntent()}
}

// A nonconforming and a contingent transition each queue their notice
// once; a second sweep queues nothing more (idempotent by notice_ref).
func TestNotifierQueuesDeviationsOnce(t *testing.T) {
	st := newMemStore()
	st.trs = []Transition{transition(1, "nonconforming"), transition(2, "contingent"), transition(3, "conforming")}
	n := newNotifier(st, controlled(true))
	for range 2 {
		if err := n.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.notices) != 2 || st.notices[0].Kind != KindNonconformance || st.notices[1].Kind != KindContingent ||
		st.notices[0].StateID != 1 || len(st.notices[0].Body) == 0 || n.Counters.Get(CounterQueued) != 2 {
		t.Fatalf("notices %+v counters %v", st.notices, n.Counters.Snapshot())
	}
}

// E-01 pair over the check: an activated intent in a controlled U-space
// airspace queues an intent_notice with its check; one in uncontrolled
// airspace only its check; an ended one in controlled airspace both of
// its notices, in order; the end of a checked intent queues ended.
func TestNotifierChecksIntents(t *testing.T) {
	st := newMemStore()
	a, b, c := testIntent(), testIntent(), testIntent()
	a.ID, b.ID, c.ID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	c.LocalState = "ended"
	st.cands = []Candidate{{Intent: a, AirspaceIDs: []string{"UA1"}}, {Intent: b, AirspaceIDs: []string{"UA2"}}, {Intent: c, AirspaceIDs: []string{"UA1"}, Ended: true}}
	as := func() (Airspaces, bool) {
		return Airspaces{Version: "3", Controlled: map[string]*bool{"UA1": ptr(true), "UA2": ptr(false)}}, true
	}
	n := newNotifier(st, as)
	if err := n.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	kinds := func(id string) (out []string) {
		for _, x := range st.notices {
			if x.IntentID == id {
				out = append(out, string(x.Kind))
			}
		}
		return out
	}
	if !st.checks[a.ID].Controlled || st.checks[b.ID].Controlled || strings.Join(kinds(a.ID), ",") != "intent_notice" ||
		len(kinds(b.ID)) != 0 || strings.Join(kinds(c.ID), ",") != "intent_notice,ended" {
		t.Fatalf("checks %+v, a %v b %v c %v", st.checks, kinds(a.ID), kinds(b.ID), kinds(c.ID))
	}
	// a ends: the next sweep queues its ended notice, once.
	a.LocalState = "ended"
	st.intents[a.ID], st.intents[b.ID] = a, func() Intent { x := b; x.LocalState = "ended"; return x }()
	for range 2 {
		if err := n.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(kinds(a.ID), ",") != "intent_notice,ended" || len(kinds(b.ID)) != 0 {
		t.Fatalf("after the end: a %v b %v", kinds(a.ID), kinds(b.ID))
	}
	if n.Counters.Get(CounterChecked) != 3 || n.Counters.Get(CounterControlled) != 2 {
		t.Errorf("counters %v", n.Counters.Snapshot())
	}
}

// Without an installed U-space airspace version nothing is checked or
// queued, and it is counted; once the version is there the check runs.
func TestNotifierWaitsForTheCIS(t *testing.T) {
	st := newMemStore()
	st.cands = []Candidate{{Intent: testIntent(), AirspaceIDs: []string{"UA1"}}}
	loaded := false
	n := newNotifier(st, func() (Airspaces, bool) {
		if !loaded {
			return Airspaces{}, false
		}
		return controlled(true)()
	})
	if err := n.Sweep(context.Background()); err != nil || len(st.checks) != 0 || n.Counters.Get(CounterCISUnavailable) != 1 {
		t.Fatalf("checked without a CIS: %v %v", err, st.checks)
	}
	loaded = true
	if err := n.Sweep(context.Background()); err != nil || len(st.checks) != 1 || len(st.notices) != 1 {
		t.Fatalf("not checked with a CIS: %v %v", err, st.checks)
	}
}

// A notice that cannot be built is queued failed with its reason (the
// console shows it), never dropped; a store that fails is an error.
func TestNotifierQueuesUnbuildableNoticesFailed(t *testing.T) {
	st := newMemStore()
	tr := transition(9, "nonconforming")
	tr.Intent.Volumes = json.RawMessage(`[]`)
	st.trs = []Transition{tr}
	n := newNotifier(st, controlled(true))
	if err := n.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.notices) != 1 || st.notices[0].state != "failed" || !strings.Contains(st.notices[0].lastError, "0 volumes") ||
		n.Counters.Get(CounterBuildFailed) != 1 {
		t.Fatalf("%+v", st.notices)
	}
	st.fail = errStore
	if err := n.Sweep(context.Background()); !errors.Is(err, errStore) {
		t.Fatalf("store failure not returned: %v", err)
	}
}

type flow struct {
	st    *memStore
	fake  *ansp.Fake
	s     *Sender
	pol   policy.Values
	ctx   context.Context
	queue func(tr Transition)
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	fake, err := ansp.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	client, err := NewClient(fake.URL(), authority.Tokens{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &flow{st: newMemStore(), fake: fake, pol: policy.Defaults(), ctx: context.Background()}
	f.s = &Sender{Store: f.st, ANSP: client, Policy: func() policy.Values { return f.pol }, Counters: &core.Counters{}}
	n := newNotifier(f.st, controlled(true))
	f.queue = func(tr Transition) {
		f.st.trs = append(f.st.trs, tr)
		if err := n.Sweep(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// The S-M2 path at the unit level: a nonconforming transition reaches the
// fake ANSP with its kind and numbers, ats_notified_at is stored, the
// fake acknowledges and the next read stores ats_ack_ref (E-01: before
// the acknowledgement the read stores nothing).
func TestSendAndAcknowledge(t *testing.T) {
	f := newFlow(t)
	f.queue(transition(5, "nonconforming"))
	if n := f.s.SendDue(f.ctx); n != 1 {
		t.Fatalf("sent %d", n)
	}
	got := f.fake.Notices()
	if len(got) != 1 || got[0].Kind != "nonconformance" || !strings.Contains(string(got[0].Body), `"distance_outside_m":73.5`) ||
		got[0].Authorization != "Bearer "+authority.Token {
		t.Fatalf("at the ANSP: %+v", got)
	}
	x := f.st.notice(Ref(testSystem, KindNonconformance, testIntentID, 5))
	if x.state != "received" || x.ackID != got[0].AckID || f.st.notified[5].IsZero() || x.pollNext == nil {
		t.Fatalf("receipt not stored: %+v", x)
	}
	f.st.advance(seconds(f.pol.ATSAckPollS))
	if f.s.PollDue(f.ctx) != 0 || f.st.ackRefs[5] != "" {
		t.Fatal("acknowledged before the ANSP said so")
	}
	f.fake.Acknowledge(got[0].AckID, "supervisor")
	f.st.advance(seconds(f.pol.ATSAckPollS))
	if f.s.PollDue(f.ctx) != 1 || f.st.ackRefs[5] != got[0].AckID {
		t.Fatalf("acknowledgement not stored: %v", f.st.ackRefs)
	}
	if x := f.st.notice(Ref(testSystem, KindNonconformance, testIntentID, 5)); x.state != "acknowledged" || x.ackedBy != "supervisor" {
		t.Fatalf("%+v", x)
	}
	if f.s.Counters.Get(CounterSent) != 1 || f.s.Counters.Get(CounterAcknowledged) != 1 {
		t.Errorf("counters %v", f.s.Counters.Snapshot())
	}
}

// Not acknowledged: escalated after ats_ack_escalate_s, still read (at
// most once a minute) and, once acknowledged after all, stored.
func TestUnacknowledgedNoticeEscalates(t *testing.T) {
	f := newFlow(t)
	f.queue(transition(6, "contingent"))
	f.s.SendDue(f.ctx)
	ref := Ref(testSystem, KindContingent, testIntentID, 6)
	f.st.advance(seconds(f.pol.ATSAckEscalateS) - time.Second)
	if f.s.EscalateDue(f.ctx) != 0 {
		t.Fatal("escalated before ats_ack_escalate_s")
	}
	f.st.advance(time.Second)
	if f.s.EscalateDue(f.ctx) != 1 || f.st.notice(ref).state != "escalated" || f.s.Counters.Get(CounterEscalated) != 1 {
		t.Fatalf("not escalated: %+v", f.st.notice(ref))
	}
	items, _, _ := f.st.Open(f.ctx, 10)
	if len(items) != 1 || items[0].State != "escalated" || items[0].AgeS < f.pol.ATSAckEscalateS {
		t.Fatalf("console %+v", items)
	}
	f.fake.Acknowledge(f.fake.Notices()[0].AckID, "supervisor")
	f.st.advance(time.Minute)
	if f.s.PollDue(f.ctx) != 1 || f.st.notice(ref).state != "acknowledged" {
		t.Fatalf("late acknowledgement not stored: %+v", f.st.notice(ref))
	}
}

// An informational notice is received and never read back.
func TestInformationalNoticeIsNotPolled(t *testing.T) {
	f := newFlow(t)
	f.st.cands = []Candidate{{Intent: testIntent(), AirspaceIDs: []string{"UA1"}}}
	if err := newNotifier(f.st, controlled(true)).Sweep(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.s.SendDue(f.ctx) != 1 {
		t.Fatal("intent_notice not sent")
	}
	x := f.st.notice(Ref(testSystem, KindIntentNotice, testIntentID, 0))
	if x.state != "received" || x.pollNext != nil {
		t.Fatalf("%+v", x)
	}
	f.st.advance(time.Hour)
	if f.s.EscalateDue(f.ctx) != 0 || f.s.PollDue(f.ctx) != 0 {
		t.Fatal("an informational notice was escalated or polled")
	}
}

// The ANSP down: the notice is tried again with backoff, counted, and
// stays on the console as pending with the error; back up, it is
// received (E-02).
func TestANSPDownRetriesThenDelivers(t *testing.T) {
	f := newFlow(t)
	f.queue(transition(7, "nonconforming"))
	f.fake.Down()
	ref := Ref(testSystem, KindNonconformance, testIntentID, 7)
	for i := 1; i <= 3; i++ {
		if f.s.SendDue(f.ctx) != 0 {
			t.Fatal("received while down")
		}
		x := f.st.notice(ref)
		if x.state != "pending" || x.attempts != i || !strings.Contains(x.lastError, "503") {
			t.Fatalf("try %d: %+v", i, x)
		}
		if f.s.SendDue(f.ctx) != 0 || f.st.notice(ref).attempts != i {
			t.Fatal("tried again before its backoff")
		}
		f.st.advance(f.s.Backoff(i))
	}
	items, _, _ := f.st.Open(f.ctx, 10)
	if len(items) != 1 || items[0].State != "pending" || items[0].LastError == nil || f.s.Counters.Get(CounterRetried) != 3 {
		t.Fatalf("console %+v counters %v", items, f.s.Counters.Snapshot())
	}
	state, detail := Probe(f.st, "")(f.ctx)
	if state != obs.StateDegraded || !strings.Contains(detail, "1 pending") || !strings.Contains(detail, "1 retrying") {
		t.Errorf("probe %s %s", state, detail)
	}
	f.fake.Up()
	if f.s.SendDue(f.ctx) != 1 || f.st.notice(ref).state != "received" {
		t.Fatalf("not delivered once up: %+v", f.st.notice(ref))
	}
	if state, _ := Probe(f.st, "")(f.ctx); state != obs.StateUp {
		t.Errorf("probe %s after delivery", state)
	}
}

// Bounded retries: after MaxAttempts tries the notice fails for good
// (E-10: the bound is exceeded here).
func TestRetriesAreBounded(t *testing.T) {
	f := newFlow(t)
	f.s.MaxAttempts = 3
	f.queue(transition(8, "nonconforming"))
	f.fake.Down()
	for range 5 {
		f.s.SendDue(f.ctx)
		f.st.advance(time.Hour)
	}
	x := f.st.notice(Ref(testSystem, KindNonconformance, testIntentID, 8))
	if x.state != "failed" || x.attempts != 3 || !strings.Contains(x.lastError, "gave up after 3 tries") || f.s.Counters.Get(CounterGaveUp) != 1 {
		t.Fatalf("%+v", x)
	}
}

// A repeat of a notice_ref with the same body is the first receipt
// (200); with another body the ANSP refuses 409 and the notice fails
// for good, counted (E-01 pair).
func TestRepeatAndConflict(t *testing.T) {
	f := newFlow(t)
	f.queue(transition(10, "nonconforming"))
	body := f.st.notice(Ref(testSystem, KindNonconformance, testIntentID, 10)).Body
	r1, err := f.s.ANSP.Submit(f.ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if f.s.SendDue(f.ctx) != 1 || f.s.Counters.Get(CounterRepeats) != 1 || f.st.notice(Ref(testSystem, KindNonconformance, testIntentID, 10)).ackID != r1.AckID {
		t.Fatalf("repeat not answered with the first receipt: %v", f.s.Counters.Snapshot())
	}
	other := []byte(strings.Replace(string(body), `"distance_outside_m":73.5`, `"distance_outside_m":80`, 1))
	_, err = f.s.ANSP.Submit(f.ctx, other)
	var perm *PermanentError
	if !errors.As(err, &perm) || perm.Status != http.StatusConflict || !strings.Contains(perm.Detail, "notice_ref_reused") {
		t.Fatalf("409 not permanent: %v", err)
	}
	// The same refusal through the outbox fails the notice for good.
	f.st.notices[0].state, f.st.notices[0].Body = "pending", other
	f.st.advance(time.Hour)
	f.s.SendDue(f.ctx)
	x := f.st.notice(Ref(testSystem, KindNonconformance, testIntentID, 10))
	if x.state != "failed" || f.s.Counters.Get(CounterConflict) != 1 {
		t.Fatalf("%+v", x)
	}
}

// One intent's notices go out in order: the second waits while the
// first is not received.
func TestOneIntentInOrder(t *testing.T) {
	f := newFlow(t)
	f.queue(transition(11, "nonconforming"))
	f.queue(transition(12, "contingent"))
	f.fake.Down()
	f.s.SendDue(f.ctx)
	if f.st.notice(Ref(testSystem, KindContingent, testIntentID, 12)).attempts != 0 {
		t.Fatal("the second notice was tried before the first was received")
	}
	f.fake.Up()
	f.st.advance(time.Hour)
	f.s.SendDue(f.ctx)
	f.st.advance(time.Hour)
	f.s.SendDue(f.ctx)
	got := f.fake.Notices()
	if len(got) != 2 || got[0].Kind != "nonconformance" || got[1].Kind != "contingent" {
		t.Fatalf("order %+v", got)
	}
}

// Without an ANSP configured nothing is claimed, and Probe says why.
func TestNoANSPConfigured(t *testing.T) {
	f := newFlow(t)
	f.s.ANSP = nil
	f.queue(transition(13, "nonconforming"))
	if f.s.SendDue(f.ctx) != 0 || f.s.PollDue(f.ctx) != 0 || f.st.notices[0].attempts != 0 {
		t.Fatal("claimed without an ANSP")
	}
	state, detail := Probe(f.st, "USSP_ANSP_BASE_URL is not set")(f.ctx)
	if state != obs.StateDegraded || !strings.Contains(detail, "USSP_ANSP_BASE_URL") {
		t.Errorf("probe %s %s", state, detail)
	}
	// Nothing queued: up, and the missing ANSP still said (E-01 pair).
	if state, detail := Probe(newMemStore(), "USSP_ANSP_BASE_URL is not set")(f.ctx); state != obs.StateUp || !strings.Contains(detail, "USSP_ANSP_BASE_URL") {
		t.Errorf("idle probe %s %s", state, detail)
	}
	f.st.fail = errStore
	if state, _ := Probe(f.st, "")(f.ctx); state != obs.StateUnknown {
		t.Errorf("an unreadable store is %s, not unknown", state)
	}
}

func TestBackoffAndNextPoll(t *testing.T) {
	s := &Sender{}
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if got := s.Backoff(i + 1); got != want {
			t.Errorf("backoff %d = %v", i+1, got)
		}
	}
	if s.Backoff(100) != DefaultBackoffMax {
		t.Error("backoff not capped")
	}
	pol := policy.Defaults()
	if NextPoll(Polled{State: "received"}, pol) != 10*time.Second {
		t.Error("received")
	}
	if NextPoll(Polled{State: "escalated", SinceReceived: 6 * time.Minute}, pol) != EscalatedPollMin {
		t.Error("escalated")
	}
	if NextPoll(Polled{State: "escalated", SinceReceived: 2 * time.Hour}, pol) != 0 {
		t.Error("escalated past its read window")
	}
}

// The client against misbehaving servers: every status is classified,
// a redirect is not followed, an oversized or malformed answer is an
// error, Retry-After is honoured, and the token source failing is
// retryable.
func TestClientClassifies(t *testing.T) {
	var status int
	var body string
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header()[k] = v
		}
		if status == http.StatusFound {
			w.Header().Set("Location", "https://elsewhere.example/")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, authority.Tokens{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status    int
		body      string
		permanent bool
		retry     time.Duration
	}{
		{400, `{"type":"https://schemas.uspace.ge/problems/validation","detail":"bad"}`, true, 0},
		{403, ``, true, 0}, {413, ``, true, 0}, {415, ``, true, 0},
		{401, ``, false, 0}, {429, ``, false, 7 * time.Second}, {500, ``, false, 0}, {503, ``, false, 0}, {http.StatusFound, ``, false, 0},
		{202, `{"ack_id":"","state":"received","received_at":"2026-10-04T12:00:00Z"}`, false, 0},
		{202, strings.Repeat(" ", MaxAnswerBytes+1), false, 0},
	} {
		status, body, header = tc.status, tc.body, http.Header{}
		if tc.retry > 0 {
			header.Set("Retry-After", "7")
		}
		_, err := c.Submit(context.Background(), []byte(`{}`))
		var perm *PermanentError
		var retry *RetryableError
		switch {
		case tc.permanent && !errors.As(err, &perm):
			t.Errorf("%d: %v, want permanent", tc.status, err)
		case !tc.permanent && !errors.As(err, &retry):
			t.Errorf("%d: %v, want retryable", tc.status, err)
		case !tc.permanent && retry.RetryAfter != tc.retry:
			t.Errorf("%d: retry after %v", tc.status, retry.RetryAfter)
		}
		_, gerr := c.Get(context.Background(), "01K6P3Q8Y2D6W4Z1V7R5T9X3MB")
		if gerr == nil {
			t.Errorf("%d: Get accepted", tc.status)
		}
	}
	status, body = 200, `{"ack_id":"A","kind":"nonconformance","sender_client_id":"s","ussp_id":"u","notice_ref":"r","intent_refs":[],"authorisation_numbers":[],"received_at":"2026-10-04T12:00:00Z","state":"lost"}`
	if _, err := c.Get(context.Background(), "A"); err == nil {
		t.Error("an unknown notice state was accepted")
	}
	if _, err := c.Submit(context.Background(), []byte(`{}`)); err == nil {
		t.Error("a 200 that is not a receipt was accepted")
	}
	bad, _ := NewClient(srv.URL, failingTokens{}, nil)
	var retry *RetryableError
	if _, err := bad.Submit(context.Background(), []byte(`{}`)); !errors.As(err, &retry) || !strings.Contains(err.Error(), "no token") {
		t.Errorf("token failure: %v", err)
	}
	if _, err := bad.Get(context.Background(), "A"); !errors.As(err, &retry) {
		t.Errorf("token failure on Get: %v", err)
	}
	for _, u := range []string{"", "ftp://x", "https://u:p@x", "https://x/?q=1", "https://x/#f"} {
		if _, err := NewClient(u, authority.Tokens{}, nil); err == nil {
			t.Errorf("base URL %q accepted", u)
		}
	}
	if _, err := NewClient(srv.URL, nil, nil); err == nil {
		t.Error("no token source accepted")
	}
	srv.Close()
	if _, err := c.Submit(context.Background(), []byte(`{}`)); !errors.As(err, &retry) || retry.Status != 0 || !strings.Contains(err.Error(), "not reached") {
		t.Errorf("unreachable: %v", err)
	}
}

type failingTokens struct{}

func (failingTokens) Token(context.Context, string, ...string) (string, error) {
	return "", errors.New("token service down")
}

// Run sends and polls on its own until the context ends.
func TestRunLoops(t *testing.T) {
	f := newFlow(t)
	f.queue(transition(14, "nonconforming"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.s.Run(ctx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.fake.Notices()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(f.fake.Notices()) != 1 {
		t.Fatal("Run did not send")
	}
	n := newNotifier(newMemStore(), controlled(true))
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	n.Run(ctx2, time.Millisecond)
}
