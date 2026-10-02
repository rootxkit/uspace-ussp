package telemetry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// E-03: every message this package writes validates against its schema
// (track/telemetry/v1 and the others are the pinned lab copies): a
// track, an identification change, a client status, a status frame, a
// gap record.
func TestMessagesValidateAgainstTheirSchemas(t *testing.T) {
	ss := schemas(t)
	r := newRig(t, rigOpts{geoid: true})
	f := frame(snA, 1, t0)
	f.IntentID = nil
	f.OperatorPosition = &OperatorPosition{Lat: 41.71, Lng: 44.82}
	one(t, r.take(clientA, nil, f), OutcomeAccepted)
	waitFor(t, func() bool { return len(r.pub.kind(bus.KindIdent)) == 1 })
	for _, m := range r.pub.kind(bus.KindTrk) {
		var v any
		_ = json.Unmarshal(m.data, &v)
		validate(t, ss, "track/telemetry/v1", v)
	}
	for _, m := range r.pub.kind(bus.KindIdent) {
		var v any
		_ = json.Unmarshal(m.data, &v)
		validate(t, ss, "ident/change/v1", v)
	}
	st := &Status{Ingest: r.in, Pub: r.pub, Sources: r.gate, Now: r.clk.now, Counters: r.counters}
	body := st.Body(clientA)
	validate(t, ss, "source/status/v1", &SourceStatus{Envelope: bus.SystemEnvelope(SchemaSourceStatus, Producer, t0), Body: body})
	s := r.in.NewSession(clientA, t0)
	srv := &Server{Ingest: r.in, Now: r.clk.now}
	validate(t, ss, "console/status/v1", srv.StatusFrame(s, body))
	r.gate.set(clientA, true)
	validate(t, ss, "source/status/v1", &SourceStatus{Envelope: bus.SystemEnvelope(SchemaSourceStatus, Producer, t0), Body: st.Body(clientA)})
	g := &SourceStatus{Envelope: bus.SystemEnvelope(SchemaSourceStatus, Producer, t0), Body: st.Body(clientA)}
	g.Body.Gap = &Gap{Cause: GapQueueAge, SourceInstance: clientA, GapStarted: t0, GapEnded: t0.Add(time.Second), Dropped: 2}
	validate(t, ss, "source/status/v1", g)
}

// The metre bounds follow the F3411 category names ("< X"); the codes
// order the categories as MAV_ODID_VER_ACC does, and the category worse
// than every code has none.
func TestAccuracyMappings(t *testing.T) {
	if v := verticalM(f3411.VA10m); v == nil || *v != 10 {
		t.Fatal(v)
	}
	if v := verticalM(f3411.VA150mPlus); v != nil {
		t.Fatal("a bound for more than 150 m")
	}
	if v := horizontalM(f3411.HA1NM); v == nil || *v != 1852 {
		t.Fatal(v)
	}
	if v := horizontalM(f3411.HAUnknown); v != nil {
		t.Fatal("a bound for unknown")
	}
	order := []f3411.VerticalAccuracy{f3411.VAUnknown, f3411.VA150m, f3411.VA45m, f3411.VA25m, f3411.VA10m, f3411.VA3m, f3411.VA1m}
	for i, a := range order {
		if verticalCode[a] != uint8(i) {
			t.Errorf("%s code %d, want %d", a, verticalCode[a], i)
		}
	}
	if _, ok := verticalCode[f3411.VA150mPlus]; ok {
		t.Error("VA150mPlus has a code")
	}
}
