// Package ridsp is the rid-sp process: the ASTM F3411-22a network
// identification Service Provider. WP-3 mounts its USS endpoints
// (internal/stdapi) behind the standard's scopes, each answering 501
// until WP-9; its workers arrive with WP-9. It serves its health,
// readiness and metrics and reports its dependencies.
package ridsp

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/stdapi"
)

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process: config.ProcessRIDSP,
	NATS:    proc.Required,
	Routes:  routes,
}

// Run runs rid-sp with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}

// routes serves /healthz and /readyz and the F3411 USS endpoints. rid-sp
// has no issuer of its own and no relational database: it accepts only
// ecosystem tokens, and a refusal is counted and logged, not audited.
func routes(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime) error {
	h := proc.HealthHandlers{Health: rt.Health}
	mux.HandleFunc("GET /healthz", h.GetHealthz)
	mux.HandleFunc("GET /readyz", h.GetReadyz)
	verifier, _, err := proc.TokenVerifier(ctx, rt, nil)
	if err != nil {
		return err
	}
	guardCounters := &core.Counters{}
	proc.Publish(rt, "guard", guardCounters)
	std := &core.Counters{}
	proc.Publish(rt, "stdapi", std)
	guard := &auth.Guard{Verifier: verifier, Counters: guardCounters, Logger: rt.Logger}
	if err := stdapi.MountF3411(mux, stdapi.NotImplementedF3411{}, stdapi.Options{Guard: guard.Require, Validate: auth.ValidateAccess, Counters: std}); err != nil {
		return fmt.Errorf("F3411 access table: %w", err)
	}
	return nil
}
