// Package trafficws is the traffic-ws process: the traffic and alert streams to operators and the console. Its routes and
// workers arrive with their work packages; until then it serves its
// health, readiness and metrics and reports its dependencies.
package trafficws

import (
	"context"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process: config.ProcessTrafficWS,
	NATS:    proc.Required,
}

// Run runs traffic-ws with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}
