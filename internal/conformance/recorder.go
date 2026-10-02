package conformance

import (
	"context"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// StateStore records a conformance state in the flight's timeline
// (conformance_states); recorded is false when the flight is not in the
// flights table yet (its fact is still on its way), which is retried.
type StateStore interface {
	RecordState(ctx context.Context, b StateBody, at time.Time) (recorded bool, err error)
}

// IntentMover moves the flight's intent as its base state says
// (intent.Service.SetConformance).
type IntentMover interface {
	SetConformance(ctx context.Context, intentID, state, reason string) (bool, error)
}

// Source is the CONF stream as the recorder reads it (bus.StreamSource).
type Source interface {
	Fetch(ctx context.Context, n int, wait time.Duration) ([]bus.Msg, error)
}

// Counters of the recorder.
const (
	CounterRecorded       = "conformance_states_recorded"
	CounterRecordUnread   = "conformance_states_unreadable"
	CounterRecordRetried  = "conformance_states_retried"
	CounterRecordGaveUp   = "conformance_states_given_up"
	CounterRecordAckFails = "conformance_states_ack_failed"
	CounterIntentsMoved   = "conformance_intents_moved"
	CounterUnchanged      = "conformance_states_unchanged"
)

// DefaultMaxAttempts bounds the tries of one message before it is given
// up, counted and logged at error level (a permanent failure is
// visible, never retried forever).
const DefaultMaxAttempts = 10

// maxRemembered bounds the recorder's memory of the last state per
// flight and of the attempts per message (E-10): beyond it the memory
// is cleared, which costs one duplicate row per flight, never a missed
// one.
const maxRemembered = 100_000

// Recorder is api's consumer of conf.v1 (docs/PLAN.md §3.2: the hot path
// judges, api persists and opens the workflow): every message whose
// state or reason differs from the last one recorded for its flight is
// written to conformance_states, and the intent is moved by the base
// state (nonconforming, contingent, back to activated), then the
// message is acknowledged. The per-sample messages in between change
// nothing and are acknowledged at once. A store that fails leaves the
// message in the stream (nak) for DefaultMaxAttempts tries; a message
// that does not read is logged, counted and acknowledged.
type Recorder struct {
	Source      Source
	Store       StateStore
	Intents     IntentMover
	Counters    *core.Counters
	Logger      *slog.Logger
	MaxAttempts int
	// Batch (64) and Wait (1 s).
	Batch int
	Wait  time.Duration

	last     map[string]string // flight id -> state|reason recorded
	attempts map[string]int    // msg id -> failed tries
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

func key(b StateBody) string {
	k := string(b.State) + "|" + string(b.BaseState)
	if b.Reason != nil {
		k += "|" + *b.Reason
	}
	return k
}

// Take handles one fetch; false when a message is left for redelivery
// (the rest of the fetch with it, so one flight's states stay in order).
func (r *Recorder) Take(ctx context.Context, msgs []bus.Msg) bool {
	if r.last == nil || len(r.last) > maxRemembered {
		r.last = map[string]string{}
	}
	if r.attempts == nil || len(r.attempts) > maxRemembered {
		r.attempts = map[string]int{}
	}
	maxTries := r.MaxAttempts
	if maxTries <= 0 {
		maxTries = DefaultMaxAttempts
	}
	ack := func(m bus.Msg) {
		if err := m.Ack(); err != nil {
			r.counters().Inc(CounterRecordAckFails)
		}
	}
	for i, m := range msgs {
		sm, err := DecodeState(m.Data())
		if err != nil {
			r.counters().Inc(CounterRecordUnread)
			r.logger().LogAttrs(ctx, slog.LevelError, "conformance state not readable; it cannot be recorded",
				slog.String("subject", m.Subject()), obs.Err(err))
			ack(m)
			continue
		}
		b := sm.Body
		k := key(b)
		if r.last[b.FlightID] == k && !b.Transition {
			r.counters().Inc(CounterUnchanged)
			ack(m)
			continue
		}
		if err := r.record(ctx, sm); err != nil {
			r.attempts[sm.MsgID]++
			if r.attempts[sm.MsgID] >= maxTries {
				delete(r.attempts, sm.MsgID)
				r.counters().Inc(CounterRecordGaveUp)
				r.logger().LogAttrs(ctx, slog.LevelError, "conformance state given up after its tries: not recorded, the intent not moved",
					obs.FlightID(b.FlightID), slog.String("state", string(b.State)), slog.Int("attempts", maxTries), obs.Err(err))
				ack(m)
				continue
			}
			r.counters().Inc(CounterRecordRetried)
			r.logger().LogAttrs(ctx, slog.LevelWarn, "conformance state not recorded yet; it stays in the stream",
				obs.FlightID(b.FlightID), slog.String("state", string(b.State)), obs.Err(err))
			for _, rest := range msgs[i:] {
				_ = rest.Nak()
			}
			return false
		}
		delete(r.attempts, sm.MsgID)
		r.last[b.FlightID] = k
		r.counters().Inc(CounterRecorded)
		ack(m)
	}
	return true
}

// flightNotRecordedError is a state of a flight the flights table does
// not hold yet.
type flightNotRecordedError struct{ flightID string }

func (e flightNotRecordedError) Error() string {
	return "flight " + e.flightID + " is not recorded yet"
}

// returnSeen is false for a conforming state that is not the tracker's
// own transition out of nonconforming or a lost link: a fresh tracker's
// first state, or a heartbeat this recorder reads after forgetting the
// last state (an api restart), never moves an intent back to activated;
// only a tracker that saw the flight nonconforming and then inside for
// the whole hysteresis does (SC-22).
func returnSeen(b StateBody) bool {
	if b.BaseState != StateConforming {
		return true
	}
	return b.Transition && b.PreviousState != nil && (*b.PreviousState == StateNonconforming || *b.PreviousState == StateLostLink)
}

// record moves the intent first (idempotent: a repeat moves nothing),
// then appends the timeline row.
func (r *Recorder) record(ctx context.Context, sm StateMessage) error {
	b := sm.Body
	reason := ""
	if b.Reason != nil {
		reason = *b.Reason
	}
	if b.IntentID != nil && r.Intents != nil && b.BaseState != StateUnknown && returnSeen(b) {
		moved, err := r.Intents.SetConformance(ctx, *b.IntentID, string(b.BaseState), reason)
		if err != nil {
			return err
		}
		if moved {
			r.counters().Inc(CounterIntentsMoved)
		}
	}
	ok, err := r.Store.RecordState(ctx, b, sm.CapturedAt.Time)
	if err != nil {
		return err
	}
	if !ok {
		return flightNotRecordedError{b.FlightID}
	}
	return nil
}
