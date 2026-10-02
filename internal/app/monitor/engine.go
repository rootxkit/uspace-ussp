package monitor

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Bounds (E-10).
const (
	// DefaultQueueLen is one worker's queue of samples; a full queue
	// drops the sample, counted (monitor_queue_full) and logged: a gap
	// is never silent.
	DefaultQueueLen = 4096
	// DefaultMaxFlights bounds the flights one worker tracks; past it a
	// new flight is refused (monitor_flights_over_bound), never one that
	// holds an alert evicted.
	DefaultMaxFlights = 10_000
	// MaxWorkers bounds the workers (one per home cell3).
	MaxWorkers = cell.MaxOwnedCells
	// MaxTableFlights bounds the neighbour table.
	MaxTableFlights = 100_000
	// DefaultConfHeartbeat is the longest a flight's conformance/state/v1
	// waits between two publishes while nothing changes (at most 0.1 Hz
	// per flight): CONF carries the transitions and this heartbeat, not
	// one message per sample.
	DefaultConfHeartbeat = 10 * time.Second
)

// Counters of the engine.
const (
	CounterDecodeFailed     = "monitor_track_unreadable"
	CounterNotOurs          = "monitor_track_not_ours"
	CounterNotOwned         = "monitor_track_not_owned"
	CounterQueueFull        = "monitor_queue_full"
	CounterFlightsOverBound = "monitor_flights_over_bound"
	CounterWorkersOverBound = "monitor_workers_over_bound"
	CounterTableOverBound   = "monitor_table_over_bound"
	CounterNoIntent         = "monitor_flight_without_intent"
	CounterIntentEnded      = "monitor_intent_ended"
	CounterFlightEnded      = "monitor_flight_ended"
	CounterFlightForgotten  = "monitor_flight_forgotten"
	CounterAlertNoCell      = "monitor_alert_without_cell"
	CounterOutboxFull       = "monitor_outbox_full"
	CounterPublishFailed    = "monitor_publish_failed"
	CounterPublishDropped   = "monitor_publish_dropped"
	CounterPublished        = "monitor_published"
	CounterTicks            = "monitor_ticks"
	CounterTableSwept       = "monitor_table_swept"
)

// IntentSource is the intent_active projection (bus.Mirror).
type IntentSource interface {
	Intent(id string) (b intent.StateBody, found bool, ageS float64, loaded bool)
}

// IntentLister lists intent_active (bus.Mirror): an IntentSource that is
// one lets a start take over the flights its intents say are
// nonconforming or contingent before their first sample.
type IntentLister interface {
	Intents() (vals map[string]intent.StateBody, loaded bool)
}

// SourceGate is the source-control follower (internal/sources).
type SourceGate interface {
	Query(sourceType string, instanceID *string) coresources.Decision
}

// Sink is where the messages go (bus.Publisher).
type Sink interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// Engine is the monitor's conformance path: it routes every track of
// this USSP's flights to the worker of the flight's home cell3 (the
// cell3 of its first sample, kept while the flight stays in this
// instance's cells so one tracker follows it), keeps the process's
// neighbour table, and publishes what the workers decide through a
// bounded outbox. A worker owns its trackers; nothing is shared between
// workers but the table, read under its own lock and copied out small.
//
// With a Store, every tracker's state machine is persisted
// (conformance_state) on each change and at least every ConfHeartbeat:
// a start restores the flights this instance owns before the track feed
// opens (a silent flight still loses its link), and a flight that
// crosses into another instance's cells is released by this one and
// taken over by that one with its alerts. A tracker that starts without
// a saved state takes its intent's nonconforming or contingent state,
// and a restored one counts the hysteresis from the restore: neither a
// restart nor a handover returns a flight to conforming early.
type Engine struct {
	Ownership  cell.Ownership
	Intents    IntentSource
	Sources    SourceGate
	Policy     func() policy.Record
	Sink       Sink
	Counters   *core.Counters
	Logger     *slog.Logger
	Now        func() time.Time
	Tick       time.Duration
	QueueLen   int
	MaxFlights int
	// OutboxLen bounds the outbox (DefaultOutboxLen).
	OutboxLen int
	// ConfHeartbeat is the period of an unchanged flight's state
	// (DefaultConfHeartbeat), published and persisted.
	ConfHeartbeat time.Duration
	// Store persists each flight's state machine (KVStates); nil keeps
	// it in memory only.
	Store StateStore
	// InstanceID names this instance as the owner of the flights it
	// persists (the host name: stable across a restart in place).
	InstanceID string

	once    sync.Once
	ctx     context.Context
	mu      sync.Mutex
	workers map[string]*worker
	home    map[string]string // flight id -> worker cell3
	table   *table
	out     *outbox
	persist *persister
	seeded  chan struct{}
	seedEnd sync.Once
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger == nil {
		return obs.Discard()
	}
	return e.Logger
}

