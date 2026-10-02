package proc

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Exit codes of Main and MainAll.
const (
	ExitOK     = 0
	ExitFailed = 1 // a runtime failure, a failed health check, or a drain that overran its bound
	ExitConfig = 2 // the configuration is invalid or incomplete; the variable is named
)

// Main is the body of cmd/<process>/main.go. args are the arguments
// after the program name:
//
//	(none)                run the process
//	--help                list the variables the process reads
//	healthcheck [path]    GET path (default /healthz) on the local
//	                      listener; exit 0 on 200 (the container health
//	                      check of a distroless image, which has no curl)
//	migrate [up | down [version] | status]
//	                      api (relational tree) and tsdb-writer (time-series
//	                      tree) only: the only code path that migrates (D5)
func Main(ctx context.Context, spec Spec, args []string, stdout, stderr io.Writer, lookup config.LookupFunc) int {
	if len(args) > 0 {
		switch {
		case isHelp(args[0]):
			_, _ = io.WriteString(stdout, usage(spec))
			return ExitOK
		case args[0] == "healthcheck" && len(args) <= 2:
			path := "/healthz"
			if len(args) == 2 {
				path = args[1]
			}
			cfg, err := config.LoadFrom(lookup)
			if err != nil {
				return configError(stderr, spec.Process, err)
			}
			return Healthcheck(ctx, cfg.Addr(spec.Process), path, stderr)
		case args[0] == "migrate" && spec.Migrate:
			return migrate(ctx, spec, args[1:], stdout, stderr, lookup)
		case spec.Commands[args[0]] != nil:
			cfg, err := config.LoadFrom(lookup)
			if err != nil {
				return configError(stderr, spec.Process, err)
			}
			return spec.Commands[args[0]](ctx, cfg, args[1:], os.Stdin, stdout, stderr)
		default:
			obs.NewLogger(stderr, "info", spec.Process).Error("unknown argument",
				slog.String("argument", strings.Join(args, " ")), slog.String("hint", "--help lists the usage"))
			return ExitConfig
		}
	}
	cfg, err := load(lookup, spec.Process)
	if err != nil {
		return configError(stderr, spec.Process, err)
	}
	return run(ctx, cfg, []Spec{spec}, stdout)
}

// MainAll is the body of cmd/ussp-dev: every spec in one process, each
// on its own listener, sharing one configuration. Development only;
// never in the image.
func MainAll(ctx context.Context, specs []Spec, args []string, stdout, stderr io.Writer, lookup config.LookupFunc) int {
	if len(args) > 0 {
		if isHelp(args[0]) {
			for _, s := range specs {
				_, _ = io.WriteString(stdout, usage(s)+"\n")
			}
			return ExitOK
		}
		obs.NewLogger(stderr, "info", "ussp-dev").Error("unknown argument", slog.String("argument", args[0]))
		return ExitConfig
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Process)
	}
	cfg, err := load(lookup, names...)
	if err != nil {
		return configError(stderr, "ussp-dev", err)
	}
	return run(ctx, cfg, specs, stdout)
}

func load(lookup config.LookupFunc, processes ...string) (config.Config, error) {
	cfg, err := config.LoadFrom(lookup)
	if err != nil {
		return cfg, err
	}
	return cfg, cfg.Require(processes...)
}

// run sets up tracing once, runs every spec and returns the exit code;
// the first process to fail stops the others.
func run(ctx context.Context, cfg config.Config, specs []Spec, stdout io.Writer) int {
	logger := obs.NewLogger(stdout, cfg.LogLevel, specs[0].Process)
	service := "uspace-ussp-" + specs[0].Process
	if len(specs) > 1 {
		logger = obs.NewLogger(stdout, cfg.LogLevel, "ussp-dev")
		service = "uspace-ussp-dev"
	}
	tracing, shutdown, err := obs.SetupTracing(ctx, cfg.OTLPURL, service)
	if err != nil {
		obs.Error(ctx, logger, "tracing setup failed", err)
		return ExitFailed
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "tracing", slog.Bool("enabled", tracing))
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(sctx); err != nil {
			obs.Error(sctx, logger, "tracing shutdown failed", err)
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	code := ExitOK
	var wg sync.WaitGroup
	for _, s := range specs {
		wg.Go(func() {
			if err := Run(runCtx, cfg, s, Options{Out: stdout}); err != nil {
				obs.Error(ctx, logger, "process failed", err, obs.Process(s.Process))
				mu.Lock()
				code = ExitFailed
				mu.Unlock()
				cancel()
			}
		})
	}
	wg.Wait()
	return code
}

func isHelp(arg string) bool { return slices.Contains([]string{"-h", "--help", "-help", "help"}, arg) }

func usage(spec Spec) string {
	cmds := "usage: ussp-" + spec.Process + " [--help | healthcheck [path]"
	if spec.Migrate {
		cmds += " | migrate [up | down [version] | status]"
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Commands)) {
		cmds += " | " + name + " ..."
	}
	return cmds + "]\n\nConfiguration is read from the environment only (deploy/ENV.md):\n\n" + config.Help(spec.Process)
}

// configError logs every invalid or missing variable on one line and
// returns ExitConfig.
func configError(stderr io.Writer, process string, err error) int {
	fes := config.FieldErrors(err)
	fields := make([]string, 0, len(fes))
	for _, fe := range fes {
		fields = append(fields, fe.Field)
	}
	obs.NewLogger(stderr, "info", process).Error("configuration invalid",
		slog.Any("variables", fields), slog.String("error", strings.ReplaceAll(err.Error(), "\n", "; ")))
	return ExitConfig
}

// Healthcheck GETs path on the local listener of addr and returns
// ExitOK on 200, ExitFailed otherwise, naming the failure on stderr.
func Healthcheck(ctx context.Context, addr, path string, stderr io.Writer) int {
	logger := obs.NewLogger(stderr, "info", "healthcheck")
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		obs.Error(ctx, logger, "healthcheck failed", err)
		return ExitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+path, http.NoBody)
	if err != nil {
		obs.Error(ctx, logger, "healthcheck failed", err)
		return ExitFailed
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		obs.Error(ctx, logger, "healthcheck failed", err)
		return ExitFailed
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Error("healthcheck failed", slog.String("path", path), slog.Int("status", resp.StatusCode))
		return ExitFailed
	}
	return ExitOK
}
