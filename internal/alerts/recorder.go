package alerts

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Record is what the record keeps of one alert message.
type Record struct {
	Body
	// PeerRef is the other aircraft of a proximity alert.
	PeerRef *string
	// Cell5 is the subject's cell (where the flight was).
	Cell5 string
}

// Store is the alerts table (pgstore).
type Store interface {
	// RecordAlert writes one alert message; recorded is false when the
	// flight is not in the flights table yet (its fact is on its way).
	RecordAlert(ctx context.Context, r Record) (recorded bool, err error)
	// RecordDelivery notes the first send of an alert to a client;
	// false when the alert is not recorded yet.
	RecordDelivery(ctx context.Context, d DeliveryBody) (bool, error)
}

// Source is the ALRT stream as the recorder reads it (bus.StreamSource).
type Source interface {
	Fetch(ctx context.Context, n int, wait time.Duration) ([]bus.Msg, error)
}

// Counters of the recorder.
const (
	CounterRecorded       = "alerts_recorded"
	CounterRefreshSkipped = "alerts_refresh_skipped"
	CounterUnread         = "alerts_unreadable"
	CounterRetried        = "alerts_retried"
	CounterGaveUp         = "alerts_given_up"
	CounterAckFailed      = "alerts_ack_failed"
	CounterDeliveries     = "alerts_deliveries_recorded"
	CounterOtherSchema    = "alerts_other_schema"
	CounterDeferred       = "alerts_deferred_flight_not_recorded"
)

// DefaultMaxAttempts bounds the tries of one message before it is given
// up, counted and logged at error level. A message whose flight is not
// recorded yet is tried again at each redelivery (the consumer's ack
// wait, 30 s), so it waits about five minutes for its flight fact.
const DefaultMaxAttempts = 10

// DefaultRefreshEvery is the longest a recorded alert's numbers wait
// for a refresh: the monitor republishes every active alert each
// second (C-08), and the record keeps one in five of those.
const DefaultRefreshEvery = 5 * time.Second

// maxRemembered bounds the recorder's memory (E-10): past it the memory
// is cleared, which costs one extra write per alert, never a missed one.
const maxRemembered = 100_000

// Recorder is api's consumer of alrt.v1: a raise, a clear, a severity
// change or an acknowledgement or escalation republish is written at
// once; a republish of an unchanged active alert refreshes its numbers
// at most every RefreshEvery. Idempotent by alert_id, which the monitor
// derives from the condition and its raise time; a cleared row is never
// reopened, so the messages of one alert may be written in any order. A
// store that fails leaves the message and the rest of the fetch in the
// stream (nak) and waits; a message whose flight (or, for a delivery,
// whose alert) is not recorded yet is left unacknowledged, redelivered
// after the ack wait, and never holds up the others (one flight whose
// fact is late must not stall every alert's record); either is given up
// after MaxAttempts tries, counted and logged. A message that does not
// read is logged, counted and acknowledged.
type Recorder struct {
	Source       Source
	Store        Store
	Counters     *core.Counters
	Logger       *slog.Logger
	MaxAttempts  int
	RefreshEvery time.Duration
	Batch        int
	Wait         time.Duration

	last     map[string]seen
	attempts map[string]int
	once     sync.Once
}

type seen struct {
	at       time.Time
	severity core.Severity
	state    string
}

func (r *Recorder) logger() *slog.Logger {
	if r.Logger == nil {
		return obs.Discard()
	}
	return r.Logger
}

func (r *Recorder) counters() *core.Counters {
	r.once.Do(func() {
		if r.Counters == nil {
			r.Counters = &core.Counters{}
		}
	})
	return r.Counters
}

