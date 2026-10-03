package traffic

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/alerting"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/cpa"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Bounds and periods of the engine (E-10).
const (
	// DefaultQueueLen bounds the samples waiting for the engine's loop;
	// a full queue drops the sample, counted and logged.
	DefaultQueueLen = 16_384
	// DefaultOutboxLen bounds the messages waiting to be published.
	DefaultOutboxLen = 16_384
	// MaxTracks bounds the engine's own table of the aircraft it fed
	// (the product facts of each); core bounds what it judges itself.
	MaxTracks = 100_000
	// DefaultHeartbeat is the longest an active alert's saved state
	// waits between two saves.
	DefaultHeartbeat = 10 * time.Second
	// WidenedPeriod is the evaluation period of a worker over its pair
	// budget (spec 05 §5: 2 s, never more, never a skip).
	WidenedPeriod = 2 * time.Second
	// StoreWait bounds how long a start waits for the saved alerts before
	// the feed opens; past it the feed opens and the saved alerts are
	// carried as soon as they are read.
	StoreWait = 10 * time.Second
	// PublishAttempts bounds the tries of one message.
	PublishAttempts = 3
)

// Counters of the engine.
const (
	CounterSamples           = "traffic_samples"
	CounterQueueFull         = "traffic_queue_full"
	CounterVelocityUnknown   = "traffic_velocity_unknown"
	CounterNotFed            = "traffic_samples_not_fed" // stale or switched-off manned frames: not a new sample
	CounterTracksOverBound   = "traffic_tracks_over_bound"
	CounterKindUnmapped      = "traffic_alert_kind_unmapped"
	CounterWithoutOwnFlight  = "traffic_conflict_without_own_flight"
	CounterNotOwned          = "traffic_alert_not_owned"
	CounterAlertNoCell       = "traffic_alert_without_cell"
	CounterRaised            = "traffic_proximity_raised"
	CounterCleared           = "traffic_proximity_cleared"
	CounterAdopted           = "traffic_proximity_adopted"
	CounterTakenOver         = "traffic_proximity_taken_over"
	CounterHandedOver        = "traffic_proximity_handed_over"
	CounterReleasedAtStop    = "traffic_proximity_released_at_stop"
	CounterCarriedCleared    = "traffic_carried_cleared"
	CounterRefused           = "traffic_aircraft_refused"
	CounterTicks             = "traffic_ticks"
	CounterWidened           = "traffic_evaluation_widened"
	CounterPairChecks        = "traffic_pair_checks"
	CounterOutboxFull        = "traffic_outbox_full"
	CounterPublished         = "traffic_published"
	CounterPublishFailed     = "traffic_publish_failed"
	CounterPublishDropped    = "traffic_publish_dropped"
	CounterMonitorRebuilt    = "traffic_monitor_rebuilt"
	CounterMonitorCountersAt = "traffic_monitor_" // + core's counter name, mirrored each tick
)

// Sink is where the messages go (bus.Publisher).
type Sink interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// item is one unit of the loop's queue.
type item struct {
	in      *Input
	ended   string
	sources *coresources.State
}

// trackInfo is what the engine keeps of one aircraft it fed: the last
// input (without its identification) and when it was last fed.
type trackInfo struct {
	in     Input
	lastAt time.Time
}

// pairState is one conflict the engine holds: active in core, or carried
// from a save, a handover or a rebuilt monitor until core raises it
// again or the evidence ends it.
type pairState struct {
	key      string
	raisedAt time.Time
	aircraft [2]SavedAircraft
	severity core.Severity
	// detail is core's detail of the last judgement that showed the
	// conflict true; losStartS core's rank of it.
	detail     map[string]any
	losStartS  float64
	lastTrueAt time.Time
	capturedAt time.Time
	policyVer  int64
	// carried: not active in core. carriedAt is when it was carried and
	// heard when each aircraft was first fed a live flying sample since.
	carried   bool
	carriedAt time.Time
	heard     [2]time.Time
	// owned: this instance publishes, saves and clears it; lastSave when
	// it was last saved. An owned pair whose anchor left the owned cells
	// is releasing: it stays owned, saved as releasing, until another
	// instance has saved it (ownership transferred), so it always has a
	// live owner that publishes its clear.
	owned     bool
	releasing bool
	lastSave  time.Time
}

// Summary is what the status line says of the CPA path.
type Summary struct {
	Tracked           int     `json:"aircraft_tracked"`
	Active            int     `json:"proximity_active"`
	Carried           int     `json:"proximity_carried"`
	EvaluationPeriodS float64 `json:"evaluation_period_s"`
	PairChecksPerS    float64 `json:"pair_checks_per_s"`
	CapacityExceeded  bool    `json:"capacity_exceeded"`
	StoreLoaded       bool    `json:"proximity_state_loaded"`
	OutboxDepth       int     `json:"outbox_depth"`
}

