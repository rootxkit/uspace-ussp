package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

var origin = core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271}

const (
	flightA = "5b3f1d2e-7c4a-4e8b-9f10-2a3b4c5d6e7f"
	flightB = "6c4f1d2e-7c4a-4e8b-9f10-2a3b4c5d6e70"
	flightC = "7d4f1d2e-7c4a-4e8b-9f10-2a3b4c5d6e71"
	intentA = "8d0e7b51-3c1e-4a5f-9a43-0b6f4c2a7e01"
)

type clock struct{ ns atomic.Int64 }

func newClock(t time.Time) *clock    { c := &clock{}; c.ns.Store(t.UnixNano()); return c }
func (c *clock) Now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) Add(d time.Duration) { c.ns.Add(int64(d)) }

type fakeIntents struct {
	mu     sync.Mutex
	loaded bool
	vals   map[string]intent.StateBody
}

func (f *fakeIntents) Intent(id string) (intent.StateBody, bool, float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded {
		return intent.StateBody{}, false, 0, false
	}
	b, ok := f.vals[id]
	return b, ok, 0, true
}

func (f *fakeIntents) set(id string, b *intent.StateBody, loaded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = loaded
	if f.vals == nil {
		f.vals = map[string]intent.StateBody{}
	}
	if b == nil {
		delete(f.vals, id)
	} else {
		f.vals[id] = *b
	}
}

type gate struct{ off atomic.Bool }

func (g *gate) Query(string, *string) coresources.Decision {
	return coresources.Decision{Enabled: !g.off.Load()}
}

type sent struct {
	subject string
	at      time.Time
	m       bus.Enveloped
}

type sink struct {
	mu   sync.Mutex
	msgs []sent
	err  error
}

func (s *sink) Publish(_ context.Context, subject string, m bus.Enveloped) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.msgs = append(s.msgs, sent{subject, time.Now(), m})
	return nil
}

func (s *sink) alerts(kind, state, flight string) []conformance.AlertBody {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []conformance.AlertBody
	for _, m := range s.msgs {
		if a, ok := m.m.(*conformance.AlertMessage); ok && a.Body.Kind == kind && a.Body.State == state && (flight == "" || a.Body.FlightID == flight) {
			out = append(out, a.Body)
		}
	}
	return out
}

