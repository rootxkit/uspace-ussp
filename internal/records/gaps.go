package records

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Gap is one gap record telemetry-ingest published on src.v1, as api
// keeps it for the records.
type Gap struct {
	MsgID          string
	SourceInstance string
	Cause          string
	// Started and Ended are when the dropped samples were captured (zero
	// for samples the queue removed unread).
	Started, Ended time.Time
	Dropped        int
	FromSeq, ToSeq uint64
}

// GapStore records gaps (idempotent by MsgID).
type GapStore interface {
	RecordGap(ctx context.Context, g Gap) error
}

// Counters of the GapRecorder.
const (
	CounterGapsRecorded   = "record_gaps_recorded"
	CounterGapsUnreadable = "record_gaps_unreadable"
	CounterGapsDropped    = "record_gaps_dropped"
	CounterGapsFailed     = "record_gaps_not_stored"
)

// GapQueue bounds the gaps waiting to be stored (E-10).
const GapQueue = 1024

// maxStatusBytes bounds a src.v1 message read.
const maxStatusBytes = 64 << 10

// GapRecorder keeps the gap records of telemetry-ingest (src.v1, core
// NATS, published once) for the records: Take decodes one message on the
// subscription's goroutine and queues a gap; Run stores the queue. A
// full queue drops the gap and counts it (the hole then says "no
// recorded cause", as for a gap published while api was not listening).
type GapRecorder struct {
	Store    GapStore
	Counters *core.Counters
	Logger   *slog.Logger

	ch chan Gap
}

// NewGapRecorder is a recorder with its queue.
func NewGapRecorder(st GapStore, counters *core.Counters, logger *slog.Logger) *GapRecorder {
	if counters == nil {
		counters = &core.Counters{}
	}
	if logger == nil {
		logger = obs.Discard()
	}
	return &GapRecorder{Store: st, Counters: counters, Logger: logger, ch: make(chan Gap, GapQueue)}
}

// DecodeGap reads the gap of one source/status/v1 message; false when
// the message carries none. An unreadable message is an error.
func DecodeGap(data []byte) (Gap, bool, error) {
	if len(data) > maxStatusBytes {
		return Gap{}, false, core.Fieldf("message", "longer than %d bytes", maxStatusBytes)
	}
	var m telemetry.SourceStatus
	if err := json.Unmarshal(data, &m); err != nil {
		return Gap{}, false, core.Fieldf("message", "not a source/status/v1 message")
	}
	g := m.Body.Gap
	if g == nil {
		return Gap{}, false, nil
	}
	switch {
	case m.MsgID == "" || len(m.MsgID) > 64:
		return Gap{}, false, core.Fieldf("msg_id", "empty or longer than 64")
	case g.Cause == "" || len(g.Cause) > 64:
		return Gap{}, false, core.Fieldf("body.gap.cause", "empty or longer than 64")
	case g.SourceInstance == "" || len(g.SourceInstance) > 128:
		return Gap{}, false, core.Fieldf("body.gap.source_instance", "empty or longer than 128")
	case g.Dropped < 0:
		return Gap{}, false, core.Fieldf("body.gap.dropped", "negative")
	case g.GapStarted.IsZero() != g.GapEnded.IsZero() || g.GapEnded.Before(g.GapStarted):
		return Gap{}, false, core.Fieldf("body.gap.gap_ended", "not after gap_started")
	}
	return Gap{MsgID: m.MsgID, SourceInstance: g.SourceInstance, Cause: g.Cause, Started: g.GapStarted, Ended: g.GapEnded,
		Dropped: g.Dropped, FromSeq: g.FromSeq, ToSeq: g.ToSeq}, true, nil
}

// Take is the subscription's handler: it never blocks.
func (r *GapRecorder) Take(subject string, data []byte) {
	g, ok, err := DecodeGap(data)
	if err != nil {
		r.Counters.Inc(CounterGapsUnreadable)
		r.Logger.Warn("src.v1 message not readable; a gap it carried is not recorded", "subject", subject, "error", err.Error())
		return
	}
	if !ok {
		return
	}
	select {
	case r.ch <- g:
	default:
		r.Counters.Inc(CounterGapsDropped)
		r.Logger.Error("gap record queue full: a telemetry gap is not recorded and its hole will say no recorded cause",
			"client_id", g.SourceInstance, "cause", g.Cause)
	}
}

// Run stores the queued gaps until ctx ends.
func (r *GapRecorder) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case g := <-r.ch:
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err := r.Store.RecordGap(sctx, g)
			cancel()
			if err != nil {
				r.Counters.Inc(CounterGapsFailed)
				r.Logger.Error("telemetry gap not stored; its hole will say no recorded cause", "client_id", g.SourceInstance, "error", err.Error())
				continue
			}
			r.Counters.Inc(CounterGapsRecorded)
		}
	}
}
