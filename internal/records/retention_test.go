package records

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

type memRetention struct {
	days     int
	sets     []int
	nullDays int
	held     []string
	nulled   int64
	errGet   error
	errSet   error
	errNull  error
	calls    int
}

func (m *memRetention) TelemetryRetentionDays(context.Context) (int, error) { return m.days, m.errGet }

func (m *memRetention) SetTelemetryRetentionDays(_ context.Context, days int) error {
	if m.errSet != nil {
		return m.errSet
	}
	m.sets = append(m.sets, days)
	m.days = days
	return nil
}

func (m *memRetention) NullOperatorPositions(_ context.Context, days int, held []string) (int64, error) {
	m.calls++
	if m.errNull != nil {
		return 0, m.errNull
	}
	m.nullDays, m.held = days, held
	return m.nulled, nil
}

func retentionOf(st *memRetention, pol *policy.Values, held []string, holdsOK bool) *Retention {
	return &Retention{Store: st, Counters: &core.Counters{},
		Policy: func() (policy.Values, bool) {
			if pol == nil {
				return policy.Values{}, false
			}
			return *pol, true
		},
		Holds: func() ([]string, bool) { return held, holdsOK }}
}

// The telemetry retention follows the policy row (set when it differs,
// left when it is the same: E-01 pair), and the positions are removed
// past their retention except the held flights' (an id that is not a
// flight id never reaches the query).
func TestRetention(t *testing.T) {
	pol := policy.Defaults()
	pol.TelemetryRetentionDays, pol.OperatorPositionRetentionDays = 120, 60
	st := &memRetention{days: 90, nulled: 7}
	held := []string{"0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d", "not-a-flight", "0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d"}
	r := retentionOf(st, &pol, held, true)
	done, err := r.RunOnce(context.Background())
	if err != nil || !done || !slices.Equal(st.sets, []int{120}) || st.nullDays != 60 || !slices.Equal(st.held, held[:1]) {
		t.Fatalf("%v %v sets %v null %d held %v", done, err, st.sets, st.nullDays, st.held)
	}
	if r.Counters.Get(CounterRetentionSet) != 1 || r.Counters.Get(CounterPositionsNulled) != 7 {
		t.Errorf("counters %v", r.Counters.Snapshot())
	}
	if _, err := r.RunOnce(context.Background()); err != nil || len(st.sets) != 1 {
		t.Fatalf("set again although equal: %v", st.sets)
	}
}

// Without the policy row nothing changes (the migration's retention
// stands); without the holds no position is removed (a held flight's
// could be). Both say so and count.
func TestRetentionWaitsForItsInputs(t *testing.T) {
	st := &memRetention{days: 90}
	r := retentionOf(st, nil, nil, true)
	if done, err := r.RunOnce(context.Background()); done || err != nil || len(st.sets) != 0 || st.calls != 0 {
		t.Fatalf("ran without the policy: %v %v", done, err)
	}
	pol := policy.Defaults()
	r = retentionOf(st, &pol, nil, false)
	if done, err := r.RunOnce(context.Background()); done || err != nil || st.calls != 0 || r.Counters.Get(CounterRetentionHoldsUnset) != 1 {
		t.Fatalf("removed positions without the holds: %v %v", done, err)
	}
}

// A retention below the floor is refused; a store that fails is the
// job's error and the rest still runs.
func TestRetentionFailures(t *testing.T) {
	pol := policy.Defaults()
	pol.TelemetryRetentionDays = 5
	st := &memRetention{days: 90}
	if _, err := retentionOf(st, &pol, nil, true).RunOnce(context.Background()); err == nil || len(st.sets) != 0 || st.calls != 1 {
		t.Fatalf("below the floor: %v %v", err, st.sets)
	}
	pol = policy.Defaults()
	for name, st := range map[string]*memRetention{
		"get":  {errGet: errDown},
		"set":  {days: 30, errSet: errDown},
		"null": {days: 90, errNull: errDown},
	} {
		if _, err := retentionOf(st, &pol, nil, true).RunOnce(context.Background()); !errors.Is(err, errDown) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Run applies at once, again soon while an input is missing, and stops
// with its context.
func TestRetentionRun(t *testing.T) {
	pol := policy.Defaults()
	st := &memRetention{days: 30}
	r := retentionOf(st, &pol, nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.Counters.Get(CounterRetentionSet) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if st.days != pol.TelemetryRetentionDays {
		t.Fatalf("not applied at start: %d", st.days)
	}
}
