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
	CounterPublished        = "monitor_published"
	CounterTicks            = "monitor_ticks"
	CounterTableSwept       = "monitor_table_swept"
)

// IntentSource is the intent_active projection (bus.Mirror).
type IntentSource interface {
	Intent(id string) (b intent.StateBody, found bool, ageS float64, loaded bool)
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
// cell3 of its first sample, kept for the flight's life so one tracker
// follows it), keeps the process's neighbour table, and publishes what
// the workers decide through a bounded outbox. A worker owns its
// trackers; nothing is shared between workers but the table, read under
// its own lock and copied out small.
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

	once    sync.Once
	ctx     context.Context
	mu      sync.Mutex
	workers map[string]*worker
	home    map[string]string // flight id -> worker cell3
	table   *table
	out     *outbox
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
		e.workers, e.home = map[string]*worker{}, map[string]string{}
		e.table = &table{flights: map[string]tableEntry{}}
		e.out = &outbox{sink: e.Sink, counters: e.Counters, logger: e.logger(), ch: make(chan outMsg, max(e.OutboxLen, DefaultOutboxLen))}
		e.mu.Lock()
		if e.ctx == nil {
			e.ctx = context.Background()
		}
		e.mu.Unlock()
	})
}

// Run starts the outbox and keeps the engine until ctx ends; workers
// start with their first flight and stop with ctx.
func (e *Engine) Run(ctx context.Context) {
	e.init()
	e.mu.Lock()
	e.ctx = ctx
	e.mu.Unlock()
	go e.sweep(ctx)
	e.out.run(ctx)
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
		SeenAt: in.Sample.CapturedAt, Flying: in.Flying != nil && *in.Flying,
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
func (e *Engine) route(flightID, cell5 string) *worker {
	e.mu.Lock()
	defer e.mu.Unlock()
	home, ok := e.home[flightID]
	if !ok {
		c3, err := cell.Parent3(cell5)
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
	e.mu.Lock()
	delete(e.home, flightID)
	e.mu.Unlock()
	e.table.remove(flightID)
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

// item is one unit of a worker's queue: a sample, or a flight's end.
type item struct {
	tr    *telemetry.Track
	in    conformance.Input
	ended string
}

// flight is one tracked flight.
type flight struct {
	tr       *conformance.Tracker
	instance string // the client the samples came from (the source instance)
	intentID string
	lastAt   time.Time
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
	}
}

func (w *worker) take(ctx context.Context, it item) {
	now := w.e.now()
	if it.ended != "" {
		w.end(ctx, it.ended, now, conformance.ClearFlightEnded)
		w.e.Counters.Inc(CounterFlightEnded)
		return
	}
	tr := it.tr
	id := *tr.Body.FlightID
	f := w.flights[id]
	if f == nil {
		if len(w.flights) >= w.e.MaxFlights {
			w.e.Counters.Inc(CounterFlightsOverBound)
			w.e.logger().LogAttrs(ctx, slog.LevelError, "monitor at its flight bound: flight not judged", obs.FlightID(id))
			return
		}
		intentID := deref(tr.Body.IntentID)
		f = &flight{tr: conformance.NewTracker(id, intentID, "", w.e.Counters), intentID: intentID}
		w.flights[id] = f
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
	if ev.Admitted || ev.Refusal == "source_disabled" {
		w.publishState(ctx, f.tr, ev, tr.Times(), cfg.PolicyVersion)
	}
	w.publishAlerts(ctx, ev.Alerts, now)
}

// end drops a flight's tracker (its alerts clear with reason) and every
// nearby alert it held as a neighbour.
func (w *worker) end(ctx context.Context, id string, now time.Time, reason string) {
	if f := w.flights[id]; f != nil {
		ev := f.tr.Drop(reason, now)
		w.publishAlerts(ctx, ev.Alerts, now)
		delete(w.flights, id)
		w.e.forget(id)
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
				ev := f.tr.Disable(now)
				w.publishState(ctx, f.tr, ev, systemTimes(now), cfg.PolicyVersion)
				w.publishAlerts(ctx, ev.Alerts, now)
			}
		}
		ev := f.tr.Tick(cfg, now)
		if len(ev.Transitions) > 0 {
			w.publishState(ctx, f.tr, ev, systemTimes(now), cfg.PolicyVersion)
		}
		w.publishAlerts(ctx, ev.Alerts, now)
		// A flight silent past the flight end with nothing active and no
		// lost link is forgotten (its flight ended at the ingest); one
		// that holds an alert is kept until its intent ends.
		snap := f.tr.Snapshot()
		if len(f.tr.Active()) == 0 && !snap.LinkLost && now.Sub(f.lastAt) > secs(vals.FlightEndAfterS) {
			delete(w.flights, id)
			w.e.forget(id)
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
			w.publishAlerts(ctx, w.nearby.Refresh(active[i], snap.LastPosition, nbs, vals.NonconformanceNearbyRadiusM, maxAge, now, cfg.PolicyVersion), now)
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
func (w *worker) publishState(_ context.Context, tr *conformance.Tracker, ev conformance.Events, times core.Times, pv int64) {
	subject, err := bus.Conf(tr.FlightID)
	if err != nil {
		w.e.Counters.Inc(CounterPublishFailed)
		return
	}
	w.e.out.put(subject, conformance.StateMessageOf(tr.Snapshot(), ev, times, pv))
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
