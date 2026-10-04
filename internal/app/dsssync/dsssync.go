// Package dsssync is the dss-sync process. As built (WP-9, WP-13) the
// outbox towards the InterUSS DSS, the F3548 writer, the subscriptions,
// the availability poll and the peer notifications run in api
// (internal/dss, internal/app/api), the only writer of the relational
// database (D5): the outbox, peer_intents and the intents' DSS columns
// are relational rows, and a second writer would break that rule.
// dss-sync serves its health, readiness and metrics and reports its
// dependencies, so a deployment that scales the DSS work out later keeps
// its process and image.
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
