package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

type recMsg struct {
	data       []byte
	acked, nak int
}

func (m *recMsg) Data() []byte               { return m.data }
func (m *recMsg) Subject() string            { return "conf.v1." + flightA }
func (m *recMsg) StreamSeq() (uint64, error) { return 1, nil }
func (m *recMsg) Ack() error                 { m.acked++; return nil }
func (m *recMsg) Nak() error                 { m.nak++; return nil }
func (m *recMsg) InProgress() error          { return nil }

type fakeStateStore struct {
	rows    []StateBody
	missing bool // the flight is not recorded yet
	err     error
}

func (s *fakeStateStore) RecordState(_ context.Context, b StateBody, _ time.Time) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.missing {
		return false, nil
	}
	s.rows = append(s.rows, b)
	return true, nil
}

type fakeMover struct {
	calls []string
	err   error
}

func (f *fakeMover) SetConformance(_ context.Context, _, state, reason string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.calls = append(f.calls, state+"/"+reason)
	return true, nil
}

func stateMsg(t *testing.T, state, base State, reason string, transition bool) *recMsg {
	t.Helper()
	snap := Snapshot{FlightID: flightA, IntentID: intentA, State: state, BaseState: base, Reason: reason}
	ev := Events{}
	if transition {
		ev.Transitions = []Transition{{From: StateConforming, To: state}}
	}
	raw, err := json.Marshal(StateMessageOf(snap, ev, core.Times{RxTS: tt0, CapturedAt: tt0, Source: core.TimeSourceClock}, 2))
	if err != nil {
		t.Fatal(err)
	}
	return &recMsg{data: raw}
}

// A transition is recorded and moves the intent by its base state; a
// repeat of the same state changes nothing (E-01 pair); a message that
// does not read is acknowledged and counted.
func TestRecorderRecordsChangesOnly(t *testing.T) {
	st, mv := &fakeStateStore{}, &fakeMover{}
	r := &Recorder{Store: st, Intents: mv}
	first := stateMsg(t, StateConforming, StateConforming, "", false)
	nc := stateMsg(t, StateNonconforming, StateNonconforming, ReasonThresholdExceeded, true)
	same := stateMsg(t, StateNonconforming, StateNonconforming, ReasonThresholdExceeded, false)
	lost := stateMsg(t, StateLostLink, StateNonconforming, ReasonTelemetryLost, true)
	bad := &recMsg{data: []byte("{")}
	if !r.Take(context.Background(), []bus.Msg{first, nc, same, bad, lost}) {
		t.Fatal("left a message")
	}
	if len(st.rows) != 3 || st.rows[1].State != StateNonconforming || st.rows[2].State != StateLostLink {
		t.Fatalf("rows %+v", st.rows)
	}
	if len(mv.calls) != 3 || mv.calls[1] != "nonconforming/threshold_exceeded" || mv.calls[2] != "nonconforming/telemetry_lost" {
		t.Fatalf("moves %v", mv.calls)
	}
	for _, m := range []*recMsg{first, nc, same, bad, lost} {
		if m.acked != 1 || m.nak != 0 {
			t.Fatalf("not acknowledged once: %+v", m)
		}
	}
	if r.Counters.Get(CounterUnchanged) != 1 || r.Counters.Get(CounterRecordUnread) != 1 || r.Counters.Get(CounterIntentsMoved) != 3 {
		t.Fatalf("counters %v", r.Counters.Snapshot())
	}
}

// A store that fails, or a flight not yet recorded, leaves the message
// for redelivery (nak, the rest of the fetch with it) up to the bound,
// then it is given up visibly: counted and acknowledged (E-01 pair).
func TestRecorderRetriesBoundedThenGivesUp(t *testing.T) {
	st := &fakeStateStore{missing: true}
	r := &Recorder{Store: st, Intents: &fakeMover{}, MaxAttempts: 3}
	m := stateMsg(t, StateNonconforming, StateNonconforming, ReasonOutsideVolumeH, true)
	after := stateMsg(t, StateConforming, StateConforming, "", true)
	for i := range 2 {
		if r.Take(context.Background(), []bus.Msg{m, after}) {
			t.Fatalf("try %d: taken", i)
		}
	}
	if m.nak != 2 || after.nak != 2 || m.acked != 0 || r.Counters.Get(CounterRecordRetried) != 2 {
		t.Fatalf("%+v %+v", m, after)
	}
	st.missing = false
	st.err = errors.New("database down")
	if !r.Take(context.Background(), []bus.Msg{m}) || m.acked != 1 || r.Counters.Get(CounterRecordGaveUp) != 1 {
		t.Fatalf("not given up after 3 tries: %+v %v", m, r.Counters.Snapshot())
	}
	// The flight recorded: the next one goes through.
	st.err = nil
	ok := stateMsg(t, StateConforming, StateConforming, "", true)
	if !r.Take(context.Background(), []bus.Msg{ok}) || ok.acked != 1 || len(st.rows) != 1 {
		t.Fatalf("%+v %d", ok, len(st.rows))
	}
	// A mover that fails is retried like a store that fails.
	r2 := &Recorder{Store: &fakeStateStore{}, Intents: &fakeMover{err: errors.New("x")}}
	m2 := stateMsg(t, StateContingent, StateContingent, "", true)
	if r2.Take(context.Background(), []bus.Msg{m2}) || m2.nak != 1 {
		t.Fatalf("%+v", m2)
	}
	// Unknown moves nothing and is still recorded.
	mv := &fakeMover{}
	r3 := &Recorder{Store: &fakeStateStore{}, Intents: mv}
	if !r3.Take(context.Background(), []bus.Msg{stateMsg(t, StateUnknown, StateUnknown, UnknownAuthorisationMissing, true)}) || len(mv.calls) != 0 {
		t.Fatalf("unknown moved the intent: %v", mv.calls)
	}
}

type fetchOnce struct {
	msgs []bus.Msg
	err  error
	n    int
}

func (f *fetchOnce) Fetch(ctx context.Context, _ int, _ time.Duration) ([]bus.Msg, error) {
	f.n++
	if f.n == 1 {
		return f.msgs, f.err
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRecorderRun(t *testing.T) {
	st := &fakeStateStore{}
	src := &fetchOnce{msgs: []bus.Msg{stateMsg(t, StateConforming, StateConforming, "", true)}}
	r := &Recorder{Source: src, Store: st, Wait: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r.Run(ctx)
	if len(st.rows) != 1 {
		t.Fatalf("rows %d", len(st.rows))
	}
}
