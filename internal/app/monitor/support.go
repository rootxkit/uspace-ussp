package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// DefaultOutboxLen bounds the messages waiting to be published.
const DefaultOutboxLen = 16_384

// publishTimeout bounds one publish of the outbox.
const publishTimeout = 2 * time.Second

type outMsg struct {
	subject string
	m       bus.Enveloped
}

// outbox publishes the workers' messages in order, off their loops: a
// worker never waits on NATS (C-13). Full, it drops the message,
// counted and logged; a dropped state is healed by the next sample's
// (the consumers apply the latest state), a dropped alert by the next
// second's republish (C-08). A failed publish is counted and logged,
// not retried: the next one carries the current state.
type outbox struct {
	sink     Sink
	counters *core.Counters
	logger   *slog.Logger
	ch       chan outMsg

	mu       sync.Mutex
	lastWarn time.Time
}

func (o *outbox) put(subject string, m bus.Enveloped) {
	select {
	case o.ch <- outMsg{subject, m}:
	default:
		o.counters.Inc(CounterOutboxFull)
		o.warn("monitor outbox full: message dropped; the next state or republish replaces it", subject)
	}
}

// warn logs at most once a second.
func (o *outbox) warn(msg, subject string) {
	o.mu.Lock()
	now := time.Now()
	if now.Sub(o.lastWarn) < time.Second {
		o.mu.Unlock()
		return
	}
	o.lastWarn = now
	o.mu.Unlock()
	o.logger.LogAttrs(context.Background(), slog.LevelError, msg, slog.String("subject", subject))
}

func (o *outbox) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-o.ch:
			if o.sink == nil {
				o.counters.Inc(CounterPublishFailed)
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, publishTimeout)
			err := o.sink.Publish(pctx, m.subject, m.m)
			cancel()
			if err != nil {
				o.counters.Inc(CounterPublishFailed)
				o.warn("monitor publish failed: "+err.Error(), m.subject)
				continue
			}
			o.counters.Inc(CounterPublished)
		}
	}
}

// tableEntry is one flight's last sample in the neighbour table.
type tableEntry struct {
	FlightID string
	IntentID string
	Position core.LatLon
	Cell5    string
	SeenAt   time.Time
	Flying   bool
	// flyingKnown is false for a sample whose status says neither
	// (Undeclared): the entry keeps the flight's last known Flying (C-05).
	flyingKnown bool
}

// table is the process's neighbour table: the last sample of every
// flight this instance receives, for the nearby fan-out. Safe for
// concurrent use; a query copies out only what is within its radius.
type table struct {
	mu      sync.RWMutex
	flights map[string]tableEntry
}

func (t *table) put(e tableEntry, counters *core.Counters) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.flights[e.FlightID]; ok {
		if e.SeenAt.Before(old.SeenAt) {
			return // never overwrite a newer state (T-06)
		}
		if !e.flyingKnown {
			e.Flying, e.flyingKnown = old.Flying, old.flyingKnown
		}
	} else if len(t.flights) >= MaxTableFlights {
		counters.Inc(CounterTableOverBound)
		return
	}
	t.flights[e.FlightID] = e
}

// sweep removes the flights last seen before cutoff, and returns how
// many it removed.
func (t *table) sweep(cutoff time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for id, e := range t.flights {
		if e.SeenAt.Before(cutoff) {
			delete(t.flights, id)
			n++
		}
	}
	return n
}

func (t *table) remove(id string) {
	t.mu.Lock()
	delete(t.flights, id)
	t.mu.Unlock()
}

// near are the flights within radiusM of p (a box prefilter, then the
// fan-out measures the geodesic distance itself).
func (t *table) near(p core.LatLon, radiusM float64) []conformance.Neighbour {
	if !p.Valid() || !core.IsFinite(radiusM) || radiusM <= 0 {
		return nil
	}
	box := geodesy.BBox{MinLat: p.LatDeg, MaxLat: p.LatDeg, MinLon: p.LonDeg, MaxLon: p.LonDeg}.PadM(radiusM)
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []conformance.Neighbour
	for _, e := range t.flights {
		if box.Contains(e.Position) {
			out = append(out, conformance.Neighbour{FlightID: e.FlightID, IntentID: e.IntentID, Cell5: e.Cell5,
				Position: e.Position, SeenAt: e.SeenAt, Flying: e.Flying})
		}
	}
	return out
}

