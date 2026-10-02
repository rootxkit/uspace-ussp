package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// AppRole is the role the api process works as on the relational
// database (deploy/compose/initdb/10-ussp.sql creates it and grants it
// to the login role; the migrations grant it table by table). It has no
// UPDATE or DELETE on events, intent_versions and conformance_states.
const AppRole = "ussp_app"

// Pool is a lazily connected pgx pool: opening it never contacts the
// server, so a process starts while its database is down and reports
// that on /readyz (B-08) instead of exiting.
type Pool struct {
	*pgxpool.Pool
}

// PoolOptions configure OpenPool.
type PoolOptions struct {
	// URL is the postgres:// URL (a secret: it may carry a password).
	URL string
	// MaxConns bounds the pool; <= 0 keeps pgxpool's default.
	MaxConns int32
	// Role is SET on every new connection; empty keeps the login role
	// (the migrate subcommand, which runs as the owner).
	Role string
	// ApplicationName names the process in pg_stat_activity.
	ApplicationName string
}

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// OpenPool parses the options and returns a pool. Only a malformed URL
// or role name is an error.
func OpenPool(o PoolOptions) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(o.URL)
	if err != nil {
		// The parse error may echo the URL, credentials included.
		return nil, errors.New("database URL does not parse")
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	cfg.MinConns = 0
	if o.ApplicationName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = o.ApplicationName
	}
	if o.Role != "" {
		if !roleName.MatchString(o.Role) {
			return nil, fmt.Errorf("role %q is not a lower-case role name", o.Role)
		}
		set := "SET ROLE " + pgx.Identifier{o.Role}.Sanitize() //nolint:misspell // pgx API name
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			if _, err := c.Exec(ctx, set); err != nil {
				return fmt.Errorf("set role %s: %w", o.Role, err)
			}
			return nil
		}
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("database pool: %w", err)
	}
	return &Pool{Pool: p}, nil
}

// Probe returns the readiness check of the pool: down when the server
// does not answer, degraded when it answers without the extension the
// database is for (postgis for the relational one, timescaledb for the
// time series; the bootstrap script creates both) or with a schema older
// than this build needs, up otherwise. A schema it cannot judge is said
// so, never reported up (CLAUDE.md rule 7).
func (p *Pool) Probe(extension string, tree Tree) obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		var version string
		err := p.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname = $1", extension).Scan(&version)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return obs.StateDegraded, "connected; extension " + extension + " is not installed"
		case err != nil:
			return obs.StateDown, describe(err)
		}
		want, err := Latest(tree)
		if err != nil {
			return obs.StateDegraded, err.Error()
		}
		have, err := p.SchemaVersion(ctx, tree)
		if err != nil {
			return obs.StateDegraded, "connected; schema version unknown: " + describe(err)
		}
		if have < want {
			return obs.StateDegraded, (&VersionError{Tree: tree, Have: have, Want: want}).Error()
		}
		return obs.StateUp, ""
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
