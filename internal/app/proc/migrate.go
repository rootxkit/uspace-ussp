package proc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// migrateTree is the tree a process owns and the variable naming its
// database: api the relational tree (USSP_PG_URL), tsdb-writer the
// time-series tree (USSP_TS_URL).
func migrateTree(process string, cfg config.Config) (store.Tree, string, string) {
	if process == config.ProcessTSDBWriter {
		return store.TreeTimeseries, "USSP_TS_URL", cfg.TSURL
	}
	return store.TreeRelational, "USSP_PG_URL", cfg.PGURL
}

// migrate is the migrate subcommand, the only code path that runs a
// migration (D5, reconciliation M36; a test greps for it):
//
//	migrate | migrate up     apply every pending migration
//	migrate down             roll back the newest migration
//	migrate down <version>   roll back to version (0 empties the tree)
//	migrate status           list every migration and whether it is applied
//
// It connects as the login role (the database owner), holds the tree's
// advisory lock while it changes anything, and refuses a database that
// holds the other tree. Every step and the final version are logged.
func migrate(ctx context.Context, spec Spec, args []string, stdout, stderr io.Writer, lookup config.LookupFunc) int {
	cfg, err := config.LoadFrom(lookup)
	if err != nil {
		return configError(stderr, spec.Process, err)
	}
	tree, variable, url := migrateTree(spec.Process, cfg)
	if url == "" {
		return configError(stderr, spec.Process, &core.FieldError{Field: variable, Reason: "required by migrate"})
	}
	action, target, err := parseMigrateArgs(args)
	if err != nil {
		obs.NewLogger(stderr, "info", spec.Process).Error("unknown argument",
			slog.String("error", err.Error()), slog.String("hint", "--help lists the usage"))
		return ExitConfig
	}
	logger := obs.NewLogger(stdout, cfg.LogLevel, spec.Process).With(
		slog.String("tree", string(tree)), slog.String("version_table", tree.VersionTable()))
	pool, err := store.OpenPool(store.PoolOptions{URL: url, MaxConns: 2, ApplicationName: "ussp-" + spec.Process + "-migrate"})
	if err != nil {
		return configError(stderr, spec.Process, &core.FieldError{Field: variable, Reason: err.Error()})
	}
	defer pool.Close()

	if err := runMigrate(ctx, logger, pool, tree, action, target); err != nil {
		obs.Error(ctx, logger, "migrate failed", err, slog.String("action", action))
		return ExitFailed
	}
	return ExitOK
}

func parseMigrateArgs(args []string) (string, int64, error) {
	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "up"):
		return "up", 0, nil
	case len(args) == 1 && args[0] == "status":
		return "status", 0, nil
	case len(args) == 1 && args[0] == "down":
		return "down", -1, nil
	case len(args) == 2 && args[0] == "down":
		v, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || v < 0 {
			return "", 0, fmt.Errorf("migrate down %q: the version is a whole number of at least 0", args[1])
		}
		return "down", v, nil
	}
	return "", 0, fmt.Errorf("migrate %v: expected up, down [version] or status", args)
}

func runMigrate(ctx context.Context, logger *slog.Logger, pool *store.Pool, tree store.Tree, action string, target int64) error {
	latest, err := store.Latest(tree)
	if err != nil {
		return err
	}
	from, err := pool.SchemaVersion(ctx, tree)
	if err != nil {
		return err
	}
	var rs []store.MigrationResult
	switch action {
	case "status":
		st, err := pool.MigrationStatus(ctx, tree)
		if err != nil {
			return err
		}
		pending := 0
		for _, s := range st {
			attrs := []slog.Attr{slog.Int64("version", s.Version), slog.String("file", s.File), slog.Bool("applied", s.Applied)}
			if s.Applied {
				attrs = append(attrs, slog.Time("applied_at", s.AppliedAt))
			} else {
				pending++
			}
			logger.LogAttrs(ctx, slog.LevelInfo, "migration", attrs...)
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "migrate status",
			slog.Int64("version", from), slog.Int64("latest", latest), slog.Int("pending", pending))
		return nil
	case "up":
		rs, err = pool.Migrate(ctx, tree)
	case "down":
		if target < 0 {
			// One step back is to the previous applied version, which
			// need not be from-1.
			st, serr := pool.MigrationStatus(ctx, tree)
			if serr != nil {
				return serr
			}
			target = 0
			for _, s := range st {
				if s.Applied && s.Version < from {
					target = max(target, s.Version)
				}
			}
		}
		rs, err = pool.MigrateDown(ctx, tree, target)
	}
	msg := "migration rolled back"
	if action == "up" {
		msg = "migration applied"
	}
	for _, r := range rs {
		logger.LogAttrs(ctx, slog.LevelInfo, msg,
			slog.Int64("version", r.Version), slog.String("file", r.File), slog.Int64("duration_ms", r.Duration.Milliseconds()))
	}
	if err != nil {
		return err
	}
	to, err := pool.SchemaVersion(ctx, tree)
	if err != nil {
		return err
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "migrate done", slog.String("action", action),
		slog.Int64("from", from), slog.Int64("to", to), slog.Int64("latest", latest), slog.Int("changed", len(rs)))
	return nil
}
