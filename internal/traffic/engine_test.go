package traffic

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/alerting"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

var (
	origin  = core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271}
	flightA = "11111111-1111-4111-8111-111111111111"
	flightB = "22222222-2222-4222-8222-222222222222"
	intentA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	t0      = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
)

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// memStore is the StateStore in memory.
type memStore struct {
	mu     sync.Mutex
	vals   map[string]Saved
	loaded bool
	puts   int
	dels   int
}

func newMemStore() *memStore { return &memStore{vals: map[string]Saved{}, loaded: true} }

func (m *memStore) Loaded() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loaded
}

func (m *memStore) All() map[string]Saved {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.vals)
}

func (m *memStore) Get(key string) (Saved, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.vals[StateKey(key)]
	return s, ok
}

func (m *memStore) Put(_ context.Context, s Saved) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[StateKey(s.Key)] = s
	m.puts++
	return nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vals, StateKey(key))
	m.dels++
	return nil
}

type rig struct {
	t     *testing.T
	clk   *clock
	e     *Engine
	store *memStore
	pv    policy.Values
	msgs  []AlertBody
}

func newRig(t *testing.T, store *memStore, instance string) *rig {
	t.Helper()
	r := &rig{t: t, clk: &clock{t: t0}, store: store, pv: policy.Defaults()}
	own, err := cell.ParseOwnership("all")
	if err != nil {
		t.Fatal(err)
	}
	r.e = &Engine{
		Policy: func() policy.Record { return policy.Record{Version: 7, Values: r.pv} }, Ownership: own, InstanceID: instance,
		Now: r.clk.Now, Authorisation: func(id string) string {
			if id == intentA {
				return "USSP-DEV-0001"
			}
			return ""
		},
	}
	if store != nil {
		r.e.Store = store
	}
	r.e.init()
	return r
}

// own is one of this USSP's flights at p, flying north-east-down at
// speed along track, sampled now.
func (r *rig) own(flightID string, p core.LatLon, speedMS, trackDeg float64) *Input {
	now := r.clk.Now()
	alt, vs := 550.0, 0.0
	status := "Airborne"
	tr := &telemetry.Track{}
	tr.Schema, tr.RxTS.Time, tr.CapturedAt.Time, tr.TS = telemetry.SchemaTrack, now, now, nil
	tr.TimeSource = core.TimeSourceClock
	tr.Body = telemetry.TrackBody{
		TrackID: flightID, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, SourceInstance: "client-" + flightID[:4],
		Position: telemetry.Position{Lat: p.LatDeg, Lng: p.LonDeg}, AltAMSLM: &alt, AltSource: core.AltGeodetic,
		SpeedMS: &speedMS, TrackDeg: &trackDeg, VSpeedMS: &vs, Status: &status, FlightID: &flightID,
		Identification: core.Identification{Status: core.IdentRegistered, Reason: core.ReasonSessionBinding, Basis: core.BasisAuthenticated},
	}
	if flightID == flightA {
		i := intentA
		tr.Body.IntentID = &i
	}
	in := TrackInputOf(NSTrack, tr)
	return &in
}

func (r *rig) feed(in *Input) {
	r.e.take(context.Background(), item{in: in})
	r.collect()
}

func (r *rig) tick() {
	r.e.tick(context.Background())
	r.collect()
}

func (r *rig) collect() {
	for {
		select {
		case m := <-r.e.out:
			b := m.m.(*AlertMessage).Body
			r.msgs = append(r.msgs, b)
		default:
			return
		}
	}
}

func (r *rig) of(flightID, state string) []AlertBody {
	var out []AlertBody
	for i := range r.msgs {
		if r.msgs[i].FlightID == flightID && r.msgs[i].State == state {
			out = append(out, r.msgs[i])
		}
	}
	return out
}

func (r *rig) drainPersist() {
	if r.e.pers != nil {
		r.e.pers.drain(context.Background(), time.Time{})
	}
}

// headOn flies A and B towards each other at 10 m/s from dist apart
// along the meridian through origin, one sample each per second for n
// seconds (B first, then A), ticking after each second.
func (r *rig) headOn(dist float64, n int) {
	for i := 0; i < n; i++ {
		d := dist/2 - 10*float64(i)
		a := geodesy.Destination(origin, 180, d)
		b := geodesy.Destination(origin, 0, d)
		if d < 0 {
			a, b = geodesy.Destination(origin, 0, -d), geodesy.Destination(origin, 180, -d)
		}
		r.feed(r.own(flightB, b, 10, 180))
		r.feed(r.own(flightA, a, 10, 0))
		r.tick()
		r.clk.add(time.Second)
	}
}

