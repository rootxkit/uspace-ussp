// Package telemetryingest is the telemetry-ingest process: operator telemetry over WebSocket and batch, placed in time and bound to its client's serials. Its routes and
// workers arrive with their work packages; until then it serves its
// health, readiness and metrics and reports its dependencies.
package telemetryingest

import (
	"context"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process: config.ProcessTelemetryIngest,
	NATS:    proc.Required,
}

// Run runs telemetry-ingest with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}
