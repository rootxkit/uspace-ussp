package telemetry

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// maxClients bounds the clients whose statistics are kept (E-10);
// beyond it the one silent longest is forgotten.
const maxClients = 10_000

// clientStat is what the ingest saw of one client since it started.
type clientStat struct {
	mu       sync.Mutex
	outcomes map[string]uint64
	lastSeen time.Time
	lagS     *float64
}

func (c *clientStat) add(reason string, rx time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes[reason]++
	if rx.After(c.lastSeen) {
		c.lastSeen = rx
	}
}

// lag records the delivery lag rx_ts - captured_at of the client's
// newest accepted sample (B-03).
func (c *clientStat) lag(lagS float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lagS = &lagS
}

type clientStats struct {
	mu  sync.Mutex
	all map[string]*clientStat
}

func newClientStats() *clientStats { return &clientStats{all: map[string]*clientStat{}} }

func (s *clientStats) get(clientID string) *clientStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.all[clientID]; ok {
		return c
	}
	if len(s.all) >= maxClients {
		var oldest string
		var at time.Time
		for id, c := range s.all {
			c.mu.Lock()
			seen := c.lastSeen
			c.mu.Unlock()
			if oldest == "" || seen.Before(at) {
				oldest, at = id, seen
			}
		}
		delete(s.all, oldest)
	}
	c := &clientStat{outcomes: map[string]uint64{}}
	s.all[clientID] = c
	return c
}

func (s *clientStats) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.all))
}

// SourceStatusBody is source/status/v1 (spec 04 §3.6) as this process
// publishes it for each operator client (src.v1.operator_ws.<client>),
// with the extras of B-03 and the work-queue gap of 05 §5.
type SourceStatusBody struct {
	Source         string            `json:"source"`
	SourceInstance *string           `json:"source_instance"`
	State          string            `json:"state"`
	Since          bus.Stamp         `json:"since"`
	AgeS           *float64          `json:"age_s"`
	DisabledBy     *string           `json:"disabled_by"`
	Counters       map[string]uint64 `json:"counters"`
	LastSeen       *bus.Stamp        `json:"last_seen"`
	LagS           *float64          `json:"lag_s"`
	Lagging        bool              `json:"lagging"`
	DroppedRate    uint64            `json:"dropped_rate"`
	Gap            *Gap              `json:"gap,omitempty"`
}

// SourceStatus is one source/status/v1 message.
type SourceStatus struct {
	bus.Envelope
	Body SourceStatusBody `json:"body"`
}

// Source states.
const (
	StateLive     = "live"
	StateStale    = "stale"
	StateDisabled = "disabled"
	StateUnknown  = "unknown"
)

// Counters of the status publisher.
const (
	CounterStatusPublished = "src_status_published"
	CounterStatusFailed    = "src_status_failed"
	CounterGaps            = "gaps_recorded"
	CounterGapDropped      = "gap_samples_dropped"
)

// Status publishes src.v1.operator_ws.<client> every period for every
// client heard (source/status/v1, core NATS), and records the gaps of
// the work queue (GapReporter): published at once on the client's
// subject, logged and counted.
type Status struct {
	Ingest   *Ingestor
	Pub      MessagePublisher
	Sources  SourceGate
	Policy   func() policy.Record
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time

	mu    sync.Mutex
	since map[string]stateSince
}

type stateSince struct {
	state string
	at    time.Time
}

func (s *Status) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Status) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