// Head-on at 10 m/s from 600 m: raised at once, on both flights, naming
// the other aircraft as authenticated, with the 04 §3.3 numbers.
func TestHeadOnRaisesOnBothFlightsWithThePeer(t *testing.T) {
	r := newRig(t, nil, "m1")
	r.headOn(600, 1)
	ra, rb := r.of(flightA, AlertRaised), r.of(flightB, AlertRaised)
	if len(ra) != 1 || len(rb) != 1 {
		t.Fatalf("raised A %d B %d: %+v", len(ra), len(rb), r.msgs)
	}
	a := ra[0]
	peer := a.Detail["peer"].(Peer)
	if peer.TrackID != flightB || peer.Trust != core.TrustAuthenticated || a.Kind != KindProximity || a.Severity != core.SeverityCritical {
		t.Fatalf("alert %+v", a)
	}
	if tc := a.Detail["t_cpa_s"].(float64); tc <= 0 || tc >= 60 {
		t.Fatalf("t_cpa_s %v", tc)
	}
	if a.AuthorisationNumber == nil || *a.AuthorisationNumber != "USSP-DEV-0001" || a.IntentID == nil || *a.IntentID != intentA {
		t.Fatalf("authorisation %+v", a)
	}
	if a.Detail["pair_id"] != rb[0].Detail["pair_id"] || a.Detail["t_cpa_s"] != rb[0].Detail["t_cpa_s"] {
		t.Fatalf("the two sides differ (C-11): %v / %v", a.Detail, rb[0].Detail)
	}
	if a.AlertID == rb[0].AlertID {
		t.Fatal("one alert id for both flights")
	}
	if _, ok := a.Detail["d_alt_m"]; !ok {
		t.Fatal("d_alt_m missing")
	}
	// The tick republished both as updated, with the same ids.
	if u := r.of(flightA, AlertUpdated); len(u) == 0 || u[0].AlertID != a.AlertID {
		t.Fatalf("republish %+v", u)
	}
}

// After the pass: cleared resolved at least the hysteresis after the
// last sample in conflict, with the final numbers and the clearing
// separation (C-14); one raise only.
func TestHeadOnPassClearsResolvedAfterHysteresis(t *testing.T) {
	r := newRig(t, nil, "m1")
	r.headOn(600, 40)
	if n := len(r.of(flightA, AlertRaised)); n != 1 {
		t.Fatalf("%d raises", n)
	}
	cl := r.of(flightA, AlertCleared)
	if len(cl) != 1 || *cl[0].ClearReason != string(alerting.ClearResolved) {
		t.Fatalf("cleared %+v", cl)
	}
	c := cl[0]
	if c.ClearingDetail == nil || c.ClearingDetail["clearing_d_horizontal_now_m"].(float64) < r.pv.CPAHorizontalMinM {
		t.Fatalf("clearing detail %v", c.ClearingDetail)
	}
	if c.UpdatedAt.Sub(c.CapturedAt.Time) < secs(r.pv.CPAClearAfterS) {
		t.Fatalf("cleared %v after the last true sample at %v", c.UpdatedAt, c.CapturedAt)
	}
	if c.AlertID != r.of(flightA, AlertRaised)[0].AlertID {
		t.Fatal("the clear names another alert")
	}
}

// SC-01 in unit form (C-03): hovering 25 m apart is raised and never
// cleared for 30 s, whatever the velocity noise says.
func TestHoverInsideMinimaNeverClears(t *testing.T) {
	r := newRig(t, nil, "m1")
	b := geodesy.Destination(origin, 90, 25)
	for i := 0; i < 35; i++ {
		noise := 0.02 * float64(i%3)
		r.feed(r.own(flightA, origin, noise, 90))
		r.feed(r.own(flightB, b, noise, 270))
		r.tick()
		r.clk.add(time.Second)
	}
	if len(r.of(flightA, AlertRaised)) != 1 || len(r.of(flightA, AlertCleared)) != 0 {
		t.Fatalf("raised %d cleared %d", len(r.of(flightA, AlertRaised)), len(r.of(flightA, AlertCleared)))
	}
}

