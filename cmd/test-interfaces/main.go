// Command ussp-test-interfaces runs the test-interfaces process
// (internal/app/testinterfaces): the InterUSS automated-testing
// interfaces, conformance profile only (docs/PLAN.md D13). It is built
// into the uspace-ussp-testif image, never into the production image.
// --help lists the configuration variables it reads.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rootxkit/uspace-ussp/internal/app/testinterfaces"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := testinterfaces.Main(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}
