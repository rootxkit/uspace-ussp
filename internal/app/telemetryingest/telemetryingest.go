// Package telemetryingest is the telemetry-ingest process (docs/PLAN.md
// §3.1; brief WP-8): WS /v1/telemetry and POST /v1/telemetry/batch for
// operator machine clients, placed in time, bound to the client's
// serials and to flights, published on trk.v1 (core NATS, captured by
// TRK), with the identification changes on ident.v1, the flight facts on
// flight.v1, the work queue ingest.v1 under backpressure and its drain,
// and every client's status on src.v1 every 2 s.
//
// It never opens a database (D6): the client bindings, the active
// intents, the registry answers, the CIS, the policy and the source
// switches are KV projections written by api, each on /readyz with its
// state; a projection never read refuses what needs it or says it was
// not judged, never passes by default. The geoid comes from
// USSP_GEOID_FILE; without it no track has an AMSL altitude and /readyz
// says geoid down, "missing" (R-07, SC-22).
package telemetryingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geoid"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Readiness dependency names beside nats and jwks.
const (
	DepGeoid          = "geoid"
	DepClientBindings = "client_bindings"
	DepIntentActive   = "intent_active"
	DepRegistry       = "registry_validity"
	DepCIS            = "cis_current"
	DepPolicy         = "policy"
	DepSourceControl  = "source_control"
)

// DrainConsumer is the durable consumer of the INGEST work queue, shared
// by every instance (a work queue takes one consumer).
const DrainConsumer = "telemetry-ingest-drain"

// Options are what a test replaces.
type Options struct {
	// QueueFrames is the publisher's memory (0: telemetry.DefaultQueueFrames;
	// negative: none, every sample through the work queue).
	QueueFrames int
	// Geoid replaces USSP_GEOID_FILE's grid.
	Geoid geoid.Undulator
	// StatusEvery is the period of the status frames and of src.v1 (2 s).
	StatusEvery time.Duration
	// Ingestor, when set, receives the ingestor once it exists.
	Ingestor func(*telemetry.Ingestor)
}

// Spec declares the process and the dependencies it reads.
var Spec = SpecWith(Options{})

// SpecWith is the process with o.
func SpecWith(o Options) proc.Spec {
	return proc.Spec{
		Process: config.ProcessTelemetryIngest,
		NATS:    proc.Required,
		Routes:  func(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime) error { return routes(ctx, mux, rt, o) },
	}
}

// Run runs telemetry-ingest with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}

// OwnIssuer is the iss of this USSP's operator tokens: USSP_ISSUER_URL,
// or https:// followed by the first USSP_AUDIENCES entry (as api issues
// them). Operator scopes are honoured only on tokens of this issuer,
// which USSP_TOKEN_ISSUERS must list with api's JWKS.
func OwnIssuer(cfg config.Config) string {
	if cfg.IssuerURL != "" {
		return cfg.IssuerURL
	}
	if len(cfg.Audiences) > 0 {
		return "https://" + cfg.Audiences[0]
	}
	return ""
}

// loadGeoid reads USSP_GEOID_FILE with core's geoid.LoadMapped (WP-19: a
// read-only memory map on linux, shared in the page cache by every
// process on the host; read into memory elsewhere); mapped says which.
// nil with the reason when it is not set or not readable.
func loadGeoid(cfg config.Config) (u geoid.Undulator, desc string, mapped bool, missing string) {
	if cfg.GeoidFile == "" {
		return nil, "", false, "missing: USSP_GEOID_FILE is not set; tracks have no AMSL altitude (alt_source none) and are not judged vertically (R-07)"
	}
	g, err := geoid.LoadMapped(cfg.GeoidFile)
	if err != nil {
		return nil, "", false, "missing: USSP_GEOID_FILE does not load (" + err.Error() + "); tracks have no AMSL altitude (R-07)"
	}
	desc, mapped = g.Description(), g.Mapped()
	return g, desc, mapped, ""
}

// geoidProbe is the readiness of the geoid: down with missing without
// one; up otherwise, with whether the grid is memory-mapped
// (Grid.Mapped), or "configured by the caller" for Options.Geoid.
func geoidProbe(und geoid.Undulator, byCaller, mapped bool, missing string) obs.Probe {
	return func(context.Context) (obs.State, string) {
		switch {
		case und == nil:
			return obs.StateDown, missing
		case byCaller:
			return obs.StateUp, "configured by the caller"
		}
		return obs.StateUp, fmt.Sprintf("mapped: %t", mapped)
	}
}