// Engine is the CPA path of monitor for one owned cell set: one
// alerting.Monitor fed every sample of its cells and their ring-1
// neighbours, owned by one goroutine (Run). Offer, FlightEnded and
// SwitchSource queue work and never block.
type Engine struct {
	Policy func() policy.Record
	Sink   Sink
	// Store persists the active alerts (KVStates); nil keeps them in
	// memory only.
	Store StateStore
	// Ownership says which alerts this instance publishes: those whose
	// anchor (the cell of the pair's first own flight) it owns.
	Ownership cell.Ownership
	// InstanceID names this instance as the owner of what it saves.
	InstanceID string
	// Authorisation is an intent's public authorisation number (from
	// intent_active); nil or "" when it has none.
	Authorisation func(intentID string) string
	Counters      *core.Counters
	Logger        *slog.Logger
	Now           func() time.Time
	// Tick is the evaluation period (1 s).
	Tick      time.Duration
	QueueLen  int
	OutboxLen int
	// Heartbeat is the longest an active alert waits between two saves
	// (DefaultHeartbeat).
	Heartbeat time.Duration
	// StoreWait bounds the start's wait for the saved alerts.
	StoreWait time.Duration

	once   sync.Once
	ch     chan item
	out    chan outMsg
	pers   *persister
	seeded chan struct{}
	base   time.Time

	// The loop's own state (Run's goroutine only).
	mon    *alerting.Monitor
	cfg    alerting.Config
	tracks map[string]*trackInfo
	pairs  map[string]*pairState
	grid   *cpa.Grid
	// restoredOnce: the start's restore has run (a live peer's alert is
	// counted there, not again at every tick).
	restoredOnce bool
	widened      bool
	pending      map[string]*Input // coalesced samples while widened
	nextFlush    time.Time
	windowAt     time.Time
	pairCount    uint64
	lastRate     float64
	coreLast     map[string]uint64
	// clearedAt is when this instance cleared each pair it deleted from
	// the store: a save not newer is never carried again (the mirror can
	// show a deleted save for a moment).
	clearedAt map[string]time.Time

	mu  sync.Mutex
	sum Summary
}

type outMsg struct {
	subject string
	m       bus.Enveloped
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
		if e.Heartbeat <= 0 {
			e.Heartbeat = DefaultHeartbeat
		}
		if e.StoreWait <= 0 {
			e.StoreWait = StoreWait
		}
		if e.InstanceID == "" {
			e.InstanceID = "monitor"
		}
		q := e.QueueLen
		if q <= 0 {
			q = DefaultQueueLen
		}
		e.ch = make(chan item, q)
		n := e.OutboxLen
		if n <= 0 {
			n = DefaultOutboxLen
		}
		e.out = make(chan outMsg, n)
		e.seeded = make(chan struct{})
		e.base = e.now()
		e.tracks, e.pairs, e.pending = map[string]*trackInfo{}, map[string]*pairState{}, map[string]*Input{}
		e.clearedAt = map[string]time.Time{}
		if e.Store != nil {
			e.pers = newPersister(e.Store, e.Counters, e.logger())
		}
		e.rebuild(e.policy().Values)
	})
}

// wallS is the engine's monotonic clock on the ingest's time base
// (Unix seconds): core's wallS, never derived from a sample.
func (e *Engine) wallS(now time.Time) float64 {
	return unixS(e.base) + now.Sub(e.base).Seconds()
}

// rebuild starts a new core monitor under v, carrying every alert the
// old one held (a changed policy: C-15 rebuilds the index with its
// aircraft; the alerts are carried until core judges them again).
func (e *Engine) rebuild(v policy.Values) {
	now := e.now()
	for _, p := range e.pairs {
		if !p.carried {
			e.carry(p, now)
		}
	}
	e.cfg = ConfigOf(v)
	e.mon = alerting.NewMonitor(e.cfg)
	e.coreLast = nil
	e.grid = cpa.NewGrid(e.mon.Config().GridCellM)
	if len(e.pairs) > 0 || e.Counters.Get(CounterTicks) > 0 {
		e.Counters.Inc(CounterMonitorRebuilt)
	}
}

func sameConfig(a, b alerting.Config) bool {
	return a.Policy == b.Policy && a.ClearAfterS == b.ClearAfterS && a.StaleAfterS == b.StaleAfterS &&
		a.LiveMaxAgeS == b.LiveMaxAgeS && a.AheadToleranceS == b.AheadToleranceS && a.GridCellM == b.GridCellM
}

// Seeded is closed once the start has read the saved alerts (or waited
// StoreWait for them): the feed opens after it.
func (e *Engine) Seeded() <-chan struct{} {
	e.init()
	return e.seeded
}

// Offer queues one sample; it never blocks: a full queue drops it,
// counted and logged (a gap is never silent).
func (e *Engine) Offer(in Input) {
	e.init()
	select {
	case e.ch <- item{in: &in}:
	default:
		e.Counters.Inc(CounterQueueFull)
		e.logger().LogAttrs(context.Background(), slog.LevelError, "CPA queue full: sample dropped, judged by nothing",
			slog.String("track_id", in.TrackID))
	}
}

// FlightEnded queues the end of one of this USSP's flights: its
// aircraft is dropped and every alert it is part of clears
// flight_ended.
func (e *Engine) FlightEnded(flightID string) {
	e.init()
	select {
	case e.ch <- item{ended: flightID}:
	default:
		e.Counters.Inc(CounterQueueFull)
		e.logger().LogAttrs(context.Background(), slog.LevelError, "CPA queue full: flight end not delivered; its aircraft goes stale",
			obs.FlightID(flightID))
	}
}

