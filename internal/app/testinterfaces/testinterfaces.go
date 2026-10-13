// Package testinterfaces is the test-interfaces process (docs/PLAN.md
// D13, §3.1; brief WP-21): the InterUSS automated-testing interfaces of
// internal/testif on their own listener (USSP_TEST_INTERFACES_ADDR), with
// /healthz, /readyz and /metrics. It opens no database and no NATS
// connection. It refuses to start, exit 2 before any listener, unless
// USSP_TEST_INTERFACES=on and USSP_SYSTEM_ID contains TEST or DEV; it is
// built into the uspace-ussp-testif image only, never the production one.
package testinterfaces

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/testif"
)

// Binary is the executable's name in the testif image.
const Binary = "ussp-test-interfaces"

// Main is the body of cmd/test-interfaces: --help, healthcheck [path],
// or the process. The gate runs first, on the raw environment, before
// any other configuration is read and before any listener opens.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer, lookup config.LookupFunc) int {
	if len(args) > 0 {
		switch {
		case args[0] == "--help" || args[0] == "-h" || args[0] == "help":
			_, _ = io.WriteString(stdout, "usage: "+Binary+" [--help | healthcheck [path]]\n\n"+
				"Configuration is read from the environment only (deploy/ENV.md, test-interfaces):\n\n"+testif.Help())
			return proc.ExitOK
		case args[0] == "healthcheck" && len(args) <= 2:
			path := "/healthz"
			if len(args) == 2 {
				path = args[1]
			}
			tc, err := testif.Load(lookup)
			if err != nil && tc.Addr == "" {
				return configError(stderr, err)
			}
			return proc.Healthcheck(ctx, tc.Addr, path, stderr)
		default:
			obs.NewLogger(stderr, "info", testif.Process).Error("unknown argument",
				slog.String("argument", strings.Join(args, " ")), slog.String("hint", "--help lists the usage"))
			return proc.ExitConfig
		}
	}
	if err := testif.Gate(lookup); err != nil {
		obs.NewLogger(stderr, "info", testif.Process).Error("refusing to start", slog.String("error", err.Error()),
			slog.Any("variables", []string{testif.VarSwitch, testif.VarSystemID}))
		return proc.ExitConfig
	}
	cfg, err := config.LoadFrom(lookup)
	if err != nil {
		return configError(stderr, err)
	}
	tc, err := testif.Load(lookup)
	if err != nil {
		return configError(stderr, err)
	}
	spec := proc.Spec{Process: testif.Process, NATS: proc.NotUsed, Routes: Routes(tc)}
	if err := proc.Run(ctx, cfg, spec, proc.Options{Out: stdout, Addr: tc.Addr}); err != nil {
		obs.Error(ctx, obs.NewLogger(stdout, cfg.LogLevel, testif.Process), "process failed", err)
		return proc.ExitFailed
	}
	return proc.ExitOK
}

func configError(stderr io.Writer, err error) int {
	fes := config.FieldErrors(err)
	fields := make([]string, 0, len(fes))
	for _, fe := range fes {
		fields = append(fields, fe.Field)
	}
	obs.NewLogger(stderr, "info", testif.Process).Error("configuration invalid",
		slog.Any("variables", fields), slog.String("error", strings.ReplaceAll(err.Error(), "\n", "; ")))
	return proc.ExitConfig
}

// Routes serves /healthz and /readyz and the InterUSS interfaces behind
// the test listener's guard; it also writes the switch's error line every
// status period.
func Routes(tc testif.Config) func(context.Context, *http.ServeMux, *proc.Runtime) error {
	return func(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime) error {
		h := proc.HealthHandlers{Health: rt.Health}
		mux.HandleFunc("GET /healthz", h.GetHealthz)
		mux.HandleFunc("GET /readyz", h.GetReadyz)
		verifier, _, err := proc.TokenVerifier(ctx, rt, nil)
		if err != nil {
			return err
		}
		ussp, err := testif.NewUSSP(tc)
		if err != nil {
			return err
		}
		rt.Health.Register(testif.DepUSSP, true, ussp.Probe)
		counters := &core.Counters{}
		proc.Publish(rt, "testif", counters)
		proc.Publish(rt, "testif_operator_token", ussp.Tokens.Counters())
		srv, err := testif.NewServer(tc, &testif.Guard{Verifier: verifier, Counters: counters}, ussp, counters, rt.Logger)
		if err != nil {
			return err
		}
		if err := srv.Mount(mux); err != nil {
			return err
		}
		interval := time.Duration(rt.Config.StatusIntervalS) * time.Second
		rt.Go(ctx, func(ctx context.Context) { testif.AnnounceSwitch(ctx, rt.Logger, interval) })
		rt.Go(ctx, srv.Run)
		return nil
	}
}