// mirrorProbe is the readiness of a KV projection: up once read, unknown
// before, degraded with its age while its watch is down.
func mirrorProbe[T any](m *bus.Mirror[T], what string) obs.Probe {
	return func(context.Context) (obs.State, string) {
		_, age, loaded := m.Snapshot()
		if !loaded {
			return obs.StateUnknown, what + " not read yet"
		}
		if age > 0 {
			return obs.StateDegraded, fmt.Sprintf("%s: the watch is down; last read %.0f s ago", what, age)
		}
		return obs.StateUp, ""
	}
}

func routes(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime, o Options) error {
	cfg := rt.Config
	logger := rt.Logger
	js := rt.Bus.JetStream()
	verifier, _, err := proc.TokenVerifier(ctx, rt, nil)
	if err != nil {
		return err
	}
	guardCounters := &core.Counters{}
	proc.Publish(rt, "guard", guardCounters)
	guard := &auth.Guard{Verifier: verifier, OwnIssuer: OwnIssuer(cfg), Counters: guardCounters, Logger: logger}

	// The projections (D6).
	followCounters := &core.Counters{}
	proc.Publish(rt, "projections", followCounters)
	pol := &bus.Follower[policy.Record]{JS: js, Bucket: bus.BucketPolicy, Key: bus.KeyPolicy, Decode: telemetry.DecodePolicy,
		Core: rt.Bus.Conn, Push: bus.CtlPolicy, Counters: followCounters, Logger: logger}
	current := func() policy.Record {
		if r, _, ok := pol.Value(); ok {
			return r
		}
		return policy.Record{Values: policy.Defaults()}
	}
	src := sources.Follow(rt.Bus, logger)
	bindings := &bus.Mirror[[]string]{JS: js, Bucket: bus.BucketClientBindings, Counters: followCounters, Logger: logger}
	intents := &bus.Mirror[telemetry.IntentFacts]{JS: js, Bucket: bus.BucketIntentActive, Counters: followCounters, Logger: logger}
	reg := &bus.Mirror[registry.Entry]{JS: js, Bucket: bus.BucketRegistryValidity, Counters: followCounters, Logger: logger}
	cisM := &bus.Mirror[telemetry.CISValue]{JS: js, Bucket: bus.BucketCISCurrent, Decode: telemetry.DecodeCIS, Counters: followCounters, Logger: logger}
	rt.Health.Register(DepClientBindings, false, mirrorProbe(bindings, "client_bindings"))
	rt.Health.Register(DepIntentActive, false, mirrorProbe(intents, "intent_active"))
	rt.Health.Register(DepRegistry, false, mirrorProbe(reg, "registry_validity"))
	rt.Health.Register(DepCIS, false, mirrorProbe(cisM, "cis_current"))
	rt.Health.Register(DepPolicy, false, func(context.Context) (obs.State, string) {
		if _, age, ok := pol.Value(); ok {
			if age > 2*bus.DefaultReread.Seconds() {
				return obs.StateDegraded, fmt.Sprintf("last confirmed %.0f s ago", age)
			}
			return obs.StateUp, ""
		}
		return obs.StateUnknown, "not read yet: the defaults apply"
	})
	rt.Health.Register(DepSourceControl, false, func(context.Context) (obs.State, string) {
		if _, ok := src.AgeS(); ok {
			return obs.StateUp, ""
		}
		return obs.StateUnknown, "not read yet: every source is enabled (B-09)"
	})

	// The geoid (R-07, SC-22).
	und, desc, mapped, missing := o.Geoid, "configured by the caller", false, ""
	byCaller := und != nil
	if !byCaller {
		und, desc, mapped, missing = loadGeoid(cfg)
	}
	rt.Health.Register(DepGeoid, false, geoidProbe(und, byCaller, mapped, missing))
	switch {
	case und == nil:
		logger.Warn("no geoid: tracks have no AMSL altitude and are not judged vertically (R-07)", slog.String("geoid", missing))
	case byCaller:
		logger.Info("geoid loaded", slog.String("geoid", desc))
	default:
		logger.Info("geoid loaded", slog.String("geoid", desc), slog.Bool("geoid_mapped", mapped))
	}

	// The bus side.
	pubCounters := &core.Counters{}
	proc.Publish(rt, "telemetry_bus", pubCounters)
	pub := bus.NewPublisher(rt.Bus, pubCounters)
	outCounters := &core.Counters{}
	proc.Publish(rt, "telemetry_outbox", outCounters)
	outbox := telemetry.NewOutbox(pub, o.QueueFrames, 0, outCounters, logger)
	outbox.QueueFor = func() time.Duration { return time.Duration(current().Values.IngestQueueS * float64(time.Second)) }
	events := telemetry.NewEvents(pub, 0, outCounters, logger)
	flightCounters := &core.Counters{}
	proc.Publish(rt, "flights", flightCounters)
	binder := &flights.Binder{
		Emit: func(e *flights.Event) {
			if subject, err := e.Subject(); err == nil {
				events.Publish(subject, e)
			}
		},
		Policy: func() policy.Values { return current().Values },
		IntentActive: func(id string) (bool, bool) {
			_, found, _, loaded := intents.Get(id)
			return found, loaded
		},
		Counters: flightCounters, Logger: logger,
	}
	ingCounters := &core.Counters{}
	proc.Publish(rt, "telemetry", ingCounters)
	seenCounters := &core.Counters{}
	proc.Publish(rt, "replay_window", seenCounters)
	seen := &bus.SeenWindow{JS: js, Counters: seenCounters, Logger: logger}
	ing := telemetry.New(telemetry.Config{
		Bindings: telemetry.KVBindings{M: bindings}, Intents: telemetry.KVIntents{M: intents},
		Registry: telemetry.KVRegistry{M: reg}, Airspace: telemetry.NewCISAirspace(cisM), Geoid: und, Sources: src,
		Policy: current, Flights: binder, Outbox: outbox, Events: events, Seen: seen, Counters: ingCounters, Logger: logger,
	})
	if o.Ingestor != nil {
		o.Ingestor(ing)
	}
	statusCounters := &core.Counters{}
	proc.Publish(rt, "telemetry_status", statusCounters)
	status := &telemetry.Status{Ingest: ing, Pub: pub, Sources: src, Policy: current, Counters: statusCounters, Logger: logger}
	drainCounters := &core.Counters{}
	proc.Publish(rt, "telemetry_drain", drainCounters)
	top := proc.TopologyOf(rt.Config)
	drain := &telemetry.Drain{
		Source: &bus.StreamSource{Open: bus.PullOpener(js, top, bus.StreamINGEST, bus.PullSpec{
			Durable: DrainConsumer, FilterSubject: bus.SubjectIngestAll, MaxAckPending: 1024,
		})},
		Pub: pub, Gaps: status, Counters: drainCounters, Logger: logger,
		MaxAge: func() time.Duration { return ingestMaxAge(current().Values, top) },
	}
	every := o.StatusEvery
	if every <= 0 {
		every = 2 * time.Second
	}
	srv := &telemetry.Server{
		Health: proc.HealthHandlers{Health: rt.Health}, Ingest: ing,
		WS:      &auth.WSAuth{Guard: guard, AllowedOrigins: cfg.WSAllowedOrigins},
		Sources: src, Policy: current, Ctx: ctx, StatusEvery: every, Counters: ingCounters, Logger: logger,
		Degraded: func() []string { return rt.Health.Snapshot().Degraded },
	}
	if err := telemetry.Register(mux, srv, guard.Require); err != nil {
		return fmt.Errorf("access table: %w", err)
	}

	for _, run := range []func(context.Context){
		pol.Run, src.Run, bindings.Run, intents.Run, reg.Run, cisM.Run,
		outbox.Run, outbox.RunSpill, events.Run, drain.Run, seen.Run,
		func(ctx context.Context) { status.Run(ctx, every) },
		func(ctx context.Context) { tick(ctx, time.Second, func() { binder.Tick() }) },
		func(ctx context.Context) {
			tick(ctx, time.Minute, func() {
				v := current().Values
				ing.Sweep(time.Duration((v.FlightEndAfterS + v.TelemetryDedupeS) * float64(time.Second)))
			})
		},
	} {
		rt.Go(ctx, run)
	}
	return nil
}

// ingestMaxAge is how old a work-queue sample may be before it is shed
// with a gap: the policy's ingest_backlog_max_s, kept 30 s inside the
// stream's own age bound so the drain sheds first and the stream never
// removes a sample unread while the drain runs.
func ingestMaxAge(v policy.Values, top bus.Topology) time.Duration {
	d := time.Duration(v.IngestBacklogMaxS * float64(time.Second))
	if s, ok := top.Stream(bus.StreamINGEST); ok && s.MaxAge > 0 {
		d = min(d, s.MaxAge-30*time.Second)
	}
	return d
}

// tick runs fn every period until ctx ends.
func tick(ctx context.Context, period time.Duration, fn func()) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
