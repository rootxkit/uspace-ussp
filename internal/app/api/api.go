// Package api is the api process: the control plane: the national API, accounts, intents, the CIS cache and the KV projections; the only writer of the relational database. Its routes and
// workers arrive with their work packages; until then it serves its
// health, readiness and metrics and reports its dependencies.
package api

import (
	"context"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process:     config.ProcessAPI,
	Postgres:    proc.Required,
	TimescaleDB: proc.Optional,
	NATS:        proc.Required,
	Migrate:     true,
}

// Run runs api with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}