// MirrorIntents is the intent_active mirror as an IntentSource. An id
// that cannot be a key is not found (and says whether the bucket was
// read).
type MirrorIntents struct{ M *bus.Mirror[intentBody] }

// Intent implements IntentSource.
func (i MirrorIntents) Intent(id string) (intent.StateBody, bool, float64, bool) {
	if !bus.ValidKey(id) {
		_, age, loaded := i.M.Snapshot()
		return intent.StateBody{}, false, age, loaded
	}
	return i.M.Get(id)
}

// Feed reads TRK on each subject from From() on, then live, into Take;
// a subject that cannot be opened (NATS down, TRK missing) is retried
// every 2 s and says so on /readyz (trk: down since T).
type Feed struct {
	Subjects []string
	Take     func([]byte)
	Open     func(ctx context.Context, subject string, from time.Time, handle func([]byte)) (stop func(), err error)
	From     func() time.Time
	Logger   *slog.Logger
	Retry    time.Duration

	mu     sync.Mutex
	open   map[string]bool
	since  time.Time
	reason string
}

func (f *Feed) set(subject string, up bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open == nil {
		f.open = map[string]bool{}
	}
	was := f.allOpenLocked()
	f.open[subject] = up
	if f.since.IsZero() || was != f.allOpenLocked() {
		f.since = time.Now().UTC()
	}
	if !up {
		f.reason = reason
	}
}

func (f *Feed) allOpenLocked() bool {
	if len(f.open) < len(f.Subjects) {
		return false
	}
	for _, up := range f.open {
		if !up {
			return false
		}
	}
	return true
}

// Probe is the readiness entry trk.
func (f *Feed) Probe(context.Context) (obs.State, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.since.IsZero():
		return obs.StateUnknown, "the track consumers have not opened yet: nothing is judged"
	case !f.allOpenLocked():
		return obs.StateDown, "down since " + f.since.Format(time.RFC3339) + ": " + f.reason + "; flights in those cells are not judged"
	}
	return obs.StateUp, ""
}

// Run keeps every subject's consumer open until ctx ends.
func (f *Feed) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range f.Subjects {
		wg.Go(func() { f.one(ctx, s) })
	}
	wg.Wait()
}

func (f *Feed) one(ctx context.Context, subject string) {
	retry := f.Retry
	if retry <= 0 {
		retry = 2 * time.Second
	}
	logger := f.Logger
	if logger == nil {
		logger = obs.Discard()
	}
	for ctx.Err() == nil {
		stop, err := f.Open(ctx, subject, f.From(), f.Take)
		if err != nil {
			f.set(subject, false, "the TRK consumer of "+subject+" does not open: "+err.Error())
			logger.LogAttrs(ctx, slog.LevelWarn, "track feed not open; retried", slog.String("subject", subject), obs.Err(err))
			t := time.NewTimer(retry)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
			continue
		}
		f.set(subject, true, "")
		<-ctx.Done()
		stop()
	}
}

// StatusAttrs are the conformance status line's attributes.
func StatusAttrs(s Summary, intentsAge float64, intentsLoaded bool, policyVersion int64) []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("workers", s.Workers), slog.Int("flights_tracked", s.Flights), slog.Any("states", s.States),
		slog.Float64("evaluation_period_s", s.EvaluationPeriodS), slog.Int("outbox_depth", s.OutboxDepth),
		slog.Int64("policy_version", policyVersion),
	}
	if intentsLoaded {
		attrs = append(attrs, slog.Float64("intent_active_age_s", intentsAge))
	} else {
		attrs = append(attrs, slog.String("intent_active", "not read: every flight is unknown, nothing is judged (SC-22)"))
	}
	return attrs
}
