// Package store opens and migrates the two databases and is the only
// package that talks to them (depguard):
//
//   - relational: PostgreSQL + PostGIS, written only by api, which works
//     as the application role ussp_app (AppRole);
//   - time series: TimescaleDB, written only by tsdb-writer; api reads it
//     for records.
//
// It holds the pgx pools (Open), the two embedded goose trees (Migrate,
// MigrateDown, MigrationStatus, behind an advisory lock, run only by the
// migrate subcommand), the start-up check that refuses an older schema
// (WaitForVersion), the sqlc-generated queries of each tree
// (internal/store/relational, internal/store/timeseries), the
// transaction helper (Tx), the DSS outbox (Enqueue, Outbox) and the
// audit writer (Audit), the only writer of events.
package store
