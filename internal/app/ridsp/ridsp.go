// Package ridsp is the rid-sp process: the ASTM F3411-22a network identification Service Provider. Its routes and
// workers arrive with their work packages; until then it serves its
// health, readiness and metrics and reports its dependencies.
package ridsp

import (
	"context"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process: config.ProcessRIDSP,
	NATS:    proc.Required,
}

// Run runs rid-sp with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}