// E-01 twin: two aircraft 2 km apart and diverging raise nothing; the
// counters stay zero.
func TestDivergingFarApartRaisesNothing(t *testing.T) {
	r := newRig(t, nil, "m1")
	for i := 0; i < 10; i++ {
		r.feed(r.own(flightA, geodesy.Destination(origin, 180, 1000+10*float64(i)), 10, 180))
		r.feed(r.own(flightB, geodesy.Destination(origin, 0, 1000+10*float64(i)), 10, 0))
		r.tick()
		r.clk.add(time.Second)
	}
	if len(r.msgs) != 0 || r.e.Counters.Get(CounterRaised) != 0 || r.e.Counters.Get(CounterCleared) != 0 {
		t.Fatalf("messages %d raised %d", len(r.msgs), r.e.Counters.Get(CounterRaised))
	}
}

// T-04: a backlog pair inside the minima raises nothing; the same pair
// live does (the presence twin).
func TestBacklogNeverRaises(t *testing.T) {
	for _, backlog := range []bool{true, false} {
		t.Run(fmt.Sprint("backlog=", backlog), func(t *testing.T) {
			r := newRig(t, nil, "m1")
			a, b := r.own(flightA, origin, 0, 0), r.own(flightB, geodesy.Destination(origin, 90, 20), 0, 0)
			a.Times.Backlog, b.Times.Backlog = backlog, backlog
			r.feed(a)
			r.feed(b)
			r.tick()
			got := len(r.of(flightA, AlertRaised))
			if backlog && got != 0 {
				t.Fatalf("backlog raised %d", got)
			}
			if !backlog && got != 1 {
				t.Fatalf("live raised %d", got)
			}
		})
	}
}

// A flight's end clears its alerts flight_ended (Drop).
func TestFlightEndClearsFlightEnded(t *testing.T) {
	r := newRig(t, nil, "m1")
	r.headOn(600, 2)
	r.e.take(context.Background(), item{ended: flightB})
	r.collect()
	cl := r.of(flightA, AlertCleared)
	if len(cl) != 1 || *cl[0].ClearReason != string(alerting.ClearFlightEnded) {
		t.Fatalf("cleared %+v", cl)
	}
}

// A source switched off: its aircraft's alerts clear source_disabled
// within the switch, and its samples raise nothing until it is back;
// back on, the next samples raise again.
func TestSourceSwitchClearsAndReturns(t *testing.T) {
	r := newRig(t, nil, "m1")
	r.headOn(600, 2)
	inst := "client-" + flightB[:4]
	off := coresources.State{Epoch: "e", Version: 1, Controls: []coresources.Control{{SourceType: telemetry.SourceOperatorWS, InstanceID: &inst, Enabled: false}}}
	r.e.take(context.Background(), item{sources: &off})
	r.collect()
	cl := r.of(flightA, AlertCleared)
	if len(cl) != 1 || *cl[0].ClearReason != string(alerting.ClearSourceDisabled) {
		t.Fatalf("cleared %+v", cl)
	}
	before := len(r.of(flightA, AlertRaised))
	r.headOn(400, 1)
	if len(r.of(flightA, AlertRaised)) != before {
		t.Fatal("a switched-off source raised")
	}
	on := coresources.State{Epoch: "e", Version: 2, Controls: []coresources.Control{{SourceType: telemetry.SourceOperatorWS, InstanceID: &inst, Enabled: true}}}
	r.e.take(context.Background(), item{sources: &on})
	r.headOn(300, 1)
	if len(r.of(flightA, AlertRaised)) != before+1 {
		t.Fatalf("not raised again once back: %d", len(r.of(flightA, AlertRaised)))
	}
}

// A manned aircraft crossing a flight raises a proximity alert naming
// it with trust surveillance; nothing is sent for the manned aircraft
// itself (no flight of ours).
func TestMannedCrossingRaisesWithSurveillancePeer(t *testing.T) {
	r := newRig(t, nil, "m1")
	m := &MannedTrack{}
	now := r.clk.Now()
	m.Schema, m.RxTS.Time, m.CapturedAt.Time, m.TimeSource = SchemaManned, now, now, core.TimeReceiver
	gs, trk, alt := 60.0, 270.0, 600.0
	p := geodesy.Destination(origin, 90, 700)
	m.Body = MannedBody{ICAO24: "4ca7b5", Position: MannedPosition{Lat: p.LatDeg, Lng: p.LonDeg}, AltWGS84M: &alt, GSMS: &gs, TrackDeg: &trk,
		SourceClass: "ads_b", Trust: core.TrustSurveillance, Source: SourceANSPFeed, SourceInstance: "adsb-tbs", State: StateLive}
	in := MannedInputOf(m, flatGeoid{})
	r.feed(r.own(flightA, origin, 0, 0))
	r.feed(&in)
	r.tick()
	ra := r.of(flightA, AlertRaised)
	if len(ra) != 1 {
		t.Fatalf("raised %+v", r.msgs)
	}
	if p := ra[0].Detail["peer"].(Peer); p.TrackID != "4ca7b5" || p.Trust != core.TrustSurveillance {
		t.Fatalf("peer %+v", p)
	}
	for _, b := range r.msgs {
		if b.FlightID != flightA {
			t.Fatalf("a message for %s", b.FlightID)
		}
	}
}

