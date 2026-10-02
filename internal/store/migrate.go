package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/rootxkit/uspace-ussp/migrations"
)

// Tree is one of the two migration trees (docs/PLAN.md D5). They are
// never merged: each has its own database, its own version table and its
// own owner process.
type Tree string

// The trees.
const (
	TreeRelational Tree = "relational"
	TreeTimeseries Tree = "timeseries"
)

// Trees lists both trees.
func Trees() []Tree { return []Tree{TreeRelational, TreeTimeseries} }

// ParseTree reads a tree name; anything but relational and timeseries
// is an error.
func ParseTree(s string) (Tree, error) {
	switch Tree(s) {
	case TreeRelational, TreeTimeseries:
		return Tree(s), nil
	}
	return "", fmt.Errorf("unknown migration tree %q: relational or timeseries", s)
}

// VersionTable is the goose version table of the tree.
func (t Tree) VersionTable() string { return "goose_db_version_" + string(t) }

// Command is the subcommand that migrates the tree.
func (t Tree) Command() string {
	if t == TreeTimeseries {
		return "ussp-tsdb-writer migrate"
	}
	return "ussp-api migrate"
}

// lockID is the session advisory lock goose holds while migrating the
// tree, so two migrate runs never interleave on one database.
func (t Tree) lockID() int64 {
	if t == TreeTimeseries {
		return 0x7573737074730002 // "ussp" "ts" 2
	}
	return 0x7573737072650001 // "ussp" "re" 1
}

func (t Tree) other() Tree {
	if t == TreeRelational {
		return TreeTimeseries
	}
	return TreeRelational
}

// Files is the tree's embedded SQL files.
func (t Tree) Files() (fs.FS, error) {
	switch t {
	case TreeRelational:
		return fs.Sub(migrations.Relational, string(t))
	case TreeTimeseries:
		return fs.Sub(migrations.Timeseries, string(t))
	}
	return nil, fmt.Errorf("unknown migration tree %q", t)
}

// Latest is the newest migration version embedded in the tree: the
// schema version this build needs.
func Latest(t Tree) (int64, error) {
	fsys, err := t.Files()
	if err != nil {
		return 0, err
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("%s: read embedded tree: %w", t, err)
	}
	var latest int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		digits, _, ok := strings.Cut(name, "_")
		v, err := strconv.ParseInt(digits, 10, 64)
		if !ok || err != nil || v <= 0 {
			return 0, fmt.Errorf("%s: migration %s does not start with a positive version", t, name)
		}
		latest = max(latest, v)
	}
	if latest == 0 {
		return 0, fmt.Errorf("%s: no migration embedded", t)
	}
	return latest, nil
}

// ErrWrongDatabase is returned when a tree is run against the database
// of the other tree (that tree's version table is there).
var ErrWrongDatabase = errors.New("this database holds the other migration tree")

// MigrationResult is one migration applied or rolled back.
type MigrationResult struct {
	Version  int64
	File     string
	Duration time.Duration
}

// MigrationStatus is one migration file and whether it is applied.
type MigrationStatus struct {
	Version   int64
	File      string
	Applied   bool
	AppliedAt time.Time // zero when pending
}

// provider is the goose provider of the tree on p, with the tree's
// version table and session advisory lock. It refuses a database that
// holds the other tree.
func (p *Pool) provider(ctx context.Context, t Tree) (*goose.Provider, func(), error) {
	var foreign bool
	if err := p.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", t.other().VersionTable()).Scan(&foreign); err != nil {
		return nil, nil, fmt.Errorf("migrate %s: %w", t, err)
	}
	if foreign {
		return nil, nil, fmt.Errorf("migrate %s: %w (%s exists)", t, ErrWrongDatabase, t.other().VersionTable())
	}
	fsys, err := t.Files()
	if err != nil {
		return nil, nil, err
	}
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(t.lockID()))
	if err != nil {
		return nil, nil, fmt.Errorf("migrate %s: %w", t, err)
	}
	db := stdlib.OpenDBFromPool(p.Pool)
	gp, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithTableName(t.VersionTable()),
		goose.WithDisableGlobalRegistry(true),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("migrate %s: %w", t, err)
	}
	// Closing the database/sql handle does not close the pool.
	return gp, func() { _ = db.Close() }, nil
}

func results(rs []*goose.MigrationResult) []MigrationResult {
	out := make([]MigrationResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, MigrationResult{Version: r.Source.Version, File: path.Base(r.Source.Path), Duration: r.Duration})
	}
	return out
}

