package cis

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

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

// FeatureZonesApplicable keeps the periods (core judges the instant),
// holds a zone whose daylight schedule has no dates as applying always
// and says so, and refuses what does not parse (E-01 pairs).
func TestFeatureZonesApplicable(t *testing.T) {
	week := prohibited("W1")
	week.applicability = []any{map[string]any{"startDateTime": "2026-10-10T00:00:00Z", "endDateTime": "2026-10-11T00:00:00Z"}}
	zs, err := FeatureZonesApplicable(week.json())
	if err != nil || len(zs) != 1 || len(zs[0].Periods) == 0 {
		t.Fatalf("dated zone %v %v", zs, err)
	}
	if zs[0].AppliesAt(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)) || !zs[0].AppliesAt(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("the dated zone's periods are not core's")
	}
	always, err := FeatureZonesApplicable(prohibited("A1").json())
	if err != nil || len(always) != 1 || len(always[0].Periods) != 0 {
		t.Fatalf("zone without applicability %v %v", always, err)
	}
	dl := prohibited("D1")
	dl.applicability = []any{map[string]any{"schedule": []any{map[string]any{"day": []string{"ANY"}, "startEvent": "SR", "endEvent": "SS"}}}}
	zs, err = FeatureZonesApplicable(dl.json())
	if !errors.Is(err, ErrApplicabilityNotBuilt) || len(zs) != 1 || len(zs[0].Periods) != 0 {
		t.Fatalf("open daylight zone %v %v", zs, err)
	}
	if zs, err := FeatureZonesApplicable(json.RawMessage(`{"type":"Feature"}`)); err == nil || errors.Is(err, ErrApplicabilityNotBuilt) || zs != nil {
		t.Fatalf("unparsable %v %v", zs, err)
	}
}
