package geo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// memMirror is cis_current as a mirror in memory.
type memMirror struct {
	mu     sync.Mutex
	vals   map[string]telemetry.CISValue
	loaded bool
	snaps  int
}

func (m *memMirror) Snapshot() (map[string]telemetry.CISValue, float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps++
	out := make(map[string]telemetry.CISValue, len(m.vals))
	for k, v := range m.vals {
		out[k] = v
	}
	return out, 0, m.loaded
}

func (m *memMirror) Get(key string) (telemetry.CISValue, bool, float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[key]
	return v, ok, 0, m.loaded
}

func (m *memMirror) put(vals map[string]telemetry.CISValue) {
	m.mu.Lock()
	m.vals, m.loaded = vals, true
	m.mu.Unlock()
}

// ZoneSource: nothing read is a set that is not loaded (never an empty
// sky); a projection is built once and kept while its basis is the
// same; a new basis builds a new set (Z-12).
func TestZoneSourceRebuildsOnANewBasis(t *testing.T) {
	m := &memMirror{}
	src := &ZoneSource{M: m, Now: time.Now}
	s0 := src.Current()
	if s0 == nil || s0.Loaded || len(s0.Zones) != 0 {
		t.Fatalf("unread %+v", s0)
	}
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	m.put(projection("zones:1", at, map[string][]feat{"zones": {amslZone("TZP001", "PROHIBITED")}}))
	s1 := src.Current()
	if !s1.Loaded || len(s1.Zones) != 1 || s1.Key == "" {
		t.Fatalf("first %+v", s1)
	}
	snaps := m.snaps
	if s := src.Current(); s != s1 || m.snaps != snaps {
		t.Fatal("rebuilt with the same basis")
	}
	m.put(projection("zones:2", at.Add(time.Minute), map[string][]feat{"zones": {amslZone("TZP001", "PROHIBITED"), amslZone("TZP002", "PROHIBITED")}}))
	if s2 := src.Current(); s2 == s1 || len(s2.Zones) != 2 || s2.CISVersion != "zones:2" {
		t.Fatalf("second %+v", s2)
	}
}

// What the set holds: restrictions ended or cancelled are left out, a
// feature listed under two cells once, a feature that does not build is
// named (never silent), an open-ended daylight schedule is held as
// applying always and said so, and the bound is said (E-10).
func TestBuildZoneSetContents(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	ended, cancelled, planned := amslZone("TRS001", "PROHIBITED"), amslZone("TRS002", "PROHIBITED"), amslZone("TRS003", "PROHIBITED")
	vals := projection("v", at, map[string][]feat{"restrictions": {ended, cancelled, planned}})
	ce := vals["c5test"].Cell
	ce.Zones[0].RestrictionState, ce.Zones[1].RestrictionState, ce.Zones[2].RestrictionState = "ended", "cancelled", "planned"
	// The same feature under a second cell, and one that does not build.
	twin := *ce
	twin.Zones = []cis.ApplicableZone{ce.Zones[2], {Identifier: "BAD", Type: "PROHIBITED", Dataset: "zones", Version: "zones:1",
		Feature: []byte(`{"type":"Feature"}`)}}
	vals["c5twin"] = telemetry.CISValue{Cell: &twin}
	dl := amslZone("TZD001", "PROHIBITED")
	dl.applicability = []any{map[string]any{"schedule": []any{map[string]any{"day": []string{"ANY"}, "startEvent": "SR", "endEvent": "SS"}}}}
	for k, v := range projection("v", at, map[string][]feat{"zones": {dl}}) {
		if k != cis.KeyBasis {
			vals["c5dl"] = v
		}
	}
	counters := &core.Counters{}
	s := BuildZoneSet(vals, counters, at)
	ids := map[string]ZoneMeta{}
	for _, z := range s.Zones {
		ids[s.Meta[z].Identifier] = s.Meta[z]
	}
	if len(ids) != 2 || ids["TRS003"].RestrictionState != "planned" || !ids["TZD001"].AlwaysApplies {
		t.Fatalf("held %+v", ids)
	}
	if len(s.Unbuildable) != 1 || s.Unbuildable[0] != "zones/BAD" || counters.Get(CounterZoneUnbuildable) != 1 || counters.Get(CounterZoneAlwaysApplies) != 1 {
		t.Fatalf("unbuildable %v %v", s.Unbuildable, counters.Snapshot())
	}
	if !s.Has("TRS003") || s.Has("TRS001") || (*ZoneSet)(nil).Has("x") {
		t.Fatal("Has")
	}
	old := zoneBound
	zoneBound = 1
	defer func() { zoneBound = old }()
	if s := BuildZoneSet(vals, counters, at); !s.OverBound || len(s.Zones) != 1 || counters.Get(CounterZoneSetOverBound) == 0 {
		t.Fatalf("bound %+v", s)
	}
}

