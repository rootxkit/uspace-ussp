// Package store opens the two databases: the relational one
// (PostgreSQL + PostGIS, written only by api) and the time-series one
// (TimescaleDB, written only by tsdb-writer). WP-0 holds the pools and
// their readiness probes; the sqlc queries, transactions and the outbox
// arrive with WP-1.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Pool is a lazily connected pgx pool: opening it never contacts the
// server, so a process starts while its database is down and reports
// that on /readyz (B-08) instead of exiting.
type Pool struct {
	*pgxpool.Pool
}

// Open parses url and returns a pool of at most maxConns connections.
// Only a malformed URL is an error.
func Open(url string, maxConns int32) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The parse error may echo the URL, credentials included.
		return nil, errors.New("database URL does not parse")
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("database pool: %w", err)
	}
	return &Pool{Pool: p}, nil
}

// Probe returns the readiness check of the pool: down when the server
// does not answer, degraded when it answers without the extension the
// database is for (postgis for the relational one, timescaledb for the
// time series; the bootstrap script creates both), up otherwise.
func (p *Pool) Probe(extension string) obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		var version string
		err := p.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname = $1", extension).Scan(&version)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return obs.StateDegraded, "connected; extension " + extension + " is not installed"
		case err != nil:
			return obs.StateDown, describe(err)
		default:
			return obs.StateUp, ""
		}
	}
}

// describe names a failure. pgconn's errors name the host, user and
// database, never the password.
func describe(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer before the check deadline"
	}
	return err.Error()
}
