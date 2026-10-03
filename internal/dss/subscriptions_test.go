package dss

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

func subsRig(t *testing.T) (*rig, *Subscriptions, *[]Area, *bool) {
	t.Helper()
	g := newRig(t)
	g.st.now = time.Now().UTC()
	areas := &[]Area{{ID: "TUA001", Box: geodesy.BBox{MinLat: 41.6, MinLon: 44.7, MaxLat: 41.8, MaxLon: 44.9}}}
	known := new(bool)
	*known = true
	s := &Subscriptions{Client: g.c, Store: g.st, USSBaseURL: g.us.URL, Counters: g.counters,
		Areas: func() ([]Area, bool) { return *areas, *known }}
	return g, s, areas, known
}

// One subscription per area of interest, its box widened by the
// policy's margin, telling of intents and constraints, at most 24 h; no
// change does nothing; 80 % of the window renews it; a changed area
// updates it; an area no longer published deletes it; unknown areas
// change nothing.
func TestSubscriptionsLifecycle(t *testing.T) {
	g, s, areas, known := subsRig(t)
	ctx := context.Background()
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	id := SubscriptionID(g.us.URL, "TUA001")
	got := g.dss.UTMSubscriptions()
	sub, ok := got[id]
	if !ok || sub.NotifyForOperationalIntents == nil || !*sub.NotifyForOperationalIntents || sub.NotifyForConstraints == nil || !*sub.NotifyForConstraints ||
		sub.TimeEnd.Value.Sub(sub.TimeStart.Value) > 24*time.Hour {
		t.Fatalf("%+v", got)
	}
	rec := g.st.subs[id]
	if rec.Area.Box.MinLat >= 41.6 || rec.Area.Box.MaxLon <= 44.9 {
		t.Fatalf("the margin was not applied: %+v", rec.Area.Box)
	}
	calls := len(g.dss.Calls())
	if err := s.Once(ctx); err != nil || len(g.dss.Calls()) != calls {
		t.Fatalf("renewed without cause: %v", err)
	}
	g.st.advance(24 * time.Hour * 81 / 100)
	if err := s.Once(ctx); err != nil || len(g.dss.Calls()) == calls {
		t.Fatalf("not renewed at 80 %%: %v", err)
	}
	if g.dss.UTMSubscriptions()[id].Version == sub.Version {
		t.Fatal("the renewal did not update the subscription")
	}
	(*areas)[0].Box.MaxLat = 41.85
	if err := s.Once(ctx); err != nil || g.st.subs[id].Area.Box.MaxLat <= 41.85 {
		t.Fatalf("area change not put: %v %+v", err, g.st.subs[id].Area)
	}
	*known = false
	*areas = nil
	if err := s.Once(ctx); err != nil || len(g.dss.UTMSubscriptions()) != 1 || g.count(CounterAreasUnknown) != 1 {
		t.Fatalf("unknown areas changed something: %v", err)
	}
	*known = true
	if err := s.Once(ctx); err != nil || len(g.dss.UTMSubscriptions()) != 0 || len(g.st.subs) != 0 || g.count(CounterSubscriptionDeleted) != 1 {
		t.Fatalf("not deleted: %v %v", err, g.dss.UTMSubscriptions())
	}
}

// A subscription the DSS holds but we did not record (a lost answer, a
// second replica) is read and updated; one we recorded but the DSS lost
// is created again.
func TestSubscriptionsRecover(t *testing.T) {
	g, s, _, _ := subsRig(t)
	ctx := context.Background()
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	id := SubscriptionID(g.us.URL, "TUA001")
	delete(g.st.subs, id)
	if err := s.Once(ctx); err != nil || g.st.subs[id].Version == "" {
		t.Fatalf("not recovered: %v", err)
	}
	if err := g.c.DeleteSubscription(ctx, id, g.st.subs[id].Version); err != nil {
		t.Fatal(err)
	}
	rec := g.st.subs[id]
	rec.TimeEnd = g.st.now
	g.st.subs[id] = rec
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.dss.UTMSubscriptions()[id]; !ok {
		t.Fatal("not created again")
	}
	// Too many areas: refused.
	many := make([]Area, MaxAreas+1)
	s.Areas = func() ([]Area, bool) { return many, true }
	if err := s.Once(ctx); err == nil {
		t.Fatal("more areas than the bound")
	}
	// The DSS down: the error is returned and counted.
	s.Areas = func() ([]Area, bool) {
		return []Area{{ID: "TUA002", Box: geodesy.BBox{MinLat: 1, MinLon: 1, MaxLat: 1.1, MaxLon: 1.1}}}, true
	}
	g.dss.Down(true)
	if err := s.Once(ctx); err == nil || g.count(CounterSubscriptionFailed) == 0 {
		t.Fatalf("%v", err)
	}
}