// Run records until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	n, wait := r.Batch, r.Wait
	if n <= 0 {
		n = 128
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

// errNotRecorded is a message whose flight (or alert) is not recorded
// yet: it is tried again.
var errNotRecorded = errors.New("the flight or the alert is not recorded yet")

// Take handles one fetch; false when a message is left for redelivery
// (the rest of the fetch with it, so one alert's messages stay in
// order).
func (r *Recorder) Take(ctx context.Context, msgs []bus.Msg) bool {
	if r.last == nil || len(r.last) > maxRemembered {
		r.last = map[string]seen{}
	}
	if r.attempts == nil || len(r.attempts) > maxRemembered {
		r.attempts = map[string]int{}
	}
	maxTries := r.MaxAttempts
	if maxTries <= 0 {
		maxTries = DefaultMaxAttempts
	}
	every := r.RefreshEvery
	if every <= 0 {
		every = DefaultRefreshEvery
	}
	ack := func(m bus.Msg) {
		if err := m.Ack(); err != nil {
			r.counters().Inc(CounterAckFailed)
		}
	}
	for i, m := range msgs {
		key, err := r.take(ctx, m, every)
		switch {
		case errors.Is(err, errSkip):
			ack(m)
			continue
		case err == nil:
			delete(r.attempts, key)
			ack(m)
			continue
		case errors.Is(err, errNotRecorded):
			// Left for redelivery after the ack wait; the others go on.
			r.attempts[key]++
			if r.attempts[key] >= maxTries {
				delete(r.attempts, key)
				r.counters().Inc(CounterGaveUp)
				r.logger().LogAttrs(ctx, slog.LevelError, "alert message given up: its flight was never recorded",
					slog.String("subject", m.Subject()), slog.Int("attempts", maxTries))
				ack(m)
				continue
			}
			r.counters().Inc(CounterDeferred)
			continue
		}
		r.attempts[key]++
		if r.attempts[key] >= maxTries {
			delete(r.attempts, key)
			r.counters().Inc(CounterGaveUp)
			r.logger().LogAttrs(ctx, slog.LevelError, "alert message given up after its tries: not recorded",
				slog.String("subject", m.Subject()), slog.Int("attempts", maxTries), obs.Err(err))
			ack(m)
			continue
		}
		r.counters().Inc(CounterRetried)
		r.logger().LogAttrs(ctx, slog.LevelWarn, "alert message not recorded yet; it stays in the stream",
			slog.String("subject", m.Subject()), obs.Err(err))
		for _, rest := range msgs[i:] {
			_ = rest.Nak()
		}
		return false
	}
	return true
}

// errSkip is a message that is acknowledged without a write.
var errSkip = errors.New("nothing to write")

func (r *Recorder) take(ctx context.Context, m bus.Msg, every time.Duration) (string, error) {
	data := m.Data()
	subj, perr := bus.Parse(m.Subject())
	switch SchemaOf(data) {
	case SchemaAlert:
	case SchemaDelivery:
		d, err := DecodeDelivery(data)
		if err != nil {
			r.unread(ctx, m, err)
			return "", errSkip
		}
		ok, err := r.Store.RecordDelivery(ctx, d.Body)
		if err == nil && !ok {
			err = errNotRecorded
		}
		if err == nil {
			r.counters().Inc(CounterDeliveries)
		}
		return d.Body.AlertID + "|" + d.Body.ClientID, err
	default:
		r.counters().Inc(CounterOtherSchema)
		return "", errSkip
	}
	am, err := Decode(data)
	if err != nil {
		r.unread(ctx, m, err)
		return "", errSkip
	}
	b := am.Body
	prev, known := r.last[b.AlertID]
	fact := b.AckedAt != nil || b.EscalatedAt != nil
	if known && !fact && b.State == StateUpdated && prev.state != StateCleared && prev.severity == b.Severity && b.UpdatedAt.Sub(prev.at) < every {
		r.counters().Inc(CounterRefreshSkipped)
		return "", errSkip
	}
	rec := Record{Body: b}
	if perr == nil {
		rec.Cell5 = subj.Cell5
	}
	if peer, ok := b.Peer(); ok {
		rec.PeerRef = &peer
	}
	ok, err := r.Store.RecordAlert(ctx, rec)
	if err == nil && !ok {
		err = errNotRecorded
	}
	if err != nil {
		return b.AlertID, err
	}
	r.last[b.AlertID] = seen{at: b.UpdatedAt, severity: b.Severity, state: b.State}
	if b.State == StateCleared {
		delete(r.last, b.AlertID)
	}
	r.counters().Inc(CounterRecorded)
	return b.AlertID, nil
}

func (r *Recorder) unread(ctx context.Context, m bus.Msg, err error) {
	r.counters().Inc(CounterUnread)
	r.logger().LogAttrs(ctx, slog.LevelError, "alert message not readable; it cannot be recorded",
		slog.String("subject", m.Subject()), obs.Err(err))
}
