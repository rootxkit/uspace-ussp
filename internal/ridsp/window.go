package ridsp

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Horizon is how far back the window reaches and the oldest a served
// position may be: F3411's NetMaxNearRealTimeDataPeriodSeconds (60 s).
const Horizon = f3411.NetMaxNearRealTimeDataPeriodSeconds * time.Second

// DefaultMaxFlights bounds the flights the window holds (E-10): the
// ingest's own bound, one flight per aircraft.
const DefaultMaxFlights = 10_000

// Counters of the window.
const (
	CounterSamplesAccepted   = "rid_sp_samples_accepted"
	CounterSamplesNotOurs    = "rid_sp_samples_not_ours"
	CounterSamplesUnreadable = "rid_sp_samples_unreadable"
	CounterSamplesStale      = "rid_sp_samples_stale"
	CounterSamplesDuplicate  = "rid_sp_samples_duplicate"
	CounterSamplesOverBound  = "rid_sp_samples_over_bound"
	CounterFlightsOverBound  = "rid_sp_flights_over_bound"
)

var flightIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Sample is one of our tracks as the window keeps it.
type Sample struct {
	// CapturedAt is the sample's time on our clock (the envelope's
	// captured_at): the F3411 timestamp.
	CapturedAt time.Time
	Cell5      string
	Body       telemetry.TrackBody
}

// Position is the sample's position.
func (s Sample) Position() core.LatLon { return s.Body.Position.LatLon() }

// FlightID is the sample's flight, which is also its RID flight id.
func (s Sample) FlightID() string {
	if s.Body.FlightID == nil {
		return ""
	}
	return *s.Body.FlightID
}

// DecodeTrack reads one trk.v1 message. ours is false (and err nil) for
// a track this Service Provider does not serve: not authenticated, not
// from the operator ingest, or without a flight; a message that does
// not read as track/telemetry/v1 is an error. Never panics.
func DecodeTrack(data []byte) (s Sample, ours bool, err error) {
	var t telemetry.Track
	if err := json.Unmarshal(data, &t); err != nil {
		return Sample{}, false, core.Fieldf("message", "not a track/telemetry/v1 message")
	}
	if t.Schema != telemetry.SchemaTrack {
		return Sample{}, false, core.Fieldf("schema", "%q where %q is expected", t.Schema, telemetry.SchemaTrack)
	}
	if err := t.Validate(); err != nil {
		return Sample{}, false, err
	}
	b := t.Body
	if b.Trust != core.TrustAuthenticated || b.Source != telemetry.SourceOperatorWS || b.FlightID == nil {
		return Sample{}, false, nil
	}
	if !flightIDRe.MatchString(*b.FlightID) {
		return Sample{}, false, core.Fieldf("body.flight_id", "not a version 4 UUID")
	}
	pos := b.Position.LatLon()
	if !pos.Valid() {
		return Sample{}, false, core.Fieldf("body.position", "not a WGS84 position")
	}
	c5, _, err := cell.Key(pos)
	if err != nil {
		return Sample{}, false, err
	}
	return Sample{CapturedAt: t.CapturedAt.UTC(), Cell5: c5, Body: b}, true, nil
}

// View is one flight as a query sees it: the newest sample and the
// samples of the requested recent duration, oldest first.
type View struct {
	ID      string
	Current Sample
	Recent  []Sample
}

type flightWin struct {
	samples []Sample // by CapturedAt, oldest first
}

// Window keeps, per flight, the samples of the last Horizon (see the
// package documentation). Safe for concurrent use; a query never waits
// on anything but this memory.
type Window struct {
	// MaxSamples is the policy's rid_recent_positions_max_count.
	MaxSamples func() int
	// MaxFlights bounds the flights held (DefaultMaxFlights).
	MaxFlights int
	Now        func() time.Time
	Counters   *core.Counters

	mu      sync.RWMutex
	flights map[string]*flightWin
	cells   map[string]map[string]int // cell5 -> flight -> samples in it
}

func (w *Window) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Window) count(name string) {
	if w.Counters != nil {
		w.Counters.Inc(name)
	}
}

func (w *Window) maxSamples() int {
	n := policy.Defaults().RIDRecentPositionsMaxCount
	if w.MaxSamples != nil {
		n = w.MaxSamples()
	}
	return max(1, min(n, policy.MaxRIDRecentPositionsMaxCount))
}

func (w *Window) maxFlights() int {
	if w.MaxFlights > 0 {
		return w.MaxFlights
	}
	return DefaultMaxFlights
}

// Take decodes one trk.v1 message and adds it when it is ours, counting
// what it does with it.
func (w *Window) Take(data []byte) {
	s, ours, err := DecodeTrack(data)
	switch {
	case err != nil:
		w.count(CounterSamplesUnreadable)
	case !ours:
		w.count(CounterSamplesNotOurs)
	default:
		w.Add(s)
	}
}