func (e *Engine) policy() policy.Record {
	if e.Policy == nil {
		return policy.Record{Values: policy.Defaults()}
	}
	return e.Policy()
}

func (e *Engine) init() {
	e.once.Do(func() {
		if e.Counters == nil {
			e.Counters = &core.Counters{}
		}
		if e.Tick <= 0 {
			e.Tick = time.Second
		}
		if e.QueueLen <= 0 {
			e.QueueLen = DefaultQueueLen
		}
		if e.MaxFlights <= 0 {
			e.MaxFlights = DefaultMaxFlights
		}
		if e.ConfHeartbeat <= 0 {
			e.ConfHeartbeat = DefaultConfHeartbeat
		}
		e.workers, e.home = map[string]*worker{}, map[string]string{}
		e.table = &table{flights: map[string]tableEntry{}}
		e.out = &outbox{sink: e.Sink, counters: e.Counters, logger: e.logger(), ch: make(chan outMsg, max(e.OutboxLen, DefaultOutboxLen))}
		e.seeded = make(chan struct{})
		if e.Store != nil {
			if e.InstanceID == "" {
				e.InstanceID = "monitor"
			}
			e.persist = newPersister(e.Store, e.InstanceID, e.Counters, e.logger(), e.yield)
		}
		e.mu.Lock()
		if e.ctx == nil {
			e.ctx = context.Background()
		}
		e.mu.Unlock()
	})
}

// Run starts the outbox and keeps the engine until ctx ends; workers
// start with their first flight and stop with ctx. With a Store it first
// restores the flights this instance owns (Seeded closes then), and
// takes over the nonconforming or contingent intents no instance
// restored once intent_active is read.
func (e *Engine) Run(ctx context.Context) {
	e.init()
	e.mu.Lock()
	e.ctx = ctx
	e.mu.Unlock()
	go e.sweep(ctx)
	if e.persist != nil {
		go e.persist.run(ctx)
		e.preload(ctx)
		go e.seedIntents(ctx)
	}
	e.seedEnd.Do(func() { close(e.seeded) })
	e.out.run(ctx)
}

// Seeded is closed once Run has restored the saved flights: the track
// feed opens after it, so no sample starts a flight a saved state holds.
func (e *Engine) Seeded() <-chan struct{} {
	e.init()
	return e.seeded
}

// preload restores every saved flight whose last cell this instance
// owns and that no live instance owns.
func (e *Engine) preload(ctx context.Context) {
	states, err := e.Store.All(ctx)
	if err != nil {
		e.Counters.Inc(CounterStateLoadFailed)
		e.logger().LogAttrs(ctx, slog.LevelError, "conformance states not read at the start: flights start again from their intents' states",
			obs.Err(err))
	}
	for i := range states {
		st := states[i]
		id := st.Tracker.FlightID
		if !bus.ValidKey(id) {
			e.Counters.Inc(CounterStateUnreadable)
			continue
		}
		if st.Owner != "" && st.Owner != e.InstanceID && time.Since(st.SavedAt) < liveOwnerS {
			e.Counters.Inc(CounterStateNotOurs)
			continue
		}
		c3, err := cell.Parent3(st.Tracker.LastCell5)
		if err != nil || !e.Ownership.Owns(c3) {
			continue
		}
		w := e.assign(id, c3)
		if w == nil {
			continue
		}
		select {
		case w.ch <- item{restore: &st}:
		case <-ctx.Done():
			return
		}
	}
}

