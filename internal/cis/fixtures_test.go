package cis

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// box is minLon, minLat, maxLon, maxLat.
type box [4]float64

// tbilisi is a small box around the vectors' fixture point (41.7151,
// 44.8271).
var tbilisi = box{44.80, 41.70, 44.85, 41.73}

type feat struct {
	id, typ            string
	lower, upper       *float64
	lowerRef, upperRef string
	rect               box
	circle             *[3]float64 // lon, lat, radius_m
	applicability      []any
	extended           map[string]any
	reason             []string
	layers             []map[string]any // a GeometryCollection of rect with these layers
}

func f64(v float64) *float64 { return &v }

func (f feat) json() json.RawMessage {
	layer := map[string]any{"uom": "m"}
	if f.lower != nil {
		layer["lower"], layer["lowerReference"] = *f.lower, f.lowerRef
	}
	if f.upper != nil {
		layer["upper"], layer["upperReference"] = *f.upper, f.upperRef
	}
	r := f.rect
	poly := []any{[]any{
		[]any{r[0], r[1]}, []any{r[2], r[1]}, []any{r[2], r[3]}, []any{r[0], r[3]}, []any{r[0], r[1]},
	}}
	var geom map[string]any
	switch {
	case f.circle != nil:
		geom = map[string]any{"type": "Point", "coordinates": []any{f.circle[0], f.circle[1]},
			"extent": map[string]any{"subType": "Circle", "radius": f.circle[2]}, "layer": layer}
	case f.layers != nil:
		var gs []any
		for _, l := range f.layers {
			gs = append(gs, map[string]any{"type": "Polygon", "coordinates": poly, "layer": l})
		}
		geom = map[string]any{"type": "GeometryCollection", "geometries": gs}
	default:
		geom = map[string]any{"type": "Polygon", "coordinates": poly, "layer": layer}
	}
	reason := f.reason
	if reason == nil {
		reason = []string{"SENSITIVE"}
		if f.typ == "USPACE" {
			reason = []string{"AIR_TRAFFIC"}
		}
	}
	props := map[string]any{
		"identifier": f.id, "country": "GEO", "name": []any{map[string]any{"text": "Test zone " + f.id, "lang": "en-GB"}},
		"type": f.typ, "variant": "COMMON", "reason": reason,
		"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test authority", "lang": "en-GB"}}, "purpose": "AUTHORIZATION"}},
	}
	if f.applicability != nil {
		props["limitedApplicability"] = f.applicability
	}
	if f.extended != nil {
		props["extendedProperties"] = f.extended
	}
	b, err := json.Marshal(map[string]any{"type": "Feature", "geometry": geom, "properties": props})
	if err != nil {
		panic(err)
	}
	return b
}

// prohibited is a PROHIBITED 0-120 m AGL zone over tbilisi.
func prohibited(id string) feat {
	return feat{id: id, typ: "PROHIBITED", lower: f64(0), lowerRef: "AGL", upper: f64(120), upperRef: "AGL", rect: tbilisi}
}

// featureOf is a feature the dataset d holds: a U-space airspace for
// uspace_airspace (which holds nothing else), else a PROHIBITED zone.
func featureOf(d Dataset, id string) feat {
	if d == USpaceAirspace {
		return airspace(id)
	}
	return prohibited(id)
}

// requirements is a cis/uspace_requirements/v1 block.
func requirements() map[string]any {
	return map[string]any{"uspace_requirements": map[string]any{
		"uas_requirements":       map[string]any{"geo_awareness": "required"},
		"service_performance":    map[string]any{"nid_update_hz": 1, "ti_update_hz": 1, "cis_latency_s": 1},
		"operational_conditions": map[string]any{},
		"airspace_constraints":   map[string]any{"max_height_agl_m": 120, "in_controlled_airspace": false},
		"services_required":      []string{"NID", "GEO", "FA", "TI"},
		"adjacent":               []string{},
	}}
}

// airspace is a USPACE airspace 0-500 m AMSL over tbilisi.
func airspace(id string) feat {
	return feat{id: id, typ: "USPACE", lower: f64(0), lowerRef: "AMSL", upper: f64(500), upperRef: "AMSL", rect: tbilisi, extended: requirements()}
}

func collection(d Dataset, v int64, fs ...json.RawMessage) []byte {
	if fs == nil {
		fs = []json.RawMessage{}
	}
	b, err := json.Marshal(map[string]any{
		"type": "FeatureCollection", "features": fs, "cis_dataset": string(d), "cis_version": v,
		"cis_updated_at": "2026-10-02T09:00:00Z", "metadata": map[string]any{"issued": "2026-10-02T09:00:00Z",
			"provider": []any{map[string]any{"text": "Test CISP", "lang": "en-GB"}}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

func mustVersion(t *testing.T, d Dataset, v int64, fs ...json.RawMessage) *Version {
	t.Helper()
	ver, rf := ParseVersion(d, collection(d, v, fs...), "", 0)
	if rf != nil {
		t.Fatalf("ParseVersion: %v", rf)
	}
	return ver
}

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// loaded is an Evaluator with the versions installed at the clock's now.
func loaded(t *testing.T, clk *clock, vs ...*Version) *Evaluator {
	t.Helper()
	e := NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: clk.Now, Counters: &core.Counters{}})
	for _, v := range vs {
		es, rf := buildEntries(v)
		if rf != nil {
			t.Fatalf("buildEntries: %v", rf)
		}
		e.install(v, es, clk.Now())
	}
	return e
}

// memStore is an in-memory Store with a Down switch.
type memStore struct {
	mu      sync.Mutex
	down    bool
	saved   map[Dataset][]*Version
	touched int
	jtis    map[string]bool
	full    bool
	notes   []Notification
	pulled  map[Dataset]int64
	sweeps  int
	stored  []StoredVersion
	loadErr error
}

func newMemStore() *memStore {
	return &memStore{saved: map[Dataset][]*Version{}, jtis: map[string]bool{}, pulled: map[Dataset]int64{}}
}

var errDown = errors.New("store down")

func (m *memStore) setDown(b bool) { m.mu.Lock(); m.down = b; m.mu.Unlock() }

func (m *memStore) SaveVersion(_ context.Context, v *Version, _ []*Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errDown
	}
	m.saved[v.Dataset] = append(m.saved[v.Dataset], v)
	return nil
}

func (m *memStore) TouchVersion(context.Context, Dataset, int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errDown
	}
	m.touched++
	return nil
}

func (m *memStore) LoadCurrent(context.Context) ([]StoredVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stored, m.loadErr
}

func (m *memStore) MarkPulled(_ context.Context, d Dataset, v int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errDown
	}
	m.pulled[d] = v
	return nil
}

func (m *memStore) RememberJTI(_ context.Context, issuer, jti string, _ time.Duration, _ int64) (bool, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, false, errDown
	}
	if m.full {
		return false, true, nil
	}
	k := issuer + " " + jti
	if m.jtis[k] {
		return false, false, nil
	}
	m.jtis[k] = true
	return true, false, nil
}

func (m *memStore) InsertNotification(_ context.Context, n Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errDown
	}
	m.notes = append(m.notes, n)
	return nil
}

func (m *memStore) Sweep(context.Context, int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweeps++
	if m.down {
		return errDown
	}
	return nil
}
