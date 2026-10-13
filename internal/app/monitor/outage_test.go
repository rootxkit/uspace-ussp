package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// busLink is the monitor's bus link as bus.Conn.Link reports it, driven
// by the scenario: up or down, since when.
type busLink struct {
	mu    sync.Mutex
	up    bool
	since time.Time
}

func (l *busLink) set(up bool, since time.Time) {
	l.mu.Lock()
	l.up, l.since = up, since
	l.mu.Unlock()
}

func (l *busLink) Link() (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.up, l.since
}

// newLinkedRig is newRig with the engine's input on link.
func newLinkedRig(t *testing.T, link *busLink) *rig {
	t.Helper()
	g := &rig{t: t, clk: newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)), intents: &fakeIntents{}, gate: &gate{}, sink: &sink{}}
	g.intents.set(intentA, stateBody(t, g.clk.Now()), true)
	link.set(true, g.clk.Now().Add(-time.Minute))
	own, _ := cell.ParseOwnership("all")
	g.eng = &Engine{Ownership: own, Intents: g.intents, Sources: g.gate, Sink: g.sink, Now: g.clk.Now, Tick: 10 * time.Millisecond,
		Policy: func() policy.Record { return policy.Record{Version: 9, Values: policy.Defaults()} }, Input: link.Link}
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	go g.eng.Run(ctx)
	t.Cleanup(cancel)
	waitFor(t, "the engine", func() bool { g.eng.mu.Lock(); defer g.eng.mu.Unlock(); return g.eng.ctx == ctx })
	return g
}

// ticks waits until the engine has ticked n more times on the clock as
// it is now: what it would have raised by then it has raised.
func (g *rig) ticks(n uint64) {
	g.t.Helper()
	from := g.eng.Counters.Get(CounterTicks)
	waitFor(g.t, "ticks", func() bool { return g.eng.Counters.Get(CounterTicks) >= from+n })
}

func (g *rig) lastState(flight string) conformance.State {
	s := g.sink.states(flight)
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1].State
}

// The Q35 scenario, its first half: a 60 s NATS outage while the
// aircraft keeps flying. Before, the monitor judged its own blindness as
// the aircraft's silence and raised lost_link for every flight. Now the
// outage raises none, is said as a monitoring outage (counted, on the
// status line, suspended until lost_link_s after the return), and the
// samples after the return are judged as they come.
func TestScenarioNATSOutageRaisesNoLostLinkAndSaysMonitoringOutage(t *testing.T) {
	link := &busLink{}
	g := newLinkedRig(t, link)
	a := ptr(intentA)
	g.offer(flightA, a, origin, "Airborne")
	waitFor(t, "conforming", func() bool { return g.lastState(flightA) == conformance.StateConforming })

	// The bus goes away; the monitor hears nothing for 60 s.
	g.clk.Add(time.Second)
	down := g.clk.Now()
	link.set(false, down)
	for range 60 {
		g.clk.Add(time.Second)
		g.ticks(2)
	}
	if n := len(g.sink.alerts("lost_link", "raised", flightA)); n != 0 {
		t.Fatalf("%d lost_link raised during a NATS outage, want none", n)
	}
	if g.lastState(flightA) == conformance.StateLostLink {
		t.Fatal("the flight was put in lost_link by the monitor's own outage")
	}
	out := g.eng.Outage()
	if out.DownSince == nil || !out.DownSince.Equal(down) || g.eng.Counters.Get(CounterInputOutage) != 1 {
		t.Fatalf("the monitoring outage was not said: %+v, outages %d", out, g.eng.Counters.Get(CounterInputOutage))
	}
	st := StatusOf("m1", g.clk.Now(), g.eng.Summary(), traffic.Summary{}, nil, geo.Freshness{}, 0, true, 9, false, false)
	if st.InputDownSince == nil || !st.InputDownSince.Equal(down) {
		t.Fatalf("the status line does not say the outage: %+v", st)
	}
	if _, err := bus.DecodeMonitorStatus(mustEncode(t, st)); err != nil {
		t.Fatalf("the status line with an outage does not read back: %v", err)
	}

	// The bus is back; the aircraft is heard again at once.
	back := g.clk.Now()
	link.set(true, back)
	g.ticks(2)
	out = g.eng.Outage()
	if out.DownSince != nil || out.BackAt == nil || !out.BackAt.Equal(back) || out.SuspendedUntil == nil ||
		!out.SuspendedUntil.Equal(back.Add(secs(policy.Defaults().LostLinkS))) {
		t.Fatalf("after the return: %+v", out)
	}
	for range 30 {
		g.clk.Add(time.Second)
		g.offer(flightA, a, origin, "Airborne")
		g.ticks(2)
	}
	if n := len(g.sink.alerts("lost_link", "raised", flightA)); n != 0 {
		t.Fatalf("%d lost_link raised for a flight heard after the return", n)
	}
	if s := g.lastState(flightA); s != conformance.StateConforming {
		t.Fatalf("state %s after the return, want conforming from the real samples", s)
	}
	if out := g.eng.Outage(); out.SuspendedUntil != nil {
		t.Fatalf("still suspended %v after lost_link_s", out.SuspendedUntil)
	}
}