// Body is the status of client at now.
func (s *Status) Body(clientID string) SourceStatusBody {
	now := s.now()
	pol := policy.Defaults()
	if s.Policy != nil {
		pol = s.Policy().Values
	}
	c := s.Ingest.stats.get(clientID)
	c.mu.Lock()
	outcomes := maps.Clone(c.outcomes)
	lastSeen, lag := c.lastSeen, c.lagS
	c.mu.Unlock()
	inst := clientID
	b := SourceStatusBody{Source: SourceOperatorWS, SourceInstance: &inst, Counters: map[string]uint64{}, LagS: lag}
	for k, v := range outcomes {
		b.Counters[k] = v
		switch {
		case k == OutcomeAccepted:
			b.Counters["accepted"] = v
		case k == OutcomeDuplicate || k == OutcomeDuplicatePending:
		case isDropped(k):
		default:
			b.Counters["refused"] += v
		}
	}
	if _, ok := b.Counters["accepted"]; !ok {
		b.Counters["accepted"] = 0
	}
	if _, ok := b.Counters["refused"]; !ok {
		b.Counters["refused"] = 0
	}
	b.DroppedRate = outcomes[DroppedRate]
	state := StateUnknown
	if !lastSeen.IsZero() {
		ls := bus.Stamp{Time: lastSeen.UTC()}
		b.LastSeen = &ls
		age := max(0, now.Sub(lastSeen).Seconds())
		b.AgeS = &age
		state = StateLive
		if age > pol.TelemetryLostS {
			state = StateStale
		}
	}
	if s.Sources != nil {
		if dec := s.Sources.Query(SourceOperatorWS, &inst); !dec.Enabled {
			state = StateDisabled
			why := "instance"
			if dec.WhyDisabled != nil {
				why = string(*dec.WhyDisabled)
			}
			b.DisabledBy = &why
		}
	}
	// B-03: lagging is not loss; the newest sample is old while the
	// client is still delivering.
	b.Lagging = lag != nil && *lag > pol.TelemetryLostS && state == StateLive
	b.State = state
	b.Since = bus.Stamp{Time: s.sinceOf(clientID, state, now).UTC()}
	return b
}

func (s *Status) sinceOf(clientID, state string, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.since == nil {
		s.since = map[string]stateSince{}
	}
	cur, ok := s.since[clientID]
	if !ok || cur.state != state {
		cur = stateSince{state: state, at: now}
		s.since[clientID] = cur
	}
	return cur.at
}

// publish sends one status message for clientID.
func (s *Status) publish(ctx context.Context, clientID string, b SourceStatusBody) {
	subject, err := bus.Src(SourceOperatorWS, clientID)
	if err != nil {
		s.Counters.Inc(CounterStatusFailed)
		return
	}
	m := &SourceStatus{Envelope: bus.SystemEnvelope(SchemaSourceStatus, Producer, s.now()), Body: b}
	if err := s.Pub.Publish(ctx, subject, m); err != nil {
		s.Counters.Inc(CounterStatusFailed)
		return
	}
	s.Counters.Inc(CounterStatusPublished)
}

// Run publishes every client's status every period until ctx ends.
func (s *Status) Run(ctx context.Context, period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, id := range s.Ingest.stats.ids() {
				s.publish(ctx, id, s.Body(id))
			}
		}
	}
}

// ReportGap implements GapReporter: the gap goes out at once on its
// client's status subject (or the work queue's), and is logged and
// counted, so a hole is never silent (05 §5, B-13).
func (s *Status) ReportGap(ctx context.Context, g Gap) {
	s.Counters.Inc(CounterGaps)
	s.Counters.Add(CounterGapDropped, uint64(max(g.Dropped, 0)))
	attrs := []slog.Attr{slog.String("cause", g.Cause), slog.String("client_id", g.SourceInstance), slog.Int("dropped", g.Dropped)}
	if !g.GapStarted.IsZero() {
		attrs = append(attrs, slog.Time("gap_started", g.GapStarted), slog.Time("gap_ended", g.GapEnded))
	}
	if g.ToSeq > 0 {
		attrs = append(attrs, slog.String("stream_seq", strconv.FormatUint(g.FromSeq, 10)+".."+strconv.FormatUint(g.ToSeq, 10)))
	}
	s.logger().LogAttrs(ctx, slog.LevelWarn, "telemetry dropped from the work queue with a gap record", attrs...)
	var b SourceStatusBody
	if g.SourceInstance == GapInstance {
		inst := GapInstance
		b = SourceStatusBody{Source: SourceOperatorWS, SourceInstance: &inst, State: StateLive, Since: bus.Stamp{Time: s.now().UTC()},
			Counters: map[string]uint64{"accepted": 0, "refused": 0}}
	} else {
		b = s.Body(g.SourceInstance)
	}
	gg := g
	b.Gap = &gg
	s.publish(ctx, g.SourceInstance, b)
}
