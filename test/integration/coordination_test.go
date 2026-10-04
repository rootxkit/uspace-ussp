//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/coordination"
	coordstore "github.com/rootxkit/uspace-ussp/internal/coordination/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/ansp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

// coordRig runs api's Notifier and Sender against the real relational
// database and the fake ANSP. The acknowledgement is read every second
// and escalated after 3 s instead of the policy's 10 s and 5 min, so the
// suite does not wait five minutes: the timing is the test's, the
// unit tests run the defaults on a simulated clock.
type coordRig struct {
	st       coordstore.Store
	fake     *ansp.Fake
	counters *core.Counters
	notifier *coordination.Notifier
	cancel   context.CancelFunc
}

func newCoordRig(t *testing.T, controlled bool) *coordRig {
	t.Helper()
	ensureSchemas(t)
	fake, err := ansp.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	client, err := coordination.NewClient(fake.URL(), authority.Tokens{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := &coordRig{st: coordstore.Store{S: appStore(t)}, fake: fake, counters: &core.Counters{}}
	pol := policy.Defaults()
	pol.ATSAckPollS, pol.ATSAckEscalateS = 1, 3
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	t.Cleanup(cancel)
	as := func() (coordination.Airspaces, bool) {
		return coordination.Airspaces{Version: "3", Controlled: map[string]*bool{"UA-WP15": &controlled}}, true
	}
	n := &coordination.Notifier{Store: g.st, Airspaces: as, SystemID: "USSP-DEV", Counters: g.counters, Logger: quiet()}
	g.notifier = n
	s := &coordination.Sender{Store: g.st, ANSP: client, Policy: func() policy.Values { return pol }, Counters: g.counters, Logger: quiet()}
	go n.Run(ctx, 200*time.Millisecond)
	go s.Run(ctx, 200*time.Millisecond)
	return g
}

func (g *coordRig) noticeOf(ref string) (ansp.Notice, bool) {
	ns := g.fake.Notices()
	for i := range ns {
		if ns[i].Ref == ref {
			return ns[i], true
		}
	}
	return ansp.Notice{}, false
}

func atsFacts(t *testing.T, stateID int64) (notified *time.Time, ackRef *string) {
	t.Helper()
	if err := appPool(t).QueryRow(context.Background(), "SELECT ats_notified_at, ats_ack_ref FROM conformance_states WHERE id = $1", stateID).Scan(&notified, &ackRef); err != nil {
		t.Fatal(err)
	}
	return notified, ackRef
}

// noticeState is the notice's state, tries and last error; state "" while
// it is not queued yet.
func noticeState(t *testing.T, ref string) (state string, attempts int, lastError *string) {
	t.Helper()
	err := appPool(t).QueryRow(context.Background(), "SELECT state, attempts, last_error FROM coordination_notices WHERE notice_ref = $1", ref).Scan(&state, &attempts, &lastError)
	if err != nil && !store.IsNoRows(err) {
		t.Fatalf("notice %s: %v", ref, err)
	}
	return state, attempts, lastError
}

// S-M2 at the fakes (E-01, E-02): a nonconforming transition reaches the
// ANSP within 5 s with its kind and numbers, ats_notified_at is stored;
// the ANSP acknowledges and ats_ack_ref is stored. A second flight whose
// notice nobody acknowledges is escalated, listed for the console and
// /readyz says so. A conforming state queues nothing (the absence twin).
func TestIntegrationCoordinationNonconformance(t *testing.T) {
	g := newCoordRig(t, true)
	ctx := context.Background()
	a := seedFlight(t, "activated", nil, time.Now().Add(-5*time.Minute))
	seedState(t, a.flightID, "conforming", "", time.Now().Add(-time.Minute), 0, 0)
	stateA := seedState(t, a.flightID, "nonconforming", "above_upper", time.Now(), 3.5, 21.25)
	refA := coordination.Ref("USSP-DEV", coordination.KindNonconformance, a.intentID, stateA)
	start := time.Now()
	waitFor(t, 5*time.Second, "the nonconformance notice at the ANSP", func() bool { _, ok := g.noticeOf(refA); return ok })
	t.Logf("nonconformance notice received by the fake ANSP %v after the state was recorded", time.Since(start).Round(time.Millisecond))
	got, _ := g.noticeOf(refA)
	body := string(got.Body)
	for _, want := range []string{`"kind":"nonconformance"`, `"distance_outside_m":3.5`, `"height_over_m":21.25`, `"reason":"threshold_exceeded"`,
		`"lat":41.7155`, `"state":"Nonconforming"`, a.intentID} {
		if !strings.Contains(body, want) {
			t.Errorf("notice lacks %s: %s", want, body)
		}
	}
	waitFor(t, 5*time.Second, "ats_notified_at", func() bool { n, _ := atsFacts(t, stateA); return n != nil })
	if g.fake.Acknowledge(got.AckID, "supervisor") != true {
		t.Fatal("fake acknowledge")
	}
	waitFor(t, 5*time.Second, "ats_ack_ref", func() bool { _, r := atsFacts(t, stateA); return r != nil && *r == got.AckID })
	if st, _, _ := noticeState(t, refA); st != "acknowledged" {
		t.Fatalf("notice %s", st)
	}

	// Not acknowledged: escalated after 3 s (the rig's figure), on the
	// console with its age and on /readyz.
	b := seedFlight(t, "activated", nil, time.Now().Add(-5*time.Minute))
	stateB := seedState(t, b.flightID, "contingent", "outside_volume_h", time.Now(), 80, 0)
	refB := coordination.Ref("USSP-DEV", coordination.KindContingent, b.intentID, stateB)
	waitFor(t, 5*time.Second, "the contingent notice at the ANSP", func() bool { _, ok := g.noticeOf(refB); return ok })
	waitFor(t, 10*time.Second, "the escalation", func() bool { st, _, _ := noticeState(t, refB); return st == "escalated" })
	items, _, err := g.st.Open(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.NoticeRef == refB && it.State == "escalated" && it.AgeS >= 3 && it.EscalatedAt != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("escalated notice not on the console: %+v", items)
	}
	if state, detail := coordination.Probe(g.st, "")(ctx); state != obs.StateDegraded || !strings.Contains(detail, "escalated") {
		t.Errorf("readyz %s %s", state, detail)
	}
	if _, r := atsFacts(t, stateB); r != nil {
		t.Errorf("ats_ack_ref set without an acknowledgement: %v", *r)
	}

	// Absence twin: a flight that stays conforming queues nothing.
	c := seedFlight(t, "activated", nil, time.Now().Add(-5*time.Minute))
	seedState(t, c.flightID, "conforming", "", time.Now(), 0, 0)
	// One sweep that has seen the state: a notice would be queued by its end.
	if err := g.notifier.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, appPool(t), "SELECT count(*) FROM coordination_notices WHERE intent_id = $1::uuid", c.intentID); n != 0 {
		t.Errorf("a conforming flight queued %d notices", n)
	}
	// The append-only rule holds for every other column.
	if _, err := appPool(t).Exec(ctx, "UPDATE conformance_states SET state = 'unknown' WHERE id = $1", stateA); err == nil {
		t.Error("conformance_states.state became updatable")
	}
}

// The ANSP down: the notice is tried again with backoff and counted,
// listed for the console with its last error; up again, it is received
// (E-02).
func TestIntegrationCoordinationANSPDown(t *testing.T) {
	g := newCoordRig(t, true)
	g.fake.Down()
	a := seedFlight(t, "activated", nil, time.Now().Add(-5*time.Minute))
	stateA := seedState(t, a.flightID, "nonconforming", "threshold_exceeded", time.Now(), 120, 0)
	ref := coordination.Ref("USSP-DEV", coordination.KindNonconformance, a.intentID, stateA)
	waitFor(t, 10*time.Second, "two failed tries", func() bool { _, n, _ := noticeState(t, ref); return n >= 2 })
	st, n, lastErr := noticeState(t, ref)
	if st != "pending" || lastErr == nil || !strings.Contains(*lastErr, "503") || g.counters.Get(coordination.CounterRetried) < 2 {
		t.Fatalf("while down: %s %d %v %v", st, n, lastErr, g.counters.Snapshot())
	}
	items, _, err := g.st.Open(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, it := range items {
		listed = listed || (it.NoticeRef == ref && it.State == "pending" && it.LastError != nil && it.NextAt != nil)
	}
	if !listed {
		t.Fatalf("undelivered notice not on the console: %+v", items)
	}
	if state, detail := coordination.Probe(g.st, "")(context.Background()); state != obs.StateDegraded || !strings.Contains(detail, "retrying") {
		t.Errorf("readyz %s %s", state, detail)
	}
	g.fake.Up()
	waitFor(t, 15*time.Second, "delivery once the ANSP is back", func() bool { s, _, _ := noticeState(t, ref); return s == "received" })
	if notified, _ := atsFacts(t, stateA); notified == nil {
		t.Error("ats_notified_at not stored after the late delivery")
	}
}

// Intents: an activated intent in controlled U-space airspace is told
// with intent_notice and, once it ends, ended; one in uncontrolled
// airspace is checked and told nothing (E-01 pair).
func TestIntegrationCoordinationIntentNotices(t *testing.T) {
	g := newCoordRig(t, true)
	a := seedFlight(t, "activated", []string{"UA-WP15"}, time.Now().Add(-5*time.Minute))
	refStart := coordination.Ref("USSP-DEV", coordination.KindIntentNotice, a.intentID, 0)
	waitFor(t, 5*time.Second, "intent_notice", func() bool { _, ok := g.noticeOf(refStart); return ok })
	endIntent(t, a.intentID)
	refEnd := coordination.Ref("USSP-DEV", coordination.KindEnded, a.intentID, 0)
	waitFor(t, 5*time.Second, "ended", func() bool { _, ok := g.noticeOf(refEnd); return ok })
	if st, _, _ := noticeState(t, refStart); st != "received" {
		t.Errorf("an informational notice is %s", st)
	}

	g.cancel()
	h := newCoordRig(t, false)
	b := seedFlight(t, "activated", []string{"UA-WP15"}, time.Now().Add(-5*time.Minute))
	waitFor(t, 5*time.Second, "the check", func() bool {
		return count(t, appPool(t), "SELECT count(*) FROM coordination_checks WHERE intent_id = $1::uuid AND NOT controlled", b.intentID) == 1
	})
	if n := count(t, appPool(t), "SELECT count(*) FROM coordination_notices WHERE intent_id = $1::uuid", b.intentID); n != 0 {
		t.Errorf("an intent in uncontrolled airspace queued %d notices", n)
	}
	if len(h.fake.Notices()) != 0 {
		t.Errorf("the ANSP was told of an intent in uncontrolled airspace: %+v", h.fake.Notices())
	}
}
