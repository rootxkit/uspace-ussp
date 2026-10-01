// Package proc is what the seven processes share: it opens the
// dependencies a process declares (lazily, so a process starts degraded
// rather than not at all; B-08), registers each in the obs.Health
// registry, serves /healthz, /readyz and /metrics with the process's own
// routes on one listener, writes the start line and the periodic status
// line, and drains on cancellation within USSP_SHUTDOWN_TIMEOUT_S.
//
// Main is the body of every cmd/<process>/main.go and MainAll the body
// of cmd/ussp-dev: argument handling, configuration (exit 2 naming the
// variable), tracing, and the exit code.
package proc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Need says whether a process uses a dependency and whether it is
// ready without it.
type Need int

// The three needs.
const (
	NotUsed  Need = iota
	Optional      // used; the process keeps working without it (degraded)
	Required      // the process is not ready without it
)

// Dependency names, as /readyz and ussp_dependency_* show them.
const (
	DepPostgres    = "postgres"
	DepTimescaleDB = "timescaledb"
	DepNATS        = "nats"
)

// Spec describes one process.
type Spec struct {
	Process     string
	Postgres    Need
	TimescaleDB Need
	NATS        Need
	// Migrate is true for the two processes with a migrate subcommand
	// (api: relational tree; tsdb-writer: time-series tree; D5).
	Migrate bool
	// Routes adds the process's own routes (nil until its work package).
	Routes func(mux *http.ServeMux, rt *Runtime)
}

// Runtime is what a running process's routes and workers share.
type Runtime struct {
	Process  string
	Config   config.Config
	Logger   *slog.Logger
	Registry *prometheus.Registry
	Health   *obs.Health
	Counters *core.Counters
	Status   *obs.Status
}

// Options are the parts of Run a test replaces.
type Options struct {
	// Out receives the JSON log; nil is os.Stdout.
	Out io.Writer
	// Listening, when set, is called with the bound address once the
	// listener is open.
	Listening func(addr string)
}

// Run runs the process described by spec with cfg until ctx ends, then
// drains. A dependency that is down never ends it.
func Run(ctx context.Context, cfg config.Config, spec Spec, opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	logger := obs.NewLogger(out, cfg.LogLevel, spec.Process)
	rt := &Runtime{
		Process:  spec.Process,
		Config:   cfg,
		Logger:   logger,
		Registry: obs.NewRegistry(spec.Process),
		Health:   obs.NewHealth(logger, time.Duration(cfg.ReadinessCheckTimeoutMS)*time.Millisecond),
		Counters: &core.Counters{},
	}
	rt.Registry.MustRegister(rt.Health, obs.NewCountersCollector("http", rt.Counters))
	rt.Status = &obs.Status{Logger: logger, Interval: time.Duration(cfg.StatusIntervalS) * time.Second, Health: rt.Health}
	rt.Status.Add("http", rt.Counters)

	closeDeps, err := openDependencies(rt, spec)
	if err != nil {
		return err
	}
	defer closeDeps()

	mux := http.NewServeMux()
	HealthRoutes(mux, rt.Health, rt.Registry)
	if spec.Routes != nil {
		spec.Routes(mux, rt)
	}
	mux.HandleFunc("/", httpx.NotFound)
	srv := httpx.NewServer(httpx.ServerOptions{
		Name: spec.Process, Addr: cfg.Addr(spec.Process), Logger: logger,
		Handler:           httpx.Baseline(mux, logger, httpx.BaselineDeps{Counters: rt.Counters, MaxBodyBytes: int64(cfg.MaxBodyBytes)}),
		ReadHeaderTimeout: time.Duration(cfg.ReadHeaderTimeoutS) * time.Second,
	})
	ln, err := srv.Listen()
	if err != nil {
		return err
	}
	deps := map[string]string{}
	for name, d := range rt.Health.Snapshot().Dependencies {
		deps[name] = "optional"
		if d.Required {
			deps[name] = "required"
		}
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "started",
		slog.String("version", obs.Version),
		slog.String("commit", obs.Commit),
		slog.String("addr", ln.Addr().String()),
		slog.Any("dependencies", deps),
		slog.String("config", cfg.Redacted()),
	)
	if opts.Listening != nil {
		opts.Listening(ln.Addr().String())
	}

	statusCtx, stopStatus := context.WithCancel(context.WithoutCancel(ctx))
	statusDone := make(chan struct{})
	go func() { defer close(statusDone); rt.Status.Run(statusCtx) }()

	drain := time.Duration(cfg.ShutdownTimeoutS) * time.Second
	err = srv.Serve(ctx, ln, drain)
	stopStatus()
	<-statusDone
	if ctx.Err() != nil {
		logger.LogAttrs(context.Background(), slog.LevelInfo, "shutting down", slog.Int("drain_timeout_s", cfg.ShutdownTimeoutS))
	}
	rt.Status.Emit(context.Background())
	if err != nil {
		return err
	}
	logger.Info("stopped")
	return nil
}

// openDependencies opens what spec declares and registers each in the
// health registry; the returned function closes them. Opening never
// waits for a server: only a malformed URL is an error.
func openDependencies(rt *Runtime, spec Spec) (func(), error) {
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	type db struct {
		name, url, extension string
		need                 Need
	}
	for _, d := range []db{
		{DepPostgres, rt.Config.PGURL, "postgis", spec.Postgres},
		{DepTimescaleDB, rt.Config.TSURL, "timescaledb", spec.TimescaleDB},
	} {
		if d.need == NotUsed {
			continue
		}
		pool, err := store.Open(d.url, 4)
		if err != nil {
			closeAll()
			return nil, errors.New(d.name + ": " + err.Error())
		}
		closers = append(closers, pool.Close)
		rt.Health.Register(d.name, d.need == Required, pool.Probe(d.extension))
	}
	if spec.NATS != NotUsed {
		nc, err := bus.Connect(rt.Config.NATSURL, rt.Config.NATSCreds, "ussp-"+spec.Process, rt.Logger)
		if err != nil {
			closeAll()
			return nil, errors.New("nats: " + err.Error())
		}
		closers = append(closers, nc.Close)
		rt.Health.Register(DepNATS, spec.NATS == Required, nc.Probe())
	}
	return closeAll, nil
}
