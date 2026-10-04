package monitor

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// cisMirror is cis_current as the monitor's mirror holds it, replaced
// by a test.
type cisMirror struct {
	mu   sync.Mutex
	vals map[string]telemetry.CISValue
}

func (m *cisMirror) Snapshot() (map[string]telemetry.CISValue, float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]telemetry.CISValue, len(m.vals))
	for k, v := range m.vals {
		out[k] = v
	}
	return out, 0, m.vals != nil
}

func (m *cisMirror) Get(key string) (telemetry.CISValue, bool, float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[key]
	return v, ok, 0, m.vals != nil
}

// project writes a projection of one PROHIBITED zone with this basis,
// as cis.BusProjector does.
func (m *cisMirror) project(b cis.Basis, at time.Time) {
	ce := &cis.CellEntry{Cell: "c5:test", CISVersion: b.CISVersion, CISAgeS: b.CISAgeS, Stale: b.Stale, At: at,
		Zones: []cis.ApplicableZone{{Identifier: "TZP001", Type: "PROHIBITED", Applies: true, Version: "zones:1",
			Dataset: "zones", Feature: zoneFeature("TZP001"), At: at, CISVersion: b.CISVersion}}}
	m.mu.Lock()
	m.vals = map[string]telemetry.CISValue{
		"c5test":     {Cell: ce},
		cis.KeyBasis: {Basis: &cis.BasisValue{Basis: b, At: at, Cells: 1}},
	}
	m.mu.Unlock()
}

// The age of the CIS the monitor judges with is the age the api
// projected plus the time since it projected it, on the monitor's clock
// (as traffic-ws ages the same basis): a projection that is not
// refreshed goes stale past cis_stale_s, and one that is refreshed with
// the same versions is fresh again without rebuilding the zones (a
// rebuild carries every active zone alert).
//
// On the demo deploy the monitor logged cis_age_s 11.032296318 with
// cis_stale true for ten minutes on end: the basis of the api's warm
// start, never aged and never rewritten.
func TestZoneSourceAgesTheProjectedBasis(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 6, 25, 48, 0, time.UTC)
	clk := t0
	m := &cisMirror{}
	src := &geo.ZoneSource{M: m, Now: func() time.Time { return clk }}
	probe := zoneProbe(src)

	// Absence: projected 11 s old, read 10 s later: fresh, 21 s.
	m.project(cis.Basis{CISVersion: "zones:1,uspace_airspace:1,restrictions:0", CISAgeS: 11}, t0)
	clk = t0.Add(10 * time.Second)
	set := src.Current()
	if st, why := probe(t.Context()); st != "up" {
		t.Fatalf("fresh projection: %s %q", st, why)
	}
	if f := src.Freshness(); !f.Loaded || f.Stale || f.CISAgeS != 21 {
		t.Fatalf("fresh freshness %+v", f)
	}

	// Presence: the api stops refreshing it (killed, or the KV is
	// unreachable): past the policy's cis_stale_s (300 s) it is stale.
	clk = t0.Add(290 * time.Second)
	if st, why := probe(t.Context()); st != "degraded" || !strings.Contains(why, "age 301 s") {
		t.Fatalf("unrefreshed projection: %s %q", st, why)
	}
	if f := src.Freshness(); !f.Stale || f.CISAgeS != 301 {
		t.Fatalf("unrefreshed freshness %+v", f)
	}
	var attrs = map[string]string{}
	for _, a := range ZoneStatusAttrs(src.Current(), src.Freshness(), false, true) {
		attrs[a.Key] = a.Value.String()
	}
	if attrs["cis_stale"] != "true" || attrs["cis_age_s"] != "301" {
		t.Fatalf("status attrs %v", attrs)
	}

	// The api confirms the same versions and projects again: fresh, and
	// the same zone set (no rebuild).
	m.project(cis.Basis{CISVersion: "zones:1,uspace_airspace:1,restrictions:0", CISAgeS: 0}, clk)
	clk = clk.Add(5 * time.Second)
	if st, why := probe(t.Context()); st != "up" {
		t.Fatalf("refreshed projection: %s %q", st, why)
	}
	if src.Current() != set {
		t.Fatal("the zone set was rebuilt for a refresh of the same versions")
	}

	// A basis the api projected stale (a dataset never loaded) stays
	// stale however young.
	m.project(cis.Basis{CISVersion: "zones:1,uspace_airspace:1", CISAgeS: 0, Stale: true}, clk)
	if st, _ := probe(t.Context()); st != "degraded" {
		t.Fatalf("projected stale: %s", st)
	}
	if src.Current() == set {
		t.Fatal("new versions kept the old zone set")
	}

	// A clock behind the api's is no younger than the projection.
	m.project(cis.Basis{CISVersion: "zones:1,uspace_airspace:1,restrictions:0", CISAgeS: 7}, clk.Add(time.Minute))
	if f := src.Freshness(); f.Stale || f.CISAgeS != 7 {
		t.Fatalf("behind the api's clock %+v", f)
	}
}