// SwitchSource queues a source-control state (B-09, B-11).
func (e *Engine) SwitchSource(st coresources.State) {
	e.init()
	select {
	case e.ch <- item{sources: &st}:
	default:
		e.Counters.Inc(CounterQueueFull)
		e.logger().LogAttrs(context.Background(), slog.LevelError, "CPA queue full: source switch not delivered; the next state carries it")
	}
}

// Summary is the last tick's summary.
func (e *Engine) Summary() Summary {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sum
	s.OutboxDepth = len(e.out)
	return s
}

// Run is the engine's loop until ctx ends: the publisher and the
// persister, the start's restore, then samples and ticks. At the end
// every alert it owns is saved released, so another instance carries it
// at its next tick instead of after liveOwnerAfter.
func (e *Engine) Run(ctx context.Context) {
	e.init()
	go e.publishLoop(ctx)
	if e.pers != nil {
		pctx, stop := context.WithCancel(context.WithoutCancel(ctx))
		persisted := make(chan struct{})
		go func() {
			defer close(persisted)
			e.pers.run(pctx)
		}()
		defer func() {
			// The loop stops (draining what it holds), then the release
			// is written here, never cut short by the loop's own stop.
			stop()
			<-persisted
			e.release()
			e.pers.drain(context.WithoutCancel(ctx), time.Now().Add(2*time.Second))
		}()
		e.waitStore(ctx)
	}
	close(e.seeded)
	t := time.NewTicker(e.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-e.ch:
			e.take(ctx, it)
		case <-t.C:
			e.tick(ctx)
		}
	}
}

// waitStore waits up to StoreWait for the saved alerts and carries
// those this instance owns.
func (e *Engine) waitStore(ctx context.Context) {
	deadline := time.NewTimer(e.StoreWait)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for !e.Store.Loaded() {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			e.Counters.Inc(CounterStateNotLoaded)
			e.logger().LogAttrs(ctx, slog.LevelError, "proximity_state not read at the start: saved alerts are carried once it is read")
			return
		case <-poll.C:
		}
	}
	e.restore(ctx)
}

// liveOwnerAfter is how recent another instance's save must be for its
// ownership of an alert to be respected (three heartbeats): older, its
// owner is gone.
func (e *Engine) liveOwnerAfter() time.Duration { return 3 * e.Heartbeat }

// mayTake reports whether this instance may take a saved alert from its
// last saver: its own save, one released at a stop (no owner), one whose
// owner stopped saving (gone), or one an owner hands over because the
// anchor left its cells (releasing) when this instance owns the anchor.
func (e *Engine) mayTake(s Saved, anchorMine bool, now time.Time) bool {
	switch {
	case s.Owner == "" || s.Owner == e.InstanceID:
		return true
	case now.Sub(s.SavedAt) >= e.liveOwnerAfter():
		return true
	}
	return s.Releasing && anchorMine
}

// restore carries every saved alert whose anchor this instance owns, that
// it does not hold, and that it may take (mayTake). It runs at the start
// and on every tick, so an alert whose owner stops or hands it over is
// never left without one.
func (e *Engine) restore(ctx context.Context) {
	if e.Store == nil || !e.Store.Loaded() {
		return
	}
	now := e.now()
	all := e.Store.All()
	e.forgetCleared(now)
	defer func() { e.restoredOnce = true }()
	for _, k := range slices.Sorted(maps.Keys(all)) {
		s := all[k]
		if _, held := e.pairs[s.Key]; held {
			continue
		}
		if at, ok := e.clearedAt[s.Key]; ok && !s.SavedAt.After(at) {
			continue
		}
		p := &pairState{key: s.Key, raisedAt: s.RaisedAt, aircraft: s.Aircraft, severity: s.Severity, detail: s.Detail,
			losStartS: s.LoSStartS, lastTrueAt: s.LastTrueAt, capturedAt: s.CapturedAt, policyVer: s.PolicyVersion}
		if !e.anchorOwned(p) {
			continue
		}
		if !e.mayTake(s, true, now) {
			if !e.restoredOnce {
				e.Counters.Inc(CounterStateOwnedByPeer)
			}
			continue
		}
		p.owned = true
		e.carry(p, now)
		e.pairs[s.Key] = p
		e.Counters.Inc(CounterStateRestored)
		e.save(p, now, true)
		e.logger().LogAttrs(ctx, slog.LevelInfo, "proximity alert carried from its saved state", slog.String("pair_id", PairID(s.Key)),
			slog.Time("raised_at", s.RaisedAt), slog.String("saved_by", s.Owner), slog.Time("saved_at", s.SavedAt),
			slog.Bool("releasing", s.Releasing))
	}
}

// forgetCleared drops the clear marks the store no longer contradicts:
// the save is gone or newer, or the bucket's TTL has passed.
func (e *Engine) forgetCleared(now time.Time) {
	for k, at := range e.clearedAt {
		s, ok := e.Store.Get(k)
		if !ok || s.SavedAt.After(at) || now.Sub(at) > bus.ProximityStateTTL {
			delete(e.clearedAt, k)
		}
	}
}

