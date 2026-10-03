//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/geo"
)

// The zone path of the monitor process on the real bus: a flight
// hovering at the rig's place raises nothing until a PROHIBITED zone is
// projected over it; the first sample after the projection and its
// cis.v1 message is judged against the zone (the delay is measured and
// printed: within one tick, Z-12); the zone withdrawn from the CIS ends
// the alert at once (not_reconfirmed, zone_no_longer_published).
func TestIntegrationZoneIncursionPushedWithinOneTick(t *testing.T) {
	g := newTrafficRig(t)
	op := g.ops[0]
	conn := g.nc
	type seen struct {
		at time.Time
		b  map[string]any
	}
	var mu sync.Mutex
	var got []seen
	c5, _, err := cell.Key(g.o)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := conn.Listen("alrt.v1.zone_incursion."+c5+".*", func(_ string, data []byte) {
		var m struct {
			Body map[string]any `json:"body"`
		}
		if json.Unmarshal(data, &m) == nil && m.Body["flight_id"] == op.flight {
			mu.Lock()
			got = append(got, seen{time.Now(), m.Body})
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	of := func(state string) []seen {
		mu.Lock()
		defer mu.Unlock()
		var out []seen
		for _, s := range got {
			if s.b["state"] == state {
				out = append(out, s)
			}
		}
		return out
	}
	a := g.fly(op, func(float64) (core.LatLon, float64, float64) { return g.o, 0, 0 })
	defer a.halt()
	// No zone: nothing for three samples.
	sent := a.seqN.Load()
	within(t, 10*time.Second, func() bool { return a.seqN.Load() >= sent+3 })
	if n := len(of("raised")); n != 0 {
		t.Fatalf("raised with no zone: %d", n)
	}
	// The projection of a PROHIBITED zone over the place, then cis.v1.
	box := geodesy.BBox{MinLat: g.o.LatDeg - 0.01, MinLon: g.o.LonDeg - 0.01, MaxLat: g.o.LatDeg + 0.01, MaxLon: g.o.LonDeg + 0.01}
	project := func(version string, withZone bool) time.Time {
		t.Helper()
		ce := cis.CellEntry{Cell: c5, CISVersion: version, At: time.Now().UTC()}
		if withZone {
			ce.Zones = []cis.ApplicableZone{{Identifier: "TZI001", Type: "PROHIBITED", Applies: true, CISApplicability: "applies", Version: "zones:9",
				Dataset: "zones", CISVersion: version, Feature: geoFeature("TZI001", "PROHIBITED", [4]float64{box.MinLat, box.MinLon, box.MaxLat, box.MaxLon}, 0, 1000, nil, nil)}}
		}
		if err := g.kv.PutJSON(context.Background(), bus.BucketCISCurrent, cell.KVToken(c5), ce); err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC()
		if err := g.kv.PutJSON(context.Background(), bus.BucketCISCurrent, cis.KeyBasis, cis.BasisValue{Basis: cis.Basis{CISVersion: version}, At: at, Cells: 1}); err != nil {
			t.Fatal(err)
		}
		subject, _ := bus.CIS("zones")
		if err := g.pub.Publish(context.Background(), subject, geo.ChangedMessageOf(cis.Change{Dataset: cis.Zones, Version: 9, FeatureIDs: []string{"TZI001"},
			Reason: cis.ChangeInstalled, CISVersion: version, At: at}, at)); err != nil {
			t.Fatal(err)
		}
		return at
	}
	t.Cleanup(func() {
		_ = g.kv.Delete(context.Background(), bus.BucketCISCurrent, cell.KVToken(c5))
		_ = g.kv.Delete(context.Background(), bus.BucketCISCurrent, cis.KeyBasis)
	})
	pushed := project("zones:9,uspace_airspace:1,restrictions:1", true)
	within(t, 10*time.Second, func() bool { return len(of("raised")) == 1 })
	r := of("raised")[0]
	captured, err := time.Parse(time.RFC3339Nano, r.b["captured_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	delay := captured.Sub(pushed)
	t.Logf("zone pushed (cis.v1) to the first judged sample: captured_at %v after the push; alert on the bus %v after it", delay, r.at.Sub(pushed))
	if delay > 2*time.Second {
		t.Fatalf("the first judged sample is %v after the push: beyond one tick", delay)
	}
	detail, _ := r.b["detail"].(map[string]any)
	if r.b["severity"] != "critical" || detail["zone_id"] != "TZI001" || detail["vertical_known"] != true {
		t.Fatalf("raised %+v", r.b)
	}
	// Withdrawn from the CIS: the alert ends at once.
	project("zones:10,uspace_airspace:1,restrictions:1", false)
	within(t, 10*time.Second, func() bool { return len(of("cleared")) == 1 })
	cl := of("cleared")[0]
	cd, _ := cl.b["clearing_detail"].(map[string]any)
	if cl.b["clear_reason"] != "not_reconfirmed" || cd["reason"] != "zone_no_longer_published" || cl.b["alert_id"] != r.b["alert_id"] {
		t.Fatalf("cleared %+v", cl.b)
	}
}
