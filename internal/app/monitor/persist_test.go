package monitor

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// memStates is the conformance_state bucket in memory: a revision per
// key, a create that finds the key and an update on another revision
// refused as a conflict, as the KV bucket does. Values go through JSON
// as they do on the bus.
type memStates struct {
	mu   sync.Mutex
	rev  uint64
	vals map[string]memEntry
}

type memEntry struct {
	raw []byte
	rev uint64
}

func (m *memStates) decode(e memEntry) StoredState {
	var s Saved
	if err := json.Unmarshal(e.raw, &s); err != nil {
		panic(err)
	}
	return StoredState{Saved: s, Rev: e.rev}
}

func (m *memStates) All(context.Context) ([]StoredState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []StoredState
	for _, e := range m.vals {
		out = append(out, m.decode(e))
	}
	return out, nil
}

func (m *memStates) Load(_ context.Context, id string) (StoredState, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.vals[id]
	if !ok {
		return StoredState{}, false, nil
	}
	return m.decode(e), true, nil
}

func (m *memStates) Save(_ context.Context, id string, s Saved, rev uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vals == nil {
		m.vals = map[string]memEntry{}
	}
	e, ok := m.vals[id]
	if (rev == 0 && ok) || (rev != 0 && (!ok || e.rev != rev)) {
		return 0, ErrStateConflict
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return 0, err
	}
	m.rev++
	m.vals[id] = memEntry{raw: raw, rev: m.rev}
	return m.rev, nil
}

func (m *memStates) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vals, id)
	return nil
}

func (m *memStates) get(id string) (StoredState, bool) {
	s, ok, _ := m.Load(context.Background(), id)
	return s, ok
}

// replica is one monitor instance on a shared clock, intent_active and
// conformance_state.
type replica struct {
	eng    *Engine
	sink   *sink
	cancel context.CancelFunc
}

