// Package monitor is the monitor process (docs/PLAN.md §3.1; briefs
// WP-10, WP-11): the conformance path and the CPA path; WP-12 adds the
// zone path to the same process. It serves only /healthz, /readyz and
// /metrics.
//
// The CPA path (cpa.go, internal/traffic) feeds one uspace-core
// alerting.Monitor for the owned cell set with every trk.v1, peer.v1
// and man.v1 sample of the owned cells and their ring-1 neighbours,
// ticks it every second (every 2 s, said so, above the pair budget),
// follows the source switches and the flight ends, and publishes each
// proximity alert on alrt.v1 for each of this USSP's flights in the
// pair, republished every tick; the active alerts persist in
// proximity_state across a restart, a handover and a policy change.
//
// It reads the tracks of this USSP's flights from TRK (the cells it owns
// by USSP_CELL_OWNERSHIP and their ring-1 neighbours, replayed from
// monitor_live_max_age_s back so a restart judges the live samples at
// once), and the projections intent_active, policy and source_control
// (D6: it never opens a database). One worker per home cell3 judges its
// flights with internal/conformance on every sample and on a 1 s tick,
// and publishes conformance/state/v1 on conf.v1.<flight_id> (each
// transition, then a heartbeat of at most 0.1 Hz per flight) and alert/v1
// on alrt.v1 (nonconformance, lost_link, nonconformance_nearby), every
// active alert republished each tick with its current numbers (C-08).
//
// Each flight's state machine is persisted in the conformance_state
// bucket and restored at a start before the feed opens, and a flight
// that crosses into another instance's cells is handed over with it
// (Engine): a restart or a handover never clears a nonconformance
// silently nor returns a flight to conforming without the hysteresis.
//
// Missing inputs are visible, never conforming (SC-22): without
// intent_active every flight is unknown (intent_active_unavailable), the
// status line says so and /readyz reports it; a failed judgement for one
// flight neither stops the tick nor clears what it could not evaluate
// (C-09). Nothing here has a send path towards an aircraft (CLAUDE.md
// rule 1): alerts go to people through api and traffic-ws.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// Readiness dependency names beside nats.
const (
	DepTrk           = "trk"
	DepIntentActive  = "intent_active"
	DepPolicy        = "policy"
	DepSourceControl = "source_control"
)

// SummaryEvery is the period of the conformance status line.
const SummaryEvery = 10 * time.Second

// SubjectFlightEnded is the filter of the flight ends telemetry-ingest
// publishes (flight.v1.ended.<flight_id>), read here on core NATS.
const SubjectFlightEnded = "flight.v1.ended.*"

type intentBody = intent.StateBody

// Options are what a test replaces.
type Options struct {
	// Tick is the workers' period (1 s).
	Tick time.Duration
	// SummaryEvery is the status line's period (SummaryEvery).
	SummaryEvery time.Duration
	// Engine, when set, receives the engine once it exists.
	Engine func(*Engine)
	// CPA, when set, receives the CPA engine once it exists.
	CPA func(*traffic.Engine)
}

// Spec declares the process and the dependencies it reads.
var Spec = SpecWith(Options{})

// SpecWith is the process with o.
func SpecWith(o Options) proc.Spec {
	return proc.Spec{
		Process: config.ProcessMonitor,
		NATS:    proc.Required,
		Routes:  func(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime) error { return routes(ctx, mux, rt, o) },
	}
}

// Run runs monitor with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}

// TrackSubjects are the TRK filters of an ownership: every track for
// all, else each owned cell3 and its ring-1 neighbours, sorted.
func TrackSubjects(o cell.Ownership) ([]string, error) { return Subjects(bus.KindTrk, o) }