type flatGeoid struct{}

func (flatGeoid) UndulationM(core.LatLon) (float64, error) { return 50, nil }

// A restart carries a saved alert under its ids (no second raise), and
// core's raise of the same pair continues it.
func TestRestartCarriesAndCoreAdopts(t *testing.T) {
	store := newMemStore()
	r := newRig(t, store, "m1")
	b := geodesy.Destination(origin, 90, 25)
	for i := 0; i < 3; i++ {
		r.feed(r.own(flightA, origin, 0, 0))
		r.feed(r.own(flightB, b, 0, 0))
		r.tick()
		r.clk.add(time.Second)
	}
	r.drainPersist()
	raised := r.of(flightA, AlertRaised)
	if len(raised) != 1 || len(store.All()) != 1 {
		t.Fatalf("raised %d saved %d", len(raised), len(store.All()))
	}
	// The restart: a new engine on the same store.
	r2 := newRig(t, store, "m1")
	r2.clk.t = r.clk.Now()
	r2.e.restore(context.Background())
	r2.tick()
	up := r2.of(flightA, AlertUpdated)
	if len(up) == 0 || up[0].AlertID != raised[0].AlertID || up[0].Detail["carried_since"] == nil {
		t.Fatalf("carried %+v", up)
	}
	r2.clk.add(time.Second)
	r2.feed(r2.own(flightA, origin, 0, 0))
	r2.feed(r2.own(flightB, b, 0, 0))
	r2.tick()
	if len(r2.of(flightA, AlertRaised)) != 0 {
		t.Fatal("a second raise after the restart")
	}
	last := r2.of(flightA, AlertUpdated)
	if l := last[len(last)-1]; l.AlertID != raised[0].AlertID || l.Detail["carried_since"] != nil {
		t.Fatalf("not adopted: %+v", l)
	}
	if r2.e.Counters.Get(CounterAdopted) != 1 {
		t.Fatalf("adopted %d", r2.e.Counters.Get(CounterAdopted))
	}
}

// A carried alert whose pair core no longer judges in conflict clears
// not_reconfirmed only after both aircraft were heard for longer than
// the hysteresis (monotonic: never before).
func TestCarriedNotReconfirmedAfterHysteresis(t *testing.T) {
	store := newMemStore()
	saved := Saved{Owner: "m1", SavedAt: t0, Key: "conflict:trk:" + flightA + ":trk:" + flightB, RaisedAt: t0.Add(-time.Minute),
		LastTrueAt: t0.Add(-2 * time.Second), Severity: core.SeverityCritical, Aircraft: [2]SavedAircraft{
			{ID: NSTrack + ":" + flightA, TrackID: flightA, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, Instance: "client-1111", FlightID: flightA, Cell5: mustCell(origin)},
			{ID: NSTrack + ":" + flightB, TrackID: flightB, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, Instance: "client-2222", FlightID: flightB, Cell5: mustCell(origin)},
		}, Detail: map[string]any{"t_cpa_s": 10.0}}
	_ = store.Put(context.Background(), saved)
	r := newRig(t, store, "m1")
	r.e.restore(context.Background())
	far := geodesy.Destination(origin, 90, 3000)
	// Heard from t0: not cleared while at most the hysteresis (3 s) has
	// passed since both were heard.
	for i := 0; i <= int(r.pv.CPAClearAfterS); i++ {
		r.feed(r.own(flightA, origin, 0, 0))
		r.feed(r.own(flightB, far, 0, 0))
		r.tick()
		if len(r.of(flightA, AlertCleared)) != 0 {
			t.Fatalf("cleared %d s after both were heard, inside the hysteresis", i)
		}
		r.clk.add(time.Second)
	}
	r.feed(r.own(flightA, origin, 0, 0))
	r.feed(r.own(flightB, far, 0, 0))
	r.tick()
	cl := r.of(flightA, AlertCleared)
	if len(cl) != 1 || *cl[0].ClearReason != ClearNotReconfirmed || cl[0].AlertID != AlertID(saved.Key, flightA, saved.RaisedAt) {
		t.Fatalf("cleared %+v", cl)
	}
	r.drainPersist()
	if len(store.All()) != 0 {
		t.Fatal("the saved state survived the clear")
	}
}

