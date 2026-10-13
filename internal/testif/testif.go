package testif

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
)

// DepUSSP is the readiness entry of the USSP's api as the test operator
// reaches it.
const DepUSSP = "ussp_api"

// retryAfter is the wait a 503 of the test listener names.
const retryAfter = 5 * time.Second

// Server is the test listener's state: the guard, the USSP client and
// the interfaces' handlers. Mount registers them.
type Server struct {
	Guard    *Guard
	USSP     *USSP
	Counters *core.Counters
	Logger   *slog.Logger
	// Interfaces mounted on the listener, each with its access table and
	// its registration; set by the constructors of the interfaces.
	interfaces []iface
	cfg        Config
	fixture    Fixture
	inject     *injector
	plan       *planner
	observe    *observer

	opMu       sync.Mutex
	operatorID client.OperatorID // the test operator's id at the USSP, from GET /v1/accounts/me
}

// NewServer builds the test listener of cfg. The interfaces are added
// here, each by its own constructor.
func NewServer(cfg Config, g *Guard, u *USSP, counters *core.Counters, logger *slog.Logger) (*Server, error) {
	fx, err := LoadFixture(cfg.OperatorFile)
	if err != nil {
		return nil, fmt.Errorf("USSP_TEST_INTERFACES_OPERATOR_FILE: %w", err)
	}
	s := &Server{Guard: g, USSP: u, Counters: counters, Logger: logger, cfg: cfg, fixture: fx}
	s.inject = newInjector(s)
	s.plan = newPlanner(s)
	s.observe = newObserver(s)
	s.interfaces = append(s.interfaces, s.inject.iface(), s.observe.iface(), s.plan.iface())
	return s, nil
}

// Run is the server's background work until ctx ends: the injected
// flights' streams, which end (an end frame each) when ctx does.
func (s *Server) Run(ctx context.Context) {
	s.inject.run(ctx)
}

// iface is one InterUSS interface: its access table and the function
// that registers its routes on the guarded mux.
type iface struct {
	name     string
	access   map[string]httpx.Access
	register func(*httpx.GuardedMux)
}

// Mount registers every interface of s on mux behind one guarded mux
// whose table is the union of their access tables, so that every route
// fails closed (a route without an entry, an entry without a route, or
// an entry outside the closed scope list refuses the start).
func (s *Server) Mount(mux *http.ServeMux) error {
	table := map[string]httpx.Access{}
	for _, i := range s.interfaces {
		maps.Copy(table, i.access)
	}
	g := httpx.NewGuardedMux(mux, table, s.Guard.Require, ValidateAccess)
	for _, i := range s.interfaces {
		i.register(g)
	}
	return g.Err()
}

// SwitchOnMessage is the error line test-interfaces writes at start and
// every status period while it runs (as USSP_MTLS_MODE=off is told, M25).
const SwitchOnMessage = "USSP_TEST_INTERFACES=on: the InterUSS automated-testing interfaces are served; never in production (D13)"

// AnnounceSwitch writes SwitchOnMessage at error level now and every
// interval until ctx ends.
func AnnounceSwitch(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	logger.LogAttrs(ctx, slog.LevelError, SwitchOnMessage, slog.String("switch", VarSwitch))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logger.LogAttrs(ctx, slog.LevelError, SwitchOnMessage, slog.String("switch", VarSwitch))
		}
	}
}