// release saves every alert this instance owns as released (no owner):
// another instance that owns its anchor carries it at its next tick.
func (e *Engine) release() {
	now := e.now()
	for _, k := range slices.Sorted(maps.Keys(e.pairs)) {
		p := e.pairs[k]
		if !p.owned || !hasOwn(p) {
			continue
		}
		s := e.savedOf(p, now)
		s.Owner, s.Releasing = "", false
		e.pers.put(p.key, persistOp{s: s})
		e.Counters.Inc(CounterReleasedAtStop)
	}
}

func (e *Engine) carry(p *pairState, now time.Time) {
	p.carried, p.carriedAt, p.heard = true, now, [2]time.Time{}
}

func (e *Engine) take(ctx context.Context, it item) {
	now := e.now()
	switch {
	case it.ended != "":
		e.drop(ctx, NSTrack+":"+it.ended, now)
	case it.sources != nil:
		ev := e.mon.SwitchSource(*it.sources, e.wallS(now))
		e.handle(ctx, ev, now)
		e.carriedSwitched(ctx, *it.sources, now)
	case it.in != nil:
		e.sample(ctx, it.in, now)
	}
}

// sample feeds one sample: at once at a 1 s evaluation period, or
// coalesced (the newest per aircraft) until the next 2 s evaluation
// while the worker is over its pair budget.
func (e *Engine) sample(ctx context.Context, in *Input, now time.Time) {
	e.Counters.Inc(CounterSamples)
	if in.State != StateLive {
		e.Counters.Inc(CounterNotFed)
		return
	}
	if e.widened {
		e.pending[in.ID] = in
		return
	}
	e.feed(ctx, in, now)
}

// feed observes one sample through core and handles what it raised and
// cleared.
func (e *Engine) feed(ctx context.Context, in *Input, now time.Time) {
	wall := e.wallS(now)
	tr, known := TrackOf(in)
	if !known {
		e.Counters.Inc(CounterVelocityUnknown)
	}
	info := e.tracks[in.ID]
	if info == nil {
		if len(e.tracks) >= MaxTracks {
			e.Counters.Inc(CounterTracksOverBound)
		} else {
			info = &trackInfo{}
			e.tracks[in.ID] = info
		}
	}
	if info != nil {
		keep := *in
		keep.Identification = nil
		info.in, info.lastAt = keep, now
	}
	live := !in.Times.Backlog && wall-unixS(in.Times.RxTS) <= e.cfg.LiveMaxAgeS
	if live && in.Flying != nil && *in.Flying && in.Position.Valid() {
		// The pair checks this sample costs: its neighbours in the
		// search radius (core's grid; the count only sizes the tick).
		e.grid.Upsert(in.ID, in.Position)
		n := len(e.grid.Near(in.Position, e.cfg.Policy.NeighbourRadiusM))
		if n > 1 {
			e.pairCount += uint64(n - 1)
		}
	} else if in.Flying != nil && !*in.Flying {
		e.grid.Remove(in.ID)
	}
	ev := e.mon.Observe(tr, wall)
	e.handle(ctx, ev, now)
	if live {
		e.carriedHeard(ctx, in, now)
	}
}

// drop ends an aircraft: core clears its alerts with flight_ended, and
// every carried alert it is part of clears so too.
func (e *Engine) drop(ctx context.Context, id string, now time.Time) {
	ev := e.mon.Drop(id, alerting.ClearFlightEnded, e.wallS(now))
	e.handle(ctx, ev, now)
	for _, k := range slices.Sorted(maps.Keys(e.pairs)) {
		p := e.pairs[k]
		if p.carried && (p.aircraft[0].ID == id || p.aircraft[1].ID == id) {
			e.clearCarried(ctx, p, string(alerting.ClearFlightEnded), now, nil)
		}
	}
	delete(e.pending, id)
	e.grid.Remove(id)
	delete(e.tracks, id)
}

// handle maps what core raised and cleared onto proximity messages.
func (e *Engine) handle(ctx context.Context, ev alerting.Events, now time.Time) {
	for i := range ev.Refused {
		r := ev.Refused[i]
		e.Counters.Inc(CounterRefused)
		e.logger().LogAttrs(ctx, slog.LevelError, "CPA monitor refused an aircraft: it is not judged (C-18: no alert is cleared to make room)",
			slog.String("track", r.ID), slog.String("source", r.Source), slog.String("station", r.Station),
			slog.String("reason", string(r.Reason)), slog.Uint64("suppressed", r.Suppressed))
	}
	for i := range ev.Raised {
		a := ev.Raised[i]
		if a.Kind != alerting.KindConflict {
			e.Counters.Inc(CounterKindUnmapped)
			continue
		}
		e.raised(ctx, a, now)
	}
	for i := range ev.Cleared {
		c := ev.Cleared[i]
		if c.Kind != alerting.KindConflict {
			e.Counters.Inc(CounterKindUnmapped)
			continue
		}
		e.cleared(ctx, c, now)
	}
}

// aircraftOf is what the engine knows of an aircraft id now, for an
// alert's side.
func (e *Engine) aircraftOf(id string) SavedAircraft {
	s := SavedAircraft{ID: id}
	if info := e.tracks[id]; info != nil {
		in := &info.in
		s.TrackID, s.Trust, s.Source, s.Instance, s.Cell5 = in.TrackID, in.Trust, in.Source, in.Instance, in.Cell5
		if in.Own() {
			s.FlightID, s.IntentID = in.FlightID, in.IntentID
			if e.Authorisation != nil && in.IntentID != "" {
				s.AuthorisationNumber = e.Authorisation(in.IntentID)
			}
		}
	}
	return s
}