// A carried alert whose aircraft is never heard again goes stale after
// the stale time, never before.
func TestCarriedSilentGoesStale(t *testing.T) {
	store := newMemStore()
	k := "conflict:trk:" + flightA + ":trk:" + flightB
	_ = store.Put(context.Background(), Saved{Owner: "", SavedAt: t0, Key: k, RaisedAt: t0, Aircraft: [2]SavedAircraft{
		{ID: NSTrack + ":" + flightA, TrackID: flightA, FlightID: flightA, Cell5: mustCell(origin)},
		{ID: NSTrack + ":" + flightB, TrackID: flightB, FlightID: flightB, Cell5: mustCell(origin)},
	}})
	r := newRig(t, store, "m2")
	r.e.restore(context.Background())
	for i := 0; i <= int(r.pv.CPAStaleAfterS); i++ {
		r.tick()
		if len(r.of(flightA, AlertCleared)) != 0 {
			t.Fatalf("cleared after %d s", i)
		}
		r.clk.add(time.Second)
	}
	r.clk.add(time.Second)
	r.tick()
	if cl := r.of(flightA, AlertCleared); len(cl) != 1 || *cl[0].ClearReason != string(alerting.ClearStale) {
		t.Fatalf("cleared %+v", cl)
	}
}

// Another live instance's saved alert is not carried at a start; one
// whose owner stopped saving is.
func TestRestoreRespectsALiveOwner(t *testing.T) {
	for _, age := range []time.Duration{time.Second, 2 * liveOwner} {
		store := newMemStore()
		k := "conflict:trk:" + flightA + ":trk:" + flightB
		_ = store.Put(context.Background(), Saved{Owner: "other", SavedAt: t0.Add(-age), Key: k, RaisedAt: t0, Aircraft: [2]SavedAircraft{
			{ID: NSTrack + ":" + flightA, TrackID: flightA, FlightID: flightA, Cell5: mustCell(origin)},
			{ID: NSTrack + ":" + flightB, TrackID: flightB, FlightID: flightB, Cell5: mustCell(origin)},
		}})
		r := newRig(t, store, "me")
		r.e.restore(context.Background())
		_, held := r.e.pairs[k]
		if want := age > liveOwner; held != want {
			t.Fatalf("owner saved %v ago: carried %v", age, held)
		}
	}
}

// A sample from a disabled source in a carried pair clears it
// source_disabled (B-11).
func TestCarriedSourceSwitchedOff(t *testing.T) {
	store := newMemStore()
	k := "conflict:trk:" + flightA + ":trk:" + flightB
	_ = store.Put(context.Background(), Saved{Owner: "m1", SavedAt: t0, Key: k, RaisedAt: t0, Aircraft: [2]SavedAircraft{
		{ID: NSTrack + ":" + flightA, TrackID: flightA, FlightID: flightA, Source: telemetry.SourceOperatorWS, Instance: "c1", Cell5: mustCell(origin)},
		{ID: NSTrack + ":" + flightB, TrackID: flightB, FlightID: flightB, Source: telemetry.SourceOperatorWS, Instance: "c2", Cell5: mustCell(origin)},
	}})
	r := newRig(t, store, "m1")
	r.e.restore(context.Background())
	inst := "c2"
	st := coresources.State{Epoch: "e", Version: 1, Controls: []coresources.Control{{SourceType: telemetry.SourceOperatorWS, InstanceID: &inst}}}
	r.e.take(context.Background(), item{sources: &st})
	r.collect()
	if cl := r.of(flightA, AlertCleared); len(cl) != 1 || *cl[0].ClearReason != string(alerting.ClearSourceDisabled) {
		t.Fatalf("cleared %+v", cl)
	}
}

// A policy change rebuilds core's monitor and carries its alerts: the
// next judgement continues them, nothing clears.
func TestPolicyChangeCarriesTheAlerts(t *testing.T) {
	r := newRig(t, newMemStore(), "m1")
	b := geodesy.Destination(origin, 90, 25)
	r.feed(r.own(flightA, origin, 0, 0))
	r.feed(r.own(flightB, b, 0, 0))
	r.tick()
	id := r.of(flightA, AlertRaised)[0].AlertID
	r.pv.CPANeighbourRadiusM = 900
	r.clk.add(time.Second)
	r.tick()
	r.feed(r.own(flightA, origin, 0, 0))
	r.feed(r.own(flightB, b, 0, 0))
	r.tick()
	if len(r.of(flightA, AlertCleared)) != 0 || len(r.of(flightA, AlertRaised)) != 1 {
		t.Fatalf("raised %d cleared %d", len(r.of(flightA, AlertRaised)), len(r.of(flightA, AlertCleared)))
	}
	if u := r.of(flightA, AlertUpdated); u[len(u)-1].AlertID != id {
		t.Fatal("another id after the policy change")
	}
	if r.e.Counters.Get(CounterMonitorRebuilt) != 1 {
		t.Fatal("not rebuilt")
	}
}

