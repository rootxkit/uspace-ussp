package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TSRetention is the retention of the hypertables tsdb-writer owns
// (WP-15): the telemetry retention policy and the remote pilot's
// position on old telemetry. It runs as the time-series owner
// (ussp_tsdb); the TimescaleDB job functions are called as SQL, which
// sqlc cannot type.
type TSRetention struct{ Pool *Pool }

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
	tag, err := r.Pool.Exec(ctx, `UPDATE telemetry SET operator_position = NULL
		WHERE operator_position IS NOT NULL
		  AND captured_at < now() - make_interval(days => $1::integer)
		  AND NOT (flight_id = ANY($2::uuid[]))`, days, held)
	if err != nil {
		return 0, fmt.Errorf("null operator positions: %w", err)
	}
	return tag.RowsAffected(), nil
}