// seedIntents waits for intent_active and starts, from its intent's
// state, every flight whose intent is nonconforming or contingent that
// no saved state started; the instance owning the smallest cell3 of the
// intent's cell set does it.
func (e *Engine) seedIntents(ctx context.Context) {
	lister, ok := e.Intents.(IntentLister)
	if !ok {
		return
	}
	t := time.NewTicker(e.Tick)
	defer t.Stop()
	var vals map[string]intent.StateBody
	for {
		var loaded bool
		if vals, loaded = lister.Intents(); loaded {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	for _, id := range slices.Sorted(maps.Keys(vals)) {
		b := vals[id]
		if b.FlightID == nil || (b.LocalState != conformance.IntentNonconforming && b.LocalState != conformance.IntentContingent) {
			continue
		}
		home := ""
		for _, c5 := range b.CellSet {
			if c3, err := cell.Parent3(c5); err == nil && (home == "" || c3 < home) {
				home = c3
			}
		}
		if home == "" || !e.Ownership.Owns(home) {
			continue
		}
		e.mu.Lock()
		_, tracked := e.home[*b.FlightID]
		e.mu.Unlock()
		if tracked {
			continue
		}
		w := e.assign(*b.FlightID, home)
		if w == nil {
			continue
		}
		select {
		case w.ch <- item{seed: &intentSeed{flightID: *b.FlightID, intentID: id, state: b.LocalState}}:
		case <-ctx.Done():
			return
		}
	}
}

// yield is the persister's finding that another instance owns the
// flight now: its worker lets it go without clearing anything.
func (e *Engine) yield(flightID string) {
	e.mu.Lock()
	w := e.workers[e.home[flightID]]
	e.mu.Unlock()
	if w == nil {
		return
	}
	select {
	case w.ch <- item{yield: flightID}:
	default:
		e.Counters.Inc(CounterQueueFull)
	}
}

// sweep drops from the neighbour table every flight silent for longer
// than flight_end_after_s, every 10 s until ctx ends.
func (e *Engine) sweep(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cut := e.now().Add(-secs(e.policy().Values.FlightEndAfterS))
			e.Counters.Add(CounterTableSwept, uint64(e.table.sweep(cut)))
		}
	}
}