// Its presence twin, the bus up all along: the aircraft stops sending,
// lost_link is raised after lost_link_s, nothing is said as a monitoring
// outage, and its next sample clears the alert (recovery clears it).
func TestScenarioRealLinkLossStillRaisesLostLinkAndClears(t *testing.T) {
	link := &busLink{}
	g := newLinkedRig(t, link)
	a := ptr(intentA)
	g.offer(flightA, a, origin, "Airborne")
	waitFor(t, "conforming", func() bool { return g.lastState(flightA) == conformance.StateConforming })
	g.clk.Add(16 * time.Second)
	waitFor(t, "lost_link raised", func() bool { return len(g.sink.alerts("lost_link", "raised", flightA)) == 1 })
	waitFor(t, "lost_link state", func() bool { return g.lastState(flightA) == conformance.StateLostLink })
	if g.eng.Counters.Get(CounterInputOutage) != 0 || g.eng.Outage().DownSince != nil {
		t.Fatal("a link loss with the bus up was said as a monitoring outage")
	}
	g.clk.Add(time.Second)
	g.offer(flightA, a, origin, "Airborne")
	waitFor(t, "lost_link cleared", func() bool {
		c := g.sink.alerts("lost_link", "cleared", flightA)
		return len(c) == 1 && *c[0].ClearReason == conformance.ClearResolved
	})
}

// After an outage, an aircraft that is still silent is judged from the
// return: lost_link lost_link_s after it, never before, and cleared by
// its next sample.
func TestScenarioSilenceAfterTheReturnRaisesLostLink(t *testing.T) {
	link := &busLink{}
	g := newLinkedRig(t, link)
	a := ptr(intentA)
	g.offer(flightA, a, origin, "Airborne")
	waitFor(t, "conforming", func() bool { return g.lastState(flightA) == conformance.StateConforming })
	g.clk.Add(time.Second)
	link.set(false, g.clk.Now())
	g.clk.Add(30 * time.Second)
	g.ticks(2)
	back := g.clk.Now()
	link.set(true, back)
	g.clk.Add(secs(policy.Defaults().LostLinkS) - time.Second)
	g.ticks(3)
	if n := len(g.sink.alerts("lost_link", "raised", flightA)); n != 0 {
		t.Fatalf("lost_link raised %d times within lost_link_s of the return", n)
	}
	g.clk.Add(time.Second)
	waitFor(t, "lost_link after the return", func() bool { return len(g.sink.alerts("lost_link", "raised", flightA)) == 1 })
	g.clk.Add(time.Second)
	g.offer(flightA, a, origin, "Airborne")
	waitFor(t, "lost_link cleared", func() bool { return len(g.sink.alerts("lost_link", "cleared", flightA)) == 1 })
}

// An outage shorter than a tick (down and back between two reads) is
// still a return: counted, and the silence counts from it.
func TestAnOutageBetweenTwoReadsIsSeen(t *testing.T) {
	e := &Engine{Now: func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }}
	e.init()
	link := &busLink{}
	link.set(true, e.now().Add(-time.Minute))
	e.iw.input = link.Link
	if f := e.iw.feed(e, e.now(), 15); f.Down || !f.BackAt.IsZero() {
		t.Fatalf("the connection made before the start is a return: %+v", f)
	}
	link.set(true, e.now().Add(time.Second))
	f := e.iw.feed(e, e.now().Add(2*time.Second), 15)
	if f.Down || !f.BackAt.Equal(e.now().Add(time.Second)) || e.Counters.Get(CounterInputOutage) != 1 {
		t.Fatalf("%+v, outages %d", f, e.Counters.Get(CounterInputOutage))
	}
}

