package api

import (
	"context"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geoid"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/bus"
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

// noDSSWriter is the DSS as this process sees it until WP-13 brings
// dss-sync: the strategic coordination write cannot be made, so an
// intent inside U-space airspace waits as pending_dss and never counts
// as deconflicted across USSPs (02 F5). Outside U-space airspace local
// checks suffice.
type noDSSWriter struct{ reason string }

// Available implements intent.DSS: never, with the reason.
func (d noDSSWriter) Available(context.Context) (bool, string) { return false, d.reason }

// loadGeoid reads USSP_GEOID_FILE; nil with the reason when it is unset
// or does not load (intents are then refused with geoid_unavailable and
// /readyz says why; the process still starts).
func loadGeoid(rt *proc.Runtime) (geoid.Undulator, string) {
	path := rt.Config.GeoidFile
	if path == "" {
		return nil, "USSP_GEOID_FILE is not set: every intent is refused with geoid_unavailable (AMSL is never approximated)"
	}
	g, err := geoid.Load(path)
	if err != nil {
		rt.Logger.Error("geoid grid not loaded", obs.Err(err))
		return nil, "the geoid grid of USSP_GEOID_FILE does not load: every intent is refused with geoid_unavailable"
	}
	return g, ""
}

// startIntents builds the intent service on the CIS cache, the registry
// cache and the policy, projects through kv and the bus, registers the
// geoid's and the DSS's readiness and starts the time_end sweep.
func startIntents(ctx context.Context, rt *proc.Runtime, pol *policy.Service, cisState *CIS, reg *registry.Cache, kv *bus.Projector) *intent.Service {
	counters := &core.Counters{}
	proc.Publish(rt, "intent", counters)
	g, why := loadGeoid(rt)
	rt.Health.Register(DepGeoid, false, func(context.Context) (obs.State, string) {
		if g == nil {
			return obs.StateDown, why
		}
		return obs.StateUp, ""
	})
	// The DSS's readiness is dss-sync's (WP-13); until then every
	// decision that needs it says dss_unavailable in its conflicts.
	dss := noDSSWriter{reason: "no DSS writer runs yet (dss-sync, WP-13)"}
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
		Projector: intent.BusProjector{KV: kv, Pub: pub},
	}
	if rt.Store != nil && rt.Store.Rel != nil {
		svc.Store = pgstore.Store{S: rt.Store}
		rt.Go(ctx, func(ctx context.Context) { svc.RunSweep(ctx, intentSweepInterval) })
	}
	return svc
}
