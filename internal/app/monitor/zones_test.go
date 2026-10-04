package monitor

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// zoneFeature is an ED-318 PROHIBITED zone 0-1000 m AMSL over a box
// around origin.
func zoneFeature(id string) json.RawMessage {
	poly := []any{[]any{
		[]any{44.80, 41.70}, []any{44.85, 41.70}, []any{44.85, 41.73}, []any{44.80, 41.73}, []any{44.80, 41.70},
	}}
	b, _ := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{"type": "Polygon", "coordinates": poly,
			"layer": map[string]any{"uom": "m", "lower": 0, "lowerReference": "AMSL", "upper": 1000, "upperReference": "AMSL"}},
		"properties": map[string]any{
			"identifier": id, "country": "GEO", "name": []any{map[string]any{"text": "Test zone", "lang": "en-GB"}},
			"type": "PROHIBITED", "variant": "COMMON", "reason": []string{"SENSITIVE"},
			"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test authority", "lang": "en-GB"}}, "purpose": "AUTHORIZATION"}},
		},
	})
	return b
}

// staticZones is a ZoneProvider whose set a test replaces.
type staticZones struct {
	mu    sync.Mutex
	set   *geo.ZoneSet
	stale bool
}

func (s *staticZones) Current() *geo.ZoneSet { s.mu.Lock(); defer s.mu.Unlock(); return s.set }

func (s *staticZones) Freshness() geo.Freshness {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set == nil || !s.set.Loaded {
		return geo.Freshness{Stale: true}
	}
	return geo.Freshness{Loaded: true, CISVersion: s.set.CISVersion, Stale: s.stale}
}

func (s *staticZones) put(version string, ids ...string) {
	ce := &cis.CellEntry{Cell: "c5:test", CISVersion: version}
	for _, id := range ids {
		ce.Zones = append(ce.Zones, cis.ApplicableZone{Identifier: id, Type: "PROHIBITED", Applies: true, Version: "zones:1",
			Dataset: "zones", Feature: zoneFeature(id)})
	}
	set := geo.BuildZoneSet(map[string]telemetry.CISValue{
		"c": {Cell: ce}, cis.KeyBasis: {Basis: &cis.BasisValue{Basis: cis.Basis{CISVersion: version}, At: time.Now()}},
	}, nil, time.Now())
	s.mu.Lock()
	s.set = set
	s.mu.Unlock()
}