// Add keeps s in its flight's window: a sample older than Horizon is
// refused (counted stale), the same flight and instant twice is kept
// once, and beyond MaxSamples the oldest samples go first (counted).
// It reports whether s was kept.
func (w *Window) Add(s Sample) bool {
	now := w.now()
	if now.Sub(s.CapturedAt) > Horizon {
		w.count(CounterSamplesStale)
		return false
	}
	id := s.FlightID()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flights == nil {
		w.flights, w.cells = map[string]*flightWin{}, map[string]map[string]int{}
	}
	f := w.flights[id]
	if f == nil {
		if len(w.flights) >= w.maxFlights() {
			w.evictOldestLocked()
		}
		f = &flightWin{}
		w.flights[id] = f
	}
	i := sort.Search(len(f.samples), func(i int) bool { return !f.samples[i].CapturedAt.Before(s.CapturedAt) })
	if i < len(f.samples) && f.samples[i].CapturedAt.Equal(s.CapturedAt) {
		w.count(CounterSamplesDuplicate)
		return false
	}
	f.samples = slices.Insert(f.samples, i, s)
	w.indexLocked(id, s.Cell5, 1)
	w.trimLocked(id, f, now)
	w.count(CounterSamplesAccepted)
	return true
}

// evictOldestLocked removes the flight whose newest sample is the
// oldest (E-10), counted. Called with mu held.
func (w *Window) evictOldestLocked() {
	var oldest string
	var at time.Time
	for id, f := range w.flights {
		if n := f.samples[len(f.samples)-1].CapturedAt; oldest == "" || n.Before(at) {
			oldest, at = id, n
		}
	}
	if oldest != "" {
		w.dropLocked(oldest, w.flights[oldest])
		w.count(CounterFlightsOverBound)
	}
}

func (w *Window) indexLocked(id, c5 string, d int) {
	m := w.cells[c5]
	if m == nil {
		if d < 0 {
			return
		}
		m = map[string]int{}
		w.cells[c5] = m
	}
	m[id] += d
	if m[id] <= 0 {
		delete(m, id)
		if len(m) == 0 {
			delete(w.cells, c5)
		}
	}
}

// trimLocked drops the samples older than Horizon and the oldest beyond
// MaxSamples, and the flight when none is left. Called with mu held.
func (w *Window) trimLocked(id string, f *flightWin, now time.Time) {
	cut := 0
	for cut < len(f.samples) && now.Sub(f.samples[cut].CapturedAt) > Horizon {
		cut++
	}
	if over := len(f.samples) - cut - w.maxSamples(); over > 0 {
		cut += over
		if w.Counters != nil {
			w.Counters.Add(CounterSamplesOverBound, uint64(over))
		}
	}
	for _, s := range f.samples[:cut] {
		w.indexLocked(id, s.Cell5, -1)
	}
	f.samples = slices.Delete(f.samples, 0, cut)
	if len(f.samples) == 0 {
		delete(w.flights, id)
	}
}

func (w *Window) dropLocked(id string, f *flightWin) {
	for _, s := range f.samples {
		w.indexLocked(id, s.Cell5, -1)
	}
	delete(w.flights, id)
}

// Sweep drops every sample older than Horizon and every flight left
// without one; it returns the flights held.
func (w *Window) Sweep() int {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, f := range w.flights {
		w.trimLocked(id, f, now)
	}
	return len(w.flights)
}

// Len is the flights held.
func (w *Window) Len() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.flights)
}

// view is f as a query sees it at now: nothing older than Horizon, the
// recent samples within recent of now. ok is false when nothing is left.
func view(id string, f *flightWin, now time.Time, recent time.Duration) (View, bool) {
	var live []Sample
	for _, s := range f.samples {
		if now.Sub(s.CapturedAt) <= Horizon {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return View{}, false
	}
	v := View{ID: id, Current: live[len(live)-1]}
	if recent > 0 {
		for _, s := range live {
			if now.Sub(s.CapturedAt) <= recent {
				v.Recent = append(v.Recent, s)
			}
		}
	}
	return v, true
}

// InView returns every flight with a sample of the last Horizon inside
// box (its current position or a recent one; F3411: any recent
// position inside the view), with the samples of the last recent, in
// flight id order. A box the cell grid cannot cover is an error.
func (w *Window) InView(box geodesy.BBox, recent time.Duration) ([]View, error) {
	cells, err := cell.CellsFor(box)
	if err != nil {
		return nil, err
	}
	now := w.now()
	w.mu.RLock()
	defer w.mu.RUnlock()
	seen := map[string]bool{}
	var out []View
	for _, c := range cells {
		for id := range w.cells[c] {
			if seen[id] {
				continue
			}
			seen[id] = true
			f := w.flights[id]
			if f == nil {
				continue
			}
			inside := false
			for _, s := range f.samples {
				if now.Sub(s.CapturedAt) <= Horizon && box.Contains(s.Position()) {
					inside = true
					break
				}
			}
			if !inside {
				continue
			}
			if v, ok := view(id, f, now, recent); ok {
				out = append(out, v)
			}
		}
	}
	slices.SortFunc(out, byID)
	return out, nil
}

func byID(a, b View) int { return strings.Compare(a.ID, b.ID) }

// Flight is one flight with every sample of the last Horizon as Recent;
// false when the window holds none of it.
func (w *Window) Flight(id string) (View, bool) {
	now := w.now()
	w.mu.RLock()
	defer w.mu.RUnlock()
	f := w.flights[id]
	if f == nil {
		return View{}, false
	}
	return view(id, f, now, Horizon)
}

// All is every flight with its current sample, in flight id order.
func (w *Window) All() []View {
	now := w.now()
	w.mu.RLock()
	out := make([]View, 0, len(w.flights))
	for id, f := range w.flights {
		if v, ok := view(id, f, now, 0); ok {
			out = append(out, v)
		}
	}
	w.mu.RUnlock()
	slices.SortFunc(out, byID)
	return out
}
