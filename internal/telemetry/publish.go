package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// MessagePublisher is internal/bus.Publisher: core NATS for trk and src,
// JetStream with an acknowledgement for ingest, ident and flight.
type MessagePublisher interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// Counters of the outbox (E-09).
const (
	CounterPublished        = "tracks_published"
	CounterPublishFailed    = "tracks_publish_failed"
	CounterSpilledFull      = "tracks_spilled_queue_full"
	CounterSpilledAged      = "tracks_spilled_queue_age"
	CounterSpilledFailed    = "tracks_spilled_publish_failed"
	CounterSpilled          = "tracks_spilled"
	CounterSpillFailed      = "tracks_spill_failed"
	CounterDroppedQueueFull = "dropped_queue_full"
)

// DefaultQueueFrames is the publisher's memory: ten seconds of the
// design rate of one instance (docs/PLAN.md §9: 2000 messages/s), the
// policy's ingest_queue_s at full load.
const DefaultQueueFrames = 20_000

// DefaultSpillFrames bounds the samples waiting for the work queue.
const DefaultSpillFrames = 20_000

// handoff is one placed sample on its way to the bus.
type handoff struct {
	subject string // trk.v1.<cell3>.<cell5>.<track_id>
	cell3   string
	track   *Track
	queued  time.Time
	// done is called once: handed true when the track was published or
	// written to the work queue (B-05: only then is it acknowledged).
	done func(handed bool)
}

// Outbox hands placed samples to the bus without ever blocking the
// ingest (spec 05 §5): a bounded memory queue in front of the core
// publish of trk.v1, and behind it the ingest.v1.<cell3> work queue on
// JetStream. A sample goes to the work queue when the memory queue is
// full, when it waited in it longer than the policy's ingest_queue_s, or
// when its core publish failed; the drain replays it as backlog with its
// own captured_at (Drain). A sample neither queue can take is not
// acknowledged, so the client keeps it and sends it again; it is counted
// as dropped_queue_full, never lost silently.
type Outbox struct {
	Pub MessagePublisher
	// QueueFor is the policy's ingest_queue_s.
	QueueFor func() time.Duration
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
	// Timeout bounds one work-queue write (bus.DefaultPublishTimeout).
	Timeout time.Duration

	q     chan *handoff
	spill chan *handoff
}

// NewOutbox is an outbox with memory for queueFrames samples and
// spillFrames samples waiting for the work queue (0: the defaults). A
// negative queueFrames has no memory at all: every sample goes through
// the work queue (the integration test of the drain).
func NewOutbox(pub MessagePublisher, queueFrames, spillFrames int, counters *core.Counters, logger *slog.Logger) *Outbox {
	if queueFrames == 0 {
		queueFrames = DefaultQueueFrames
	}
	if spillFrames <= 0 {
		spillFrames = DefaultSpillFrames
	}
	if counters == nil {
		counters = &core.Counters{}
	}
	if logger == nil {
		logger = obs.Discard()
	}
	o := &Outbox{Pub: pub, Counters: counters, Logger: logger, spill: make(chan *handoff, spillFrames)}
	if queueFrames > 0 {
		o.q = make(chan *handoff, queueFrames) // nil otherwise: a send on it never proceeds
	}
	return o
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Depth is the samples in memory and waiting for the work queue.
func (o *Outbox) Depth() (memory, spill int) { return len(o.q), len(o.spill) }

// Offer takes h without blocking: into memory, else towards the work
// queue; false when neither can take it (h.done(false) has been called).
func (o *Outbox) Offer(h *handoff) bool {
	h.queued = o.now()
	select {
	case o.q <- h:
		return true
	default:
	}
	o.Counters.Inc(CounterSpilledFull)
	return o.toSpill(h)
}

func (o *Outbox) toSpill(h *handoff) bool {
	select {
	case o.spill <- h:
		return true
	default:
		o.Counters.Inc(CounterDroppedQueueFull)
		h.done(false)
		return false
	}
}

// Run publishes from memory until ctx ends; what is left is offered to
// the work queue and then refused (never acknowledged unhanded).
func (o *Outbox) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			o.drainLeft()
			return
		}
		select {
		case <-ctx.Done():
			o.drainLeft()
			return
		case h := <-o.q:
			o.publish(ctx, h)
		}
	}
}