func (s *sink) states(flight string) []conformance.StateBody {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []conformance.StateBody
	for _, m := range s.msgs {
		if c, ok := m.m.(*conformance.StateMessage); ok && c.Body.FlightID == flight {
			out = append(out, c.Body)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stateBody(t *testing.T, now time.Time) *intent.StateBody {
	t.Helper()
	tm := func(d time.Duration) *f3548.Time { return &f3548.Time{Value: now.Add(d), Format: "RFC3339"} }
	var v f3548.Volume4D
	raw := `{"volume":{"outline_circle":{"center":{"lat":41.7151,"lng":44.8271},"radius":{"value":500,"units":"M"}},
	"altitude_lower":{"value":520,"reference":"W84","units":"M"},"altitude_upper":{"value":620,"reference":"W84","units":"M"}}}`
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	v.TimeStart, v.TimeEnd = tm(-time.Hour), tm(time.Hour)
	no := "USSP-DEV-1"
	return &intent.StateBody{IntentID: intentA, LocalState: "activated", AuthorisationNumber: &no, Volumes: []f3548.Volume4D{v},
		VolumesAMSL:         []intent.VolumeAMSL{{LowerAMSLM: 500, UpperAMSLM: 600, UndulationM: 20, LowerW84M: 520, UpperW84M: 620}},
		DeviationThresholds: &intent.Thresholds{HM: 50, VM: 15, TS: 60}}
}

func track(t *testing.T, flight string, intentID *string, p core.LatLon, at time.Time, status string) []byte {
	t.Helper()
	id := flight
	alt := 550.0
	tr := &telemetry.Track{
		Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer, core.Times{RxTS: at, CapturedAt: at, Source: core.TimeSourceClock}),
		Body: telemetry.TrackBody{TrackID: id, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, SourceInstance: "client-1",
			Position: telemetry.Position{Lat: p.LatDeg, Lng: p.LonDeg}, AltAMSLM: &alt, AltSource: core.AltGeodetic,
			Status: &status, FlightID: &id, IntentID: intentID},
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type rig struct {
	t       *testing.T
	clk     *clock
	intents *fakeIntents
	gate    *gate
	sink    *sink
	eng     *Engine
	cancel  context.CancelFunc
}

func newRig(t *testing.T, loaded bool) *rig {
	t.Helper()
	g := &rig{t: t, clk: newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)), intents: &fakeIntents{}, gate: &gate{}, sink: &sink{}}
	if loaded {
		g.intents.set(intentA, stateBody(t, g.clk.Now()), true)
	}
	own, _ := cell.ParseOwnership("all")
	g.eng = &Engine{Ownership: own, Intents: g.intents, Sources: g.gate, Sink: g.sink, Now: g.clk.Now, Tick: 10 * time.Millisecond,
		Policy: func() policy.Record { return policy.Record{Version: 9, Values: policy.Defaults()} }}
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	go g.eng.Run(ctx)
	t.Cleanup(cancel)
	waitFor(t, "the engine", func() bool { g.eng.mu.Lock(); defer g.eng.mu.Unlock(); return g.eng.ctx == ctx })
	return g
}

func (g *rig) offer(flight string, intentID *string, p core.LatLon, status string) {
	g.eng.Offer(track(g.t, flight, intentID, p, g.clk.Now(), status))
}

func ptr(s string) *string { return &s }

// The done-when path on the engine: a flight leaves its volume and the
// nonconformance is raised within one sample (latency measured), a
// flight 500 m away receives nonconformance_nearby and one 5 km away
// none (E-01 pair); back inside for more than 3 s clears both resolved
// with the numbers; 15 s of silence raises lost_link; the intent's end
// clears it as flight_ended.
func TestEngineConformanceLifecycle(t *testing.T) {
	g := newRig(t, true)
	a := ptr(intentA)
	g.offer(flightA, a, origin, "Airborne")
	waitFor(t, "conforming", func() bool {
		s := g.sink.states(flightA)
		return len(s) == 1 && s[0].State == conformance.StateConforming
	})
	g.offer(flightB, nil, geodesy.Destination(origin, 90, 1100), "Airborne")
	g.offer(flightC, nil, geodesy.Destination(origin, 0, 5600), "Airborne")
	g.clk.Add(time.Second)
	out := geodesy.Destination(origin, 90, 600)
	captured := g.clk.Now()
	sentAt := time.Now()
	g.offer(flightA, a, out, "Airborne")
	waitFor(t, "nonconformance raised", func() bool { return len(g.sink.alerts("nonconformance", "raised", flightA)) == 1 })
	raised := g.sink.alerts("nonconformance", "raised", flightA)[0]
	if !raised.CapturedAt.Equal(captured) || raised.Severity != core.SeverityCritical || raised.PolicyVersion != 9 ||
		raised.AuthorisationNumber == nil || *raised.AuthorisationNumber != "USSP-DEV-1" {
		t.Fatalf("%+v", raised)
	}
	t.Logf("nonconformance published %v after the sample was offered", time.Since(sentAt))
	if d, _ := raised.Detail["distance_outside_m"].(float64); d < 99.9 || d > 100.1 {
		t.Fatalf("distance %v", raised.Detail)
	}
	waitFor(t, "nearby to B", func() bool { return len(g.sink.alerts("nonconformance_nearby", "raised", flightB)) == 1 })
	waitFor(t, "a republish", func() bool { return len(g.sink.alerts("nonconformance", "updated", flightA)) > 0 })
	if n := g.sink.alerts("nonconformance_nearby", "raised", flightC); len(n) != 0 {
		t.Fatalf("5 km away received %+v", n)
	}
	if s := g.sink.states(flightA); s[len(s)-1].State != conformance.StateNonconforming || !s[len(s)-1].Transition {
		t.Fatalf("%+v", s[len(s)-1])
	}
	for _, d := range []time.Duration{time.Second, time.Second, 1500 * time.Millisecond} {
		g.clk.Add(d)
		g.offer(flightA, a, origin, "Airborne")
	}
	waitFor(t, "resolved", func() bool { return len(g.sink.alerts("nonconformance", "cleared", flightA)) == 1 })
	cl := g.sink.alerts("nonconformance", "cleared", flightA)[0]
	if *cl.ClearReason != "resolved" || cl.ClearingDetail == nil || cl.Detail["distance_outside_m"] == nil {
		t.Fatalf("%+v", cl)
	}
	waitFor(t, "nearby cleared with it", func() bool {
		c := g.sink.alerts("nonconformance_nearby", "cleared", flightB)
		return len(c) == 1 && *c[0].ClearReason == "resolved"
	})
	g.clk.Add(16 * time.Second)
	waitFor(t, "lost_link", func() bool { return len(g.sink.alerts("lost_link", "raised", flightA)) == 1 })
	waitFor(t, "lost_link state", func() bool {
		s := g.sink.states(flightA)
		return s[len(s)-1].State == conformance.StateLostLink
	})
	g.intents.set(intentA, nil, true)
	waitFor(t, "flight_ended", func() bool {
		c := g.sink.alerts("lost_link", "cleared", flightA)
		return len(c) == 1 && *c[0].ClearReason == "flight_ended"
	})
	if g.eng.Counters.Get(CounterIntentEnded) != 1 {
		t.Fatal(g.eng.Counters.Snapshot())
	}
	sum := g.eng.Summary()
	if sum.Workers != 1 {
		t.Fatalf("%+v", sum)
	}
	attrs := StatusAttrs(sum, 0, true, 9)
	if len(attrs) == 0 {
		t.Fatal("no status attributes")
	}
}

// SC-22, E-02: without intent_active every flight is unknown and says
// why; the bucket appearing resolves the next sample (presence pair).
func TestEngineWithoutIntentActive(t *testing.T) {
	g := newRig(t, false)
	a := ptr(intentA)
	g.offer(flightA, a, geodesy.Destination(origin, 90, 600), "Airborne")
	waitFor(t, "unknown", func() bool {
		s := g.sink.states(flightA)
		return len(s) == 1 && s[0].State == conformance.StateUnknown && *s[0].Reason == conformance.UnknownProjectionUnavailable
	})
	if len(g.sink.alerts("nonconformance", "raised", "")) != 0 {
		t.Fatal("raised without an authorisation")
	}
	said := false
	for _, a := range StatusAttrs(Summary{}, 0, false, 0) {
		said = said || (a.Key == "intent_active" && strings.Contains(a.Value.String(), "not read"))
	}
	if !said {
		t.Fatal("the status line does not say intent_active is not read")
	}
	g.intents.set(intentA, stateBody(t, g.clk.Now()), true)
	g.clk.Add(time.Second)
	g.offer(flightA, a, origin, "Airborne")
	waitFor(t, "conforming", func() bool {
		s := g.sink.states(flightA)
		return s[len(s)-1].State == conformance.StateConforming
	})
	// A flight without an intent is unknown, counted.
	g.offer(flightB, nil, origin, "Airborne")
	waitFor(t, "no intent", func() bool {
		s := g.sink.states(flightB)
		return len(s) == 1 && *s[0].Reason == conformance.UnknownNoIntent
	})
}

// A switched-off source clears the flight's alerts as source_disabled
// on the next tick (B-11), and the flight end clears what remains.
func TestEngineSourceDisabledAndFlightEnd(t *testing.T) {
	g := newRig(t, true)
	a := ptr(intentA)
	g.offer(flightA, a, geodesy.Destination(origin, 90, 600), "Airborne")
	waitFor(t, "raised", func() bool { return len(g.sink.alerts("nonconformance", "raised", flightA)) == 1 })
	g.gate.off.Store(true)
	waitFor(t, "source_disabled", func() bool {
		c := g.sink.alerts("nonconformance", "cleared", flightA)
		return len(c) == 1 && *c[0].ClearReason == "source_disabled"
	})
	g.gate.off.Store(false)
	g.clk.Add(time.Second)
	g.offer(flightA, a, geodesy.Destination(origin, 90, 600), "Airborne")
	waitFor(t, "raised again", func() bool { return len(g.sink.alerts("nonconformance", "raised", flightA)) == 2 })
	g.eng.FlightEnded(flightA)
	waitFor(t, "flight_ended", func() bool {
		c := g.sink.alerts("nonconformance", "cleared", flightA)
		return len(c) == 2 && *c[1].ClearReason == "flight_ended"
	})
	if g.eng.Counters.Get(CounterFlightEnded) == 0 {
		t.Fatal("not counted")
	}
}

// E-10: a worker whose queue is full drops and counts; a full outbox
// drops and counts; a failing sink counts.
func TestEngineBounds(t *testing.T) {
	own, _ := cell.ParseOwnership("all")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // workers stop at once: nothing drains their queues
	e := &Engine{Ownership: own, Intents: &fakeIntents{}, QueueLen: 1, OutboxLen: 1, Now: time.Now}
	e.mu.Lock()
	e.ctx = ctx
	e.mu.Unlock()
	now := time.Now()
	for range 50 {
		e.Offer(track(t, flightA, nil, origin, now, "Airborne"))
	}
	waitFor(t, "queue full", func() bool { return e.Counters.Get(CounterQueueFull) > 0 })
	e.Offer([]byte("{"))
	if e.Counters.Get(CounterDecodeFailed) != 1 {
		t.Fatal("unreadable track not counted")
	}
	peer := track(t, flightB, nil, origin, now, "Airborne")
	peer = []byte(strings.Replace(string(peer), `"trust":"authenticated"`, `"trust":"provider"`, 1))
	e.Offer(peer)
	if e.Counters.Get(CounterNotOurs) != 1 {
		t.Fatal("a peer track was judged")
	}
	for range DefaultOutboxLen + 5 {
		e.out.put("conf.v1."+flightA, &conformance.StateMessage{})
	}
	if e.Counters.Get(CounterOutboxFull) == 0 {
		t.Fatal("outbox bound not counted")
	}
	// Not owned: a cell outside the ownership is not judged.
	part, _ := cell.ParseOwnership("c3:0:0")
	e2 := &Engine{Ownership: part, Intents: &fakeIntents{}}
	e2.Offer(track(t, flightA, nil, origin, now, "Airborne"))
	if e2.Counters.Get(CounterNotOwned) != 1 {
		t.Fatal("a cell not owned was judged")
	}
	// A sink that fails is counted, never retried in a loop.
	s := &sink{err: errors.New("down")}
	e3 := &Engine{Ownership: own, Intents: &fakeIntents{}, Sink: s}
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	go e3.Run(ctx3)
	waitFor(t, "engine 3", func() bool { e3.mu.Lock(); defer e3.mu.Unlock(); return e3.ctx == ctx3 })
	e3.out.put("conf.v1."+flightA, &conformance.StateMessage{})
	waitFor(t, "publish failure counted", func() bool { return e3.Counters.Get(CounterPublishFailed) == 1 })
}

func TestTrackSubjects(t *testing.T) {
	all, _ := cell.ParseOwnership("all")
	if s, err := TrackSubjects(all); err != nil || !slices.Equal(s, []string{bus.SubjectTrkAll}) {
		t.Fatalf("%v %v", s, err)
	}
	_, c3, _ := cell.Key(origin)
	o, _ := cell.ParseOwnership(c3)
	s, err := TrackSubjects(o)
	if err != nil || len(s) != 9 || !slices.Contains(s, "trk.v1."+c3+".>") {
		t.Fatalf("%v %v", s, err)
	}
}

// The feed's readiness: unknown before it opens, down since T with the
// reason while a subject does not open, up once all are open.
func TestFeedProbe(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	f := &Feed{Subjects: []string{"trk.v1.>"}, Take: func([]byte) {}, From: time.Now, Retry: 10 * time.Millisecond,
		Open: func(context.Context, string, time.Time, func([]byte)) (func(), error) {
			if fail.Load() {
				return nil, errors.New("TRK not found")
			}
			return func() {}, nil
		}}
	if st, _ := f.Probe(context.Background()); st != obs.StateUnknown {
		t.Fatal(st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx)
	waitFor(t, "down", func() bool {
		st, d := f.Probe(context.Background())
		return st == obs.StateDown && strings.Contains(d, "TRK not found")
	})
	fail.Store(false)
	waitFor(t, "up", func() bool { st, _ := f.Probe(context.Background()); return st == obs.StateUp })
}

// C-05 in the neighbour table: an undeclared sample keeps the flight's
// last known flying state, so a neighbour seen airborne still receives
// the nearby alert; a sample that says ground changes it (E-01 pair).
func TestTableKeepsFlyingOnUndeclared(t *testing.T) {
	g := newRig(t, true)
	g.offer(flightB, nil, origin, "Airborne")
	g.clk.Add(time.Second)
	g.offer(flightB, nil, origin, "Undeclared")
	flyingOf := func() (bool, bool) {
		nb := g.eng.table.near(origin, 10)
		if len(nb) != 1 {
			return false, false
		}
		return nb[0].Flying, nb[0].SeenAt.Equal(g.clk.Now())
	}
	waitFor(t, "the undeclared sample in the table", func() bool { _, fresh := flyingOf(); return fresh })
	if f, _ := flyingOf(); !f {
		t.Fatal("an undeclared sample landed a flight seen airborne")
	}
	g.clk.Add(time.Second)
	g.offer(flightB, nil, origin, "Ground")
	waitFor(t, "the ground sample in the table", func() bool { _, fresh := flyingOf(); return fresh })
	if f, _ := flyingOf(); f {
		t.Fatal("a ground sample left the flight flying")
	}
}

// B-11: switching a source off publishes the flight's source_disabled
// state once, on the tick that sees it; the ticks after it, the flight
// already disabled, publish nothing more for it (E-01 pair).
func TestEngineDisabledPublishesOnce(t *testing.T) {
	g := newRig(t, true)
	g.offer(flightA, ptr(intentA), origin, "Airborne")
	waitFor(t, "conforming", func() bool { return len(g.sink.states(flightA)) == 1 })
	g.gate.off.Store(true)
	disabled := func() int {
		n := 0
		for _, s := range g.sink.states(flightA) {
			if s.Reason != nil && *s.Reason == conformance.UnknownSourceDisabled {
				n++
			}
		}
		return n
	}
	waitFor(t, "source_disabled", func() bool { return disabled() > 0 })
	ticks := g.eng.Counters.Get(CounterTicks)
	waitFor(t, "ten more ticks", func() bool { return g.eng.Counters.Get(CounterTicks) >= ticks+10 })
	if n := disabled(); n != 1 {
		t.Fatalf("%d source_disabled states for one switch-off", n)
	}
}