// Over the pair budget the worker evaluates every 2 s and says so on
// its alerts; it never skips a sample's aircraft (the newest is judged).
// Within the budget it evaluates every second (the twin).
func TestPairBudgetWidensToTwoSeconds(t *testing.T) {
	for _, budget := range []int{1, 50_000} {
		t.Run(fmt.Sprint("budget=", budget), func(t *testing.T) {
			r := newRig(t, nil, "m1")
			r.pv.CPAPairBudget = budget
			b := geodesy.Destination(origin, 90, 25)
			for i := 0; i < 6; i++ {
				r.feed(r.own(flightA, origin, 0, 0))
				r.feed(r.own(flightB, b, 0, 0))
				r.tick()
				r.clk.add(time.Second)
			}
			u := r.of(flightA, AlertUpdated)
			got := u[len(u)-1].Detail["evaluation_period_s"].(float64)
			want := 1.0
			if budget == 1 {
				want = 2
			}
			if got != want || r.e.Summary().EvaluationPeriodS != want {
				t.Fatalf("evaluation_period_s %v (summary %v), want %v", got, r.e.Summary().EvaluationPeriodS, want)
			}
			if budget == 1 && r.e.Counters.Get(CounterWidened) == 0 {
				t.Fatal("not counted")
			}
			if len(r.of(flightA, AlertCleared)) != 0 {
				t.Fatal("the widened period cleared the alert")
			}
		})
	}
}

// E-10: a full queue drops the sample, counted; the engine never blocks.
func TestQueueBound(t *testing.T) {
	e := &Engine{QueueLen: 2}
	e.init()
	in := Input{ID: "trk:x", TrackID: "x"}
	for i := 0; i < 5; i++ {
		e.Offer(in)
	}
	if e.Counters.Get(CounterQueueFull) != 3 {
		t.Fatalf("queue full %d", e.Counters.Get(CounterQueueFull))
	}
}

// A message is JSON in alert/v1's shape (the schema test validates it).
func TestAlertMessageJSON(t *testing.T) {
	r := newRig(t, nil, "m1")
	r.headOn(600, 1)
	raw, err := json.Marshal(&AlertMessage{Body: r.of(flightA, AlertRaised)[0]})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	body := m["body"].(map[string]any)
	for _, k := range []string{"alert_id", "kind", "severity", "state", "clear_reason", "flight_id", "captured_at", "raised_at", "policy_version", "detail"} {
		if _, ok := body[k]; !ok {
			t.Errorf("no %s", k)
		}
	}
}

func mustCell(p core.LatLon) string {
	c5, _, err := cell.Key(p)
	if err != nil {
		panic(err)
	}
	return c5
}

// newRigOwning is a rig whose instance owns the cell3 cells own names.
func newRigOwning(t *testing.T, store *memStore, instance, own string) *rig {
	t.Helper()
	r := newRig(t, store, instance)
	o, err := cell.ParseOwnership(own)
	if err != nil {
		t.Fatal(err)
	}
	r.e.Ownership = o
	return r
}

// share makes r read other's clock (two instances, one time).
func (r *rig) share(other *rig) {
	r.clk = other.clk
	r.e.Now = other.clk.Now
}

// hover feeds A at a and B at b, both flying and still, and ticks.
func (r *rig) hover(a, b core.LatLon) {
	r.feed(r.own(flightA, a, 0, 0))
	r.feed(r.own(flightB, b, 0, 0))
	r.tick()
}

// clearsOf are the clears of flightA's alert id across rigs.
func clearsOf(id string, rs ...*rig) []AlertBody {
	var out []AlertBody
	for _, r := range rs {
		ms := r.of(flightA, AlertCleared)
		for i := range ms {
			if ms[i].AlertID == id {
				out = append(out, ms[i])
			}
		}
	}
	return out
}