// Subjects are the filters of a located kind (trk, man, peer) for an
// ownership: every subject of the kind for all, else each owned cell3
// and its ring-1 neighbours, sorted.
func Subjects(kind string, o cell.Ownership) ([]string, error) {
	if o.All() {
		all := map[string]string{bus.KindTrk: bus.SubjectTrkAll, bus.KindMan: bus.SubjectManAll, bus.KindPeer: bus.SubjectPeerAll}[kind]
		if all == "" {
			return nil, core.Fieldf("kind", "%q is not a located kind", kind)
		}
		return []string{all}, nil
	}
	set := map[string]bool{}
	for _, c := range o.Cells() {
		id, err := cell.Parse3(c)
		if err != nil {
			return nil, err
		}
		set[c] = true
		for _, n := range id.Ring1() {
			set[n.String()] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		s, err := bus.Cell3Filter(kind, c)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out, nil
}

func routes(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime, o Options) error {
	h := proc.HealthHandlers{Health: rt.Health}
	mux.HandleFunc("GET /healthz", h.GetHealthz)
	mux.HandleFunc("GET /readyz", h.GetReadyz)
	own, err := cell.ParseOwnership(rt.Config.CellOwnership)
	if err != nil {
		return err
	}
	subjects, err := TrackSubjects(own)
	if err != nil {
		return err
	}
	// The ANSP's manned-traffic stream (USSP_ANSP_STREAM_URL): read and
	// reported, its tracks held until WP-14 (anspfeed.go).
	if err := startANSPFeed(ctx, rt); err != nil {
		return err
	}
	js := rt.Bus.JetStream()
	logger := rt.Logger

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
	intents := &bus.Mirror[intentBody]{JS: js, Bucket: bus.BucketIntentActive, Counters: followCounters, Logger: logger}
	src := sources.Follow(rt.Bus, logger)
	rt.Health.Register(DepIntentActive, false, IntentsProbe(intents))
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

	// The zone path's inputs (brief WP-12): the zones of cis_current,
	// rebuilt within one tick of a new projection, and the ground and the
	// geoid under each sample.
	und, geoidMissing := loadGeoid(rt.Config.GeoidFile)
	rt.Health.Register(DepGeoid, false, func(context.Context) (obs.State, string) {
		if und == nil {
			return obs.StateDown, geoidMissing
		}
		return obs.StateUp, ""
	})
	ground, terrainMissing := loadTerrain(rt.Config.TerrainDir)
	rt.Health.Register(DepTerrain, false, func(context.Context) (obs.State, string) {
		if ground == nil {
			return obs.StateDegraded, terrainMissing
		}
		return obs.StateUp, ""
	})
	zoneCounters := &core.Counters{}
	proc.Publish(rt, "zones", zoneCounters)
	cisM := &bus.Mirror[telemetry.CISValue]{JS: js, Bucket: bus.BucketCISCurrent, Decode: telemetry.DecodeCIS, Counters: followCounters, Logger: logger}
	zoneSrc := &geo.ZoneSource{M: cisM, Counters: zoneCounters}
	rt.Health.Register(DepCISCurrent, false, zoneProbe(zoneSrc))

	// The engine and its feed.
	engCounters := &core.Counters{}
	proc.Publish(rt, "conformance", engCounters)
	pubCounters := &core.Counters{}
	proc.Publish(rt, "monitor_bus", pubCounters)
	instance, err := os.Hostname()
	if err != nil || instance == "" {
		instance = "monitor"
	}
	eng := &Engine{
		Ownership: own, Intents: MirrorIntents{M: intents}, Sources: src, Policy: current,
		Sink: bus.NewPublisher(rt.Bus, pubCounters), Counters: engCounters, Logger: logger, Tick: o.Tick,
		Store: KVStates{KV: bus.KVStore{JS: js, Bucket: bus.BucketConformanceState}}, InstanceID: instance,
		Zones: zoneSrc, Env: NewEnv(und, ground),
	}
	if o.Engine != nil {
		o.Engine(eng)
	}
	cpa, err := startCPA(ctx, rt, own, current, intents, instance, und, geoidMissing, o)
	if err != nil {
		return err
	}
	fd := &Feed{
		Subjects: subjects, Logger: logger,
		Take: func(data []byte) {
			eng.Offer(data)
			cpa.offerTrack(traffic.NSTrack, data)
		},
		Open: func(ctx context.Context, subject string, from time.Time, handle func([]byte)) (func(), error) {
			return bus.Replay{JS: js, Stream: bus.StreamTRK, Subject: subject}.Open(ctx, from, handle)
		},
		From: func() time.Time { return time.Now().Add(-secs(current().Values.MonitorLiveMaxAgeS)) },
	}
	rt.Health.Register(DepTrk, true, fd.Probe)
	logger.Info("monitor conformance path", slog.String("cell_ownership", own.String()), slog.Any("track_subjects", subjects),
		slog.String("instance", instance))
	every := o.SummaryEvery
	if every <= 0 {
		every = SummaryEvery
	}
	for _, run := range []func(context.Context){
		pol.Run, intents.Run, src.Run, cisM.Run, eng.Run,
		// The feed opens once the saved flights are restored, so no
		// sample starts a flight its saved state holds.
		func(ctx context.Context) {
			select {
			case <-ctx.Done():
				return
			case <-eng.Seeded():
			}
			select {
			case <-ctx.Done():
				return
			case <-cpa.eng.Seeded():
			}
			fd.Run(ctx)
		},
		func(ctx context.Context) {
			flightEnds(ctx, rt.Bus, logger, eng.FlightEnded, cpa.eng.FlightEnded)
		},
		func(ctx context.Context) {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_, age, loaded := intents.Snapshot()
					logger.LogAttrs(ctx, slog.LevelInfo, "conformance status", StatusAttrs(eng.Summary(), age, loaded, current().Version)...)
					logger.LogAttrs(ctx, slog.LevelInfo, "cpa status", CPAStatusAttrs(cpa.eng.Summary())...)
					logger.LogAttrs(ctx, slog.LevelInfo, "zone status", ZoneStatusAttrs(zoneSrc.Current(), ground != nil, und != nil)...)
				}
			}
		},
	} {
		rt.Go(ctx, run)
	}
	return nil
}

// IntentsProbe is the readiness of intent_active: unknown while never
// read (every flight unknown), degraded with its age while the watch is
// down, up otherwise.
func IntentsProbe(m *bus.Mirror[intentBody]) obs.Probe {
	return func(context.Context) (obs.State, string) {
		_, age, loaded := m.Snapshot()
		switch {
		case !loaded:
			return obs.StateUnknown, "intent_active not read: every flight is unknown, nothing is judged for conformance (SC-22)"
		case age > 0:
			return obs.StateDegraded, fmt.Sprintf("intent_active: the watch is down; judged on the values read %.0f s ago", age)
		}
		return obs.StateUp, ""
	}
}

// flightEnds feeds the flight ends of flight.v1 to the engines (core
// subscription; a missed end is caught by the intent's end, and on the
// CPA path by the stale time).
func flightEnds(ctx context.Context, nc *bus.Conn, logger *slog.Logger, ended ...func(flightID string)) {
	stop, err := nc.Listen(SubjectFlightEnded, func(subject string, data []byte) {
		s, err := bus.Parse(subject)
		if err != nil || s.Kind != bus.KindFlight {
			return
		}
		var probe struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(data, &probe) != nil || !strings.HasPrefix(probe.Schema, "flight/event/") {
			return
		}
		for _, f := range ended {
			f(s.ID)
		}
	})
	if err != nil {
		logger.Warn("flight ends not subscribed; the intent's end clears the alerts", obs.Err(err))
		return
	}
	<-ctx.Done()
	stop()
}
