package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/store/relational"
	"github.com/rootxkit/uspace-ussp/internal/store/timeseries"
)

// Config opens a Store. An empty URL leaves that pool nil: a hot-path
// process opens neither database, tsdb-writer only the time series.
type Config struct {
	// RelURL is the relational database (USSP_PG_URL).
	RelURL string
	// TSURL is the time-series database (USSP_TS_URL).
	TSURL string
	// RelRole is SET on every relational connection: AppRole for the api
	// process, empty for the migrate subcommand (the owner).
	RelRole string
	// MaxConns bounds each pool; <= 0 keeps pgxpool's default.
	MaxConns int32
	// ApplicationName names the process in pg_stat_activity.
	ApplicationName string
}

// Store holds the two pools. Neither is connected until first use.
type Store struct {
	Rel *Pool
	TS  *Pool
}

// Open builds the pools of cfg without connecting (B-08: a database
// that is down is reported on /readyz, not at start).
func Open(_ context.Context, cfg Config) (*Store, error) {
	s := &Store{}
	if cfg.RelURL != "" {
		p, err := OpenPool(PoolOptions{URL: cfg.RelURL, MaxConns: cfg.MaxConns, Role: cfg.RelRole, ApplicationName: cfg.ApplicationName})
		if err != nil {
			return nil, fmt.Errorf("relational: %w", err)
		}
		s.Rel = p
	}
	if cfg.TSURL != "" {
		p, err := OpenPool(PoolOptions{URL: cfg.TSURL, MaxConns: cfg.MaxConns, ApplicationName: cfg.ApplicationName})
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("timeseries: %w", err)
		}
		s.TS = p
	}
	return s, nil
}

// Close closes both pools.
func (s *Store) Close() {
	if s.Rel != nil {
		s.Rel.Close()
	}
	if s.TS != nil {
		s.TS.Close()
	}
}

// ErrNoPool is returned for a tree whose database the Store did not open.
var ErrNoPool = errors.New("database not configured for this process")

// Pool is the pool of the tree's database.
func (s *Store) Pool(t Tree) (*Pool, error) {
	p := s.Rel
	if t == TreeTimeseries {
		p = s.TS
	}
	if p == nil {
		return nil, fmt.Errorf("%s: %w", t, ErrNoPool)
	}
	return p, nil
}

// Migrate applies the pending migrations of the tree (Pool.Migrate).
func (s *Store) Migrate(ctx context.Context, t Tree) ([]MigrationResult, error) {
	p, err := s.Pool(t)
	if err != nil {
		return nil, err
	}
	return p.Migrate(ctx, t)
}

// WaitForVersion waits for the tree's schema (Pool.WaitForVersion).
func (s *Store) WaitForVersion(ctx context.Context, t Tree, want int64) error {
	p, err := s.Pool(t)
	if err != nil {
		return err
	}
	return p.WaitForVersion(ctx, t, want)
}

// Queries are the relational queries outside a transaction.
func (s *Store) Queries() *relational.Queries { return relational.New(s.Rel) }

// TSQueries are the time-series queries.
func (s *Store) TSQueries() *timeseries.Queries { return timeseries.New(s.TS) }

// Tx runs fn in one relational transaction: committed when fn returns
// nil, rolled back otherwise (and when ctx ends). Every mutating path
// writes its events row through Audit with the same q.
func (s *Store) Tx(ctx context.Context, fn func(q *relational.Queries) error) error {
	if s.Rel == nil {
		return fmt.Errorf("%s: %w", TreeRelational, ErrNoPool)
	}
	tx, err := s.Rel.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(relational.New(tx)); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// IsNoRows reports whether err is "no rows in result set".
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// SQLState is the PostgreSQL error code of err, or "".
func SQLState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// StateInsufficientPrivilege is the SQLSTATE of a refused grant (42501).
const StateInsufficientPrivilege = "42501"

// UUID parses s (the canonical text form) into the database's uuid; a
// malformed s is a *core.FieldError on field.
func UUID(field, s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if len(s) != 36 || u.Scan(s) != nil {
		return pgtype.UUID{}, &core.FieldError{Field: field, Reason: "not a UUID"}
	}
	return u, nil
}

// UUIDText is the canonical text form of u, or "" when it is NULL.
func UUIDText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}

// StateUniqueViolation is the SQLSTATE of a unique constraint (23505).
const StateUniqueViolation = "23505"

// Date is the database date of t's UTC day.
func Date(t time.Time) pgtype.Date {
	y, m, d := t.UTC().Date()
	return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
}

// DateTime is midnight UTC of d (the zero time when d is NULL).
func DateTime(d pgtype.Date) time.Time {
	if !d.Valid {
		return time.Time{}
	}
	y, m, dd := d.Time.Date()
	return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)
}
