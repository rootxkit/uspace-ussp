package flights

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Store records a flight fact (pgstore on the relational database).
type Store interface {
	Record(ctx context.Context, b Body) error
}

// Source is the FLIGHT stream as the recorder reads it
// (bus.StreamSource).
type Source interface {
	Fetch(ctx context.Context, n int, wait time.Duration) ([]bus.Msg, error)
}

// Counters of the recorder.
const (
	CounterRecorded       = "flight_events_recorded"
	CounterRecordFailed   = "flight_events_record_failed"
	CounterRecordRefused  = "flight_events_unreadable"
	CounterRecordAckFails = "flight_events_ack_failed"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var endReasons = map[string]bool{EndLanded: true, EndTelemetryLost: true, EndIntentEnded: true, EndOperator: true}

var kinds = map[string]bool{EventStarted: true, EventTelemetryLost: true, EventTelemetryResumed: true, EventEnded: true}

// Decode reads one flight/event/v1 message and checks what the schema
// says: the envelope, a UUID flight id, a known event, an end reason
// exactly on ended, the client and the serial.
func Decode(data []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return Event{}, core.Fieldf("message", "not a flight/event/v1 message")
	}
	b := e.Body
	switch {
	case e.Schema != Schema:
		return Event{}, core.Fieldf("schema", "%q where %q is expected", e.Schema, Schema)
	case !uuidRe.MatchString(b.FlightID):
		return Event{}, core.Fieldf("body.flight_id", "not a version 4 UUID")
	case !kinds[b.Event]:
		return Event{}, core.Fieldf("body.event", "unknown event")
	case (b.Event == EventEnded) != (b.EndReason != nil):
		return Event{}, core.Fieldf("body.end_reason", "set exactly when the event is ended")
	case b.EndReason != nil && !endReasons[*b.EndReason]:
		return Event{}, core.Fieldf("body.end_reason", "unknown end reason")
	case b.ClientID == "" || b.UASSerial == "":
		return Event{}, core.Fieldf("body", "client_id and uas_serial are required")
	case b.IntentID != nil && !uuidRe.MatchString(*b.IntentID):
		return Event{}, core.Fieldf("body.intent_id", "not a version 4 UUID")
	case b.StartedAt.IsZero() || b.At.IsZero():
		return Event{}, core.Fieldf("body", "at and started_at are required")
	case b.Position != nil && !b.Position.LatLon().Valid():
		return Event{}, core.Fieldf("body.position", "not a WGS84 position")
	}
	if err := e.Validate(); err != nil {
		return Event{}, err
	}
	return e, nil
}

// Recorder records every fact of the FLIGHT stream (api; docs/PLAN.md
// §3.2): each message is recorded, then acknowledged (B-05). A message
// the store cannot take is left in the stream (nak) and the recorder
// waits before it reads again; one that does not read as a flight fact
// is logged at error level with its subject, counted and acknowledged
// (it can never be recorded).
type Recorder struct {
	Source   Source
	Store    Store
	Counters *core.Counters
	Logger   *slog.Logger
	// Batch (64) and Wait (1 s).
	Batch int
	Wait  time.Duration
}

func (r *Recorder) logger() *slog.Logger {
	if r.Logger == nil {
		return obs.Discard()
	}
	return r.Logger
}

func (r *Recorder) counters() *core.Counters {
	if r.Counters == nil {
		r.Counters = &core.Counters{}
	}
	return r.Counters
}

// Run records until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	n, wait := r.Batch, r.Wait
	if n <= 0 {
		n = 64
	}
	if wait <= 0 {
		wait = time.Second
	}
	for ctx.Err() == nil {
		msgs, err := r.Source.Fetch(ctx, n, wait)
		if err != nil || !r.Take(ctx, msgs) {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
		}
	}
}

// Take records one fetch; false when the store failed (the rest is left
// for redelivery).
func (r *Recorder) Take(ctx context.Context, msgs []bus.Msg) bool {
	for i, m := range msgs {
		e, err := Decode(m.Data())
		if err != nil {
			r.counters().Inc(CounterRecordRefused)
			r.logger().LogAttrs(ctx, slog.LevelError, "flight fact not readable; it cannot be recorded",
				slog.String("subject", m.Subject()), obs.Err(err))
			if err := m.Ack(); err != nil {
				r.counters().Inc(CounterRecordAckFails)
			}
			continue
		}
		if err := r.Store.Record(ctx, e.Body); err != nil {
			r.counters().Inc(CounterRecordFailed)
			r.logger().LogAttrs(ctx, slog.LevelWarn, "flight fact not recorded yet; it stays in the stream",
				slog.String("flight_id", e.Body.FlightID), slog.String("event", e.Body.Event), obs.Err(err))
			for _, rest := range msgs[i:] {
				_ = rest.Nak()
			}
			return false
		}
		r.counters().Inc(CounterRecorded)
		if err := m.Ack(); err != nil {
			r.counters().Inc(CounterRecordAckFails)
		}
	}
	return true
}