// Offer takes one trk.v1 message. It never blocks: a full worker queue
// drops the sample, counted.
func (e *Engine) Offer(data []byte) {
	e.init()
	tr, ours, err := conformance.DecodeTrack(data)
	switch {
	case err != nil:
		e.Counters.Inc(CounterDecodeFailed)
		return
	case !ours:
		e.Counters.Inc(CounterNotOurs)
		return
	}
	in := conformance.InputOf(tr)
	flightID := *tr.Body.FlightID
	e.table.put(tableEntry{
		FlightID: flightID, IntentID: deref(tr.Body.IntentID), Position: in.Sample.Position, Cell5: in.Cell5,
		SeenAt: in.Sample.CapturedAt, Flying: in.Flying != nil && *in.Flying, flyingKnown: in.Flying != nil,
	}, e.Counters)
	w := e.route(flightID, in.Cell5)
	if w == nil {
		return
	}
	select {
	case w.ch <- item{tr: tr, in: in}:
	default:
		e.Counters.Inc(CounterQueueFull)
		e.logger().LogAttrs(context.Background(), slog.LevelError, "monitor queue full: sample dropped, judged by nothing",
			obs.FlightID(flightID), slog.String("cell", w.cell3))
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// route is the worker of the flight's home cell3, started on first use;
// nil when this instance does not own the flight or the bound is hit.
// With a Store, a sample of a tracked flight in a cell this instance
// does not own releases the flight to the instance that does.
func (e *Engine) route(flightID, cell5 string) *worker {
	e.mu.Lock()
	defer e.mu.Unlock()
	c3, err := cell.Parent3(cell5)
	home, ok := e.home[flightID]
	switch {
	case ok && e.persist != nil && err == nil && !e.Ownership.Owns(c3):
		if w := e.workers[home]; w != nil {
			select {
			case w.ch <- item{release: flightID}:
			default:
				e.Counters.Inc(CounterQueueFull)
			}
		}
		return nil
	case !ok:
		if err != nil {
			e.Counters.Inc(CounterDecodeFailed)
			return nil
		}
		if !e.Ownership.Owns(c3) {
			e.Counters.Inc(CounterNotOwned)
			return nil
		}
		if len(e.home) >= MaxTableFlights {
			e.Counters.Inc(CounterFlightsOverBound)
			return nil
		}
		home = c3
		e.home[flightID] = home
	}
	return e.workerLocked(home)
}

// assign homes a flight in home (unless it has a home) and returns its
// worker; nil at a bound.
func (e *Engine) assign(flightID, home string) *worker {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.home[flightID]; ok {
		home = h
	} else {
		if len(e.home) >= MaxTableFlights {
			e.Counters.Inc(CounterFlightsOverBound)
			return nil
		}
		e.home[flightID] = home
	}
	return e.workerLocked(home)
}

// workerLocked is home's worker, started on first use (e.mu held).
func (e *Engine) workerLocked(home string) *worker {
	w := e.workers[home]
	if w == nil {
		if len(e.workers) >= MaxWorkers {
			e.Counters.Inc(CounterWorkersOverBound)
			return nil
		}
		w = &worker{e: e, cell3: home, ch: make(chan item, e.QueueLen), flights: map[string]*flight{},
			nearby: &conformance.Nearby{Counters: e.Counters}}
		e.workers[home] = w
		ctx := e.ctx
		go w.run(ctx)
	}
	return w
}

// FlightEnded is the end of a flight (flight.v1 ended): every alert it
// holds or caused clears flight_ended, in every worker.
func (e *Engine) FlightEnded(flightID string) {
	e.init()
	e.mu.Lock()
	ws := slices.Collect(maps.Values(e.workers))
	e.mu.Unlock()
	for _, w := range ws {
		select {
		case w.ch <- item{ended: flightID}:
		default:
			e.Counters.Inc(CounterQueueFull)
			e.logger().LogAttrs(context.Background(), slog.LevelError, "monitor queue full: flight end not delivered; the intent's end clears it",
				obs.FlightID(flightID), slog.String("cell", w.cell3))
		}
	}
}

// forget removes a flight's route once its worker dropped it.
func (e *Engine) forget(flightID string) {
	e.unhome(flightID)
	e.table.remove(flightID)
}

func (e *Engine) unhome(flightID string) {
	e.mu.Lock()
	delete(e.home, flightID)
	e.mu.Unlock()
}

// Summary is what the status line says of the conformance path.
type Summary struct {
	Workers int            `json:"workers"`
	Flights int            `json:"flights_tracked"`
	States  map[string]int `json:"states"`
	// EvaluationPeriodS is the longest tick period a worker measured in
	// the last status period (spec 05 §3: widened, never skipped).
	EvaluationPeriodS float64 `json:"evaluation_period_s"`
	OutboxDepth       int     `json:"outbox_depth"`
}

// Summary collects every worker's last summary.
func (e *Engine) Summary() Summary {
	e.init()
	e.mu.Lock()
	ws := slices.Collect(maps.Values(e.workers))
	e.mu.Unlock()
	s := Summary{Workers: len(ws), States: map[string]int{}}
	for _, w := range ws {
		w.mu.Lock()
		s.Flights += w.sum.flights
		for k, v := range w.sum.states {
			s.States[k] += v
		}
		s.EvaluationPeriodS = max(s.EvaluationPeriodS, w.sum.periodS)
		w.sum.periodS = 0
		w.mu.Unlock()
	}
	s.OutboxDepth = len(e.out.ch)
	return s
}

// item is one unit of a worker's queue: a sample, a flight's end, a
// saved flight to restore, a flight to release to another instance or
// to yield to one that took it, or an intent to start a flight from.
type item struct {
	tr      *telemetry.Track
	in      conformance.Input
	ended   string
	restore *StoredState
	release string
	yield   string
	seed    *intentSeed
}

// intentSeed is a flight started from its intent's state.
type intentSeed struct {
	flightID, intentID, state string
}

// flight is one tracked flight.
type flight struct {
	tr       *conformance.Tracker
	instance string // the client the samples came from (the source instance)
	intentID string
	lastAt   time.Time
	// lastConf is when the flight's state was last published.
	lastConf time.Time
	// lastSave is when it was last persisted; claimed once its first
	// save (which claims the flight for this instance) is queued;
	// lastCheck when another instance's ownership was last asked.
	lastSave, lastCheck time.Time
	claimed             bool
}

// worker is one home cell3's loop: samples and a tick every Engine.Tick.
type worker struct {
	e       *Engine
	cell3   string
	ch      chan item
	flights map[string]*flight
	nearby  *conformance.Nearby

	mu      sync.Mutex
	sum     workerSummary
	lastTck time.Time
}

type workerSummary struct {
	flights int
	states  map[string]int
	periodS float64
}

func (w *worker) run(ctx context.Context) {
	t := time.NewTicker(w.e.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-w.ch:
			w.take(ctx, it)
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// config is the state machine's configuration of the policy in force.
func (w *worker) config() (conformance.Config, policy.Values) {
	r := w.e.policy()
	return conformance.ConfigOf(r), r.Values
}

// authorisation reads the flight's intent from intent_active.
func (w *worker) authorisation(in *conformance.Input, intentID string) {
	if intentID == "" {
		in.MissingReason = conformance.UnknownNoIntent
		return
	}
	b, found, _, loaded := w.e.Intents.Intent(intentID)
	switch {
	case !loaded:
		in.MissingReason = conformance.UnknownProjectionUnavailable
	case !found:
		in.MissingReason = conformance.UnknownAuthorisationMissing
	default:
		a, err := conformance.AuthorisationOf(b)
		if err != nil {
			in.AuthErr = err
			return
		}
		in.Auth = &a
		in.IntentState = b.LocalState
	}
}

func (w *worker) take(ctx context.Context, it item) {
	now := w.e.now()
	switch {
	case it.ended != "":
		w.end(ctx, it.ended, now, conformance.ClearFlightEnded)
		w.e.Counters.Inc(CounterFlightEnded)
		return
	case it.restore != nil:
		if w.flights[it.restore.Tracker.FlightID] == nil && w.bounded(ctx, it.restore.Tracker.FlightID) {
			f := w.restore(it.restore, now)
			cfg, _ := w.config()
			w.publishState(ctx, f, conformance.Events{}, systemTimes(now), cfg.PolicyVersion, now)
			w.save(f, now, true)
		}
		return
	case it.release != "":
		w.letGo(it.release, true)
		return
	case it.yield != "":
		w.letGo(it.yield, false)
		return
	case it.seed != nil:
		w.seed(ctx, it.seed, now)
		return
	}
	tr := it.tr
	id := *tr.Body.FlightID
	f := w.flights[id]
	if f == nil {
		if !w.bounded(ctx, id) {
			return
		}
		intentID := deref(tr.Body.IntentID)
		if f = w.load(ctx, id, now); f == nil {
			f = &flight{tr: conformance.NewTracker(id, intentID, "", w.e.Counters), intentID: intentID}
			w.flights[id] = f
		}
		if intentID == "" {
			w.e.Counters.Inc(CounterNoIntent)
		}
	}
	f.instance, f.lastAt = tr.Body.SourceInstance, now
	in := it.in
	if w.e.Sources != nil {
		inst := tr.Body.SourceInstance
		in.SourceDisabled = !w.e.Sources.Query(telemetry.SourceOperatorWS, &inst).Enabled
	}
	w.authorisation(&in, f.intentID)
	cfg, _ := w.config()
	ev := f.tr.Observe(in, cfg, now)
	// A change is published at once; an admitted sample that changed
	// nothing only as the heartbeat.
	changed := len(ev.Transitions) > 0 || len(ev.Alerts) > 0
	if changed || (ev.Admitted && now.Sub(f.lastConf) >= w.e.ConfHeartbeat) {
		w.publishState(ctx, f, ev, tr.Times(), cfg.PolicyVersion, now)
	}
	w.publishAlerts(ctx, ev.Alerts, now)
	w.save(f, now, changed)
}

// bounded is false, counted and logged, when the worker tracks as many
// flights as it may.
func (w *worker) bounded(ctx context.Context, id string) bool {
	if len(w.flights) < w.e.MaxFlights {
		return true
	}
	w.e.Counters.Inc(CounterFlightsOverBound)
	w.e.logger().LogAttrs(ctx, slog.LevelError, "monitor at its flight bound: flight not judged", obs.FlightID(id))
	return false
}

// load restores a flight this worker takes over from its saved state
// (a handover, or a flight that was not restored at the start); nil
// when there is none or it cannot be read, and the flight starts from
// its intent's state.
func (w *worker) load(ctx context.Context, id string, now time.Time) *flight {
	if w.e.Store == nil {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, loadTimeout)
	st, found, err := w.e.Store.Load(lctx, id)
	cancel()
	switch {
	case err != nil:
		w.e.Counters.Inc(CounterStateLoadFailed)
		w.e.logger().LogAttrs(ctx, slog.LevelWarn, "conformance state not read: the flight starts from its intent's state", obs.FlightID(id), obs.Err(err))
		return nil
	case !found || st.Tracker.FlightID != id:
		return nil
	}
	return w.restore(&st, now)
}

// restore makes the worker track a saved flight, with the nearby alerts
// it raised, from now (conformance.RestoreTracker).
func (w *worker) restore(st *StoredState, now time.Time) *flight {
	id := st.Tracker.FlightID
	f := &flight{tr: conformance.RestoreTracker(st.Tracker, w.e.Counters, now), instance: st.Instance,
		intentID: st.Tracker.IntentID, lastAt: st.LastAt}
	if f.lastAt.IsZero() || f.lastAt.After(now) {
		f.lastAt = now
	}
	w.nearby.Restore(st.Nearby)
	w.flights[id] = f
	if w.e.persist != nil {
		w.e.persist.adopt(id, st.Rev)
	}
	w.e.Counters.Inc(CounterStateRestored)
	return f
}

// seed starts a flight from its intent's nonconforming or contingent
// state (its saved state when there is one).
func (w *worker) seed(ctx context.Context, s *intentSeed, now time.Time) {
	if w.flights[s.flightID] != nil || !w.bounded(ctx, s.flightID) {
		return
	}
	f := w.load(ctx, s.flightID, now)
	ev := conformance.Events{}
	if f == nil {
		f = &flight{tr: conformance.NewTracker(s.flightID, s.intentID, "", w.e.Counters), intentID: s.intentID, lastAt: now}
		w.flights[s.flightID] = f
		ev = f.tr.SeedFromIntent(s.state, now)
		w.e.Counters.Inc(CounterStateSeeded)
	}
	cfg, _ := w.config()
	w.publishState(ctx, f, ev, systemTimes(now), cfg.PolicyVersion, now)
	w.save(f, now, true)
}

// letGo stops tracking a flight without clearing anything: released
// (its state saved with no owner) when it left this instance's cells,
// yielded when another instance took it over. Its nearby alerts go with
// it.
func (w *worker) letGo(id string, release bool) {
	f := w.flights[id]
	if f == nil {
		return
	}
	if release && w.e.persist != nil {
		w.e.persist.put(id, persistOp{kind: opRelease, s: w.saved(f)})
		w.e.Counters.Inc(CounterReleased)
	} else {
		w.e.Counters.Inc(CounterYielded)
	}
	active := f.tr.Active()
	for i := range active {
		w.nearby.Forget(active[i].ID)
	}
	delete(w.flights, id)
	w.e.unhome(id)
}

// saved is the flight's persisted state.
func (w *worker) saved(f *flight) Saved {
	s := Saved{Home: w.cell3, Instance: f.instance, LastAt: f.lastAt, Tracker: f.tr.State()}
	active := f.tr.Active()
	for i := range active {
		s.Nearby = append(s.Nearby, conformance.SaveAlerts(w.nearby.Of(active[i].ID))...)
	}
	return s
}

// save persists the flight's state on a change (force) and otherwise at
// most every heartbeat; the first save claims the flight.
func (w *worker) save(f *flight, now time.Time, force bool) {
	if w.e.persist == nil || (!force && f.claimed && now.Sub(f.lastSave) < w.e.ConfHeartbeat) {
		return
	}
	f.lastSave = now
	kind := opSave
	if !f.claimed {
		kind, f.claimed = opClaim, true
	}
	w.e.persist.put(f.tr.FlightID, persistOp{kind: kind, s: w.saved(f)})
}

// end drops a flight's tracker (its alerts clear with reason) and every
// nearby alert it held as a neighbour.
func (w *worker) end(ctx context.Context, id string, now time.Time, reason string) {
	if f := w.flights[id]; f != nil {
		ev := f.tr.Drop(reason, now)
		w.publishAlerts(ctx, ev.Alerts, now)
		delete(w.flights, id)
		w.e.forget(id)
		if w.e.persist != nil {
			w.e.persist.put(id, persistOp{kind: opDelete})
		}
	}
	w.publishAlerts(ctx, w.nearby.DropFlight(id, now), now)
}

func (w *worker) tick(ctx context.Context) {
	now := w.e.now()
	w.e.Counters.Inc(CounterTicks)
	cfg, vals := w.config()
	_, _, _, loaded := w.e.Intents.Intent("")
	states := map[string]int{}
	ids := slices.Sorted(maps.Keys(w.flights))
	for _, id := range ids {
		f := w.flights[id]
		// The intent ended (it left intent_active): flight_ended.
		if f.intentID != "" && loaded {
			if _, found, _, _ := w.e.Intents.Intent(f.intentID); !found {
				w.e.Counters.Inc(CounterIntentEnded)
				w.end(ctx, id, now, conformance.ClearFlightEnded)
				continue
			}
		}
		if w.e.Sources != nil && f.instance != "" {
			inst := f.instance
			if !w.e.Sources.Query(telemetry.SourceOperatorWS, &inst).Enabled {
				// Published once, when it changes the flight: a flight
				// already disabled has nothing new to say.
				if ev := f.tr.Disable(now); len(ev.Transitions) > 0 || len(ev.Alerts) > 0 {
					w.publishState(ctx, f, ev, systemTimes(now), cfg.PolicyVersion, now)
					w.publishAlerts(ctx, ev.Alerts, now)
					w.save(f, now, true)
				}
			}
		}
		ev := f.tr.Tick(cfg, now)
		// A link-lost flight sends no sample that would heal a dropped
		// state, so its state is republished every tick.
		if len(ev.Transitions) > 0 || f.tr.Snapshot().LinkLost {
			w.publishState(ctx, f, ev, systemTimes(now), cfg.PolicyVersion, now)
		}
		w.publishAlerts(ctx, ev.Alerts, now)
		w.save(f, now, len(ev.Transitions) > 0 || len(ev.Alerts) > 0)
		// A flight silent here may have crossed into another instance's
		// cells without a sample in this one's ring: ask whether that
		// instance took it over.
		if w.e.persist != nil && now.Sub(f.lastAt) > yieldCheckAfter && now.Sub(f.lastCheck) > yieldCheckAfter {
			f.lastCheck = now
			w.e.persist.put(id, persistOp{kind: opCheck})
		}
		// A flight silent past the flight end with nothing active and no
		// lost link is forgotten (its flight ended at the ingest); one
		// that holds an alert is kept until its intent ends.
		snap := f.tr.Snapshot()
		if len(f.tr.Active()) == 0 && !snap.LinkLost && now.Sub(f.lastAt) > secs(vals.FlightEndAfterS) {
			delete(w.flights, id)
			w.e.forget(id)
			if w.e.persist != nil {
				w.e.persist.put(id, persistOp{kind: opDelete})
			}
			w.e.Counters.Inc(CounterFlightForgotten)
			continue
		}
		states[string(snap.State)]++
	}
	// The nearby fan-out of every active source alert, then the
	// republish of every active alert with its current numbers (C-08).
	maxAge := secs(vals.LostLinkS)
	for _, id := range slices.Sorted(maps.Keys(w.flights)) {
		f := w.flights[id]
		snap := f.tr.Snapshot()
		active := f.tr.Active()
		for i := range active {
			nbs := w.e.table.near(snap.LastPosition, vals.NonconformanceNearbyRadiusM)
			evs := w.nearby.Refresh(active[i], snap.LastPosition, nbs, vals.NonconformanceNearbyRadiusM, maxAge, now, cfg.PolicyVersion)
			w.publishAlerts(ctx, evs, now)
			if len(evs) > 0 {
				w.save(f, now, true)
			}
		}
	}
	w.clearOrphanNearby(ctx, now)
	var active []conformance.Alert
	for _, f := range w.flights {
		active = append(active, f.tr.Active()...)
	}
	active = append(active, w.nearby.Active()...)
	for i := range active {
		w.publish(ctx, conformance.AlertEvent{State: conformance.AlertUpdated, Alert: active[i]}, now)
	}
	w.mu.Lock()
	if !w.lastTck.IsZero() {
		w.sum.periodS = max(w.sum.periodS, now.Sub(w.lastTck).Seconds())
	}
	w.lastTck = now
	w.sum.flights, w.sum.states = len(w.flights), states
	w.mu.Unlock()
}

// clearOrphanNearby clears the nearby alerts whose source alert is no
// longer active (the source cleared it this tick or before), with the
// reason it cleared with when known, else flight_ended.
func (w *worker) clearOrphanNearby(ctx context.Context, now time.Time) {
	active := map[string]bool{}
	for _, f := range w.flights {
		as := f.tr.Active()
		for i := range as {
			active[as[i].ID] = true
		}
	}
	nearby := w.nearby.Active()
	for i := range nearby {
		src, _ := nearby[i].Detail["source_alert_id"].(string)
		if !active[src] {
			w.publishAlerts(ctx, w.nearby.Clear(src, conformance.ClearFlightEnded, now), now)
		}
	}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func systemTimes(now time.Time) core.Times {
	return core.Times{TS: &now, RxTS: now, CapturedAt: now, Source: core.TimeSystem}
}

// publishState sends the flight's conformance/state/v1.
func (w *worker) publishState(_ context.Context, f *flight, ev conformance.Events, times core.Times, pv int64, now time.Time) {
	subject, err := bus.Conf(f.tr.FlightID)
	if err != nil {
		w.e.Counters.Inc(CounterPublishFailed)
		return
	}
	f.lastConf = now
	w.e.out.put(subject, conformance.StateMessageOf(f.tr.Snapshot(), ev, times, pv))
}

// publishAlerts sends each event; a source alert's clear clears its
// nearby alerts with the same reason.
func (w *worker) publishAlerts(ctx context.Context, evs []conformance.AlertEvent, now time.Time) {
	for i := range evs {
		e := &evs[i]
		w.publish(ctx, *e, now)
		if e.State == conformance.AlertCleared && e.Alert.Kind != conformance.KindNonconformanceNearby {
			cleared := w.nearby.Clear(e.Alert.ID, e.ClearReason, now)
			for j := range cleared {
				w.publish(ctx, cleared[j], now)
			}
		}
	}
}

func (w *worker) publish(_ context.Context, e conformance.AlertEvent, now time.Time) {
	c5 := e.Alert.Cell5
	if c5 == "" {
		w.e.Counters.Inc(CounterAlertNoCell)
		w.e.logger().Error("alert without a cell: not published", slog.String("alert_id", e.Alert.ID), obs.FlightID(e.Alert.FlightID))
		return
	}
	subject, err := bus.Alrt(e.Alert.Kind, c5, e.Alert.ID)
	if err != nil {
		w.e.Counters.Inc(CounterPublishFailed)
		return
	}
	w.e.out.put(subject, conformance.AlertMessageOf(e, now))
}