func zoneReplica(t *testing.T, clk *clock, intents *fakeIntents, store StateStore, zs ZoneProvider, id string) *replica {
	t.Helper()
	own, _ := cell.ParseOwnership("all")
	r := &replica{sink: &sink{}}
	r.eng = &Engine{Ownership: own, Intents: intents, Sources: &gate{}, Sink: r.sink, Now: clk.Now, Tick: 10 * time.Millisecond,
		Policy: func() policy.Record { return policy.Record{Version: 9, Values: policy.Defaults()} }, InstanceID: id,
		Zones: zs, Env: NewEnv(nil, nil)}
	if store != nil {
		r.eng.Store = store
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.eng.Run(ctx)
	t.Cleanup(cancel)
	waitFor(t, "the engine seeded", func() bool {
		select {
		case <-r.eng.Seeded():
			return true
		default:
			return false
		}
	})
	return r
}

// The zone path on the engine: a flight inside a PROHIBITED zone raises
// zone_incursion (critical) at once, republished every tick; out of the
// zone it clears resolved after the hysteresis (E-01 pair); a new
// projection is judged within one tick.
func TestEngineZoneIncursionLifecycleAndReload(t *testing.T) {
	clk := newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	intents := &fakeIntents{}
	intents.set(intentA, stateBody(t, clk.Now()), true)
	zs := &staticZones{}
	zs.put("zones:1")
	r := zoneReplica(t, clk, intents, nil, zs, "m-1")
	a := ptr(intentA)
	r.eng.Offer(track(t, flightA, a, origin, clk.Now(), "Airborne"))
	waitFor(t, "judged", func() bool { return r.eng.Counters.Get("conformance_judged") >= 1 || len(r.sink.states(flightA)) > 0 })
	if n := len(r.sink.alerts(geo.KindZoneIncursion, "raised", flightA)); n != 0 {
		t.Fatalf("raised with no zone: %d", n)
	}
	// A zone is published over the flight: the next sample is judged
	// against it.
	zs.put("zones:2", "TZP001")
	clk.Add(time.Second)
	r.eng.Offer(track(t, flightA, a, origin, clk.Now(), "Airborne"))
	waitFor(t, "raised", func() bool { return len(r.sink.alerts(geo.KindZoneIncursion, "raised", flightA)) == 1 })
	raised := r.sink.alerts(geo.KindZoneIncursion, "raised", flightA)[0]
	if raised.Severity != core.SeverityCritical || raised.IntentID == nil || *raised.IntentID != intentA ||
		raised.AuthorisationNumber == nil || *raised.AuthorisationNumber != "USSP-DEV-1" || raised.Detail["zone_id"] != "TZP001" {
		t.Fatalf("raised %+v", raised)
	}
	waitFor(t, "republished", func() bool { return len(r.sink.alerts(geo.KindZoneIncursion, "updated", flightA)) >= 2 })
	// Out of the zone (2 km south), more than 3 s.
	south := geodesy.Destination(origin, 180, 2000)
	for range 5 {
		clk.Add(time.Second)
		r.eng.Offer(track(t, flightA, a, south, clk.Now(), "Airborne"))
	}
	waitFor(t, "cleared resolved", func() bool {
		c := r.sink.alerts(geo.KindZoneIncursion, "cleared", flightA)
		return len(c) == 1 && *c[0].ClearReason == "resolved" && c[0].AlertID == raised.AlertID
	})
}

// A rolling update with a new host name: the instance that raised the
// zone alert stops; another instance id takes the flight over from
// conformance_state with its zone alert and continues it under the same
// id, never a second raise, never a silent clear.
func TestZoneAlertSurvivesARestartUnderAnotherInstance(t *testing.T) {
	clk := newClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	intents := &fakeIntents{}
	intents.set(intentA, stateBody(t, clk.Now()), true)
	zs := &staticZones{}
	zs.put("zones:1", "TZP001")
	store := &memStates{}
	a := ptr(intentA)
	first := zoneReplica(t, clk, intents, store, zs, "m-1")
	first.eng.Offer(track(t, flightA, a, origin, clk.Now(), "Airborne"))
	waitFor(t, "raised", func() bool { return len(first.sink.alerts(geo.KindZoneIncursion, "raised", flightA)) == 1 })
	id := first.sink.alerts(geo.KindZoneIncursion, "raised", flightA)[0].AlertID
	waitFor(t, "saved with the zone alert", func() bool {
		s, ok := store.get(flightA)
		return ok && len(s.Zones) == 1 && s.Zones[0].ID == id
	})
	first.cancel()
	clk.Add(2 * time.Second)

	second := zoneReplica(t, clk, intents, store, zs, "m-2")
	second.eng.Offer(track(t, flightA, a, origin, clk.Now(), "Airborne"))
	waitFor(t, "continued", func() bool {
		for _, u := range second.sink.alerts(geo.KindZoneIncursion, "updated", flightA) {
			if u.AlertID == id && u.Detail["carried_since"] == nil {
				return true
			}
		}
		return false
	})
	if n := len(second.sink.alerts(geo.KindZoneIncursion, "raised", flightA)); n != 0 {
		t.Fatalf("raised again on the new instance: %d", n)
	}
	if n := len(second.sink.alerts(geo.KindZoneIncursion, "cleared", flightA)); n != 0 {
		t.Fatalf("cleared on the new instance: %d", n)
	}
	// The flight ends: its zone alert clears flight_ended.
	second.eng.FlightEnded(flightA)
	waitFor(t, "flight_ended", func() bool {
		c := second.sink.alerts(geo.KindZoneIncursion, "cleared", flightA)
		return len(c) == 1 && *c[0].ClearReason == "flight_ended" && c[0].AlertID == id
	})
}

// Without cis_current nothing is judged and the probe says so; the
// environment says what it knows of the ground and the geoid.
func TestZoneProbeAndEnv(t *testing.T) {
	st, why := zoneProbe(&staticZones{set: &geo.ZoneSet{}})(t.Context())
	if st != "unknown" || why == "" {
		t.Fatalf("unloaded: %s %q", st, why)
	}
	zs := &staticZones{}
	zs.put("zones:1", "TZP001")
	if st, _ := zoneProbe(zs)(t.Context()); st != "up" {
		t.Fatalf("loaded: %s", st)
	}
	zs.stale = true
	if st, _ := zoneProbe(zs)(t.Context()); st != "degraded" {
		t.Fatalf("stale: %s", st)
	}
	if env := NewEnv(nil, nil)(origin); env.UndulationM != nil || env.Ground != 0 {
		t.Fatalf("env with nothing: %+v", env)
	}
	if g, why := loadTerrain(""); g != nil || why == "" {
		t.Fatal("terrain without a directory")
	}
	if g, why := loadTerrain(t.TempDir()); g != nil || why == "" {
		t.Fatal("terrain without an index")
	}
}
