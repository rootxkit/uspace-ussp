package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TSRetention is the retention of the hypertables tsdb-writer owns
// (WP-15): the telemetry retention policy and the remote pilot's
// position on old telemetry. It runs as the time-series owner
// (ussp_tsdb); the TimescaleDB job functions are called as SQL, which
// sqlc cannot type.
type TSRetention struct {
	Pool *Pool
	// Window is the span of captured_at one statement of
	// NullOperatorPositions changes (DefaultOperatorPositionWindow when
	// zero): one telemetry chunk, so no statement locks or rewrites the
	// whole history at once.
	Window time.Duration
}

// DefaultOperatorPositionWindow is telemetry's chunk interval
// (migrations/timeseries/00002).
const DefaultOperatorPositionWindow = 24 * time.Hour

// window is one [lo, hi) span of captured_at.
type window struct{ lo, hi time.Time }

// operatorPositionWindows cuts [from, cutoff) into consecutive windows
// of at most w (DefaultOperatorPositionWindow when w <= 0).
func operatorPositionWindows(from, cutoff time.Time, w time.Duration) []window {
	if w <= 0 {
		w = DefaultOperatorPositionWindow
	}
	var out []window
	for lo := from; lo.Before(cutoff); {
		hi := lo.Add(w)
		if hi.After(cutoff) {
			hi = cutoff
		}
		out = append(out, window{lo, hi})
		lo = hi
	}
	return out
}

// errNoTS is a TSRetention without the time-series pool.
var errNoTS = errors.New("timeseries: " + ErrNoPool.Error())

// TelemetryRetentionDays is the drop_after of telemetry's retention
// policy in whole days; 0 when it has none.
func (r TSRetention) TelemetryRetentionDays(ctx context.Context) (int, error) {
	if r.Pool == nil {
		return 0, errNoTS
	}
	var secs *float64
	err := r.Pool.QueryRow(ctx, `SELECT extract(epoch FROM (config ->> 'drop_after')::interval)::double precision
		FROM timescaledb_information.jobs
		WHERE proc_name = 'policy_retention' AND hypertable_name = 'telemetry'
		LIMIT 1`).Scan(&secs)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("telemetry retention policy: %w", err)
	}
	if secs == nil {
		return 0, nil
	}
	return int(*secs / 86400), nil
}

// SetTelemetryRetentionDays replaces telemetry's retention policy with
// one of days, in one transaction.
func (r TSRetention) SetTelemetryRetentionDays(ctx context.Context, days int) error {
	if r.Pool == nil {
		return errNoTS
	}
	if days < 1 {
		return fmt.Errorf("telemetry retention of %d days refused", days)
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT remove_retention_policy('telemetry', if_exists => true)`); err != nil {
		return fmt.Errorf("remove the telemetry retention policy: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT add_retention_policy('telemetry', make_interval(days => $1::integer))`, days); err != nil {
		return fmt.Errorf("add the telemetry retention policy: %w", err)
	}
	return tx.Commit(ctx)
}

// NullOperatorPositions removes the remote pilot's position from every
// telemetry row captured more than days ago (database clock), except
// the rows of the held flights, and returns how many rows it changed.
// It runs one statement, in its own transaction, per Window of
// captured_at from the oldest such row to the cutoff; a failure keeps
// what the earlier windows changed (the next run takes the rest).
func (r TSRetention) NullOperatorPositions(ctx context.Context, days int, held []string) (int64, error) {
	if r.Pool == nil {
		return 0, errNoTS
	}
	if days < 1 {
		return 0, fmt.Errorf("an operator position retention of %d days refused", days)
	}
	if held == nil {
		held = []string{}
	}
	var cutoff time.Time
	var from *time.Time
	if err := r.Pool.QueryRow(ctx, `SELECT c.cutoff,
		       (SELECT min(captured_at) FROM telemetry WHERE operator_position IS NOT NULL AND captured_at < c.cutoff)
		FROM (SELECT now() - make_interval(days => $1::integer) AS cutoff) c`, days).Scan(&cutoff, &from); err != nil {
		return 0, fmt.Errorf("operator positions past their retention: %w", err)
	}
	if from == nil {
		return 0, nil
	}
	var n int64
	for _, w := range operatorPositionWindows(*from, cutoff, r.Window) {
		tag, err := r.Pool.Exec(ctx, `UPDATE telemetry SET operator_position = NULL
			WHERE operator_position IS NOT NULL
			  AND captured_at >= $1 AND captured_at < $2
			  AND NOT (flight_id = ANY($3::uuid[]))`, w.lo, w.hi, held)
		if err != nil {
			return n, fmt.Errorf("null operator positions in [%s, %s): %w", w.lo.Format(time.RFC3339), w.hi.Format(time.RFC3339), err)
		}
		n += tag.RowsAffected()
	}
	return n, nil
}
