// Package tsdbwriter is the tsdb-writer process: the only writer of the time-series database. Its routes and
// workers arrive with their work packages; until then it serves its
// health, readiness and metrics and reports its dependencies.
package tsdbwriter

import (
	"context"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process:     config.ProcessTSDBWriter,
	TimescaleDB: proc.Optional,
	NATS:        proc.Required,
	Migrate:     true,
}

// Run runs tsdb-writer with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}