// The ids are stable for one base URL and area, and differ otherwise.
func TestSubscriptionID(t *testing.T) {
	a, b := SubscriptionID("https://u", "A"), SubscriptionID("https://u", "A")
	if a != b || a == SubscriptionID("https://u", "B") || a == SubscriptionID("https://v", "A") || !validUUID(a) {
		t.Fatalf("%s %s", a, b)
	}
}

// The purge removes peer data past 24 h, on the database clock.
func TestPurger(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	_, _ = st.UpsertPeerIntent(ctx, PeerRecord{EntityID: "old", Version: 1})
	_, _ = st.UpsertConstraint(ctx, ConstraintRecord{EntityID: "old-c", Version: 1})
	st.advance(23 * time.Hour)
	_, _ = st.UpsertPeerIntent(ctx, PeerRecord{EntityID: "new", Version: 1})
	st.advance(2 * time.Hour)
	c := &core.Counters{}
	p := &Purger{Store: st, Policy: policy.Defaults, Counters: c}
	n, err := p.Once(ctx)
	if err != nil || n.PeerIntents != 1 || n.Constraints != 1 || c.Get(CounterPurgedPeers) != 1 {
		t.Fatalf("%+v %v", n, err)
	}
	if x, _ := st.PeerIntent(ctx, "new"); x == nil {
		t.Fatal("a 23 h row purged")
	}
}

// Availability: Down and Normal read from the DSS, recorded and told;
// the recorded state is loaded at a start; a poll that fails says so.
func TestAvailability(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	var told []f3548.UssAvailabilityState
	a := &Availability{Client: g.c, Store: g.st, USSID: ourManager, Counters: g.counters,
		OnChange: func(s f3548.UssAvailabilityState) { told = append(told, s) }}
	if a.Down() {
		t.Fatal("down before any read")
	}
	g.dss.SetAvailability(ourManager, f3548.Down)
	if err := a.Poll(ctx); err != nil || !a.Down() || len(told) != 1 {
		t.Fatalf("%v %v", err, told)
	}
	b := &Availability{Client: g.c, Store: g.st, USSID: ourManager}
	if err := b.Load(ctx); err != nil || !b.Down() {
		t.Fatalf("not loaded: %v", err)
	}
	g.dss.Down(true)
	if err := a.Poll(ctx); err == nil {
		t.Fatal("a poll of a DSS down")
	}
	if _, known, _, lastErr := a.State(); !known || lastErr == "" {
		t.Fatalf("%v %q", known, lastErr)
	}
	if st, _ := g.st.State(ctx); st.UnreachableSince == nil {
		t.Fatalf("%+v", st)
	}
	var nilA *Availability
	if nilA.Down() {
		t.Fatal("a nil availability is down")
	}
	if ok, why := (*Gate)(nil).Available(ctx); ok || why == "" {
		t.Fatal("a nil gate")
	}
	if ok, why := (&Gate{Unconfigured: "no URL"}).Available(ctx); ok || why != "no URL" {
		t.Fatal(why)
	}
}

