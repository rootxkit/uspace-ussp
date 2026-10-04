package api

import (
	"context"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geoid"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/intent/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

// DepGeoid is the readiness dependency of the geoid grid flight
// authorisation converts volumes to AMSL with.
const DepGeoid = "geoid"

// intentSweepInterval is how often intents past time_end are ended.
const intentSweepInterval = time.Minute

// loadGeoid reads USSP_GEOID_FILE with core's geoid.LoadMapped (WP-19: a
// read-only memory map on linux, shared in the page cache by every
// process on the host; read into memory elsewhere) and says which in
// mapped; nil with the reason when it is unset or does not load (intents
// are then refused with geoid_unavailable and /readyz says why; the
// process still starts).
func loadGeoid(rt *proc.Runtime) (und geoid.Undulator, mapped bool, why string) {
	path := rt.Config.GeoidFile
	if path == "" {
		return nil, false, "USSP_GEOID_FILE is not set: every intent is refused with geoid_unavailable (AMSL is never approximated)"
	}
	g, err := geoid.LoadMapped(path)
	if err != nil {
		rt.Logger.Error("geoid grid not loaded", obs.Err(err))
		return nil, false, "the geoid grid of USSP_GEOID_FILE does not load: every intent is refused with geoid_unavailable"
	}
	mapped = g.Mapped()
	return g, mapped, ""
}

// geoidProbe is the readiness of the geoid: down with why without one,
// up with whether the grid is memory-mapped (Grid.Mapped) otherwise.
func geoidProbe(und geoid.Undulator, mapped bool, why string) obs.Probe {
	return func(context.Context) (obs.State, string) {
		if und == nil {
			return obs.StateDown, why
		}
		return obs.StateUp, fmt.Sprintf("mapped: %t", mapped)
	}
}

// startIntents builds the intent service on the CIS cache, the registry
// cache, the policy and the DSS gate (WP-13: whether the strategic
// coordination write can be made now; an intent the DSS must hold waits
// pending_dss until the DSS writer has written it), projects through kv
// and the bus, registers the geoid's readiness and starts the time_end
// sweep.
func startIntents(ctx context.Context, rt *proc.Runtime, pol *policy.Service, cisState *CIS, reg *registry.Cache, kv *bus.Projector, dss intent.DSS) *intent.Service {
	counters := &core.Counters{}
	proc.Publish(rt, "intent", counters)
	g, mapped, why := loadGeoid(rt)
	rt.Health.Register(DepGeoid, false, geoidProbe(g, mapped, why))
	current := func() policy.Record {
		if r, ok := pol.Current(); ok {
			return r
		}
		return policy.Record{Values: policy.Defaults()}
	}
	d := &intent.Decider{
		CIS: cisState.Evaluator, Integrity: cisState.Cache, Registry: reg, DSS: dss,
		SystemID: rt.Config.SystemID, Counters: counters,
	}
	pub := bus.NewPublisher(rt.Bus, counters)
	svc := &intent.Service{
		Decider: d, Geoid: g, Policy: current, Counters: counters, Logger: rt.Logger.With("component", "intent"),
		Projector: intent.BusProjector{KV: kv, Pub: pub, Notices: geo.NoticeBus{Pub: pub, Counters: counters}},
		DSSForAll: rt.Config.DSSForAll == "on",
	}
	if rt.Store != nil && rt.Store.Rel != nil {
		svc.Store = pgstore.Store{S: rt.Store}
		rt.Go(ctx, func(ctx context.Context) { svc.RunSweep(ctx, intentSweepInterval) })
	}
	return svc
}
