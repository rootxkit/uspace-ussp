// Package ridsp is the rid-sp process: the ASTM F3411-22a network
// identification Service Provider (docs/PLAN.md §3.1; brief WP-9). It
// serves GET /uss/flights and GET /uss/flights/{id}/details from the
// in-memory 60 s window of our own flights (fed from the TRK stream),
// and POST /uss/identification_service_areas/{id} into the KV bucket
// rid_isa_notifications, each behind the standard's scope. It never
// opens a database (D6): the intents are the intent_active projection,
// the policy the policy bucket. The ISAs of our flights are written to
// the DSS by api, which records the flights (internal/ridsp ISAWorker).
package ridsp

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	sp "github.com/rootxkit/uspace-ussp/internal/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/stdapi"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Readiness dependency names beside nats and jwks.
const (
	DepTrk          = "trk"
	DepIntentActive = "intent_active"
	DepPolicy       = "policy"
)

// sweepEvery is how often the window drops what aged out.
const sweepEvery = 5 * time.Second

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process: config.ProcessRIDSP,
	NATS:    proc.Required,
	Routes:  routes,
}

// Run runs rid-sp with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}

// mirrorIntents is the intent_active projection as sp.Intents.
type mirrorIntents struct{ m *bus.Mirror[sp.IntentFacts] }

func (i mirrorIntents) Intent(id string) (sp.IntentFacts, bool) {
	v, found, _, _ := i.m.Get(id)
	return v, found
}

// routes serves /healthz and /readyz and the F3411 USS endpoints behind
// the standard's scopes. rid-sp has no issuer of its own and no
// relational database: it accepts only ecosystem tokens, and a refusal
// is counted and logged, not audited.
func routes(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime) error {
	h := proc.HealthHandlers{Health: rt.Health}
	mux.HandleFunc("GET /healthz", h.GetHealthz)
	mux.HandleFunc("GET /readyz", h.GetReadyz)
	verifier, _, err := proc.TokenVerifier(ctx, rt, nil)
	if err != nil {
		return err
	}
	guardCounters := &core.Counters{}
	proc.Publish(rt, "guard", guardCounters)
	guard := &auth.Guard{Verifier: verifier, Counters: guardCounters, Logger: rt.Logger}
	js := rt.Bus.JetStream()

	// The projections (D6).
	followCounters := &core.Counters{}
	proc.Publish(rt, "projections", followCounters)
	pol := &bus.Follower[policy.Record]{JS: js, Bucket: bus.BucketPolicy, Key: bus.KeyPolicy, Decode: telemetry.DecodePolicy,
		Core: rt.Bus.Conn, Push: bus.CtlPolicy, Counters: followCounters, Logger: rt.Logger}
	current := func() policy.Values {
		if r, _, ok := pol.Value(); ok {
			return r.Values
		}
		return policy.Defaults()
	}
	intents := &bus.Mirror[sp.IntentFacts]{JS: js, Bucket: bus.BucketIntentActive, Counters: followCounters, Logger: rt.Logger}
	rt.Health.Register(DepIntentActive, false, func(context.Context) (obs.State, string) {
		_, age, loaded := intents.Snapshot()
		switch {
		case !loaded:
			return obs.StateUnknown, "intent_active not read yet: flight details carry no Annex IV declaration"
		case age > 0:
			return obs.StateDegraded, fmt.Sprintf("intent_active: the watch is down; last read %.0f s ago", age)
		}
		return obs.StateUp, ""
	})
	rt.Health.Register(DepPolicy, false, func(context.Context) (obs.State, string) {
		if _, age, ok := pol.Value(); ok {
			if age > 2*bus.DefaultReread.Seconds() {
				return obs.StateDegraded, fmt.Sprintf("last confirmed %.0f s ago", age)
			}
			return obs.StateUp, ""
		}
		return obs.StateUnknown, "not read yet: the defaults apply"
	})

	// The window and its feed.
	spCounters := &core.Counters{}
	proc.Publish(rt, "rid_sp", spCounters)
	window := &sp.Window{MaxSamples: func() int { return current().RIDRecentPositionsMaxCount }, Counters: spCounters}
	feed := &sp.TrackFeed{Source: sp.JSTracks{JS: js}, Window: window, Logger: rt.Logger}
	rt.Health.Register(DepTrk, false, feed.Probe())
	latency := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: obs.MetricName("rid_sp_flights_seconds"), Help: "time to answer one GET /uss/flights",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 3},
	})
	rt.Registry.MustRegister(latency)
	srv := &sp.Server{
		Window: window, Intents: mirrorIntents{intents}, Notifications: sp.KVNotifications{JS: js},
		Counters: spCounters, Logger: rt.Logger,
		ObserveFlights: func(d time.Duration) { latency.Observe(d.Seconds()) },
	}
	std := &core.Counters{}
	proc.Publish(rt, "stdapi", std)
	if err := stdapi.MountF3411(mux, srv, stdapi.Options{Guard: guard.Require, Validate: auth.ValidateAccess, Counters: std}); err != nil {
		return fmt.Errorf("F3411 access table: %w", err)
	}
	for _, run := range []func(context.Context){pol.Run, intents.Run, feed.Run, func(ctx context.Context) { sweep(ctx, window) }} {
		rt.Go(ctx, run)
	}
	return nil
}

// sweep drops what aged out of the window every sweepEvery until ctx
// ends.
func sweep(ctx context.Context, w *sp.Window) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Sweep()
		}
	}
}
