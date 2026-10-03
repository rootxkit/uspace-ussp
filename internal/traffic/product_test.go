package traffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

type offGate map[string]bool

func (g offGate) Enabled(typ, inst string) bool { return !g[typ+"/"+inst] }

func area(radiusM float64) Area {
	b := geodesy.BBox{MinLat: origin.LatDeg, MaxLat: origin.LatDeg, MinLon: origin.LonDeg, MaxLon: origin.LonDeg}.PadM(radiusM)
	return Area{Boxes: []geodesy.BBox{b}}
}

func sample(id string, p core.LatLon, at time.Time) Input {
	return Input{ID: NSTrack + ":" + id, TrackID: id, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, Instance: "c1",
		Position: p, Times: core.Times{CapturedAt: at, RxTS: at}, State: StateLive,
		Identification: &core.Identification{Status: core.IdentRegistered, Serial: ptr("TEST0001")}}
}

func ptr(s string) *string { return &s }

// The picture keeps the newest sample of each track, ages it (live,
// stale, source_disabled), keeps the identification's status only, and
// is bounded without evicting a held track.
func TestPictureAgesAndBounds(t *testing.T) {
	pic := &Picture{MaxTracks: 2}
	v := policy.Defaults()
	pic.Put(sample("a", origin, t0), []byte(`{"a":1}`))
	pic.Put(sample("a", origin, t0.Add(-time.Second)), nil) // older: refused
	pic.Put(sample("b", geodesy.Destination(origin, 0, 100), t0), nil)
	pic.Put(sample("c", origin, t0), nil) // over the bound
	if pic.Len() != 2 || pic.counters().Get(CounterPictureOlder) != 1 || pic.counters().Get(CounterPictureOverBound) != 1 {
		t.Fatalf("len %d counters %v", pic.Len(), pic.counters().Snapshot())
	}
	got := pic.Tracks(area(500), t0.Add(time.Second), v, offGate{})
	if len(got) != 2 || got[0].Track.State != StateLive || string(got[0].Raw) != `{"a":1}` || got[0].Track.Identification.Status != core.IdentRegistered {
		t.Fatalf("%+v", got)
	}
	raw, _ := json.Marshal(got[0].Track)
	if json.Valid(raw) && containsAny(string(raw), "TEST0001", "serial") {
		t.Fatalf("a serial reached the product: %s", raw)
	}
	stale := pic.Tracks(area(500), t0.Add(time.Duration(v.TrafficStaleAfterS+1)*time.Second), v, offGate{})
	if stale[0].Track.State != StateStale || stale[0].Track.AgeS < v.TrafficStaleAfterS {
		t.Fatalf("%+v", stale[0].Track)
	}
	off := pic.Tracks(area(500), t0, v, offGate{telemetry.SourceOperatorWS + "/c1": true})
	if off[0].Track.State != StateSourceDisabled {
		t.Fatalf("%+v", off[0].Track)
	}
	if n := len(pic.Tracks(area(50), t0, v, nil)); n != 1 {
		t.Fatalf("radius 50 m: %d", n)
	}
	own := pic.Tracks(Area{OwnTrack: NSTrack + ":b"}, t0, v, nil)
	if len(own) != 1 || !own[0].Track.Own {
		t.Fatalf("own %+v", own)
	}
	if n := pic.Sweep(t0.Add(time.Second)); n != 2 || pic.Len() != 0 {
		t.Fatalf("swept %d", n)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		for i := 0; i+len(x) <= len(s); i++ {
			if s[i:i+len(x)] == x {
				return true
			}
		}
	}
	return false
}

// The throttle keeps every track at or below the limit and holds back
// about half above it, each track every other tick.
func TestThrottle(t *testing.T) {
	mk := func(n int) []Selected {
		out := make([]Selected, n)
		for i := range out {
			out[i] = Selected{ID: fmt.Sprintf("trk:%d", i)}
		}
		return out
	}
	if kept, held := Throttle(mk(200), 200, 1); len(kept) != 200 || held != 0 {
		t.Fatal("throttled at the limit")
	}
	sel := mk(300)
	k1, h1 := Throttle(sel, 200, 1)
	k2, h2 := Throttle(sel, 200, 2)
	if len(k1)+len(k2) != 300 || h1+h2 != 300 || len(k1) < 100 || len(k2) < 100 {
		t.Fatalf("%d/%d %d/%d", len(k1), h1, len(k2), h2)
	}
	if got := Tracked(k1); len(got) != len(k1) {
		t.Fatal("tracked")
	}
}

