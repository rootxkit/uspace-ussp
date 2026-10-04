package monitor

import (
	"context"
	"log/slog"
	"maps"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// Counters of the status written to monitor_status.
const (
	CounterStatusWritten     = "monitor_status_written"
	CounterStatusWriteFailed = "monitor_status_write_failed"
)

// StatusOf is the status line of instance at now as monitor_status holds
// it (WP-18): the conformance summary s, the CPA summary c, the zone set
// judged and the CIS freshness, the age of intent_active (none while it
// was never read), the policy version, and whether the terrain and the
// geoid are loaded.
func StatusOf(instance string, now time.Time, s Summary, c traffic.Summary, zs *geo.ZoneSet, f geo.Freshness,
	intentsAge float64, intentsLoaded bool, policyVersion int64, terrainKnown, geoidKnown bool) bus.MonitorStatus {
	st := bus.MonitorStatus{
		Instance: instance, At: now.UTC(), Workers: s.Workers, FlightsTracked: s.Flights, States: maps.Clone(s.States),
		EvaluationPeriodS: s.EvaluationPeriodS, CPAEvaluationPeriodS: c.EvaluationPeriodS, OutboxDepth: s.OutboxDepth,
		PolicyVersion: policyVersion, CISVersion: f.CISVersion, CISAgeS: f.CISAgeS, CISStale: f.Stale,
		Terrain: terrainKnown, Geoid: geoidKnown,
	}
	if st.States == nil {
		st.States = map[string]int{}
	}
	if zs != nil {
		st.CISLoaded = zs.Loaded
	}
	if intentsLoaded {
		age := intentsAge
		st.IntentActiveAgeS = &age
	}
	return st
}

// kvPutter writes one key (bus.KVStore).
type kvPutter interface {
	Put(ctx context.Context, key string, value []byte) error
}

// statusWriter writes an instance's status to monitor_status, each put
// bounded by the KV store's timeout; a put that fails is counted and
// logged, and the next period writes again (the console says the
// monitor is down only after monitor_status_missing_s without one).
type statusWriter struct {
	KV       kvPutter
	Counters *core.Counters
	Logger   *slog.Logger
}

func (w *statusWriter) write(ctx context.Context, st bus.MonitorStatus) {
	data, err := bus.EncodeMonitorStatus(st)
	if err == nil {
		err = w.KV.Put(ctx, bus.KeyToken(st.Instance), data)
	}
	if err != nil {
		w.Counters.Inc(CounterStatusWriteFailed)
		if ctx.Err() == nil {
			w.Logger.LogAttrs(ctx, slog.LevelWarn, "monitor status not written; the next period writes it",
				slog.String("bucket", bus.BucketMonitorStatus), obs.Err(err))
		}
		return
	}
	w.Counters.Inc(CounterStatusWritten)
}