// A tick takes its now before it reads the input, so the link can go
// down between the two: its since is after the tick's now, and is when
// the outage began. It was taken for a time in the future and replaced
// by the tick's now, an outage said as starting before it did (the
// intermittent failure of the Q35 scenario, whose clock moves between a
// tick's two reads). Only a since after the clock read with the input
// is in the future.
func TestAnOutageThatBeganAfterTheTicksNowStartsAtItsSince(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	clk := newClock(t0)
	e := &Engine{Now: clk.Now}
	e.init()
	link := &busLink{}
	link.set(true, t0.Add(-time.Minute))
	e.iw.input = link.Link
	tickNow := clk.Now()
	clk.Add(time.Second)
	link.set(false, clk.Now())
	if f := e.iw.feed(e, tickNow, 15); !f.Down {
		t.Fatalf("the input is down: %+v", f)
	}
	if o := e.iw.outage(clk.Now(), 15); o.DownSince == nil || !o.DownSince.Equal(t0.Add(time.Second)) {
		t.Fatalf("down since %v, want %v", o.DownSince, t0.Add(time.Second))
	}

	// The pair: a since later than the clock itself is in the future and
	// is said as the clock's now.
	e2 := &Engine{Now: clk.Now}
	e2.init()
	link2 := &busLink{}
	link2.set(false, clk.Now().Add(time.Hour))
	e2.iw.input = link2.Link
	e2.iw.feed(e2, tickNow, 15)
	if o := e2.iw.outage(clk.Now(), 15); o.DownSince == nil || !o.DownSince.Equal(clk.Now()) {
		t.Fatalf("a future since: down since %v, want the clock's %v", o.DownSince, clk.Now())
	}
}

func mustEncode(t *testing.T, st bus.MonitorStatus) []byte {
	t.Helper()
	b, err := bus.EncodeMonitorStatus(st)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// failingKV is monitor_status on a bus that is down: every put fails.
type failingKV struct{ puts int }

func (f *failingKV) Put(context.Context, string, []byte) error {
	f.puts++
	return errors.New("nats: connection closed")
}

// Q35 review: a monitoring outage is written to monitor_status, on the
// bus that is down, so nothing of it reached anyone while it lasted, and
// the return cleared its start. It is now recorded locally: the
// monitor's own /readyz entry input says the outage and its start while
// the bus is down (no bus needed), and says the outage window during the
// grace after the return. The window stays on the status line
// (last_input_down_since, input_back_at) once the bus takes it again.
// With the input up all along the entry is up and nothing is said (the
// pair).
func TestScenarioMonitoringOutageRecordedLocally(t *testing.T) {
	link := &busLink{}
	g := newLinkedRig(t, link)
	g.offer(flightA, ptr(intentA), origin, "Airborne")
	waitFor(t, "conforming", func() bool { return g.lastState(flightA) == conformance.StateConforming })
	probe := g.eng.InputProbe()
	if st, detail := probe(context.Background()); st != obs.StateUp || detail != "" {
		t.Fatalf("input up: %s %q", st, detail)
	}
	g.clk.Add(time.Second)
	down := g.clk.Now()
	link.set(false, down)
	g.clk.Add(30 * time.Second)
	g.ticks(2)

	kv := &failingKV{}
	w := &statusWriter{KV: kv, Counters: g.eng.Counters, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.write(context.Background(), StatusOf("m1", g.clk.Now(), g.eng.Summary(), traffic.Summary{}, nil, geo.Freshness{}, 0, true, 9, false, false))
	if kv.puts != 1 || g.eng.Counters.Get(CounterStatusWriteFailed) != 1 {
		t.Fatalf("the status write on the bus that is down: %d puts, %d failed", kv.puts, g.eng.Counters.Get(CounterStatusWriteFailed))
	}
	st, detail := probe(context.Background())
	if st != obs.StateDown || !strings.Contains(detail, "monitoring outage since "+down.UTC().Format(time.RFC3339)) ||
		!strings.Contains(detail, "no lost_link is judged") {
		t.Fatalf("readiness during the outage: %s %q", st, detail)
	}

	back := g.clk.Now()
	link.set(true, back)
	g.ticks(2)
	st, detail = probe(context.Background())
	window := "monitoring outage from " + down.UTC().Format(time.RFC3339) + " to " + back.UTC().Format(time.RFC3339)
	if st != obs.StateDegraded || !strings.Contains(detail, window) {
		t.Fatalf("readiness in the grace: %s %q", st, detail)
	}
	line := StatusOf("m1", g.clk.Now(), g.eng.Summary(), traffic.Summary{}, nil, geo.Freshness{}, 0, true, 9, false, false)
	if line.InputDownSince != nil || line.LastInputDownSince == nil || !line.LastInputDownSince.Equal(down) ||
		line.InputBackAt == nil || !line.InputBackAt.Equal(back) {
		t.Fatalf("the status line after the return does not carry the outage: %+v", line)
	}
	if _, err := bus.DecodeMonitorStatus(mustEncode(t, line)); err != nil {
		t.Fatalf("the status line with the window does not read back: %v", err)
	}
	g.clk.Add(secs(policy.Defaults().LostLinkS))
	g.ticks(2)
	if st, detail := probe(context.Background()); st != obs.StateUp || !strings.Contains(detail, window) {
		t.Fatalf("readiness after the grace: %s %q", st, detail)
	}
}