// Migrate applies every pending migration of the tree, under the
// tree's advisory lock, and returns what it applied (empty when nothing
// was pending). Only the migrate subcommand calls it (D5, M36).
func (p *Pool) Migrate(ctx context.Context, t Tree) ([]MigrationResult, error) {
	gp, done, err := p.provider(ctx, t)
	if err != nil {
		return nil, err
	}
	defer done()
	rs, err := gp.Up(ctx)
	if err != nil {
		return results(rs), fmt.Errorf("migrate %s up: %w", t, err)
	}
	return results(rs), nil
}

// MigrateDown rolls the tree back to version (0 empties it) and returns
// what it rolled back, newest first. Only the migrate subcommand and the
// tests call it.
func (p *Pool) MigrateDown(ctx context.Context, t Tree, version int64) ([]MigrationResult, error) {
	if version < 0 {
		return nil, fmt.Errorf("migrate %s down: version %d is negative", t, version)
	}
	gp, done, err := p.provider(ctx, t)
	if err != nil {
		return nil, err
	}
	defer done()
	rs, err := gp.DownTo(ctx, version)
	if err != nil {
		return results(rs), fmt.Errorf("migrate %s down to %d: %w", t, version, err)
	}
	return results(rs), nil
}

// MigrationStatus lists every migration file of the tree and whether it
// is applied, oldest first.
func (p *Pool) MigrationStatus(ctx context.Context, t Tree) ([]MigrationStatus, error) {
	gp, done, err := p.provider(ctx, t)
	if err != nil {
		return nil, err
	}
	defer done()
	st, err := gp.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate %s status: %w", t, err)
	}
	out := make([]MigrationStatus, 0, len(st))
	for _, s := range st {
		out = append(out, MigrationStatus{
			Version:   s.Source.Version,
			File:      path.Base(s.Source.Path),
			Applied:   s.State == goose.StateApplied,
			AppliedAt: s.AppliedAt,
		})
	}
	return out, nil
}

// SchemaVersion is the highest version recorded in the tree's version
// table, 0 when the table does not exist (nothing migrated yet). It
// reads only; it never creates the table.
func (p *Pool) SchemaVersion(ctx context.Context, t Tree) (int64, error) {
	return schemaVersion(ctx, p, t)
}

// VersionError says that a database's schema is older than this build
// needs, naming both versions and the command that fixes it.
type VersionError struct {
	Tree Tree
	Have int64
	Want int64
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("%s schema is at version %d, this build needs %d: run %#q", e.Tree, e.Have, e.Want, e.Tree.Command())
}

// ErrSchemaUnreachable is returned by WaitForVersion when the database
// does not answer: the schema version is unknown, not lower. The caller
// starts degraded (B-08) and the readiness probe keeps checking it.
var ErrSchemaUnreachable = errors.New("database unreachable; schema version not checked")

// WaitPoll is how often WaitForVersion reads the version while it waits.
const WaitPoll = 500 * time.Millisecond

// WaitForVersion waits until the tree's schema on p is at version want
// or newer, re-reading every WaitPoll; it never migrates. When ctx ends
// first it returns a *VersionError naming the version found and the one
// wanted. A database that does not answer returns ErrSchemaUnreachable
// at once: there is nothing to wait for that this process can see.
func (p *Pool) WaitForVersion(ctx context.Context, t Tree, want int64) error {
	var have int64
	for {
		conn, err := p.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return &VersionError{Tree: t, Have: have, Want: want}
			}
			return fmt.Errorf("%s: %w: %s", t, ErrSchemaUnreachable, describe(err))
		}
		v, err := schemaVersion(ctx, conn, t)
		conn.Release()
		if err != nil {
			if ctx.Err() != nil {
				return &VersionError{Tree: t, Have: have, Want: want}
			}
			return err
		}
		have = v
		if have >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return &VersionError{Tree: t, Have: have, Want: want}
		case <-time.After(WaitPoll):
		}
	}
}

// querier is what SchemaVersion needs from a pool or a connection.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func schemaVersion(ctx context.Context, q querier, t Tree) (int64, error) {
	var exists bool
	if err := q.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", t.VersionTable()).Scan(&exists); err != nil {
		return 0, fmt.Errorf("%s schema version: %w", t, err)
	}
	if !exists {
		return 0, nil
	}
	var v int64
	sql := "SELECT COALESCE(max(version_id), 0) FROM " + pgx.Identifier{t.VersionTable()}.Sanitize() //nolint:misspell // pgx API name
	if err := q.QueryRow(ctx, sql).Scan(&v); err != nil {
		return 0, fmt.Errorf("%s schema version: %w", t, err)
	}
	return v, nil
}