// The probe: unknown before a call, up with the availability named,
// degraded while work waits or the authority holds us Down, down while
// the DSS does not answer; Merge keeps the worst and names each.
func TestProbe(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	a := &Availability{Client: g.c, Store: g.st, USSID: ourManager}
	p := Probe(g.c, a, g.st)
	if st, d := p(ctx); st != obs.StateUnknown || !strings.Contains(d, "no F3548 DSS call") {
		t.Fatalf("%s %s", st, d)
	}
	g.dss.SetAvailability(ourManager, f3548.Normal)
	if err := a.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if st, d := p(ctx); st != obs.StateUp || !strings.Contains(d, "uss_availability Normal") {
		t.Fatalf("%s %s", st, d)
	}
	g.st.enqueue(store.OutboxOIRPut, oursID, 1, map[string]any{})
	g.st.enqueue(store.OutboxPeerNotify, oursID+"/x", 1, map[string]any{})
	if st, d := p(ctx); st != obs.StateDegraded || !strings.Contains(d, "1 intent writes waiting") || !strings.Contains(d, "1 notifications waiting") {
		t.Fatalf("%s %s", st, d)
	}
	g.dss.SetAvailability(ourManager, f3548.Down)
	_ = a.Poll(ctx)
	if st, d := p(ctx); st != obs.StateDegraded || !strings.Contains(d, "availability Down") {
		t.Fatalf("%s %s", st, d)
	}
	g.dss.Down(true)
	_, _ = g.c.QueryOperationalIntents(ctx, aoi)
	if st, d := p(ctx); st != obs.StateDown || !strings.Contains(d, "down since") {
		t.Fatalf("%s %s", st, d)
	}
	m := Merge(map[string]obs.Probe{
		"b": func(context.Context) (obs.State, string) { return obs.StateUp, "" },
		"a": func(context.Context) (obs.State, string) { return obs.StateDegraded, "slow" },
	})
	if st, d := m(ctx); st != obs.StateDegraded || d != "a: slow" {
		t.Fatalf("%s %q", st, d)
	}
}

// The exchange log: a full queue drops and counts (E-10); a failed write
// is counted; Run writes what is queued, and on its end what is left.
func TestExchangeLogBounds(t *testing.T) {
	c := &core.Counters{}
	st := newMemStore()
	l := NewExchangeLog(st, c, nil)
	for range ExchangeQueue + 5 {
		l.Record(Exchange{Method: "GET"})
	}
	if c.Get(CounterExchangeDropped) != 5 {
		t.Fatalf("dropped %d", c.Get(CounterExchangeDropped))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.Run(ctx)
	if len(st.exchanges) != ExchangeQueue || c.Get(CounterExchangeRecorded) != ExchangeQueue {
		t.Fatalf("%d written", len(st.exchanges))
	}
	var nilLog *ExchangeLog
	nilLog.Record(Exchange{})
	h := nilLog.Middleware(http.NotFoundHandler())
	if h == nil {
		t.Fatal("nil middleware")
	}
	l.write(context.Background(), Exchange{})
	f := NewExchangeLog(failingExchanges{}, c, nil)
	f.write(context.Background(), Exchange{})
	if c.Get(CounterExchangeFailed) != 1 {
		t.Fatal("a failed write not counted")
	}
	if clipBody(make([]byte, MaxExchangeBodyBytes+10)) == "" || len(clipBody(make([]byte, MaxExchangeBodyBytes+10))) != MaxExchangeBodyBytes {
		t.Fatal("body not clipped")
	}
}

type failingExchanges struct{}

func (failingExchanges) InsertExchange(context.Context, Exchange) error { return errors.New("down") }

// The loops run until their context ends: the availability polls, the
// subscriptions are put, the purge runs.
func TestRunLoops(t *testing.T) {
	g, s, _, _ := subsRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.dss.SetAvailability(ourManager, f3548.Normal)
	a := &Availability{Client: g.c, Store: g.st, USSID: ourManager, Every: 10 * time.Millisecond, Counters: g.counters}
	s.Every = 10 * time.Millisecond
	p := &Purger{Store: g.st, Every: 10 * time.Millisecond, Counters: g.counters}
	done := make(chan struct{}, 3)
	go func() { a.Run(ctx); done <- struct{}{} }()
	go func() { s.Run(ctx); done <- struct{}{} }()
	go func() { p.Run(ctx); done <- struct{}{} }()
	within(t, 5*time.Second, func() bool {
		return g.counters.Get(CounterAvailabilityPolled) >= 2 && len(g.dss.UTMSubscriptions()) == 1
	})
	g.dss.Down(true)
	within(t, 5*time.Second, func() bool { return g.counters.Get(CounterAvailabilityFailed) >= 1 })
	cancel()
	for range 3 {
		<-done
	}
}