// refreshAircraft updates a pair's sides with what is known now,
// keeping what a side knew when nothing newer is known.
func (e *Engine) refreshAircraft(p *pairState) {
	for i := range p.aircraft {
		if e.tracks[p.aircraft[i].ID] == nil {
			continue
		}
		cur := e.aircraftOf(p.aircraft[i].ID)
		if cur.AuthorisationNumber == "" {
			cur.AuthorisationNumber = p.aircraft[i].AuthorisationNumber
		}
		p.aircraft[i] = cur
	}
}

// anchor is the cell3 of the pair's first own flight (by track id), and
// false when no aircraft of the pair is one of this USSP's flights.
func anchorOf(p *pairState) (string, bool) {
	ids := []int{0, 1}
	slices.SortFunc(ids, func(a, b int) int {
		switch {
		case p.aircraft[a].TrackID < p.aircraft[b].TrackID:
			return -1
		case p.aircraft[a].TrackID > p.aircraft[b].TrackID:
			return 1
		}
		return 0
	})
	for _, i := range ids {
		if p.aircraft[i].FlightID != "" {
			c3, err := cell.Parent3(p.aircraft[i].Cell5)
			return c3, err == nil
		}
	}
	return "", false
}

func (e *Engine) anchorOwned(p *pairState) bool {
	c3, ok := anchorOf(p)
	return ok && e.Ownership.Owns(c3)
}

func hasOwn(p *pairState) bool {
	return p.aircraft[0].FlightID != "" || p.aircraft[1].FlightID != ""
}

// raised takes a conflict core raised: a pair the engine carries or
// finds saved continues under its raise time (an update, no second
// raise); a new one is raised. A severity change of a held pair is
// raised again (C-07).
func (e *Engine) raised(ctx context.Context, a alerting.Alert, now time.Time) {
	p, held := e.pairs[a.Key]
	state := AlertRaised
	switch {
	case held && p.carried:
		p.carried = false
		state = AlertUpdated
		e.Counters.Inc(CounterAdopted)
	case held:
		// C-07: core raised the held pair again on a new severity.
	default:
		p = &pairState{key: a.Key, raisedAt: timeOfS(a.RaisedAtS)}
		p.aircraft = [2]SavedAircraft{e.aircraftOf(a.Aircraft[0]), e.aircraftOf(a.Aircraft[1])}
		if e.Store != nil {
			if s, ok := e.Store.Get(a.Key); ok {
				p.raisedAt = s.RaisedAt
				state = AlertUpdated
				e.Counters.Inc(CounterAdopted)
			}
		}
		e.pairs[a.Key] = p
	}
	e.refreshAircraft(p)
	e.takeCore(p, a, now)
	if !hasOwn(p) {
		e.Counters.Inc(CounterWithoutOwnFlight)
		return
	}
	// A held pair keeps its owner (a releasing one is the old owner's to
	// publish until it is taken); the others go to the anchor's owner.
	if !held || !p.owned {
		p.owned = e.anchorOwned(p)
	}
	if !p.owned {
		e.Counters.Inc(CounterNotOwned)
		return
	}
	if state == AlertRaised {
		e.Counters.Inc(CounterRaised)
	}
	e.publishPair(ctx, p, state, "", nil, now)
	e.save(p, now, true)
}

// takeCore copies core's view of an active conflict into the pair.
func (e *Engine) takeCore(p *pairState, a alerting.Alert, now time.Time) {
	p.severity, p.detail = a.Severity, a.Detail
	if ls, ok := a.Detail["los_start_s"].(float64); ok {
		p.losStartS = ls
	}
	p.lastTrueAt = timeOfS(a.LastTrueS)
	p.capturedAt = p.lastTrueAt
	if p.capturedAt.After(now) || p.capturedAt.IsZero() {
		p.capturedAt = now
	}
	p.policyVer = e.policy().Version
}

// cleared takes a conflict core cleared.
func (e *Engine) cleared(ctx context.Context, c alerting.Cleared, now time.Time) {
	p, held := e.pairs[c.Key]
	if !held {
		p = &pairState{key: c.Key, raisedAt: timeOfS(c.RaisedAtS)}
		p.aircraft = [2]SavedAircraft{e.aircraftOf(c.Aircraft[0]), e.aircraftOf(c.Aircraft[1])}
	}
	e.refreshAircraft(p)
	e.takeCore(p, c.Alert, now)
	delete(e.pairs, c.Key)
	if !hasOwn(p) {
		return
	}
	owned := p.owned || (!held && e.anchorOwned(p))
	if !owned {
		return
	}
	e.Counters.Inc(CounterCleared)
	e.publishPair(ctx, p, AlertCleared, string(c.Reason), ClearingDetail(c.ClearingDetail), now)
	e.forget(c.Key, now)
}

// forget deletes a cleared pair's saved state and marks it cleared, so
// the restore never carries its last save again.
func (e *Engine) forget(key string, now time.Time) {
	if e.pers == nil {
		return
	}
	e.pers.put(key, persistOp{del: true})
	e.clearedAt[key] = now
}