// A rollover under another instance id (a rolling update, a recreated
// container): B starts while A holds an alert and saves it, so B leaves
// it to A; A then stops, at once released (a clean stop) or silently (a
// crash, carried once A's save is stale), and the conflict ends. B
// carries the alert under its id and ends it on the evidence
// (not_reconfirmed): no alert is left without an owner.
func TestRolloverCarriesWhenTheOldOwnerIsGone(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprint("crash=", crash), func(t *testing.T) {
			store := newMemStore()
			ra := newRig(t, store, "monitor-a")
			b := geodesy.Destination(origin, 90, 25)
			for range 3 {
				ra.hover(origin, b)
				ra.clk.add(time.Second)
			}
			ra.drainPersist()
			id := ra.of(flightA, AlertRaised)[0].AlertID
			rb := newRig(t, store, "monitor-b")
			rb.share(ra)
			rb.e.restore(context.Background())
			rb.tick()
			if _, held := rb.e.pairs["conflict:trk:"+flightA+":trk:"+flightB]; held {
				t.Fatal("B took a live owner's alert")
			}
			// A is gone. B hears nothing of the pair meanwhile.
			wait := 1
			if crash {
				wait = int(liveOwner/time.Second) + 1
			} else {
				ra.e.release()
				ra.drainPersist()
			}
			for i := range wait {
				rb.clk.add(time.Second)
				rb.tick()
				if _, held := rb.e.pairs["conflict:trk:"+flightA+":trk:"+flightB]; held && crash && i < wait-2 {
					t.Fatalf("carried %d s after A's last save, while it was live", i+1)
				}
			}
			up := rb.of(flightA, AlertUpdated)
			if len(up) == 0 || up[len(up)-1].AlertID != id || up[len(up)-1].Detail["carried_since"] == nil {
				t.Fatalf("B did not carry the orphaned alert: %+v", up)
			}
			far := geodesy.Destination(origin, 90, 3000)
			for range int(rb.pv.CPAClearAfterS) + 2 {
				rb.clk.add(time.Second)
				rb.hover(origin, far)
			}
			cl := clearsOf(id, rb)
			if len(cl) != 1 || *cl[0].ClearReason != ClearNotReconfirmed {
				t.Fatalf("B's clears %+v", cl)
			}
			rb.drainPersist()
			if len(store.All()) != 0 {
				t.Fatal("the saved state survived the clear")
			}
		})
	}
}

// A cleared alert is never carried again from its last save while the
// store still shows it (a delete not yet seen, or one that failed).
func TestClearedNotCarriedFromItsLastSave(t *testing.T) {
	store := newMemStore()
	r := newRig(t, store, "m1")
	r.hover(origin, geodesy.Destination(origin, 90, 25))
	r.drainPersist()
	r.e.take(context.Background(), item{ended: flightB})
	r.collect()
	if len(r.of(flightA, AlertCleared)) != 1 {
		t.Fatal("not cleared")
	}
	// The store still holds the last save (the delete is pending).
	if len(store.All()) != 1 {
		t.Fatalf("saved %d", len(store.All()))
	}
	r.tick()
	if len(r.e.pairs) != 0 {
		t.Fatal("the cleared alert was carried again")
	}
	r.drainPersist()
	r.tick()
	if len(store.All()) != 0 || len(r.e.clearedAt) != 0 {
		t.Fatalf("saved %d, marks %d", len(store.All()), len(r.e.clearedAt))
	}
}

// boundary is a point just west of a cell3 edge east of origin, and the
// cell3 names on either side.
func boundary(t *testing.T) (west, east core.LatLon, c3w, c3e string) {
	t.Helper()
	_, start, _ := cell.Key(origin)
	for d := 0.0; d < 200_000; d += 10 {
		p := geodesy.Destination(origin, 90, d)
		if _, c3, _ := cell.Key(p); c3 != start {
			west, east = geodesy.Destination(origin, 90, d-15), geodesy.Destination(origin, 90, d+5)
			_, c3w, _ = cell.Key(west)
			_, c3e, _ = cell.Key(east)
			return west, east, c3w, c3e
		}
	}
	t.Fatal("no cell3 edge within 200 km")
	return
}

