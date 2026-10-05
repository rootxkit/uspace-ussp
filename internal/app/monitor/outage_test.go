package monitor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/geo"
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

func mustEncode(t *testing.T, st bus.MonitorStatus) []byte {
	t.Helper()
	b, err := bus.EncodeMonitorStatus(st)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
