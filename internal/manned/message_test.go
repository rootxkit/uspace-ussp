package manned

import (
	"context"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

func TestNormRegistration(t *testing.T) {
	for _, c := range [][2]string{{"4L-ABC", "4LABC"}, {" 4l.abc ", "4LABC"}, {"", ""}, {"D-EABC", "DEABC"}} {
		if got := NormRegistration(c[0]); got != c[1] {
			t.Errorf("%q -> %q, want %q", c[0], got, c[1])
		}
	}
}

// publish: an echo is never sent; a position no cell holds and a bus
// that refuses are counted; a track goes to man.v1.<cell3>.<cell5>.<icao24>.
func TestPublish(t *testing.T) {
	c := &core.Counters{}
	s := &sink{}
	now := time.Now()
	m := &Track{Envelope: bus.NewEnvelope(SchemaManned, ProducerANSP, core.Times{RxTS: now, CapturedAt: now, Source: core.TimeReceiver}),
		Body: Body{ICAO24: "4ca123", Position: Position{Lat: 41.75, Lng: 44.85}, SourceClass: "ads_b", Trust: core.TrustSurveillance,
			Source: SourceANSPFeed, SourceInstance: "a-1", State: StateLive}}
	if fid, err := publish(context.Background(), s, own{reg: "X1", flight: "f"}, c, m, func() *string { s := "x1"; return &s }()); fid != "f" || err != nil || len(s.msgs) != 0 {
		t.Fatalf("echo published: %q %v", fid, err)
	}
	if _, err := publish(context.Background(), s, nil, c, m, nil); err != nil || len(s.msgs) != 1 || s.msgs[0].subject != "man.v1.c3:131:224.c5:1317:2248.4ca123" {
		t.Fatalf("published %v %+v", err, s.msgs)
	}
	m.Body.Position = Position{Lat: 95}
	if _, err := publish(context.Background(), s, nil, c, m, nil); err == nil {
		t.Fatal("a position no cell holds was published")
	}
	if c.Get(CounterPublished) != 1 || c.Get(CounterPublishFailed) != 1 || c.Get(CounterEchoOwnFlight) != 1 {
		t.Fatalf("counters %v", c.Snapshot())
	}
}
