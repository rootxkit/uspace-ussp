package records

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

func statusMsg(t *testing.T, gap *telemetry.Gap) []byte {
	t.Helper()
	inst := "client-1"
	m := telemetry.SourceStatus{Envelope: bus.SystemEnvelope("source/status/v1", "ussp/telemetry-ingest", t0),
		Body: telemetry.SourceStatusBody{Source: telemetry.SourceOperatorWS, SourceInstance: &inst, State: telemetry.StateLive, Since: bus.Stamp{Time: t0},
			Counters: map[string]uint64{}, Gap: gap}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A gap on src.v1 is decoded and stored with its window; a status
// without one stores nothing (E-01 pair); an unreadable message is
// counted.
func TestGapRecorder(t *testing.T) {
	m := newMemReader(t0)
	r := NewGapRecorder(m, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	g := &telemetry.Gap{Cause: telemetry.GapQueueAge, SourceInstance: "client-1", GapStarted: t0, GapEnded: t0.Add(5 * time.Second), Dropped: 5, FromSeq: 10, ToSeq: 14}
	r.Take("src.v1.operator_ws.client-1", statusMsg(t, g))
	r.Take("src.v1.operator_ws.client-1", statusMsg(t, nil))
	r.Take("src.v1.operator_ws.client-1", []byte(`{`))
	deadline := time.Now().Add(5 * time.Second)
	for len(m.storedGaps()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := m.storedGaps()
	if len(got) != 1 || got[0].Cause != telemetry.GapQueueAge || got[0].Dropped != 5 || !got[0].Ended.Equal(t0.Add(5*time.Second)) || got[0].MsgID == "" {
		t.Fatalf("stored %+v", got)
	}
	if r.Counters.Get(CounterGapsUnreadable) != 1 {
		t.Errorf("counters %v", r.Counters.Snapshot())
	}
}

func TestDecodeGapRefusals(t *testing.T) {
	ok := telemetry.Gap{Cause: "c", SourceInstance: "s", GapStarted: t0, GapEnded: t0.Add(time.Second), Dropped: 1}
	for name, mutate := range map[string]func(*telemetry.Gap){
		"no cause":      func(g *telemetry.Gap) { g.Cause = "" },
		"no instance":   func(g *telemetry.Gap) { g.SourceInstance = "" },
		"negative":      func(g *telemetry.Gap) { g.Dropped = -1 },
		"end before":    func(g *telemetry.Gap) { g.GapEnded = t0.Add(-time.Second) },
		"half a window": func(g *telemetry.Gap) { g.GapEnded = time.Time{} },
		"long cause":    func(g *telemetry.Gap) { g.Cause = strings.Repeat("c", 65) },
		"long instance": func(g *telemetry.Gap) { g.SourceInstance = strings.Repeat("s", 129) },
	} {
		g := ok
		mutate(&g)
		if _, _, err := DecodeGap(statusMsg(t, &g)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	unread := ok
	unread.GapStarted, unread.GapEnded = time.Time{}, time.Time{}
	if g, found, err := DecodeGap(statusMsg(t, &unread)); err != nil || !found || !g.Started.IsZero() {
		t.Errorf("a gap of samples removed unread: %+v %v %v", g, found, err)
	}
	if _, _, err := DecodeGap(make([]byte, maxStatusBytes+1)); err == nil {
		t.Error("oversized message accepted")
	}
}

// E-10: past GapQueue waiting gaps, a gap is dropped and counted, never
// blocks the subscription; a store that fails is counted.
func TestGapQueueBound(t *testing.T) {
	m := newMemReader(t0)
	r := NewGapRecorder(m, &core.Counters{}, nil)
	g := &telemetry.Gap{Cause: "c", SourceInstance: "s", GapStarted: t0, GapEnded: t0, Dropped: 1}
	for range GapQueue + 3 {
		r.Take("s", statusMsg(t, g))
	}
	if r.Counters.Get(CounterGapsDropped) != 3 {
		t.Fatalf("dropped %d", r.Counters.Get(CounterGapsDropped))
	}
	m.errOf["record_gap"] = errDown
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.Counters.Get(CounterGapsFailed) < GapQueue && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if r.Counters.Get(CounterGapsFailed) != GapQueue {
		t.Fatalf("failed %d", r.Counters.Get(CounterGapsFailed))
	}
}
