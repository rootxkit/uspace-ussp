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
	out, next, ps := place(rx, []Frame{frame(snA, 1, rx.Add(-3*time.Second))}, nil, false, a, pol)
	if ps.kept || next == nil || !next.learnt.Equal(rx) || !out[0].capturedAt.Equal(rx.Add(-3*time.Second)) {
		t.Fatalf("relearn: kept %v next %+v out %+v", ps.kept, next, out[0])
	}
	// The same age with input 30 s late by the old relation: kept.
	out, next, ps = place(rx, []Frame{frame(snA, 2, t0.Add(40*time.Second))}, nil, false, a, pol)
	if !ps.kept || next.learnt != a.learnt || !out[0].byAnchor || !out[0].backlog {
		t.Fatalf("kept: kept %v next %+v out %+v", ps.kept, next, out[0])
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

// The probe of the review: a live stream, then one batch whose sent_at
// claims it was sent 300 s after its sample. Only receipt timing of the
// live stream teaches the anchor, so the batch neither moves the later
// live samples into the past nor blocks the anchor from following them;
// the batch sample itself disagrees with that timing, so it is placed at
// its receipt and the disagreement is counted. Twin: an honest sent_at
// batch of 60 s old samples is still placed by its sent_at.
func TestSentAtCannotPoisonTheAnchor(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.pol.Values.TelemetryBacklogRateHz = 100
	s := r.in.NewSession(clientA, t0)
	live := func(seq int64, at time.Duration) {
		t.Helper()
		r.clk.set(t0.Add(at + 200*time.Millisecond))
		one(t, r.take(clientA, s, frame(snA, seq, t0.Add(at))), OutcomeAccepted)
	}
	for i := range 3 {
		live(int64(i), time.Duration(i)*time.Second)
	}
	// The poisoning batch: sample of t0+3 s, sent_at = ts + 300 s.
	r.clk.set(t0.Add(3*time.Second + 200*time.Millisecond))
	ts := t0.Add(3 * time.Second)
	sent := ts.Add(300 * time.Second)
	res := r.in.Take(t.Context(), Delivery{ClientID: clientA, RxTS: r.clk.now(), SentAt: &sent, Frames: []Frame{frame(snA, 3, ts)}})
	one(t, res, OutcomeAccepted)
	if r.counters.Get(CounterSentAtDisagrees) != 1 {
		t.Fatalf("disagreement not counted: %v", r.counters.Snapshot())
	}
	// Later live samples, also after the anchor's age.
	for i := 4; i < 8; i++ {
		live(int64(i), time.Duration(i)*time.Second)
	}
	for i := 8; i < 12; i++ {
		live(int64(i), 70*time.Second+time.Duration(i)*time.Second)
	}
	waitFor(t, func() bool { return len(r.pub.tracks(t)) == 12 })
	for _, tr := range r.pub.tracks(t) {
		if lag := tr.RxTS.Sub(tr.CapturedAt.Time); tr.Backlog || lag < 0 || lag > time.Second {
			t.Fatalf("seq %d captured %v rx %v backlog %v", tr.Body.Seq, tr.CapturedAt.Time, tr.RxTS.Time, tr.Backlog)
		}
	}
	if r.counters.Get(CounterAnchorKept) != 0 {
		t.Fatalf("anchor kept: %v", r.counters.Snapshot())
	}

	// Twin: an honest sent_at agrees with receipt timing and is believed.
	r.clk.set(t0.Add(100 * time.Second))
	old := t0.Add(40 * time.Second)
	sent = r.clk.now()
	f := frame(snA, 100, old)
	f.Backlog = true
	one(t, r.in.Take(t.Context(), Delivery{ClientID: clientA, RxTS: r.clk.now(), SentAt: &sent, Frames: []Frame{f}}), OutcomeAccepted)
	waitFor(t, func() bool { return len(r.pub.tracks(t)) == 13 })
	if tr := r.pub.tracks(t)[12]; !tr.CapturedAt.Equal(old) || !tr.Backlog || r.counters.Get(CounterSentAtDisagrees) != 1 {
		t.Fatalf("honest sent_at: captured %v backlog %v %v", tr.CapturedAt.Time, tr.Backlog, r.counters.Snapshot())
	}
}

// A live stream whose clock stepped back further than backlog_after_s is
// followed once it has been consistent for the anchor's age: the old
// relation would place every later sample as history (anchor_kept) for
// ever. Twin: a stalled reader (ever smaller delays at one read) never
// relearns.
func TestConsistentLiveStreamRelearnsTheAnchor(t *testing.T) {
	pol := defaultPlace()
	a := &anchor{ts: t0, placed: t0, learnt: t0}
	// The client's clock steps back 120 s; samples arrive on time.
	var out []placed
	var ps placeStats
	relearnt := false
	for i := 1; i <= 80; i++ {
		rx := t0.Add(time.Duration(i) * time.Second)
		out, a, ps = place(rx, []Frame{frame(snA, int64(i), rx.Add(-120*time.Second))}, nil, false, a, pol)
		if !out[0].backlog {
			relearnt = true
			if i < 60 {
				t.Fatalf("relearnt after %d s, before the anchor's age", i)
			}
			if !out[0].capturedAt.Equal(rx.Add(-120*time.Second)) && !out[0].capturedAt.Equal(rx) {
				t.Fatalf("captured %v at rx %v", out[0].capturedAt, rx)
			}
		} else if relearnt {
			t.Fatalf("%d: back to history after the relearn (kept %v)", i, ps.kept)
		}
	}
	if !relearnt {
		t.Fatal("a consistent live stream never relearnt the anchor")
	}
	// Twin: a reader stalled for 90 s reads everything at one instant.
	a = &anchor{ts: t0, placed: t0, learnt: t0}
	rx := t0.Add(91 * time.Second)
	for i := 1; i <= 90; i++ {
		out, a, _ = place(rx, []Frame{frame(snA, int64(i), t0.Add(time.Duration(i)*time.Second))}, nil, false, a, pol)
		if !out[0].capturedAt.Equal(t0.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("stall %d: captured %v", i, out[0].capturedAt)
		}
	}
}
