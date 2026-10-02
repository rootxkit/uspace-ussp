// Package migrations embeds the two goose migration trees (docs/PLAN.md
// D5, §5.3; CLAUDE.md rule 10):
//
//	relational/  PostgreSQL 16 + PostGIS 3.4, database ussp_relational,
//	             version table goose_db_version_relational; applied by
//	             `ussp-api migrate`.
//	timeseries/  TimescaleDB, database ussp_timeseries, version table
//	             goose_db_version_timeseries; applied by
//	             `ussp-tsdb-writer migrate`.
//
// The trees are never merged. internal/store runs them, and only the
// migrate subcommand calls it.
package migrations

import "embed"

// Relational is the relational tree (relational/*.sql).
//
//go:embed relational/*.sql
var Relational embed.FS

// Timeseries is the time-series tree (timeseries/*.sql).
//
//go:embed timeseries/*.sql
var Timeseries embed.FS