func (o *Outbox) publish(ctx context.Context, h *handoff) {
	if o.QueueFor != nil && o.now().Sub(h.queued) > o.QueueFor() {
		o.Counters.Inc(CounterSpilledAged)
		o.toSpill(h)
		return
	}
	if err := o.Pub.Publish(ctx, h.subject, h.track); err != nil {
		o.Counters.Inc(CounterPublishFailed)
		o.Counters.Inc(CounterSpilledFailed)
		o.Logger.LogAttrs(ctx, slog.LevelWarn, "track not published; it goes to the work queue",
			slog.String("flight_id", h.track.Body.TrackID), slog.String("client_id", h.track.Body.SourceInstance), obs.Err(err))
		o.toSpill(h)
		return
	}
	o.Counters.Inc(CounterPublished)
	h.done(true)
}

// drainLeft refuses what memory still holds when the process stops: the
// clients send it again.
func (o *Outbox) drainLeft() {
	for {
		select {
		case h := <-o.q:
			o.Counters.Inc(CounterDroppedQueueFull)
			h.done(false)
		default:
			return
		}
	}
}

// RunSpill writes samples to the work queue until ctx ends: each is
// acknowledged to its client once JetStream stored it (B-05).
func (o *Outbox) RunSpill(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			o.refuseSpill()
			return
		}
		select {
		case <-ctx.Done():
			o.refuseSpill()
			return
		case h := <-o.spill:
			o.writeQueue(ctx, h)
		}
	}
}

// refuseSpill refuses what waits for the work queue when the process
// stops: the clients send it again.
func (o *Outbox) refuseSpill() {
	for {
		select {
		case h := <-o.spill:
			o.Counters.Inc(CounterDroppedQueueFull)
			h.done(false)
		default:
			return
		}
	}
}

func (o *Outbox) writeQueue(ctx context.Context, h *handoff) {
	subject, err := bus.Ingest(h.cell3)
	if err == nil {
		err = o.Pub.Publish(ctx, subject, h.track)
	}
	if err != nil {
		o.Counters.Inc(CounterSpillFailed)
		o.Logger.LogAttrs(ctx, slog.LevelWarn, "track not written to the work queue; the client is not acknowledged and sends it again",
			slog.String("flight_id", h.track.Body.TrackID), slog.String("client_id", h.track.Body.SourceInstance), obs.Err(err))
		h.done(false)
		return
	}
	o.Counters.Inc(CounterSpilled)
	h.done(true)
}

// Counters of the event queue.
const (
	CounterEventsPublished = "events_published"
	CounterEventsRetried   = "events_publish_retried"
	CounterEventsDropped   = "events_dropped_queue_full"
)

// DefaultEventFrames bounds the events waiting for JetStream.
const DefaultEventFrames = 10_000

// event is one durable message: an identification change or a flight
// fact.
type event struct {
	subject string
	msg     bus.Enveloped
}

// Events publishes the durable messages of the ingest (ident.v1,
// flight.v1) off the hot path: a bounded queue and one worker that
// retries a failed publish until it succeeds or the process stops. The
// msg_id is the JetStream dedupe id, so a retry stores the message once.
// An event the queue cannot take is counted and logged, never dropped
// silently.
type Events struct {
	Pub      MessagePublisher
	Counters *core.Counters
	Logger   *slog.Logger
	// Retry is the wait between attempts (1 s).
	Retry time.Duration

	q chan event
}

// NewEvents is an event queue of n (0: DefaultEventFrames).
func NewEvents(pub MessagePublisher, n int, counters *core.Counters, logger *slog.Logger) *Events {
	if n <= 0 {
		n = DefaultEventFrames
	}
	if counters == nil {
		counters = &core.Counters{}
	}
	if logger == nil {
		logger = obs.Discard()
	}
	return &Events{Pub: pub, Counters: counters, Logger: logger, q: make(chan event, n)}
}

// Publish queues m for subject without blocking.
func (e *Events) Publish(subject string, m bus.Enveloped) {
	select {
	case e.q <- event{subject: subject, msg: m}:
	default:
		e.Counters.Inc(CounterEventsDropped)
		e.Logger.LogAttrs(context.Background(), slog.LevelError, "event not published: the event queue is full",
			slog.String("subject", subject), slog.String("msg_id", m.Head().MsgID))
	}
}

// Len is the events waiting.
func (e *Events) Len() int { return len(e.q) }

// Run publishes until ctx ends.
func (e *Events) Run(ctx context.Context) {
	retry := e.Retry
	if retry <= 0 {
		retry = time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.q:
			for {
				err := e.Pub.Publish(ctx, ev.subject, ev.msg)
				if err == nil {
					e.Counters.Inc(CounterEventsPublished)
					break
				}
				if ctx.Err() != nil {
					return
				}
				e.Counters.Inc(CounterEventsRetried)
				e.Logger.LogAttrs(ctx, slog.LevelWarn, "event not published yet; retrying", slog.String("subject", ev.subject), obs.Err(err))
				t := time.NewTimer(retry)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
		}
	}
}