// clearCarried clears a carried pair with reason.
func (e *Engine) clearCarried(ctx context.Context, p *pairState, reason string, now time.Time, clearing map[string]any) {
	delete(e.pairs, p.key)
	e.Counters.Inc(CounterCarriedCleared)
	if !p.owned || !hasOwn(p) {
		return
	}
	e.Counters.Inc(CounterCleared)
	e.publishPair(ctx, p, AlertCleared, reason, clearing, now)
	e.forget(p.key, now)
}

// carriedHeard notes a live sample of an aircraft of a carried pair: a
// landing clears the pair landed; a flying sample starts the pair's
// reconfirmation for that side.
func (e *Engine) carriedHeard(ctx context.Context, in *Input, now time.Time) {
	for _, k := range slices.Sorted(maps.Keys(e.pairs)) {
		p := e.pairs[k]
		if !p.carried {
			continue
		}
		for i := range p.aircraft {
			if p.aircraft[i].ID != in.ID {
				continue
			}
			switch {
			case in.Flying != nil && !*in.Flying:
				e.clearCarried(ctx, p, string(alerting.ClearLanded), now, nil)
			case in.Flying != nil && *in.Flying && p.heard[i].IsZero():
				p.heard[i] = now
			}
		}
	}
}

// carriedSwitched clears every carried pair with an aircraft whose
// source the state switches off (B-11).
func (e *Engine) carriedSwitched(ctx context.Context, st coresources.State, now time.Time) {
	for _, k := range slices.Sorted(maps.Keys(e.pairs)) {
		p := e.pairs[k]
		if !p.carried {
			continue
		}
		for i := range p.aircraft {
			a := p.aircraft[i]
			var inst *string
			if a.Instance != "" {
				s := a.Instance
				inst = &s
			}
			if a.Source != "" && !st.Query(a.Source, inst).Enabled {
				e.clearCarried(ctx, p, string(alerting.ClearSourceDisabled), now, nil)
				break
			}
		}
	}
}

