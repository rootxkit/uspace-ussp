package ridsp

import (
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Only our own flights are served: an authenticated operator_ws track
// with a flight is taken; a broadcast, another source, a track without
// a flight is not ours; a message that does not read is unreadable
// (each counted, E-01 both ways).
func TestTakeKeepsOnlyOurOwnFlights(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	w.Take(trackMsg(t, body(1, origin), t0))
	if w.Len() != 1 || w.Counters.Get(CounterSamplesAccepted) != 1 {
		t.Fatalf("our track not kept: %d %v", w.Len(), w.Counters.Snapshot())
	}
	broadcast := body(2, origin)
	broadcast.Trust = core.TrustBroadcast
	peer := body(3, origin)
	peer.Source = "peer_sp"
	none := body(4, origin)
	none.FlightID = nil
	for _, b := range []telemetry.TrackBody{broadcast, peer, none} {
		w.Take(trackMsg(t, b, t0))
	}
	if w.Len() != 1 || w.Counters.Get(CounterSamplesNotOurs) != 3 {
		t.Fatalf("a track that is not ours was kept: %d %v", w.Len(), w.Counters.Snapshot())
	}
	bad := body(5, origin)
	bad.FlightID = sptr("not-a-uuid")
	for _, raw := range [][]byte{[]byte("{"), []byte(`{"schema":"track/manned/v1"}`), trackMsg(t, bad, t0)} {
		w.Take(raw)
	}
	if w.Len() != 1 || w.Counters.Get(CounterSamplesUnreadable) != 3 {
		t.Fatalf("unreadable messages: %d %v", w.Len(), w.Counters.Snapshot())
	}
}

// Nothing older than 60 s: a sample older than the horizon is refused,
// one within it is kept; as time passes the query stops returning what
// aged out even before a sweep, and the sweep removes the flight.
func TestNothingOlderThanTheHorizon(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	if w.Add(sample(1, origin, t0.Add(-Horizon-time.Millisecond))) {
		t.Fatal("a sample older than 60 s was kept")
	}
	if w.Counters.Get(CounterSamplesStale) != 1 {
		t.Fatalf("stale not counted: %v", w.Counters.Snapshot())
	}
	if !w.Add(sample(1, origin, t0.Add(-Horizon))) || !w.Add(sample(1, origin, t0)) {
		t.Fatal("samples within 60 s refused")
	}
	box := geodesy.BBox{MinLat: 41.7, MaxLat: 41.73, MinLon: 44.8, MaxLon: 44.85}
	vs, err := w.InView(box, Horizon)
	if err != nil || len(vs) != 1 || len(vs[0].Recent) != 2 {
		t.Fatalf("%v %+v", err, vs)
	}
	c.add(time.Second)
	vs, _ = w.InView(box, Horizon)
	if len(vs) != 1 || len(vs[0].Recent) != 1 || !vs[0].Recent[0].CapturedAt.Equal(t0) {
		t.Fatalf("a position older than 60 s served: %+v", vs)
	}
	c.add(Horizon)
	if vs, _ := w.InView(box, Horizon); len(vs) != 0 {
		t.Fatalf("a flight silent for more than 60 s served: %+v", vs)
	}
	if _, ok := w.Flight(flightN(1)); ok {
		t.Fatal("details of a flight silent for more than 60 s")
	}
	if n := w.Sweep(); n != 0 || len(w.cells) != 0 {
		t.Fatalf("sweep left %d flights, %d cells", n, len(w.cells))
	}
}

// E-10: beyond rid_recent_positions_max_count the oldest samples go,
// counted; within it nothing goes. Beyond MaxFlights the flight silent
// the longest goes, counted.
func TestWindowBounds(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	w.MaxSamples = func() int { return 3 }
	for i := range 3 {
		w.Add(sample(1, origin, t0.Add(time.Duration(i-10)*time.Second)))
	}
	if v, _ := w.Flight(flightN(1)); len(v.Recent) != 3 || w.Counters.Get(CounterSamplesOverBound) != 0 {
		t.Fatalf("within the bound: %d %v", len(v.Recent), w.Counters.Snapshot())
	}
	w.Add(sample(1, origin, t0))
	v, _ := w.Flight(flightN(1))
	if len(v.Recent) != 3 || w.Counters.Get(CounterSamplesOverBound) != 1 || !v.Recent[0].CapturedAt.Equal(t0.Add(-9*time.Second)) {
		t.Fatalf("over the bound: %+v %v", v.Recent, w.Counters.Snapshot())
	}
	w.MaxFlights = 2
	w.Add(sample(2, origin, t0.Add(-20*time.Second)))
	w.Add(sample(3, origin, t0))
	if w.Len() != 2 || w.Counters.Get(CounterFlightsOverBound) != 1 {
		t.Fatalf("flights over the bound: %d %v", w.Len(), w.Counters.Snapshot())
	}
	if _, ok := w.Flight(flightN(2)); ok {
		t.Fatal("the flight silent the longest was kept")
	}
}

// A sample that arrives late is placed by its time; the same instant
// twice is kept once (E-01 pair).
func TestOrderAndDuplicates(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	w.Add(sample(1, origin, t0))
	w.Add(sample(1, offset(origin, 100, 0), t0.Add(-2*time.Second)))
	if w.Add(sample(1, origin, t0)) || w.Counters.Get(CounterSamplesDuplicate) != 1 {
		t.Fatal("a duplicate was kept")
	}
	v, _ := w.Flight(flightN(1))
	if len(v.Recent) != 2 || !v.Current.CapturedAt.Equal(t0) || !v.Recent[0].CapturedAt.Before(v.Recent[1].CapturedAt) {
		t.Fatalf("%+v", v)
	}
}

// A flight is in a view when its current position is, and when only a
// recent position is (F3411: any recent position inside the view); a
// flight that was never inside is not.
func TestInViewByAnyRecentPosition(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	inside := origin
	outside := offset(origin, 0, 5000)
	w.Add(sample(1, inside, t0))                                   // current inside
	w.Add(sample(2, inside, t0.Add(-10*time.Second)))              // was inside
	w.Add(sample(2, outside, t0))                                  // now outside
	w.Add(sample(3, outside, t0))                                  // never inside
	w.Add(sample(4, offset(inside, 300, 0), t0.Add(-time.Second))) // inside, other cell side
	box := geodesy.BBox{MinLat: inside.LatDeg - 0.005, MaxLat: inside.LatDeg + 0.005, MinLon: inside.LonDeg - 0.01, MaxLon: inside.LonDeg + 0.01}
	vs, err := w.InView(box, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, v := range vs {
		ids = append(ids, v.ID)
		if len(v.Recent) != 0 {
			t.Errorf("recent positions without a duration: %+v", v.Recent)
		}
	}
	if len(ids) != 3 || ids[0] != flightN(1) || ids[1] != flightN(2) || ids[2] != flightN(4) {
		t.Fatalf("flights in view %v", ids)
	}
}
