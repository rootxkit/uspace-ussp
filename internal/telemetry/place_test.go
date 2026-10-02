package telemetry

import (
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/timeplace"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

func defaultPlace() PlacePolicy { return placePolicy(policy.Defaults()) }

// SC-15 in unit form (T-11): an aircraft streams live, then the reader
// stalls for 30 s and reads 30 s of samples at once. They are placed by
// their own time through the aircraft's anchor, not stamped at the read,
// and the ones older than backlog_after_s are backlog; the newest is
// live. The same samples on a session that never learnt the clock
// (E-01 twin) would be stamped at the read: the anchor is what places
// them.
func TestStalledReaderPlacedByTheProducersTime(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.pol.Values.TelemetryRateHz = 100 // the stall delivers 30 s at once; the rate is not under test
	s := r.in.NewSession(clientA, t0)
	// Live: one sample a second, 200 ms in transit.
	for i := range 3 {
		r.clk.set(t0.Add(time.Duration(i)*time.Second + 200*time.Millisecond))
		one(t, r.take(clientA, s, frame(snA, int64(i), t0.Add(time.Duration(i)*time.Second))), OutcomeAccepted)
	}
	// The reader stalls; at t0+33 s it reads the samples of t0+3..t0+32.
	r.clk.set(t0.Add(33 * time.Second))
	for i := 3; i <= 32; i++ {
		one(t, r.take(clientA, s, frame(snA, int64(i), t0.Add(time.Duration(i)*time.Second))), OutcomeAccepted)
	}
	trs := r.pub.tracks(t)
	if len(trs) != 33 {
		t.Fatalf("%d tracks", len(trs))
	}
	for _, tr := range trs[3:] {
		want := tr.TS.Time // the live samples taught a believed client clock
		if !tr.CapturedAt.Equal(want) {
			t.Fatalf("seq %d captured %v, want %v (stamped at the read: %v)", tr.Body.Seq, tr.CapturedAt.Time, want, tr.RxTS.Time)
		}
		lag := tr.RxTS.Sub(tr.CapturedAt.Time)
		if tr.Backlog != (lag > 10*time.Second) {
			t.Fatalf("seq %d lag %v backlog %v", tr.Body.Seq, lag, tr.Backlog)
		}
	}
	if !trs[3].Backlog || trs[32].Backlog {
		t.Fatal("the oldest read late must be backlog and the newest live")
	}
	if r.counters.Get(CounterPlacedByAnchor) == 0 || r.counters.Get(CounterBacklogByAge) == 0 {
		t.Fatalf("counters %v", r.counters.Snapshot())
	}

	// Twin: without the anchor the core rule alone places a 30 s old
	// sample at the read (too_old), which is what the anchor prevents.
	p, note, shown := PlaceOne(t0.Add(3*time.Second), nil, t0.Add(33*time.Second), defaultPlace())
	if !shown || note != timeplace.NoteTooOld || !p.CapturedAt.Equal(t0.Add(33*time.Second)) {
		t.Fatalf("core alone: %+v %s %v", p, note, shown)
	}
}

// SC-15 for a batch: 30 s of samples delivered in one request without
// sent_at are placed by the batch rule (their own spacing) and the older
// ones are backlog, never stamped at the read.
func TestBatchOfThirtySecondsPlacedBySpacing(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.pol.Values.TelemetryBacklogRateHz, r.pol.Values.TelemetryRateHz = 100, 100
	var frames []Frame
	for i := range 30 {
		frames = append(frames, frame(snA, int64(i), t0.Add(time.Duration(i)*time.Second-time.Hour))) // a client clock an hour slow
	}
	r.clk.set(t0.Add(30 * time.Second))
	res := r.in.Take(t.Context(), Delivery{ClientID: clientA, RxTS: r.clk.now(), BatchRule: true, Frames: frames})
	for _, x := range res {
		if x.Reason != OutcomeAccepted {
			t.Fatalf("%v", reasons(res))
		}
	}
	waitFor(t, func() bool { return len(r.pub.tracks(t)) == 30 })
	trs := r.pub.tracks(t)
	for i, tr := range trs {
		want := r.clk.now().Add(-time.Duration(29-i) * time.Second)
		if !tr.CapturedAt.Equal(want) || tr.TimeSource != core.TimeSourceClock {
			t.Fatalf("%d: captured %v want %v", i, tr.CapturedAt.Time, want)
		}
		if tr.Backlog != (29-i > 10) {
			t.Fatalf("%d backlog %v", i, tr.Backlog)
		}
	}
}

// T-02 with sent_at: a drained backlog keeps its own times on our clock
// whatever the client's clock says.
func TestBatchWithSentAtPlacedAgainstIt(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.pol.Values.TelemetryBacklogRateHz = 100
	skew := -5 * time.Minute // the client's clock is five minutes slow
	sent := t0.Add(skew)
	var frames []Frame
	for i := range 3 {
		f := frame(snA, int64(i), sent.Add(time.Duration(i-60)*time.Second))
		f.Backlog = true
		frames = append(frames, f)
	}
	r.clk.set(t0.Add(100 * time.Millisecond))
	r.in.Take(t.Context(), Delivery{ClientID: clientA, RxTS: r.clk.now(), SentAt: &sent, Frames: frames})
	waitFor(t, func() bool { return len(r.pub.tracks(t)) == 3 })
	for i, tr := range r.pub.tracks(t) {
		want := r.clk.now().Add(time.Duration(i-60) * time.Second)
		if !tr.CapturedAt.Equal(want) || !tr.Backlog {
			t.Fatalf("%d: captured %v want %v backlog %v", i, tr.CapturedAt.Time, want, tr.Backlog)
		}
	}
}

// The anchor is relearnt after its age from on-time input (a clock that
// stepped is followed), and kept when the input is late (a stall is not
// a clock change).
func TestAnchorRelearntOnTimeKeptWhenLate(t *testing.T) {
	pol := defaultPlace()
	a := &anchor{ts: t0, placed: t0.Add(time.Second), learnt: t0}
	// 70 s later the client's clock stepped 3 s: on time by the new
	// relation, 3 s late by the old one (within backlog_after): relearnt.
	rx := t0.Add(70 * time.Second)
	out, _, next, kept := place(rx, []Frame{frame(snA, 1, rx.Add(-3*time.Second))}, nil, false, a, pol)
	if kept || next == nil || !next.learnt.Equal(rx) || !out[0].capturedAt.Equal(rx.Add(-3*time.Second)) {
		t.Fatalf("relearn: kept %v next %+v out %+v", kept, next, out[0])
	}
	// The same age with input 30 s late by the old relation: kept.
	out, _, next, kept = place(rx, []Frame{frame(snA, 2, t0.Add(40*time.Second))}, nil, false, a, pol)
	if !kept || next.learnt != a.learnt || !out[0].byAnchor || !out[0].backlog {
		t.Fatalf("kept: kept %v next %+v out %+v", kept, next, out[0])
	}
}

// PlaceOne maps core's sources at this boundary: a believed client time
// is source_clock, a placement at receipt receiver.
func TestPlaceOneSources(t *testing.T) {
	pol := defaultPlace()
	p, _, _ := PlaceOne(t0, nil, t0.Add(time.Second), pol)
	if p.Source != core.TimeSourceClock || !p.CapturedAt.Equal(t0) {
		t.Fatalf("believed: %+v", p)
	}
	p, note, _ := PlaceOne(t0.Add(5*time.Second), nil, t0, pol)
	if p.Source != core.TimeReceiver || note != timeplace.NoteClockAhead {
		t.Fatalf("ahead: %+v %s", p, note)
	}
	sent := t0.Add(-time.Second)
	p, note, _ = PlaceOne(t0, &sent, t0, pol)
	if p.Source != core.TimeReceiver || note != timeplace.NoteAheadOfResponse {
		t.Fatalf("ahead of sent_at: %+v %s", p, note)
	}
}
