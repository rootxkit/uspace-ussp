package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Gap causes (05 §5, B-13: a hole is never silent).
const (
	// GapQueueAge: a sample waited in the work queue longer than the
	// policy's ingest_backlog_max_s and was shed, oldest first.
	GapQueueAge = "ingest_queue_age"
	// GapAgedOutUnread: the work queue removed samples nobody was ever
	// delivered (its own ten-minute bound, or a purge).
	GapAgedOutUnread = "ingest_queue_aged_out"
	// GapCorrupt: a work-queue message that does not read as a track.
	GapCorrupt = "ingest_queue_corrupt"
)

// Gap is one record of samples dropped from the work queue: when the
// dropped samples were captured (from the first to the last; zero when
// they are gone unread) and how many.
type Gap struct {
	Cause          string    `json:"cause"`
	SourceInstance string    `json:"source_instance"`
	GapStarted     time.Time `json:"gap_started"`
	GapEnded       time.Time `json:"gap_ended"`
	Dropped        int       `json:"dropped"`
	FromSeq        uint64    `json:"from_seq,omitempty"`
	ToSeq          uint64    `json:"to_seq,omitempty"`
}

// GapReporter records a gap: published on src.v1 with the client's
// status, logged and counted (Status implements it).
type GapReporter interface {
	ReportGap(ctx context.Context, g Gap)
}

// QueueSource is the INGEST work queue as the drain reads it
// (bus.StreamSource).
type QueueSource interface {
	Fetch(ctx context.Context, n int, wait time.Duration) ([]bus.Msg, error)
	NeverDelivered(ctx context.Context) (from, to uint64, err error)
}

// Counters of the drain (E-09).
const (
	CounterDrained      = "drain_replayed"
	CounterDrainFailed  = "drain_publish_failed"
	CounterDrainShedAge = "drain_shed_age"
	CounterDrainCorrupt = "drain_corrupt"
	CounterDrainAgedOut = "drain_aged_out_unread"
	CounterDrainAckFail = "drain_ack_failed"
)

// GapInstance is the source instance of a gap whose client is unknown
// (samples the queue removed unread).
const GapInstance = "work_queue"

// Drain replays the work queue (spec 05 §5): every track is published
// on trk.v1 as backlog with its own captured_at and msg_id (so a sample
// is never published twice under two ids), then acknowledged, which
// removes it from the queue. A track older than the policy's
// ingest_backlog_max_s is shed with a gap record instead, the oldest
// first (the queue delivers in order), never the newest. Every period
// the samples the queue removed before anyone was delivered them are
// counted as a gap too. A publish that fails leaves the track queued
// (nak) and the drain waits before the next fetch.
type Drain struct {
	Source QueueSource
	Pub    MessagePublisher
	Gaps   GapReporter
	// MaxAge is the policy's ingest_backlog_max_s.
	MaxAge   func() time.Duration
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
	// Batch (256), Wait (1 s) and CheckEvery (10 s): the fetch and the
	// unread-loss check.
	Batch      int
	Wait       time.Duration
	CheckEvery time.Duration

	reportedTo uint64
}

func (d *Drain) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Drain) logger() *slog.Logger {
	if d.Logger == nil {
		return obs.Discard()
	}
	return d.Logger
}

// Run drains until ctx ends.
func (d *Drain) Run(ctx context.Context) {
	batch, wait, every := d.Batch, d.Wait, d.CheckEvery
	if batch <= 0 {
		batch = 256
	}
	if wait <= 0 {
		wait = time.Second
	}
	if every <= 0 {
		every = 10 * time.Second
	}
	next := d.now()
	for ctx.Err() == nil {
		if !d.now().Before(next) {
			d.CheckUnread(ctx)
			next = d.now().Add(every)
		}
		msgs, err := d.Source.Fetch(ctx, batch, wait)
		if err != nil {
			pause(ctx, wait)
			continue
		}
		if !d.Take(ctx, msgs) {
			pause(ctx, wait)
		}
	}
}

func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// shedRun accumulates the consecutive shed tracks of one client into
// one gap record.
type shedRun struct {
	gap  Gap
	msgs []bus.Msg
}

// Take handles one fetch; false when a publish failed (the rest of the
// fetch is left for redelivery).
func (d *Drain) Take(ctx context.Context, msgs []bus.Msg) bool {
	var run *shedRun
	flush := func() {
		if run == nil {
			return
		}
		d.Gaps.ReportGap(ctx, run.gap) // recorded before the samples leave the queue
		for _, m := range run.msgs {
			if err := m.Ack(); err != nil {
				d.Counters.Inc(CounterDrainAckFail)
			}
		}
		run = nil
	}
	maxAge := time.Duration(0)
	if d.MaxAge != nil {
		maxAge = d.MaxAge()
	}
	for i, m := range msgs {
		var t Track
		if err := json.Unmarshal(m.Data(), &t); err != nil || t.Validate() != nil || t.Body.SourceInstance == "" {
			flush()
			d.Counters.Inc(CounterDrainCorrupt)
			seq, _ := m.StreamSeq()
			d.Gaps.ReportGap(ctx, Gap{Cause: GapCorrupt, SourceInstance: GapInstance, Dropped: 1, FromSeq: seq, ToSeq: seq})
			if err := m.Ack(); err != nil {
				d.Counters.Inc(CounterDrainAckFail)
			}
			continue
		}
		if maxAge > 0 && d.now().Sub(t.RxTS.Time) > maxAge {
			d.Counters.Inc(CounterDrainShedAge)
			if run != nil && run.gap.SourceInstance != t.Body.SourceInstance {
				flush()
			}
			if run == nil {
				run = &shedRun{gap: Gap{Cause: GapQueueAge, SourceInstance: t.Body.SourceInstance, GapStarted: t.CapturedAt.Time}}
			}
			run.gap.GapEnded = t.CapturedAt.Time
			run.gap.Dropped++
			run.msgs = append(run.msgs, m)
			continue
		}
		flush()
		t.Backlog = true
		subject, err := trackSubject(&t.Body)
		if err == nil {
			err = d.Pub.Publish(ctx, subject, &t)
		}
		if err != nil {
			d.Counters.Inc(CounterDrainFailed)
			d.logger().LogAttrs(ctx, slog.LevelWarn, "work-queue track not replayed; it stays queued",
				slog.String("flight_id", t.Body.TrackID), obs.Err(err))
			for _, rest := range msgs[i:] {
				_ = rest.Nak()
			}
			return false
		}
		d.Counters.Inc(CounterDrained)
		if err := m.Ack(); err != nil {
			d.Counters.Inc(CounterDrainAckFail)
		}
	}
	flush()
	return true
}

// CheckUnread records, once, the samples the queue removed before they
// were delivered to anyone.
func (d *Drain) CheckUnread(ctx context.Context) {
	from, to, err := d.Source.NeverDelivered(ctx)
	if err != nil || from > to {
		return
	}
	if to <= d.reportedTo {
		return
	}
	from = max(from, d.reportedTo+1)
	n := int(to - from + 1)
	d.Counters.Add(CounterDrainAgedOut, uint64(n))
	d.Gaps.ReportGap(ctx, Gap{Cause: GapAgedOutUnread, SourceInstance: GapInstance, Dropped: n, FromSeq: from, ToSeq: to})
	d.reportedTo = to
}

// Validate checks the envelope of a track read back from the queue.
func (t *Track) Validate() error {
	if t.Schema != SchemaTrack {
		return core.Fieldf("schema", "%q where %q is expected", t.Schema, SchemaTrack)
	}
	return t.Envelope.Validate()
}