// The book keeps active alerts, drops a cleared one, never undoes an
// acknowledgement, refuses past its bound, ranks critical then by the
// time to the loss of separation, and fans out to subscribers.
func TestBook(t *testing.T) {
	b := &Book{Max: 2}
	ch, cancel := b.Subscribe()
	defer cancel()
	b.Put(Entry{AlertID: "a", Severity: core.SeverityWarning, State: AlertRaised, UpdatedAt: t0, Seen: t0})
	b.Put(Entry{AlertID: "b", Severity: core.SeverityCritical, State: AlertRaised, UpdatedAt: t0, Seen: t0, LoSStartS: 9})
	b.Put(Entry{AlertID: "c", Severity: core.SeverityCritical, State: AlertRaised, UpdatedAt: t0, Seen: t0, LoSStartS: 3})
	if b.counters().Get(CounterBookOverBound) != 1 {
		t.Fatal("bound")
	}
	b.Put(Entry{AlertID: "b", Severity: core.SeverityCritical, State: AlertUpdated, UpdatedAt: t0.Add(time.Second), Seen: t0, Acked: true})
	b.Put(Entry{AlertID: "b", Severity: core.SeverityCritical, State: AlertUpdated, UpdatedAt: t0.Add(2 * time.Second), Seen: t0.Add(2 * time.Second)})
	b.Put(Entry{AlertID: "b", State: AlertUpdated, UpdatedAt: t0}) // older
	act := b.Active(nil)
	if len(act) != 2 || act[0].AlertID != "b" || !act[0].Acked || b.counters().Get(CounterBookOlder) != 1 {
		t.Fatalf("%+v", act)
	}
	if u := b.Unrefreshed(t0.Add(10*time.Second), 5*time.Second); !u.Equal(t0) {
		t.Fatalf("unrefreshed %v", u)
	}
	b.Put(Entry{AlertID: "a", State: AlertCleared, UpdatedAt: t0.Add(3 * time.Second)})
	if len(b.Active(func(e *Entry) bool { return e.AlertID == "a" })) != 0 {
		t.Fatal("cleared alert kept")
	}
	n := 0
	for len(ch) > 0 {
		<-ch
		n++
	}
	if n != 5 {
		t.Fatalf("fan-out %d", n)
	}
	// A full subscriber misses, counted.
	for i := 0; i < DefaultSubscriberLen+1; i++ {
		b.Put(Entry{AlertID: "b", Severity: core.SeverityCritical, State: AlertUpdated, UpdatedAt: t0.Add(time.Duration(10+i) * time.Second)})
	}
	if b.counters().Get(CounterBookSubFull) == 0 {
		t.Fatal("a full subscriber not counted")
	}
	// Sorting with equal ranks falls back to the id.
	if sevRank("other") != 3 || sevRank(core.SeverityInfo) != 2 {
		t.Fatal("rank")
	}
}

// The persister writes the latest operation per pair, counts a failed
// save and delete, and drains on stop within its deadline.
type failStore struct {
	*memStore
	fail error
}

func (f failStore) Put(ctx context.Context, s Saved) error {
	if f.fail != nil {
		return f.fail
	}
	return f.memStore.Put(ctx, s)
}

func (f failStore) Delete(ctx context.Context, k string) error {
	if f.fail != nil {
		return f.fail
	}
	return f.memStore.Delete(ctx, k)
}

func TestPersister(t *testing.T) {
	ms := newMemStore()
	c := &core.Counters{}
	p := newPersister(failStore{memStore: ms}, c, nil)
	p.logger = discard()
	p.put("k", persistOp{s: Saved{Key: "k", RaisedAt: t0}})
	p.put("k", persistOp{s: Saved{Key: "k", RaisedAt: t0.Add(time.Second)}})
	p.drain(context.Background(), time.Time{})
	if ms.puts != 1 || !ms.vals[StateKey("k")].RaisedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("puts %d %+v", ms.puts, ms.vals)
	}
	p.put("k", persistOp{del: true})
	p.drain(context.Background(), time.Time{})
	if len(ms.vals) != 0 || c.Get(CounterStateDeleted) != 1 {
		t.Fatal("not deleted")
	}
	pf := newPersister(failStore{memStore: ms, fail: errors.New("kv down")}, c, discard())
	pf.put("k", persistOp{s: Saved{Key: "k"}})
	pf.put("j", persistOp{del: true})
	pf.drain(context.Background(), time.Time{})
	if c.Get(CounterStateSaveFailed) != 1 || c.Get(CounterStateDelFailed) != 1 {
		t.Fatalf("%v", c.Snapshot())
	}
	// run: a kick drains, a stop drains what is left.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	p.put("x", persistOp{s: Saved{Key: "x", RaisedAt: t0}})
	waitFor(t, func() bool { _, ok := ms.Get("x"); return ok })
	cancel()
	<-done
}

