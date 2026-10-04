package api

import (
	"context"
	"net/http"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	flightstore "github.com/rootxkit/uspace-ussp/internal/flights/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/ridsp"
	ridsppg "github.com/rootxkit/uspace-ussp/internal/ridsp/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// DepDSS is the readiness entry of the DSS as the ISA worker sees it.
const DepDSS = "dss"

// isaRecorder records a flight fact and plans its F3411 ISA in one
// transaction (WP-9): the fact and the DSS work it implies commit
// together. Without a planner (no DSS configured) it records the fact
// alone.
type isaRecorder struct {
	S       *store.Store
	Planner *ridsp.Planner
}

var _ flights.Store = isaRecorder{}

// Record implements flights.Store.
func (r isaRecorder) Record(ctx context.Context, b flights.Body) error {
	return r.S.Tx(ctx, func(q *relational.Queries) error {
		if err := flightstore.RecordIn(ctx, q, b); err != nil {
			return err
		}
		if r.Planner == nil {
			return nil
		}
		return r.Planner.Plan(ctx, ridsppg.PlanStore{Q: q}, b)
	})
}

// startISA plans and writes the ISA of every flight when the DSS is
// configured (USSP_DSS_BASE_URL, USSP_USS_BASE_URL and an outgoing token
// client): the planner for the flight recorder and the worker, and the
// worker's part of dss on /readyz (merged with the F3548 part, WP-13).
// Otherwise no ISA is planned, and /readyz says so.
func startISA(ctx context.Context, rt *proc.Runtime, current func() policy.Values, tokens *auth.Outgoing) (*ridsp.Planner, obs.Probe) {
	cfg := rt.Config
	why := ""
	switch {
	case cfg.DSSBaseURL == "":
		why = "USSP_DSS_BASE_URL is not set"
	case cfg.USSBaseURL == "":
		why = "USSP_USS_BASE_URL is not set"
	case tokens == nil:
		why = "no outgoing token client (USSP_TOKEN_ISSUERS, USSP_TOKEN_CLIENT_SECRET_FILE)"
	}
	if why != "" {
		probe := func(context.Context) (obs.State, string) {
			return obs.StateDown, why + ": no F3411 ISA is written, so Display Providers do not find our flights"
		}
		rt.Health.Register(DepDSS, false, probe)
		rt.Logger.Warn("no F3411 ISA is written: " + why)
		return nil, probe
	}
	counters := &core.Counters{}
	proc.Publish(rt, "rid_isa", counters)
	planner := &ridsp.Planner{Policy: current, Counters: counters, Logger: rt.Logger}
	worker := &ridsp.ISAWorker{
		Store: ridsppg.WorkStore{S: rt.Store}, DSSBaseURL: cfg.DSSBaseURL, USSBaseURL: cfg.USSBaseURL,
		Tokens: tokens, HTTP: &http.Client{Timeout: ridsp.DefaultCallTimeout}, Policy: current,
		Counters: counters, Logger: rt.Logger,
	}
	rt.Health.Register(DepDSS, false, worker.Probe())
	rt.Go(ctx, worker.Run)
	return planner, worker.Probe()
}
