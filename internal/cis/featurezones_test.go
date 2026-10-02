package cis

import (
	"encoding/json"
	"testing"

	"github.com/rootxkit/uspace-core/core"
)

// The feature as the projection lists it builds the same zones the
// Evaluator judges with: inside the box it contains the fixture point,
// and a feature Parse refuses is an error, never a zone (E-01 pair).
func TestFeatureZones(t *testing.T) {
	v := mustVersion(t, USpaceAirspace, 3, airspace("A1").json())
	es, rf := buildEntries(v)
	if rf != nil {
		t.Fatal(rf)
	}
	zs, err := FeatureZones(es[0].Raw)
	if err != nil || len(zs) != len(es[0].Parts) || len(zs) != 1 {
		t.Fatalf("zones %v %v", zs, err)
	}
	in, err := zs[0].ContainsHorizontally(core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271})
	if err != nil || !in {
		t.Fatalf("inside: %v %v", in, err)
	}
	out, err := zs[0].ContainsHorizontally(core.LatLon{LatDeg: 41.9, LonDeg: 44.8271})
	if err != nil || out {
		t.Fatalf("outside: %v %v", out, err)
	}
	if zs[0].Type != core.ZoneUSpace {
		t.Fatalf("type %q", zs[0].Type)
	}
	for _, bad := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`[1]`), json.RawMessage(`{"type":"Feature"}`)} {
		if zs, err := FeatureZones(bad); err == nil {
			t.Errorf("%s: zones %v", bad, zs)
		}
	}
}
