// Command ussp-dev runs all seven processes in one process, each on its
// own listener, for local work. It never ships in the image.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/app/dsssync"
	"github.com/rootxkit/uspace-ussp/internal/app/monitor"
	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/app/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/app/telemetryingest"
	"github.com/rootxkit/uspace-ussp/internal/app/trafficws"
	"github.com/rootxkit/uspace-ussp/internal/app/tsdbwriter"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	specs := []proc.Spec{api.Spec, telemetryingest.Spec, ridsp.Spec, monitor.Spec, trafficws.Spec, dsssync.Spec, tsdbwriter.Spec}
	code := proc.MainAll(ctx, specs, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}
