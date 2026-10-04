package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geoid"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// Readiness dependencies of the CPA path.
const (
	DepProximityState = "proximity_state"
	DepMan            = "man"
	DepPeer           = "peer"
	DepGeoid          = "geoid"
)

// Counters of the CPA feed.
const (
	CounterCPAUnreadable = "cpa_track_unreadable"
)

// cpaPath is the CPA path of the process (brief WP-11): one
// traffic.Engine for the owned cell set, fed every trk.v1, peer.v1 and
// man.v1 sample of the owned cells and their ring-1 neighbours.
type cpaPath struct {
	eng      *traffic.Engine
	und      geoid.Undulator
	counters *core.Counters
	logger   *slog.Logger
	// policy is the current policy row's values (the altitude selection
	// of a manned aircraft).
	policy func() policy.Values
	// onOwn is told of every sample of this USSP's flights on trk.v1
	// (the echo guard of the peer and manned inputs, PLAN §15 Q23).
	onOwn func(in traffic.Input)
}

// offerTrack reads one trk.v1 or peer.v1 message and offers it.
func (c *cpaPath) offerTrack(ns string, data []byte) {
	tr, err := traffic.DecodeTrack(data)
	if err != nil {
		c.counters.Inc(CounterCPAUnreadable)
		return
	}
	in := traffic.TrackInputOf(ns, tr)
	if ns == traffic.NSTrack && in.Own() && c.onOwn != nil {
		c.onOwn(in)
	}
	c.eng.Offer(in)
}

// offerManned reads one man.v1 message and offers it.
func (c *cpaPath) offerManned(data []byte) {
	m, err := traffic.DecodeManned(data)
	if err != nil {
		c.counters.Inc(CounterCPAUnreadable)
		return
	}
	c.eng.Offer(traffic.MannedInputOf(m, c.und, c.policy().AltPolicy()))
}

// startCPA builds the CPA path: the engine with its persisted alerts
// (proximity_state), its own follower of the source switches (each
// state goes to core's SwitchSource), the geoid for manned AMSL, and the
// man.v1 and peer.v1 feeds; the trk.v1 feed is shared with the
// conformance path. It runs everything but the trk feed.
func startCPA(ctx context.Context, rt *proc.Runtime, own cell.Ownership, current func() policy.Record,
	intents *bus.Mirror[intentBody], instance string, und geoid.Undulator, missing string, o Options) (*cpaPath, error) {
	js := rt.Bus.JetStream()
	logger := rt.Logger
	counters := &core.Counters{}
	proc.Publish(rt, "cpa", counters)
	storeCounters := &core.Counters{}
	proc.Publish(rt, "proximity_state", storeCounters)
	mirror := &bus.Mirror[traffic.Saved]{JS: js, Bucket: bus.BucketProximityState, Decode: traffic.DecodeSaved, Counters: storeCounters, Logger: logger}
	store := traffic.KVStates{M: mirror, KV: bus.KVStore{JS: js, Bucket: bus.BucketProximityState}}
	eng := &traffic.Engine{
		Policy: current, Sink: bus.NewPublisher(rt.Bus, counters), Store: store, Ownership: own, InstanceID: instance,
		Authorisation: func(intentID string) string {
			if !bus.ValidKey(intentID) {
				return ""
			}
			if b, found, _, _ := intents.Get(intentID); found && b.AuthorisationNumber != nil {
				return *b.AuthorisationNumber
			}
			return ""
		},
		Counters: counters, Logger: logger, Tick: o.Tick,
	}
	if o.CPA != nil {
		o.CPA(eng)
	}
	c := &cpaPath{eng: eng, und: und, counters: counters, logger: logger, policy: func() policy.Values { return current().Values }}
	rt.Health.Register(DepProximityState, false, func(context.Context) (obs.State, string) {
		_, age, loaded := mirror.Snapshot()
		switch {
		case !loaded:
			return obs.StateUnknown, "proximity_state not read: saved proximity alerts are not carried yet"
		case age > 0:
			return obs.StateDegraded, fmt.Sprintf("proximity_state: the watch is down; last read %.0f s ago", age)
		}
		return obs.StateUp, ""
	})
	switches := &bus.Follower[coresources.State]{
		JS: js, Bucket: sources.BucketSourceControl, Key: bus.KeySources, Decode: bus.DecodeSources,
		Apply: eng.SwitchSource, Core: rt.Bus.Conn, Push: bus.CtlSources, Logger: logger, Counters: &core.Counters{},
	}
	feeds := map[string]struct {
		stream string
		take   func([]byte)
		dep    string
	}{
		bus.KindMan:  {bus.StreamMAN, c.offerManned, DepMan},
		bus.KindPeer: {bus.StreamPEER, func(d []byte) { c.offerTrack(traffic.NSPeer, d) }, DepPeer},
	}
	for kind, f := range feeds {
		subjects, err := Subjects(kind, own)
		if err != nil {
			return nil, err
		}
		fd := &Feed{
			Subjects: subjects, Take: f.take, Logger: logger,
			Open: func(ctx context.Context, subject string, from time.Time, handle func([]byte)) (func(), error) {
				return bus.Replay{JS: js, Stream: f.stream, Subject: subject}.Open(ctx, from, handle)
			},
			From: func() time.Time { return time.Now().Add(-secs(current().Values.MonitorLiveMaxAgeS)) },
		}
		rt.Health.Register(f.dep, false, fd.Probe)
		rt.Go(ctx, func(ctx context.Context) {
			select {
			case <-ctx.Done():
				return
			case <-eng.Seeded():
			}
			fd.Run(ctx)
		})
	}
	for _, run := range []func(context.Context){mirror.Run, switches.Run, eng.Run} {
		rt.Go(ctx, run)
	}
	if und == nil {
		logger.Warn("no geoid: manned aircraft have no AMSL altitude and are judged on the horizontal alone (R-09)", slog.String("geoid", missing))
	}
	return c, nil
}

// loadGeoid is the geoid grid of path, loaded with core's
// geoid.LoadMapped (WP-19: a read-only memory map on linux, shared in the
// page cache by every process on the host; read into memory elsewhere),
// and whether it is mapped; nil with why when there is none.
func loadGeoid(path string) (und geoid.Undulator, mapped bool, why string) {
	if path == "" {
		return nil, false, "USSP_GEOID_FILE is not set: manned aircraft have no AMSL altitude and are judged on the horizontal alone (R-09)"
	}
	g, err := geoid.LoadMapped(path)
	if err != nil {
		return nil, false, "the geoid grid of USSP_GEOID_FILE does not load (" + err.Error() + "): manned aircraft are judged on the horizontal alone"
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

// CPAStatusAttrs are the CPA path's attributes of the status line.
func CPAStatusAttrs(s traffic.Summary) []slog.Attr {
	return []slog.Attr{
		slog.Int("aircraft_tracked", s.Tracked), slog.Int("proximity_active", s.Active), slog.Int("proximity_carried", s.Carried),
		slog.Float64("cpa_evaluation_period_s", s.EvaluationPeriodS), slog.Float64("pair_checks_per_s", s.PairChecksPerS),
		slog.Bool("cpa_capacity_exceeded", s.CapacityExceeded), slog.Bool("proximity_state_loaded", s.StoreLoaded),
		slog.Int("cpa_outbox_depth", s.OutboxDepth),
	}
}