// KVStates reads its mirror: loaded, all, by pair key; a key whose
// saved pair differs is not this pair's.
func TestKVStatesReadsTheMirror(t *testing.T) {
	m := &bus.Mirror[Saved]{}
	k := KVStates{M: m}
	if k.Loaded() {
		t.Fatal("loaded before a read")
	}
	s := Saved{Key: "conflict:a:b", RaisedAt: t0, Aircraft: [2]SavedAircraft{{ID: "a"}, {ID: "b"}}}
	m.Seed(map[string]Saved{StateKey(s.Key): s, StateKey("other"): {Key: "not-other"}})
	if !k.Loaded() || len(k.All()) != 2 {
		t.Fatal("not loaded")
	}
	if got, ok := k.Get(s.Key); !ok || !got.RaisedAt.Equal(t0) {
		t.Fatal("not found")
	}
	if _, ok := k.Get("other"); ok {
		t.Fatal("a pair found under another pair's key")
	}
	raw, _ := json.Marshal(s)
	if _, err := DecodeSaved("", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSaved("", make([]byte, bus.ProximityStateBytes+1)); err == nil {
		t.Fatal("over the bound accepted")
	}
	if _, err := DecodeSaved("", []byte(`{"key":"x"}`)); err == nil {
		t.Fatal("incomplete accepted")
	}
}

// The publisher retries a failing sink, then drops the message,
// counted; a working sink publishes it.
type sink struct {
	mu    sync.Mutex
	fails int
	n     int
}

func (s *sink) Publish(context.Context, string, bus.Enveloped) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails > 0 {
		s.fails--
		return errors.New("nats down")
	}
	s.n++
	return nil
}

func TestPublishRetriesThenDrops(t *testing.T) {
	sk := &sink{fails: PublishAttempts}
	e := &Engine{Sink: sk}
	e.init()
	m := outMsg{subject: "alrt.v1.proximity.c5:1317:2248.x", m: &AlertMessage{}}
	e.send(context.Background(), m)
	if e.Counters.Get(CounterPublishDropped) != 1 || e.Counters.Get(CounterPublishFailed) != PublishAttempts {
		t.Fatalf("%v", e.Counters.Snapshot())
	}
	e.send(context.Background(), m)
	if sk.n != 1 || e.Counters.Get(CounterPublished) != 1 {
		t.Fatalf("%v", e.Counters.Snapshot())
	}
	none := &Engine{}
	none.init()
	none.send(context.Background(), m)
	if none.Counters.Get(CounterPublishDropped) != 1 {
		t.Fatal("no sink not counted")
	}
	// A full outbox drops, counted.
	full := &Engine{OutboxLen: 1}
	full.init()
	r := &rig{t: t, clk: &clock{t: t0}, e: full, pv: policy.Defaults()}
	_ = r
	p := &pairState{key: "conflict:trk:a:trk:b", raisedAt: t0, owned: true, aircraft: [2]SavedAircraft{
		{ID: "trk:a", TrackID: flightA, FlightID: flightA, Cell5: mustCell(origin)}, {ID: "trk:b", TrackID: flightB, FlightID: flightB, Cell5: mustCell(origin)}}}
	full.publishPairPeriod(context.Background(), p, AlertUpdated, "", nil, t0, 1)
	if full.Counters.Get(CounterOutboxFull) != 1 {
		t.Fatalf("%v", full.Counters.Snapshot())
	}
	p.aircraft[1].Cell5 = ""
	full.publishPairPeriod(context.Background(), p, AlertUpdated, "", nil, t0, 1)
	if full.Counters.Get(CounterAlertNoCell) != 1 {
		t.Fatal("no cell not counted")
	}
}

// Run: the start waits for the saved alerts (or StoreWait, said so),
// carries them, opens, ticks and publishes until stopped.
func TestRunLifecycle(t *testing.T) {
	ms := newMemStore()
	ms.loaded = false
	sk := &sink{}
	own, _ := cell.ParseOwnership("all")
	e := &Engine{Store: ms, Sink: sk, StoreWait: 100 * time.Millisecond, Tick: 20 * time.Millisecond, Ownership: own}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	<-e.Seeded()
	if e.Counters.Get(CounterStateNotLoaded) != 1 {
		t.Fatal("a store not read in time not said")
	}
	// Read later: carried at the next tick.
	_ = ms.Put(context.Background(), Saved{Owner: "monitor", SavedAt: time.Now(), Key: "conflict:trk:x:trk:y", RaisedAt: t0,
		Aircraft: [2]SavedAircraft{{ID: "trk:x", TrackID: flightA, FlightID: flightA, Cell5: mustCell(origin)}, {ID: "trk:y", TrackID: flightB, FlightID: flightB, Cell5: mustCell(origin)}},
		Detail:   map[string]any{"t_cpa_s": 1.0}})
	ms.mu.Lock()
	ms.loaded = true
	ms.mu.Unlock()
	waitFor(t, func() bool { return e.Summary().Carried == 1 })
	waitFor(t, func() bool { sk.mu.Lock(); defer sk.mu.Unlock(); return sk.n > 0 })
	e.Offer(Input{ID: "trk:z", TrackID: "z", State: StateLive, Position: origin})
	e.FlightEnded(flightA)
	e.SwitchSource(coresourcesState())
	waitFor(t, func() bool { return e.Counters.Get(CounterSamples) == 1 })
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func discard() *slog.Logger { return obs.Discard() }

func coresourcesState() coresources.State { return coresources.State{Epoch: "e", Version: 1} }
