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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
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
	// Commands are the process's own subcommands beside healthcheck and
	// migrate (api: staff-add), by name.
	Commands map[string]Command
	// Routes adds the process's operations (nil until its work
	// package). A process with Routes serves its health operations
	// itself (HealthHandlers, through the generated router); Run adds
	// only /metrics. ctx ends after the server has drained: background
	// work started with rt.Go stops with it. An error refuses the start.
	Routes func(ctx context.Context, mux *http.ServeMux, rt *Runtime) error
}

// Command is a subcommand: it gets the loaded configuration, the
// arguments after its name and the standard streams, and returns the
// exit code.
type Command func(ctx context.Context, cfg config.Config, args []string, stdin io.Reader, stdout, stderr io.Writer) int

// Runtime is what a running process's routes and workers share.
type Runtime struct {
	Process  string
	Config   config.Config
	Logger   *slog.Logger
	Registry *prometheus.Registry
	Health   *obs.Health
	Counters *core.Counters
	Status   *obs.Status
	// Store holds the pools this process opened (Rel for api only, TS
	// for api and tsdb-writer); nil fields are databases it does not use.
	Store *store.Store
	// Bus is the NATS connection (nil for a process without NATS). The
	// topology of docs/PLAN.md §7 is kept in place in the background and
	// its drift is on /readyz under nats.
	Bus *bus.Conn

	topology *bus.Maintainer

	work sync.WaitGroup
}

// Go runs fn in the background with the process's work context; Run
// waits for it after the server has drained.
func (rt *Runtime) Go(ctx context.Context, fn func(context.Context)) {
	rt.work.Go(func() { fn(ctx) })
}