func startReplica(t *testing.T, clk *clock, intents *fakeIntents, store StateStore, id, ownership string) *replica {
	t.Helper()
	own, err := cell.ParseOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	r := &replica{sink: &sink{}}
	r.eng = &Engine{Ownership: own, Intents: intents, Sources: &gate{}, Sink: r.sink, Now: clk.Now, Tick: 10 * time.Millisecond,
		Policy: func() policy.Record { return policy.Record{Version: 9, Values: policy.Defaults()} }, InstanceID: id}
	if store != nil {
		r.eng.Store = store
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.eng.Run(ctx)
	t.Cleanup(cancel)
	waitFor(t, "the engine seeded", func() bool {
		select {
		case <-r.eng.Seeded():
			return true
		default:
			return false
		}
	})
	return r
}

func (r *replica) states(flight string, s conformance.State) int {
	n := 0
	states := r.sink.states(flight)
	for i := range states {
		if states[i].State == s {
			n++
		}
	}
	return n
}

func (r *replica) published() int {
	r.sink.mu.Lock()
	defer r.sink.mu.Unlock()
	return len(r.sink.msgs)
}

func nonconformingBody(t *testing.T, now time.Time, state string) *intent.StateBody {
	b := stateBody(t, now)
	b.LocalState = state
	return b
}

// The restart scenario (blocking review item 1): a flight is
// nonconforming when the monitor stops; the next instance restores it
// from conformance_state and keeps it nonconforming, with the same
// alert, through samples inside until the full hysteresis counted from
// its own start has passed; then it clears that alert resolved and
// returns the flight. It never publishes conforming before.
func TestRestartKeepsNonconformance(t *testing.T) {
	clk := newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	intents := &fakeIntents{}
	intents.set(intentA, stateBody(t, clk.Now()), true)
	store := &memStates{}
	a := ptr(intentA)
	first := startReplica(t, clk, intents, store, "m-1", "all")
	first.eng.Offer(track(t, flightA, a, geodesy.Destination(origin, 90, 600), clk.Now(), "Airborne"))
	waitFor(t, "raised", func() bool { return len(first.sink.alerts("nonconformance", "raised", flightA)) == 1 })
	raised := first.sink.alerts("nonconformance", "raised", flightA)[0].AlertID
	waitFor(t, "saved nonconforming", func() bool {
		s, ok := store.get(flightA)
		return ok && s.Tracker.Base == conformance.StateNonconforming && s.Tracker.NC != nil && s.Owner == "m-1"
	})
	first.cancel()
	// The intent went nonconforming meanwhile (api moved it).
	intents.set(intentA, nonconformingBody(t, clk.Now(), "nonconforming"), true)
	clk.Add(5 * time.Second)

	second := startReplica(t, clk, intents, store, "m-1", "all")
	pv := policy.Defaults()
	start := clk.Now()
	for i := uint64(1); clk.Now().Sub(start) <= secs(pv.ConformanceClearAfterS); i++ {
		second.eng.Offer(track(t, flightA, a, origin, clk.Now(), "Airborne"))
		waitFor(t, "judged", func() bool { return second.eng.Counters.Get(conformance.CounterJudged) == i })
		if second.states(flightA, conformance.StateConforming) != 0 || len(second.sink.alerts("nonconformance", "cleared", flightA)) != 0 {
			t.Fatalf("conforming %v after the restart, within the hysteresis: %+v", clk.Now().Sub(start), second.sink.states(flightA))
		}
		clk.Add(time.Second)
	}
	waitFor(t, "republished", func() bool { return len(second.sink.alerts("nonconformance", "updated", flightA)) > 0 })
	if up := second.sink.alerts("nonconformance", "updated", flightA); up[0].AlertID != raised {
		t.Fatalf("another alert after the restart: %s, raised %s", up[0].AlertID, raised)
	}
	second.eng.Offer(track(t, flightA, a, origin, clk.Now(), "Airborne"))
	waitFor(t, "cleared resolved", func() bool {
		c := second.sink.alerts("nonconformance", "cleared", flightA)
		return len(c) == 1 && *c[0].ClearReason == conformance.ClearResolved && c[0].AlertID == raised
	})
	waitFor(t, "conforming", func() bool {
		s := second.sink.states(flightA)
		last := s[len(s)-1]
		return last.State == conformance.StateConforming && last.Transition && last.PreviousState != nil &&
			*last.PreviousState == conformance.StateNonconforming
	})
}

// Without a saved state (the bucket lost, or never written) a fresh
// tracker takes the intent's nonconforming state: the first sample
// inside does not show the flight conforming (E-01 pair with the
// activated intent, conforming at once).
func TestRestartWithoutSavedStateTakesTheIntentState(t *testing.T) {
	for _, tc := range []struct {
		intent string
		want   conformance.State
	}{{"nonconforming", conformance.StateNonconforming}, {"activated", conformance.StateConforming}} {
		t.Run(tc.intent, func(t *testing.T) {
			clk := newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
			intents := &fakeIntents{}
			intents.set(intentA, nonconformingBody(t, clk.Now(), tc.intent), true)
			r := startReplica(t, clk, intents, &memStates{}, "m-1", "all")
			r.eng.Offer(track(t, flightA, ptr(intentA), origin, clk.Now(), "Airborne"))
			waitFor(t, "a state", func() bool { return len(r.sink.states(flightA)) > 0 })
			if s := r.sink.states(flightA)[0]; s.State != tc.want {
				t.Fatalf("%+v", s)
			}
		})
	}
}

// A flight that falls silent across a restart is restored with its last
// sample: the new instance raises lost_link after lost_link_s of
// silence without any sample, and republishes the lost_link state.
func TestRestartSilentFlightLosesItsLink(t *testing.T) {
	clk := newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	intents := &fakeIntents{}
	intents.set(intentA, stateBody(t, clk.Now()), true)
	store := &memStates{}
	first := startReplica(t, clk, intents, store, "m-1", "all")
	first.eng.Offer(track(t, flightA, ptr(intentA), origin, clk.Now(), "Airborne"))
	waitFor(t, "saved", func() bool { s, ok := store.get(flightA); return ok && s.Tracker.HasLive })
	first.cancel()
	clk.Add(16 * time.Second)
	second := startReplica(t, clk, intents, store, "m-1", "all")
	waitFor(t, "lost_link", func() bool { return len(second.sink.alerts("lost_link", "raised", flightA)) == 1 })
	waitFor(t, "lost_link republished", func() bool { return second.states(flightA, conformance.StateLostLink) >= 3 })
}

// A restored flight whose intent ended while the monitor was down is
// ended at once: its alerts clear flight_ended and its state is deleted.
func TestRestartEndsAFlightWhoseIntentEnded(t *testing.T) {
	clk := newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	intents := &fakeIntents{}
	intents.set(intentA, stateBody(t, clk.Now()), true)
	store := &memStates{}
	first := startReplica(t, clk, intents, store, "m-1", "all")
	first.eng.Offer(track(t, flightA, ptr(intentA), geodesy.Destination(origin, 90, 600), clk.Now(), "Airborne"))
	waitFor(t, "saved with its alert", func() bool { s, ok := store.get(flightA); return ok && s.Tracker.NC != nil })
	first.cancel()
	intents.set(intentA, nil, true)
	second := startReplica(t, clk, intents, store, "m-1", "all")
	waitFor(t, "flight_ended", func() bool {
		c := second.sink.alerts("nonconformance", "cleared", flightA)
		return len(c) == 1 && *c[0].ClearReason == conformance.ClearFlightEnded
	})
	waitFor(t, "deleted", func() bool { _, ok := store.get(flightA); return !ok })
}

// boundary is a point east of origin in another cell3, and the cell3s
// of both sides.
func boundary(t *testing.T) (west, east core.LatLon, c3West, c3East string) {
	t.Helper()
	_, c3West, _ = cell.Key(origin)
	for d := 100.0; d < 200_000; d += 100 {
		p := geodesy.Destination(origin, 90, d)
		if _, c3, _ := cell.Key(p); c3 != c3West {
			return geodesy.Destination(origin, 90, d-300), geodesy.Destination(origin, 90, d+300), c3West, c3
		}
	}
	t.Fatal("no cell3 boundary within 200 km")
	return
}

// The handover between two replicas (blocking review item 1): replica A
// owns the west cell3, B the east one, and both receive every sample
// (the ring-1 subscriptions overlap). A flight nonconforming in A's cell
// crosses into B's, back inside its volume: A releases it and publishes
// nothing more of it; B takes it over from conformance_state with the
// same alert, keeps it nonconforming until the full hysteresis from the
// takeover, then clears that alert resolved. No replica ever shows it
// conforming early, and the alert is cleared once.
func TestHandoverBetweenTwoReplicas(t *testing.T) {
	west, east, c3W, c3E := boundary(t)
	clk := newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	mid := geodesy.Destination(west, 90, 300)
	intents := &fakeIntents{}
	intents.set(intentA, volumeAt(t, clk.Now(), mid), true)
	store := &memStates{}
	ra := startReplica(t, clk, intents, store, "m-a", c3W)
	rb := startReplica(t, clk, intents, store, "m-b", c3E)
	a := ptr(intentA)
	offer := func(p core.LatLon) {
		raw := track(t, flightA, a, p, clk.Now(), "Airborne")
		ra.eng.Offer(raw)
		rb.eng.Offer(raw)
	}
	outside := geodesy.Destination(mid, 270, 2600) // 600 m outside, in A's cell
	if _, c3, _ := cell.Key(outside); c3 != c3W {
		t.Fatalf("outside point in %s", c3)
	}
	offer(outside)
	waitFor(t, "A raised", func() bool { return len(ra.sink.alerts("nonconformance", "raised", flightA)) == 1 })
	raised := ra.sink.alerts("nonconformance", "raised", flightA)[0].AlertID
	if rb.published() != 0 {
		t.Fatal("B judged a flight in A's cell")
	}
	waitFor(t, "A saved it", func() bool { s, ok := store.get(flightA); return ok && s.Tracker.NC != nil })
	clk.Add(time.Second)
	offer(east)
	waitFor(t, "B took it over", func() bool { s, ok := store.get(flightA); return ok && s.Owner == "m-b" })
	waitFor(t, "B republished A's alert", func() bool {
		u := rb.sink.alerts("nonconformance", "updated", flightA)
		return len(u) > 0 && u[0].AlertID == raised
	})
	waitFor(t, "A let it go", func() bool { return ra.eng.Counters.Get(CounterReleased) == 1 })
	quiet := ra.published()
	pv := policy.Defaults()
	took := clk.Now()
	judged := rb.eng.Counters.Get(conformance.CounterJudged)
	for {
		clk.Add(time.Second)
		offer(east)
		judged++
		waitFor(t, "B judged", func() bool { return rb.eng.Counters.Get(conformance.CounterJudged) == judged })
		if clk.Now().Sub(took) > secs(pv.ConformanceClearAfterS) {
			break
		}
		if rb.states(flightA, conformance.StateConforming) != 0 || len(rb.sink.alerts("nonconformance", "cleared", flightA)) != 0 {
			t.Fatalf("conforming %v after the takeover, within the hysteresis", clk.Now().Sub(took))
		}
	}
	waitFor(t, "B cleared resolved", func() bool {
		c := rb.sink.alerts("nonconformance", "cleared", flightA)
		return len(c) == 1 && *c[0].ClearReason == conformance.ClearResolved && c[0].AlertID == raised
	})
	waitFor(t, "B conforming", func() bool { return rb.states(flightA, conformance.StateConforming) == 1 })
	if n := ra.published(); n != quiet {
		t.Fatalf("A published %d messages after the handover", n-quiet)
	}
	if c := ra.sink.alerts("nonconformance", "cleared", flightA); len(c) != 0 {
		t.Fatalf("A cleared the alert it handed over: %+v", c)
	}
}

// volumeAt is the test intent with its volume a 2 km circle around c.
func volumeAt(t *testing.T, now time.Time, c core.LatLon) *intent.StateBody {
	t.Helper()
	b := stateBody(t, now)
	raw := `{"volume":{"outline_circle":{"center":{"lat":` + ftoa(c.LatDeg) + `,"lng":` + ftoa(c.LonDeg) + `},"radius":{"value":2000,"units":"M"}},
	"altitude_lower":{"value":520,"reference":"W84","units":"M"},"altitude_upper":{"value":620,"reference":"W84","units":"M"}}}`
	ts, te := b.Volumes[0].TimeStart, b.Volumes[0].TimeEnd
	if err := json.Unmarshal([]byte(raw), &b.Volumes[0]); err != nil {
		t.Fatal(err)
	}
	b.Volumes[0].TimeStart, b.Volumes[0].TimeEnd = ts, te
	return b
}

func ftoa(v float64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// A flight that leaves A's cells without A seeing a sample outside them
// (a gap, or beyond A's ring) is taken over by B on its first sample in
// B's cell; A, finding the flight silent, sees B's ownership and lets it
// go: it raises no lost_link for a flight B is judging (E-01 pair: a
// flight nobody took over loses its link at A).
func TestSilentFlightTakenOverIsYielded(t *testing.T) {
	west, east, c3W, c3E := boundary(t)
	clk := newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	intents := &fakeIntents{}
	// Both sides inside the volume: nothing changes that would make A
	// save (and find B's ownership) before its silence check does.
	intents.set(intentA, volumeAt(t, clk.Now(), geodesy.Destination(west, 90, 300)), true)
	store := &memStates{}
	ra := startReplica(t, clk, intents, store, "m-a", c3W)
	rb := startReplica(t, clk, intents, store, "m-b", c3E)
	a := ptr(intentA)
	ra.eng.Offer(track(t, flightA, a, west, clk.Now(), "Airborne"))
	ra.eng.Offer(track(t, flightB, a, west, clk.Now(), "Airborne"))
	waitFor(t, "A claimed both", func() bool {
		sa, okA := store.get(flightA)
		sb, okB := store.get(flightB)
		return okA && okB && sa.Owner == "m-a" && sb.Owner == "m-a"
	})
	clk.Add(time.Second)
	rb.eng.Offer(track(t, flightA, a, east, clk.Now(), "Airborne")) // only B sees it
	waitFor(t, "B took it over", func() bool { s, ok := store.get(flightA); return ok && s.Owner == "m-b" })
	clk.Add(3 * time.Second)
	waitFor(t, "A yielded", func() bool { return ra.eng.Counters.Get(CounterYielded) == 1 })
	clk.Add(15 * time.Second)
	waitFor(t, "B's flight and A's other flight lose their links", func() bool {
		return len(rb.sink.alerts("lost_link", "raised", flightA)) == 1 && len(ra.sink.alerts("lost_link", "raised", flightB)) == 1
	})
	if n := ra.sink.alerts("lost_link", "raised", flightA); len(n) != 0 {
		t.Fatalf("A raised lost_link for a flight B judges: %+v", n)
	}
}
