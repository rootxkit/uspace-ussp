package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

type putter struct {
	err  error
	keys []string
	vals [][]byte
}

func (p *putter) Put(_ context.Context, key string, value []byte) error {
	if p.err != nil {
		return p.err
	}
	p.keys, p.vals = append(p.keys, key), append(p.vals, value)
	return nil
}

// The status line written to monitor_status (WP-18) carries the
// conformance, CPA and zone paths' summary and round-trips; a put that
// fails is counted and nothing is held (the presence twin writes it).
func TestStatusWritten(t *testing.T) {
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	st := StatusOf("mon-1", at, Summary{Workers: 2, Flights: 3, States: map[string]int{"conforming": 3}, EvaluationPeriodS: 1.2, OutboxDepth: 4},
		traffic.Summary{EvaluationPeriodS: 0.9}, &geo.ZoneSet{Loaded: true}, geo.Freshness{CISVersion: "zones:3", CISAgeS: 12, Stale: false},
		5, true, 7, false, true)
	if st.IntentActiveAgeS == nil || *st.IntentActiveAgeS != 5 || !st.CISLoaded || st.Terrain || !st.Geoid || st.EvaluationPeriodS != 1.2 ||
		st.CPAEvaluationPeriodS != 0.9 || st.PolicyVersion != 7 || st.CISVersion != "zones:3" {
		t.Fatalf("%+v", st)
	}
	if never := StatusOf("mon-1", at, Summary{}, traffic.Summary{}, nil, geo.Freshness{}, 0, false, 0, false, false); never.IntentActiveAgeS != nil ||
		never.States == nil || never.CISLoaded {
		t.Fatalf("never read: %+v", never)
	}
	c := &core.Counters{}
	failing := &putter{err: errors.New("kv down")}
	w := &statusWriter{KV: failing, Counters: c, Logger: obs.Discard()}
	w.write(context.Background(), st)
	if c.Snapshot()[CounterStatusWriteFailed] != 1 || len(failing.keys) != 0 {
		t.Fatalf("failed put: %+v", c.Snapshot())
	}
	ok := &putter{}
	w.KV = ok
	w.write(context.Background(), st)
	if c.Snapshot()[CounterStatusWritten] != 1 || len(ok.keys) != 1 || ok.keys[0] != bus.KeyToken("mon-1") {
		t.Fatalf("put: %+v %v", c.Snapshot(), ok.keys)
	}
	back, err := bus.DecodeMonitorStatus(ok.vals[0])
	if err != nil || back.Instance != "mon-1" || back.FlightsTracked != 3 || back.States["conforming"] != 3 {
		t.Fatalf("round trip %+v %v", back, err)
	}
	if _, err := bus.DecodeMonitorStatus([]byte(`{"instance":"m","at":"2026-10-05T09:00:00Z","extra":1}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	if _, err := bus.DecodeMonitorStatus(make([]byte, bus.MonitorStatusBytes+1)); err == nil {
		t.Fatal("an oversized status was accepted")
	}
}
