// Package dsssync is the dss-sync process: the outbox towards the InterUSS DSS and peer notifications. Its routes and
// workers arrive with their work packages; until then it serves its
// health, readiness and metrics and reports its dependencies.
package dsssync

import (
	"context"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process: config.ProcessDSSSync,
	NATS:    proc.Required,
}

// Run runs dss-sync with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}