// topologyCheckTimeout bounds one background check of the streams and
// buckets.
const topologyCheckTimeout = 10 * time.Second

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
		Store:    &store.Store{},
	}
	rt.Registry.MustRegister(rt.Health, obs.NewCountersCollector("http", rt.Counters))
	rt.Status = &obs.Status{Logger: logger, Interval: time.Duration(cfg.StatusIntervalS) * time.Second, Health: rt.Health}
	rt.Status.Add("http", rt.Counters)

	pools, closeDeps, err := openDependencies(rt, spec)
	if err != nil {
		return err
	}
	defer closeDeps()
	if err := waitForSchemas(ctx, rt, pools); err != nil {
		if ctx.Err() != nil {
			return nil // stopped while waiting: nothing started, nothing to drain
		}
		return err
	}

	workCtx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer func() { stopWork(); rt.work.Wait() }()
	if rt.topology != nil {
		// captured_trk/man/peer beside every process's published_trk
		// (audit B1, N6): a core publish cannot see a capture refused.
		Publish(rt, "bus_capture", rt.topology.Counters())
		rt.Go(workCtx, func(ctx context.Context) { rt.topology.Run(ctx, topologyCheckTimeout) })
	}
	mux := http.NewServeMux()
	if spec.Routes != nil {
		MetricsRoute(mux, rt.Registry)
		if err := spec.Routes(workCtx, mux, rt); err != nil {
			return fmt.Errorf("refusing to start: %w", err)
		}
	} else {
		HealthRoutes(mux, rt.Health, rt.Registry)
	}
	mux.HandleFunc("/", httpx.NotFound)
	proxies, err := httpx.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return fmt.Errorf("USSP_TRUSTED_PROXIES: %w", err)
	}
	watch := &httpx.ProxyWatch{Counters: rt.Counters, Logger: logger}
	if spec.Process == config.ProcessAPI {
		// The sign-in limits and the audit rows are api's (audit S7).
		rt.Health.Register(DepClientAddress, false, ClientAddressProbe(watch, time.Now))
	}
	srv := httpx.NewServer(httpx.ServerOptions{
		Name: spec.Process, Addr: cfg.Addr(spec.Process), Logger: logger,
		Handler: httpx.Baseline(mux, logger, httpx.BaselineDeps{Counters: rt.Counters, MaxBodyBytes: int64(cfg.MaxBodyBytes),
			TrustedProxies: proxies, ProxyWatch: watch}),
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

// schemaPool is an opened database and the migration tree it holds.
type schemaPool struct {
	pool *store.Pool
	tree store.Tree
}

// openDependencies opens what spec declares and registers each in the
// health registry; the returned function closes them. Opening never
// waits for a server: only a malformed URL is an error. The relational
// pool works as store.AppRole (the grants of the migrations); the
// time-series pool as its login role.
func openDependencies(rt *Runtime, spec Spec) ([]schemaPool, func(), error) {
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	type db struct {
		name, url, extension, role string
		tree                       store.Tree
		need                       Need
	}
	var pools []schemaPool
	for _, d := range []db{
		{DepPostgres, rt.Config.PGURL, "postgis", store.AppRole, store.TreeRelational, spec.Postgres},
		{DepTimescaleDB, rt.Config.TSURL, "timescaledb", "", store.TreeTimeseries, spec.TimescaleDB},
	} {
		if d.need == NotUsed {
			continue
		}
		pool, err := store.OpenPool(store.PoolOptions{URL: d.url, MaxConns: 4, Role: d.role, ApplicationName: "ussp-" + spec.Process})
		if err != nil {
			closeAll()
			return nil, nil, errors.New(d.name + ": " + err.Error())
		}
		closers = append(closers, pool.Close)
		rt.Health.Register(d.name, d.need == Required, pool.Probe(d.extension, d.tree))
		pools = append(pools, schemaPool{pool: pool, tree: d.tree})
		if d.tree == store.TreeRelational {
			rt.Store.Rel = pool
		} else {
			rt.Store.TS = pool
		}
	}
	if spec.NATS != NotUsed {
		nc, err := bus.Connect(rt.Config.NATSURL, rt.Config.NATSCreds, "ussp-"+spec.Process, rt.Logger)
		if err != nil {
			closeAll()
			return nil, nil, errors.New("nats: " + err.Error())
		}
		closers = append(closers, nc.Close)
		rt.Bus = nc
		rt.topology = nc.Maintain(TopologyOf(rt.Config), rt.Logger)
		rt.Health.Register(DepNATS, spec.NATS == Required, nc.Probe())
	}
	return pools, closeAll, nil
}

// waitForSchemas is the start-up check of D5: a process never migrates;
// it waits up to USSP_SCHEMA_WAIT_S for each database it opens to reach
// the version this build needs (the migrate subcommand brings it there)
// and refuses to start on a lower one, naming both versions. A database
// that does not answer is not a lower version: the process starts
// degraded (B-08) and its readiness probe reports the schema once the
// database answers.
func waitForSchemas(ctx context.Context, rt *Runtime, pools []schemaPool) error {
	for _, sp := range pools {
		want, err := store.Latest(sp.tree)
		if err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, time.Duration(rt.Config.SchemaWaitS)*time.Second)
		err = sp.pool.WaitForVersion(wctx, sp.tree, want)
		cancel()
		switch {
		case err == nil:
			rt.Logger.LogAttrs(ctx, slog.LevelInfo, "schema", slog.String("tree", string(sp.tree)), slog.Int64("need", want), slog.String("state", "ready"))
		case errors.Is(err, store.ErrSchemaUnreachable):
			rt.Logger.LogAttrs(ctx, slog.LevelWarn, "schema version not checked", slog.String("tree", string(sp.tree)),
				slog.Int64("need", want), slog.String("reason", err.Error()))
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			return fmt.Errorf("refusing to start: %w", err)
		}
	}
	return nil
}

// TopologyOf is the bus topology with the bounds cfg configures.
func TopologyOf(cfg config.Config) bus.Topology {
	return bus.TopologyWith(bus.TopologyOptions{
		Streams: map[string]bus.StreamBounds{
			bus.StreamCONF:    {MaxAge: secs(cfg.ConfStreamMaxAgeS), MaxBytes: int64(cfg.ConfStreamMaxBytes)},
			bus.StreamTRK:     {MaxAge: secs(cfg.TRKStreamMaxAgeS), MaxBytes: int64(cfg.TRKStreamMaxBytes)},
			bus.StreamMAN:     {MaxAge: secs(cfg.MANStreamMaxAgeS), MaxBytes: int64(cfg.MANStreamMaxBytes)},
			bus.StreamPEER:    {MaxAge: secs(cfg.PEERStreamMaxAgeS), MaxBytes: int64(cfg.PEERStreamMaxBytes)},
			bus.StreamALRT:    {MaxAge: secs(cfg.ALRTStreamMaxAgeS), MaxBytes: int64(cfg.ALRTStreamMaxBytes)},
			bus.StreamIDENT:   {MaxAge: secs(cfg.IDENTStreamMaxAgeS), MaxBytes: int64(cfg.IDENTStreamMaxBytes)},
			bus.StreamINTENT:  {MaxAge: secs(cfg.INTENTStreamMaxAgeS), MaxBytes: int64(cfg.INTENTStreamMaxBytes)},
			bus.StreamCIS:     {MaxAge: secs(cfg.CISStreamMaxAgeS), MaxBytes: int64(cfg.CISStreamMaxBytes)},
			bus.StreamTRAFFIC: {MaxAge: secs(cfg.TRAFFICStreamMaxAgeS), MaxBytes: int64(cfg.TRAFFICStreamMaxBytes)},
			bus.StreamINGEST:  {MaxAge: secs(cfg.INGESTStreamMaxAgeS), MaxBytes: int64(cfg.INGESTStreamMaxBytes)},
			bus.StreamFLIGHT:  {MaxAge: secs(cfg.FLIGHTStreamMaxAgeS), MaxBytes: int64(cfg.FLIGHTStreamMaxBytes)},
		},
		BucketMaxBytes: map[string]int64{
			bus.BucketCISCurrent:       int64(cfg.CISCurrentBucketMaxBytes),
			bus.BucketPolicy:           int64(cfg.PolicyBucketMaxBytes),
			bus.BucketSourceControl:    int64(cfg.SourceControlBucketMaxBytes),
			bus.BucketRegistryValidity: int64(cfg.RegistryValidityBucketMaxBytes),
			bus.BucketClientBindings:   int64(cfg.ClientBindingsBucketMaxBytes),
			bus.BucketIntentActive:     int64(cfg.IntentActiveBucketMaxBytes),
			bus.BucketTelemetrySeen:    int64(cfg.TelemetrySeenBucketMaxBytes),
			bus.BucketISANotifications: int64(cfg.ISANotificationsBucketMaxBytes),
			bus.BucketConformanceState: int64(cfg.ConformanceStateBucketMaxBytes),
			bus.BucketProximityState:   int64(cfg.ProximityStateBucketMaxBytes),
			bus.BucketSessionsLive:     int64(cfg.SessionsLiveBucketMaxBytes),
			bus.BucketRecordHolds:      int64(cfg.RecordHoldsBucketMaxBytes),
			bus.BucketRIDSubscriptions: int64(cfg.RIDSubscriptionsBucketMaxBytes),
			bus.BucketMonitorStatus:    int64(cfg.MonitorStatusBucketMaxBytes),
			bus.BucketFlightBinding:    int64(cfg.FlightBindingBucketMaxBytes),
		},
	})
}

func secs(n int) time.Duration {
	return time.Duration(n) * time.Second
}

// DepClientAddress is api's readiness entry of the client address
// behind the reverse proxy (audit S7).
const DepClientAddress = "client_address"

// clientAddressRecent is how long an X-Forwarded-For from an untrusted
// peer keeps client_address degraded.
const clientAddressRecent = 10 * time.Minute

// ClientAddressProbe is degraded while a peer that is not a trusted
// proxy sent X-Forwarded-For within the last ten minutes: behind a
// reverse proxy with USSP_TRUSTED_PROXIES unset every client is keyed on
// the proxy, so 31 sign-ins a minute from anyone lock every user out and
// every audit row names the proxy. Up otherwise.
func ClientAddressProbe(w *httpx.ProxyWatch, now func() time.Time) obs.Probe {
	return func(context.Context) (obs.State, string) {
		peer, at, n := w.Last()
		if at.IsZero() || now().Sub(at) > clientAddressRecent {
			return obs.StateUp, ""
		}
		return obs.StateDegraded, fmt.Sprintf("%d requests since start carried X-Forwarded-For from %s, which is not a trusted proxy "+
			"(last %.0f s ago): every client behind it shares its address for rate limits and audit rows; set USSP_TRUSTED_PROXIES",
			n, peer, max(now().Sub(at).Seconds(), 0))
	}
}