// A cell handover: the pair's anchor (flight A) crosses from A's cell
// into B's while the pair is in conflict, and the conflict ends during
// the handover. The old owner keeps publishing the alert, and clears it,
// until the new one has taken it; once B has taken it A stops. Exactly
// one clear is published either way.
func TestHandoverOldOwnerClearsUntilTransferred(t *testing.T) {
	west, east, c3w, c3e := boundary(t)
	if c3w == c3e {
		t.Fatal("one cell either side")
	}
	behind := geodesy.Destination(west, 270, 10) // flight B, in A's cell
	far := geodesy.Destination(west, 270, 3000)
	for _, withB := range []bool{false, true} {
		t.Run(fmt.Sprint("new_owner_running=", withB), func(t *testing.T) {
			store := newMemStore()
			ra := newRigOwning(t, store, "monitor-a", c3w)
			rb := newRigOwning(t, store, "monitor-b", c3e)
			rb.share(ra)
			for range 2 {
				ra.hover(west, behind)
				ra.clk.add(time.Second)
			}
			ra.drainPersist()
			raised := ra.of(flightA, AlertRaised)
			if len(raised) != 1 {
				t.Fatalf("raised %+v", raised)
			}
			id := raised[0].AlertID
			// The anchor crosses into B's cell, still in conflict.
			ra.hover(east, behind)
			ra.drainPersist()
			if withB {
				rb.tick() // B is fed nothing of the pair yet
				rb.drainPersist()
				ra.clk.add(time.Second)
				ra.tick()
			}
			// The conflict ends: B flies off; A judges it resolved.
			for range int(ra.pv.CPAClearAfterS) + 3 {
				ra.clk.add(time.Second)
				ra.hover(east, far)
				ra.drainPersist()
				if withB {
					rb.hover(east, far)
					rb.drainPersist()
				}
			}
			cl := clearsOf(id, ra, rb)
			if len(cl) != 1 {
				t.Fatalf("%d clears of %s (A %d, B %d)", len(cl), id, len(clearsOf(id, ra)), len(clearsOf(id, rb)))
			}
			if withB && len(clearsOf(id, rb)) != 1 {
				t.Fatal("B took the alert but did not clear it")
			}
			if withB {
				// After the transfer A no longer publishes the alert.
				last := ra.msgs[len(ra.msgs)-1]
				if last.AlertID == id && last.State == AlertUpdated {
					t.Fatalf("A still publishes after the transfer: %+v", last)
				}
			}
			if len(store.All()) != 0 {
				t.Fatalf("saved state left: %+v", store.All())
			}
		})
	}
}

// liveOwner is the engine's liveOwnerAfter at the default heartbeat.
const liveOwner = 3 * DefaultHeartbeat

// A carried alert is reconfirmed only on samples core admitted: samples
// core refuses (here placed ahead of their receipt, a clock ahead) are
// heard by nobody, so they never end the alert not_reconfirmed, however
// long they last; the same aircraft admitted end it after the
// hysteresis (the twin).
func TestCarriedNotReconfirmedOnlyByAdmittedSamples(t *testing.T) {
	store := newMemStore()
	k := "conflict:trk:" + flightA + ":trk:" + flightB
	_ = store.Put(context.Background(), Saved{Owner: "m1", SavedAt: t0, Key: k, RaisedAt: t0.Add(-time.Minute), Severity: core.SeverityCritical,
		Aircraft: [2]SavedAircraft{
			{ID: NSTrack + ":" + flightA, TrackID: flightA, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, Instance: "client-1111", FlightID: flightA, Cell5: mustCell(origin)},
			{ID: NSTrack + ":" + flightB, TrackID: flightB, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, Instance: "client-2222", FlightID: flightB, Cell5: mustCell(origin)},
		}})
	r := newRig(t, store, "m1")
	r.e.restore(context.Background())
	far := geodesy.Destination(origin, 90, 3000)
	ahead := func(in *Input) *Input {
		in.Times.CapturedAt = in.Times.RxTS.Add(5 * time.Second)
		return in
	}
	// Inside the stale time, longer than the hysteresis.
	for range int(r.pv.CPAClearAfterS) + 4 {
		r.feed(ahead(r.own(flightA, origin, 0, 0)))
		r.feed(ahead(r.own(flightB, far, 0, 0)))
		r.tick()
		r.clk.add(time.Second)
	}
	if cl := r.of(flightA, AlertCleared); len(cl) != 0 {
		t.Fatalf("refused samples ended the carried alert: %+v", cl)
	}
	if r.e.mon.Counters().Get(alerting.CounterRejectedPlacedAhead) == 0 {
		t.Fatal("core did not refuse the samples")
	}
	for range int(r.pv.CPAClearAfterS) + 2 {
		r.feed(r.own(flightA, origin, 0, 0))
		r.feed(r.own(flightB, far, 0, 0))
		r.tick()
		r.clk.add(time.Second)
	}
	if cl := r.of(flightA, AlertCleared); len(cl) != 1 || *cl[0].ClearReason != ClearNotReconfirmed {
		t.Fatalf("admitted samples: cleared %+v", cl)
	}
}