// carriedTick ends the carried pairs the evidence ends: an aircraft not
// heard for the stale time since it was last fed or carried (stale);
// both heard flying for longer than the hysteresis with core not
// raising the pair (not_reconfirmed).
func (e *Engine) carriedTick(ctx context.Context, now time.Time) {
	stale := secs(e.cfg.StaleAfterS)
	hyst := secs(e.cfg.ClearAfterS)
	for _, k := range slices.Sorted(maps.Keys(e.pairs)) {
		p := e.pairs[k]
		if !p.carried {
			continue
		}
		gone := false
		for i := range p.aircraft {
			last := p.carriedAt
			if info := e.tracks[p.aircraft[i].ID]; info != nil && info.lastAt.After(last) {
				last = info.lastAt
			}
			if now.Sub(last) > stale {
				gone = true
			}
		}
		if gone {
			e.clearCarried(ctx, p, string(alerting.ClearStale), now, nil)
			continue
		}
		if p.heard[0].IsZero() || p.heard[1].IsZero() {
			continue
		}
		since := p.heard[0]
		if p.heard[1].After(since) {
			since = p.heard[1]
		}
		if now.Sub(since) > hyst {
			e.clearCarried(ctx, p, ClearNotReconfirmed, now, map[string]any{
				"clearing_basis": "carried across a restart, a handover or a policy change; both aircraft heard flying for longer than the hysteresis and the pair was not judged in conflict again",
				"carried_at":     bus.Stamp{Time: p.carriedAt}, "heard_since": bus.Stamp{Time: since},
			})
		}
	}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// tick runs core's tick (silence ends conditions), flushes the
// coalesced samples on a widened period, ends what the evidence ends of
// the carried pairs, republishes every active alert with its current
// numbers (C-08) in core's order (critical first, then the time to the
// loss of separation), and sizes the next period by the pair checks.
func (e *Engine) tick(ctx context.Context) {
	now := e.now()
	e.Counters.Inc(CounterTicks)
	pol := e.policy()
	if cfg := ConfigOf(pol.Values); !sameConfig(cfg, e.cfg) {
		e.rebuild(pol.Values)
	}
	e.restore(ctx)
	if e.widened && !now.Before(e.nextFlush) {
		e.flush(ctx, now)
	}
	e.handle(ctx, e.mon.Tick(e.wallS(now)), now)
	e.carriedTick(ctx, now)
	e.ownership(ctx, now)
	period := e.Tick.Seconds()
	if e.widened {
		period = WidenedPeriod.Seconds()
	}
	e.republish(ctx, now, period)
	e.budget(now, pol.Values)
	e.sweepTracks(now)
	e.summarise(period)
}

// flush feeds the coalesced samples, oldest placement first.
func (e *Engine) flush(ctx context.Context, now time.Time) {
	ins := slices.Collect(maps.Values(e.pending))
	slices.SortFunc(ins, func(a, b *Input) int {
		if c := a.Times.CapturedAt.Compare(b.Times.CapturedAt); c != 0 {
			return c
		}
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	clear(e.pending)
	for _, in := range ins {
		e.feed(ctx, in, now)
	}
	e.nextFlush = now.Add(WidenedPeriod)
}

// budget measures the pair checks per second over the last window and
// widens the evaluation period to 2 s above the budget; it returns to
// 1 s once the checks a 1 s period would make fit in it again.
func (e *Engine) budget(now time.Time, v policy.Values) {
	if e.windowAt.IsZero() {
		e.windowAt = now
		return
	}
	// The window is the evaluation period in force, so a widened
	// worker's rate counts the 2 s flush it made.
	el := now.Sub(e.windowAt).Seconds()
	if el < e.period()-1e-9 {
		return
	}
	rate := float64(e.pairCount) / el
	e.Counters.Add(CounterPairChecks, e.pairCount)
	e.pairCount, e.windowAt, e.lastRate = 0, now, rate
	budget := float64(v.CPAPairBudget)
	switch {
	case !e.widened && rate > budget:
		e.widened, e.nextFlush = true, now.Add(WidenedPeriod)
		e.Counters.Inc(CounterWidened)
		e.logger().LogAttrs(context.Background(), slog.LevelWarn, "CPA pair checks over the budget: evaluated every 2 s (evaluation_period_s 2), never skipped",
			slog.Float64("pair_checks_per_s", rate), slog.Int("budget", v.CPAPairBudget))
	case e.widened && rate*WidenedPeriod.Seconds() <= budget:
		e.widened = false
		e.flush(context.Background(), now)
		e.logger().LogAttrs(context.Background(), slog.LevelInfo, "CPA pair checks within the budget again: evaluated every second",
			slog.Float64("pair_checks_per_s", rate))
	}
}

// ownership settles who publishes each pair this instance holds. An
// owned pair stays owned until another live instance has saved it after
// this one last did (the transfer of a handover or a rollover; the anchor's
// owner never yields to a releasing save); its anchor leaving the owned
// cells only marks it releasing. A pair held but not owned is taken when
// this instance may take its save (mayTake) or, unsaved, when it owns
// the anchor.
func (e *Engine) ownership(ctx context.Context, now time.Time) {
	for _, k := range slices.Sorted(maps.Keys(e.pairs)) {
		p := e.pairs[k]
		if !hasOwn(p) {
			continue
		}
		e.refreshAircraft(p)
		mine := e.anchorOwned(p)
		if e.Store == nil {
			p.owned = mine
			continue
		}
		s, saved := e.Store.Get(p.key)
		if p.owned {
			if saved && s.Owner != "" && s.Owner != e.InstanceID && !s.SavedAt.Before(p.lastSave) &&
				now.Sub(s.SavedAt) < e.liveOwnerAfter() && (!mine || !s.Releasing) {
				p.owned, p.releasing = false, false
				e.Counters.Inc(CounterHandedOver)
				e.logger().LogAttrs(ctx, slog.LevelInfo, "proximity alert handed over: its new owner publishes it",
					slog.String("pair_id", PairID(p.key)), slog.String("owner", s.Owner))
				continue
			}
			if p.releasing == mine {
				p.releasing = !mine
				e.save(p, now, true)
			}
			continue
		}
		take := mine
		if saved {
			take = e.mayTake(s, mine, now)
		}
		if !take {
			continue
		}
		if saved {
			p.raisedAt = s.RaisedAt
		}
		p.owned, p.releasing = true, !mine
		e.Counters.Inc(CounterTakenOver)
		e.logger().LogAttrs(ctx, slog.LevelInfo, "proximity alert taken over", slog.String("pair_id", PairID(p.key)),
			slog.String("saved_by", s.Owner), slog.Bool("anchor_owned", mine))
		e.save(p, now, true)
	}
}

// republish sends every active and carried pair this instance owns as
// updated, with its current numbers, in core's order, and saves each at
// least every heartbeat.
func (e *Engine) republish(ctx context.Context, now time.Time, periodS float64) {
	order := e.mon.Active()
	seen := map[string]bool{}
	for i := range order {
		a := order[i]
		if a.Kind != alerting.KindConflict {
			continue
		}
		p := e.pairs[a.Key]
		if p == nil {
			continue
		}
		seen[a.Key] = true
		e.takeCore(p, a, now)
		if p.owned && hasOwn(p) {
			e.publishPairPeriod(ctx, p, AlertUpdated, "", nil, now, periodS)
			e.save(p, now, false)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(e.pairs)) {
		p := e.pairs[k]
		if seen[k] || !p.owned || !hasOwn(p) {
			continue
		}
		e.publishPairPeriod(ctx, p, AlertUpdated, "", nil, now, periodS)
		e.save(p, now, false)
	}
}

// sweepTracks forgets the aircraft not fed for four stale times that no
// pair names.
func (e *Engine) sweepTracks(now time.Time) {
	cut := 4 * secs(e.cfg.StaleAfterS)
	named := map[string]bool{}
	for _, p := range e.pairs {
		named[p.aircraft[0].ID], named[p.aircraft[1].ID] = true, true
	}
	for id, info := range e.tracks {
		if now.Sub(info.lastAt) > cut && !named[id] {
			delete(e.tracks, id)
			e.grid.Remove(id)
		}
	}
}

func (e *Engine) summarise(periodS float64) {
	carried := 0
	for _, p := range e.pairs {
		if p.carried {
			carried++
		}
	}
	// Core's counters, carried into the engine's as they grow (a
	// rebuilt monitor starts its own from zero).
	if e.coreLast == nil {
		e.coreLast = map[string]uint64{}
	}
	for name, v := range e.mon.Counters().Snapshot() {
		if last := e.coreLast[name]; v > last {
			e.Counters.Add(CounterMonitorCountersAt+name, v-last)
		}
		e.coreLast[name] = v
	}
	s := Summary{
		Tracked: e.mon.Tracked(), Active: len(e.pairs) - carried, Carried: carried, EvaluationPeriodS: periodS,
		PairChecksPerS: e.lastRate, CapacityExceeded: e.mon.CapacityExceeded(), StoreLoaded: e.Store == nil || e.Store.Loaded(),
	}
	e.mu.Lock()
	e.sum = s
	e.mu.Unlock()
}

// save writes the pair's saved state on a change (force) and otherwise
// at most every heartbeat.
func (e *Engine) save(p *pairState, now time.Time, force bool) {
	if e.pers == nil || !p.owned || !hasOwn(p) || (!force && now.Sub(p.lastSave) < e.Heartbeat) {
		return
	}
	p.lastSave = now
	e.pers.put(p.key, persistOp{s: e.savedOf(p, now)})
}

// savedOf is the pair's saved state, owned by this instance.
func (e *Engine) savedOf(p *pairState, now time.Time) Saved {
	return Saved{
		Owner: e.InstanceID, SavedAt: now.UTC(), Key: p.key, RaisedAt: p.raisedAt, LastTrueAt: p.lastTrueAt,
		CapturedAt: p.capturedAt, Severity: p.severity, Aircraft: p.aircraft, Detail: maps.Clone(p.detail),
		LoSStartS: p.losStartS, PolicyVersion: p.policyVer, Releasing: p.releasing,
	}
}

func (e *Engine) period() float64 {
	if e.widened {
		return WidenedPeriod.Seconds()
	}
	return e.Tick.Seconds()
}

func (e *Engine) publishPair(ctx context.Context, p *pairState, state, reason string, clearing map[string]any, now time.Time) {
	e.publishPairPeriod(ctx, p, state, reason, clearing, now, e.period())
}

// publishPairPeriod sends one alert/v1 for each of this USSP's flights in
// the pair, naming the other aircraft; both carry the same numbers from
// the same judgement (C-11).
func (e *Engine) publishPairPeriod(_ context.Context, p *pairState, state, reason string, clearing map[string]any, now time.Time, periodS float64) {
	pairID := PairID(p.key)
	sev := p.severity
	if sev == "" {
		sev = core.SeverityCritical
	}
	for i := range p.aircraft {
		me, other := p.aircraft[i], p.aircraft[1-i]
		if me.FlightID == "" {
			continue
		}
		if me.Cell5 == "" {
			e.Counters.Inc(CounterAlertNoCell)
			continue
		}
		id := AlertID(p.key, me.FlightID, p.raisedAt)
		detail := Detail(p.detail, Peer{TrackID: other.TrackID, Trust: other.Trust, Source: other.Source}, pairID, periodS)
		if p.carried {
			detail["carried_since"] = bus.Stamp{Time: p.carriedAt}
		}
		b := AlertBody{
			AlertID: id, Kind: KindProximity, Severity: sev, State: state, ClearReason: optStr(reason),
			FlightID: me.FlightID, IntentID: optStr(me.IntentID), AuthorisationNumber: optStr(me.AuthorisationNumber),
			CapturedAt: bus.Stamp{Time: p.capturedAt}, RaisedAt: bus.Stamp{Time: p.raisedAt}, UpdatedAt: bus.Stamp{Time: now.UTC()},
			PolicyVersion: p.policyVer, Detail: detail, ClearingDetail: clearing,
		}
		subject, err := bus.Alrt(KindProximity, me.Cell5, id)
		if err != nil {
			e.Counters.Inc(CounterPublishFailed)
			continue
		}
		m := &AlertMessage{Envelope: bus.SystemEnvelope(SchemaAlert, Producer, now), Body: b}
		select {
		case e.out <- outMsg{subject, m}:
		default:
			e.Counters.Inc(CounterOutboxFull)
			e.logger().LogAttrs(context.Background(), slog.LevelError, "CPA outbox full: alert message dropped; the next second's republish replaces it",
				slog.String("alert_id", id))
		}
	}
}

// publishLoop publishes the outbox in order, off the loop (C-13): a
// failed publish is retried PublishAttempts times with a doubling wait,
// then dropped, counted and logged; the next second's republish heals a
// dropped update (C-08).
func (e *Engine) publishLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-e.out:
			e.send(ctx, m)
		}
	}
}

func (e *Engine) send(ctx context.Context, m outMsg) {
	wait := 100 * time.Millisecond
	var err error
	for try := range PublishAttempts {
		if try > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				e.Counters.Inc(CounterPublishDropped)
				return
			case <-t.C:
			}
			wait *= 2
		}
		if e.Sink == nil {
			e.Counters.Inc(CounterPublishDropped)
			return
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = e.Sink.Publish(pctx, m.subject, m.m)
		cancel()
		if err == nil {
			e.Counters.Inc(CounterPublished)
			return
		}
		e.Counters.Inc(CounterPublishFailed)
	}
	e.Counters.Inc(CounterPublishDropped)
	e.logger().LogAttrs(ctx, slog.LevelError, "proximity alert not published after its tries: dropped; the next republish replaces it",
		slog.String("subject", m.subject), obs.Err(err))
}