// Inside a PROHIBITED zone an unidentified aircraft also raises
// identification (G-03), and an identification mismatch raises
// identification_mismatch anywhere (G-02).
func TestIdentificationKinds(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	g := newRig(t, setOf(t, "v", at, map[string][]feat{"zones": {amslZone("TZP001", "PROHIBITED")}}))
	g.clk = g.clk.Add(time.Second)
	tr := g.track(inside, 300, true)
	tr.Identification = &core.Identification{Status: core.IdentUnidentified, Reason: core.IdentReason("no_identity")}
	evs := g.tr.Observe(tr, g.ref, g.wallS(), g.clk)
	kinds := map[string]Event{}
	for i := range evs {
		kinds[evs[i].Alert.Kind] = evs[i]
	}
	if _, ok := kinds[KindZoneIncursion]; !ok {
		t.Fatalf("no zone alert: %s", states(evs))
	}
	if e, ok := kinds[KindIdentification]; !ok || e.Alert.Detail["zone_id"] != "TZP001" || e.Alert.Detail["status"] != "unidentified" {
		t.Fatalf("identification: %s %+v", states(evs), e)
	}
	g.clk = g.clk.Add(time.Second)
	mm := g.track(westOut, 300, true)
	mm.Identification = &core.Identification{Status: core.IdentRegistered, Mismatch: true}
	evs = g.tr.Observe(mm, g.ref, g.wallS(), g.clk)
	found := false
	for i := range evs {
		if evs[i].Alert.Kind == KindIdentificationMismatch && evs[i].State == StateRaised {
			found = true
		}
	}
	if !found {
		t.Fatalf("mismatch: %s", states(evs))
	}
}

// The service answers an intent from its windows, passes the source
// error on, and refuses a box the cache does not answer with its
// problem.
func TestServiceIntentAndRefusals(t *testing.T) {
	g := newCISRig(t)
	g.publish(t, cis.Zones, amslZone("TZP001", "PROHIBITED"))
	svc := &Service{CIS: g.eval, Now: g.clock}
	src := windowsFunc(func(context.Context, string, string) ([]geodesy.BBox, []time.Time, []time.Time, error) {
		b := geodesy.BBox{MinLon: zoneBox[0], MinLat: zoneBox[1], MaxLon: zoneBox[2], MaxLat: zoneBox[3]}
		return []geodesy.BBox{b, b}, []time.Time{g.now, g.now.Add(-time.Hour)}, []time.Time{g.now.Add(time.Hour), g.now.Add(2 * time.Hour)}, nil
	})
	a, err := svc.Intent(t.Context(), src, "client", "id-1")
	if err != nil || a.IntentID == nil || *a.IntentID != "id-1" || len(a.Zones) != 1 || !a.From.Equal(g.now.Add(-time.Hour)) || !a.To.Equal(g.now.Add(2*time.Hour)) {
		t.Fatalf("%+v %v", a, err)
	}
	failing := windowsFunc(func(context.Context, string, string) ([]geodesy.BBox, []time.Time, []time.Time, error) {
		return nil, nil, nil, errors.New("not found")
	})
	if _, err := svc.Intent(t.Context(), failing, "client", "id-1"); err == nil {
		t.Fatal("the source error was lost")
	}
	_, err = svc.Box(geodesy.BBox{MinLat: 10, MaxLat: 5}, time.Time{})
	var na *NotAnsweredError
	if !errors.As(err, &na) || na.ProblemSlug() != "geo_query_invalid" || na.ProblemDetail() == "" || na.Error() == "" {
		t.Fatalf("invalid box: %v", err)
	}
}

type windowsFunc func(ctx context.Context, clientID, intentID string) ([]geodesy.BBox, []time.Time, []time.Time, error)

func (f windowsFunc) Windows(ctx context.Context, clientID, intentID string) ([]geodesy.BBox, []time.Time, []time.Time, error) {
	return f(ctx, clientID, intentID)
}

// Freshness: nothing read is stale; the bound is the policy's
// cis_stale_s as configured, read on every call, the age at the bound
// is fresh and one past it is stale.
func TestZoneSourceFreshnessBound(t *testing.T) {
	m := &memMirror{}
	at := time.Date(2026, 10, 4, 6, 25, 48, 0, time.UTC)
	clk := at
	bound := 60.0
	src := &ZoneSource{M: m, Now: func() time.Time { return clk }, StaleS: func() float64 { return bound }}
	if f := src.Freshness(); f.Loaded || !f.Stale {
		t.Fatalf("unread %+v", f)
	}
	vals := projection("zones:1", at, map[string][]feat{"zones": {amslZone("TZP001", "PROHIBITED")}})
	vals[cis.KeyBasis].Basis.CISAgeS = 11
	m.put(vals)
	clk = at.Add(49 * time.Second)
	if f := src.Freshness(); !f.Loaded || f.Stale || f.CISAgeS != 60 || f.CISVersion != "zones:1" {
		t.Fatalf("at the bound %+v", f)
	}
	clk = clk.Add(time.Second)
	if f := src.Freshness(); !f.Stale || f.CISAgeS != 61 {
		t.Fatalf("past the bound %+v", f)
	}
	bound = 300
	if f := src.Freshness(); f.Stale {
		t.Fatalf("under a raised bound %+v", f)
	}
}
