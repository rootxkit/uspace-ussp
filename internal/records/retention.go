package records

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Retention of the records (spec 05 §4, brief WP-15; the defaults of
// the policy row, owner question Q18):
//
//   - telemetry: 90 days online (telemetry_retention_days, floor 30
//     days, Art. 15(1)(g)), a TimescaleDB retention policy that
//     tsdb-writer sets from the policy row at start and whenever the
//     row changes;
//   - the remote pilot's position on telemetry: removed after
//     operator_position_retention_days unless an occurrence report
//     holds the flight (the record_holds projection);
//   - peer data: 24 h, WP-13's purge and peer_flights' retention policy;
//   - the gap records api keeps: as long as the telemetry they explain;
//   - alerts, intents and conformance states: kept 5 years
//     (record_retention_days); no job removes them yet, a recorded
//     decision (docs/RUNBOOKS/WP-15.md), since nothing here is older.
//
// Every job logs what it removed and counts it.

// TSRetentionStore is the retention of the hypertables (store.TSRetention).
type TSRetentionStore interface {
	TelemetryRetentionDays(ctx context.Context) (int, error)
	SetTelemetryRetentionDays(ctx context.Context, days int) error
	NullOperatorPositions(ctx context.Context, days int, held []string) (int64, error)
}

// Counters of the Retention job.
const (
	CounterRetentionSet        = "retention_telemetry_policy_set"
	CounterPositionsNulled     = "retention_operator_positions_removed"
	CounterRetentionFailed     = "retention_failed"
	CounterRetentionHoldsUnset = "retention_positions_skipped_holds_unknown"
	CounterRetentionPolicyWait = "retention_skipped_policy_unknown"
)

// HoldEvery is how often tsdb-writer's retention runs, and PendingEvery
// how soon it runs again while the policy row or the holds are not read
// yet (the policy is applied at start, not an hour later).
const (
	HoldEvery    = time.Hour
	PendingEvery = 10 * time.Second
)

// Hold is one record_holds value: a flight whose records an occurrence
// report holds, why, and since when.
type Hold struct {
	FlightID string    `json:"flight_id"`
	Reasons  []string  `json:"reasons"`
	Since    time.Time `json:"since"`
}

var flightIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Retention is tsdb-writer's retention job.
type Retention struct {
	Store TSRetentionStore
	// Policy is the policy row as the policy bucket gives it; false
	// while none is read (nothing is changed on defaults: the migration's
	// retention stands and no position is removed).
	Policy func() (policy.Values, bool)
	// Holds are the flight ids record_holds holds; false while the
	// bucket is not read (no position is removed: a held flight's could
	// be).
	Holds    func() ([]string, bool)
	Counters *core.Counters
	Logger   *slog.Logger
}

func (r *Retention) logger() *slog.Logger {
	if r.Logger == nil {
		return obs.Discard()
	}
	return r.Logger
}

func (r *Retention) add(name string, n uint64) {
	if r.Counters != nil && n > 0 {
		r.Counters.Add(name, n)
	}
}

// Run runs the job every every (PendingEvery while an input is not read
// yet) until ctx ends.
func (r *Retention) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = HoldEvery
	}
	for {
		done, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.add(CounterRetentionFailed, 1)
			r.logger().LogAttrs(ctx, slog.LevelWarn, "retention not applied; tried again", obs.Err(err))
		}
		next := every
		if !done || err != nil {
			next = min(every, PendingEvery)
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// RunOnce sets telemetry's retention policy from the policy row when it
// differs, and removes the remote pilot's position from the telemetry
// past its retention, except the held flights'. done is false while the
// policy row or the holds are not read (what depends on them waited).
// The error is the first failure; the rest still runs.
func (r *Retention) RunOnce(ctx context.Context) (done bool, err error) {
	pol, ok := r.Policy()
	if !ok {
		r.add(CounterRetentionPolicyWait, 1)
		r.logger().LogAttrs(ctx, slog.LevelWarn, "retention waits for the policy row: the migration's telemetry retention stands and no position is removed")
		return false, nil
	}
	var errs []error
	if err := r.telemetry(ctx, pol.TelemetryRetentionDays); err != nil {
		errs = append(errs, err)
	}
	held, err := r.positions(ctx, pol.OperatorPositionRetentionDays)
	if err != nil {
		errs = append(errs, err)
	}
	return held, errors.Join(errs...)
}

func (r *Retention) telemetry(ctx context.Context, days int) error {
	if days < policy.TelemetryRetentionFloorDays {
		return errors.New("telemetry_retention_days below the floor: refused")
	}
	cur, err := r.Store.TelemetryRetentionDays(ctx)
	if err != nil {
		return err
	}
	if cur == days {
		return nil
	}
	if err := r.Store.SetTelemetryRetentionDays(ctx, days); err != nil {
		return err
	}
	r.add(CounterRetentionSet, 1)
	r.logger().LogAttrs(ctx, slog.LevelInfo, "retention: telemetry retention policy set from the policy row",
		slog.Int("from_days", cur), slog.Int("to_days", days))
	return nil
}

// positions removes the positions; false (nothing removed) while the
// holds are not read.
func (r *Retention) positions(ctx context.Context, days int) (bool, error) {
	held, ok := r.Holds()
	if !ok {
		r.add(CounterRetentionHoldsUnset, 1)
		r.logger().LogAttrs(ctx, slog.LevelWarn, "record_holds not read: no remote pilot position is removed until it is")
		return false, nil
	}
	ids := make([]string, 0, len(held))
	for _, id := range held {
		if flightIDRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	n, err := r.Store.NullOperatorPositions(ctx, days, slices.Compact(ids))
	if err != nil {
		return true, err
	}
	r.add(CounterPositionsNulled, uint64(max(n, 0)))
	r.logger().LogAttrs(ctx, slog.LevelInfo, "retention: remote pilot positions removed from old telemetry",
		slog.Int64("rows", n), slog.Int("older_than_days", days), slog.Int("held_flights", len(ids)))
	return true, nil
}
