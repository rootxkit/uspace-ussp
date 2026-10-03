package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed318"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/cis/cispclient"
	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

// The fixed clock of the unit tests and the vectors.
var testNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const (
	testOperator = "GEOTESTOP0001"
	testSerial   = "TEST0001"
	testClient   = "client-test-1"
	testSystem   = "USSP-DEV"
)

// fakeGeoid is a constant undulation, or an error.
type fakeGeoid struct {
	n   float64
	err error
}

func (g fakeGeoid) UndulationM(core.LatLon) (float64, error) { return g.n, g.err }

// feature is one CIS feature of a test or a vector case.
type feature struct {
	Dataset string       `json:"dataset"`
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	Polygon [][2]float64 `json:"polygon,omitempty"`
	Circle  *struct {
		Lat     float64 `json:"lat"`
		Lng     float64 `json:"lng"`
		RadiusM float64 `json:"radius_m"`
	} `json:"circle,omitempty"`
	Lower            *zones.Limit       `json:"-"`
	Upper            *zones.Limit       `json:"-"`
	LowerRaw         *limit             `json:"lower,omitempty"`
	UpperRaw         *limit             `json:"upper,omitempty"`
	MaxHeightAGLM    *float64           `json:"max_height_agl_m,omitempty"`
	NoRequirements   bool               `json:"no_requirements,omitempty"`
	ServicePerf      map[string]float64 `json:"service_performance,omitempty"`
	RestrictionState string             `json:"restriction_state,omitempty"`
	Message          string             `json:"message,omitempty"`
	From             *time.Time         `json:"from,omitempty"`
	To               *time.Time         `json:"to,omitempty"`
}

type limit struct {
	ValueM float64 `json:"value_m"`
	Ref    string  `json:"ref"`
}

func (l *limit) zl() *zones.Limit {
	if l == nil {
		return nil
	}
	return &zones.Limit{ValueM: l.ValueM, Ref: core.VerticalRef(l.Ref)}
}

// entry builds the cis.Entry and zone of f.
func (f feature) candidate(t testing.TB) cis.ZoneCandidate {
	t.Helper()
	z := &zones.Zone{Identifier: f.ID, Type: core.ZoneType(f.Type), Lower: f.LowerRaw.zl(), Upper: f.UpperRaw.zl()}
	if f.Lower != nil {
		z.Lower = f.Lower
	}
	if f.Upper != nil {
		z.Upper = f.Upper
	}
	switch {
	case f.Circle != nil:
		z.Circle = &geodesy.Circle{Center: core.LatLon{LatDeg: f.Circle.Lat, LonDeg: f.Circle.Lng}, RadiusM: f.Circle.RadiusM}
		z.BBox = z.Circle.BBox()
	default:
		r := make(geodesy.Ring, 0, len(f.Polygon)+1)
		for _, p := range f.Polygon {
			r = append(r, core.LatLon{LatDeg: p[0], LonDeg: p[1]})
		}
		r = append(r, r[0])
		z.Polygon = &geodesy.Polygon{Rings: []geodesy.Ring{r}}
		z.BBox = z.Polygon.BBox()
	}
	e := &cis.Entry{Dataset: cis.Dataset(f.Dataset), Version: 1, Identifier: f.ID, Type: core.ZoneType(f.Type), Parts: []*zones.Zone{z}}
	if f.Message != "" {
		msg := f.Message
		e.Feature = &ed318.Feature{Properties: ed318.UASZone{Message: []ed318.Text{{Text: &msg, Lang: "en-GB"}}}}
	}
	if e.Dataset == cis.USpaceAirspace && !f.NoRequirements {
		raw := map[string]any{"airspace_constraints": map[string]any{}}
		if f.MaxHeightAGLM != nil {
			raw["airspace_constraints"] = map[string]any{"max_height_agl_m": *f.MaxHeightAGLM}
		}
		if f.ServicePerf != nil {
			raw["service_performance"] = f.ServicePerf
		}
		b, _ := json.Marshal(raw)
		e.Requirements = &cis.Requirements{Raw: b}
		e.Requirements.AirspaceConstraints.MaxHeightAglM = f.MaxHeightAGLM
	} else if e.Dataset == cis.USpaceAirspace {
		e.RequirementsProblem = "extendedProperties.uspace_requirements is absent"
	}
	if f.RestrictionState != "" {
		e.Restriction = &cispclient.CisRestriction{Id: f.ID, State: cispclient.CisRestrictionState(f.RestrictionState)}
	}
	c := cis.ZoneCandidate{Entry: e, Part: 0, Zone: z, Kind: cis.WindowAlways}
	if f.From != nil || f.To != nil {
		c.Kind, c.From, c.To = cis.WindowDuring, f.From, f.To
	}
	return c
}

// fakeCIS answers every query with every feature (the decision judges
// the geometry) and the basis it is given.
type fakeCIS struct {
	mu       sync.Mutex
	features []cis.ZoneCandidate
	basis    cis.Basis
	err      string
	calls    int
}

func (c *fakeCIS) Age() (string, float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.basis.CISVersion, c.basis.CISAgeS, c.basis.Stale
}

func (c *fakeCIS) ZonesFor(geodesy.BBox, time.Time, time.Time) cis.ZonesResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return cis.ZonesResult{Basis: c.basis, Zones: slices.Clone(c.features), Error: c.err}
}

type fakeIntegrity struct{ out []string }

func (f fakeIntegrity) Outdated() []string { return f.out }

// fakeRegistry answers each entity with a status (valid by default), or
// unknown with a reason, or an error.
type fakeRegistry struct {
	mu      sync.Mutex
	status  map[string]registry.Status
	reason  string
	err     error
	calls   int
	purpose registry.Purpose
	ageS    float64
	// classLabel and mtomBand are what the registry holds for the UAS.
	classLabel, mtomBand string
}

func (r *fakeRegistry) Validate(_ context.Context, qs []registry.Query, p registry.Purpose) ([]registry.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.purpose = p
	if r.err != nil {
		return nil, r.err
	}
	ans := func(entity, key string) *registry.Answer {
		if key == "" {
			return nil
		}
		st := registry.StatusValid
		if s, ok := r.status[entity]; ok {
			st = s
		}
		a := &registry.Answer{Key: key, Status: st, CacheAgeS: &r.ageS}
		if st == registry.StatusUnknown {
			a.Reason = r.reason
		} else if entity == "uas" {
			a.ClassLabel, a.MTOMBand = r.classLabel, r.mtomBand
		}
		return a
	}
	q := qs[0]
	return []registry.Result{{Operator: ans("operator", regnum.PublicPart(q.Operator)), UAS: ans("uas", q.Serial), Pilot: ans("pilot", q.Pilot)}}, nil
}

type fakeTerrain struct {
	minM, maxM float64
	ok         bool
}

func (t fakeTerrain) GroundRangeM(deconflict.Shape) (float64, float64, bool) {
	return t.minM, t.maxM, t.ok
}

type fakeDSS struct {
	ok     bool
	reason string
}

func (d fakeDSS) Available(context.Context) (bool, string) { return d.ok, d.reason }

// freshBasis is a CIS basis that is current.
func freshBasis() cis.Basis {
	return cis.Basis{CISVersion: "zones:1,uspace_airspace:1,restrictions:1", CISAgeS: 3}
}

func testPolicy() policy.Record { return policy.Record{Version: 7, Values: policy.Defaults()} }

// rig is a decider with every dependency present and current.
type rig struct {
	cis      *fakeCIS
	reg      *fakeRegistry
	decider  *Decider
	counters *core.Counters
}

func newRig() *rig {
	c := &fakeCIS{basis: freshBasis()}
	reg := &fakeRegistry{status: map[string]registry.Status{}}
	counters := &core.Counters{}
	return &rig{cis: c, reg: reg, counters: counters, decider: &Decider{
		CIS: c, Integrity: fakeIntegrity{}, Registry: reg, DSS: fakeDSS{ok: true},
		Terrain: fakeTerrain{minM: 400, maxM: 450, ok: true}, SystemID: testSystem, Counters: counters,
	}}
}

// square is an F3548 polygon outline.
func squareWire(lat, lon, size float64) map[string]any {
	return map[string]any{"outline_polygon": map[string]any{"vertices": []map[string]float64{
		{"lat": lat, "lng": lon}, {"lat": lat, "lng": lon + size}, {"lat": lat + size, "lng": lon + size}, {"lat": lat + size, "lng": lon},
	}}}
}

func wireVolumeJSON(outline map[string]any, lo, hi float64, start, end time.Time) map[string]any {
	v := map[string]any{}
	for k, x := range outline {
		v[k] = x
	}
	v["altitude_lower"] = map[string]any{"value": lo, "reference": "W84", "units": "M"}
	v["altitude_upper"] = map[string]any{"value": hi, "reference": "W84", "units": "M"}
	return map[string]any{"volume": v,
		"time_start": map[string]any{"value": start.Format(time.RFC3339), "format": "RFC3339"},
		"time_end":   map[string]any{"value": end.Format(time.RFC3339), "format": "RFC3339"}}
}

var (
	t0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(30 * time.Minute)
)

// baseRequest is a valid request outside every fixture zone.
func baseRequest() map[string]any {
	return map[string]any{
		"client_ref": "ref-1", "uas_serial": testSerial, "mode": "VLOS", "flight_type": "normal", "category": "specific",
		"volumes":                   []any{wireVolumeJSON(squareWire(41.70, 44.80, 0.01), 500, 550, t0, t1)},
		"identification_technology": "network", "connectivity_methods": []string{"lte"}, "endurance_s": 3600,
		"loss_of_c2_procedure": "return_to_home", "operator_reg": testOperator,
		"contingency": map[string]any{"procedure": "land at the nearest landing site"}, "emergency_contact_ref": "EC-TEST-1",
	}
}

func encode(t testing.TB, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func with(m map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(out, kv[i].(string))
			continue
		}
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// normalise decodes and validates m at testNow with a 20 m geoid.
func normalise(t testing.TB, m map[string]any) *Normalised {
	t.Helper()
	r, err := Decode(encode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	n, probs, err := Validate(r, ValidateEnv{Geoid: fakeGeoid{n: 20}, Now: testNow, SpecialPriority: 100})
	if err != nil || probs != nil {
		t.Fatalf("validate: %v %v", err, probs)
	}
	return n
}

func reasons(d Decision) []string {
	var out []string
	for _, c := range d.Conflicts {
		out = append(out, c.Reason)
	}
	sort.Strings(out)
	return out
}

// memStore is the Store contract in memory: a transaction is applied
// whole or not at all (the maps are copied and restored on error).
type memStore struct {
	mu       sync.Mutex
	now      time.Time
	owners   map[string]Owner
	bound    map[string]bool
	byID     map[string]*Record
	flags    map[string]string
	versions map[string][]Record
	peers    []PeerIntent
	failNow  error
	failTx   error
	// failCommit fails a transaction after fn succeeded (the commit).
	failCommit error
	// projected is the version of each intent last projected.
	projected map[string]int
	// beforeTx, when set, runs before each transaction's fn, outside the
	// store's lock (a test installs a CIS version between an assessment
	// and its commit there).
	beforeTx func()
}

func newMemStore() *memStore {
	return &memStore{
		now: testNow,
		owners: map[string]Owner{testClient: {
			ClientID: testClient, OperatorID: "00000000-0000-4000-8000-000000000001", OperatorKey: regnum.CompareKey(testOperator),
			OperatorStatus: "active", ClientStatus: "active",
		}},
		bound: map[string]bool{testClient + "/" + serial.FoldKey(testSerial): true},
		byID:  map[string]*Record{}, flags: map[string]string{}, versions: map[string][]Record{},
		projected: map[string]int{},
	}
}

func (m *memStore) Now(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now, m.failNow
}

func (m *memStore) Owner(_ context.Context, clientID string) (Owner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.owners[clientID]
	if !ok {
		return Owner{}, ErrNotFound
	}
	return o, nil
}

func (m *memStore) SerialBound(_ context.Context, clientID, fold string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bound[clientID+"/"+fold], nil
}

func clone(r *Record) *Record {
	b, _ := json.Marshal(r)
	var out Record
	_ = json.Unmarshal(b, &out)
	return &out
}

func (m *memStore) ByClientRef(_ context.Context, clientID, ref string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.byID {
		if r.ClientID == clientID && r.ClientRef == ref {
			return clone(r), nil
		}
	}
	return nil, nil
}

func (m *memStore) Get(_ context.Context, id string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.byID[id]; ok {
		return clone(r), nil
	}
	return nil, nil
}

func (m *memStore) List(_ context.Context, operatorID string, f ListFilter) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.byID {
		if r.OperatorID == operatorID && (f.State == "" || r.LocalState == f.State) {
			out = append(out, *clone(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) || out[i].ID < out[j].ID })
	if len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *memStore) InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	if m.beforeTx != nil {
		m.beforeTx()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failTx != nil {
		return m.failTx
	}
	saved := map[string]*Record{}
	for k, v := range m.byID {
		saved[k] = clone(v)
	}
	savedFlags := map[string]string{}
	for k, v := range m.flags {
		savedFlags[k] = v
	}
	err := fn(ctx, memTx{m})
	if err == nil && m.failCommit != nil {
		err = fmt.Errorf("commit: %w", m.failCommit)
	}
	if err != nil {
		m.byID, m.flags = saved, savedFlags
		return err
	}
	return nil
}

func (m *memStore) Unprojected(_ context.Context, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, r := range m.byID {
		if m.projected[id] < r.Version {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) Project(ctx context.Context, id string, fn func(context.Context, *Record) error) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok || m.projected[id] >= r.Version {
		return false, nil
	}
	if err := fn(ctx, clone(r)); err != nil {
		return false, err
	}
	m.projected[id] = r.Version
	return true, nil
}

type memTx struct{ m *memStore }

func (t memTx) Now(context.Context) (time.Time, error) { return t.m.now, nil }

func (t memTx) Overlapping(_ context.Context, _ []geodesy.BBox, _ float64, from, to time.Time, excludeID string, limit int) ([]Record, error) {
	var out []Record
	for _, r := range t.m.byID {
		if r.ID == excludeID || r.Exempt || !slices.Contains(ActiveStates, r.LocalState) || r.TimeStart.After(to) || r.TimeEnd.Before(from) {
			continue
		}
		out = append(out, *clone(r))
		if len(out) > limit {
			break
		}
	}
	return out, nil
}

func (t memTx) PeerIntents(_ context.Context, _, _, _ time.Time, limit int) ([]PeerIntent, error) {
	if len(t.m.peers) > limit+1 {
		return t.m.peers[:limit+1], nil
	}
	return t.m.peers, nil
}

func (t memTx) CountOpen(_ context.Context, operatorID string) (int, error) {
	n := 0
	for _, r := range t.m.byID {
		if r.OperatorID == operatorID && slices.Contains(OpenStates, r.LocalState) {
			n++
		}
	}
	return n, nil
}

func (t memTx) Lock(_ context.Context, id string) (*Record, error) {
	if r, ok := t.m.byID[id]; ok {
		return clone(r), nil
	}
	return nil, nil
}

func (t memTx) Insert(_ context.Context, r *Record) error {
	for _, x := range t.m.byID {
		if x.ClientID == r.ClientID && x.ClientRef == r.ClientRef {
			return ErrDuplicate
		}
	}
	if len(r.Envelope) == 0 {
		return errors.New("no envelope")
	}
	t.m.byID[r.ID] = clone(r)
	t.m.versions[r.ID] = append(t.m.versions[r.ID], *clone(r))
	return nil
}

func (t memTx) Update(_ context.Context, r *Record, _ string) error {
	cur, ok := t.m.byID[r.ID]
	if !ok || cur.Version != r.Version-1 {
		return errors.New("version is not the next one")
	}
	t.m.byID[r.ID] = clone(r)
	t.m.versions[r.ID] = append(t.m.versions[r.ID], *clone(r))
	return nil
}

func (t memTx) FlagUpdate(_ context.Context, ids []string, by string, _ time.Time) error {
	for _, id := range ids {
		t.m.flags[id] = by
		if r, ok := t.m.byID[id]; ok && slices.Contains(ActiveStates, r.LocalState) {
			r.UpdateRequired = json.RawMessage(`{"by_intent_id":"` + by + `"}`)
		}
	}
	return nil
}

func (t memTx) SetNotice(_ context.Context, id string, notice json.RawMessage) error {
	if r, ok := t.m.byID[id]; ok {
		r.UpdateRequired = slices.Clone(notice)
	}
	return nil
}

func (t memTx) DueToEnd(_ context.Context, now time.Time, limit int) ([]Record, error) {
	var out []Record
	for _, r := range t.m.byID {
		if slices.Contains(OpenStates, r.LocalState) && r.TimeEnd.Before(now) && len(out) < limit {
			out = append(out, *clone(r))
		}
	}
	return out, nil
}

// memProjector records the KV and the subjects; err fails it.
type memProjector struct {
	mu       sync.Mutex
	err      error
	kv       map[string]StateBody
	subjects []string
}

func (p *memProjector) Project(_ context.Context, r *Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if p.kv == nil {
		p.kv = map[string]StateBody{}
	}
	if slices.Contains(ActiveStates, r.LocalState) {
		p.kv[r.ID] = StateOf(r)
	} else {
		delete(p.kv, r.ID)
	}
	p.subjects = append(p.subjects, "intent.v1."+r.LocalState+"."+r.ID)
	return nil
}

func (p *memProjector) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.subjects) == 0 {
		return ""
	}
	return p.subjects[len(p.subjects)-1]
}

// service is a Service over a memStore with a current rig.
func newService(g *rig) (*Service, *memStore, *memProjector) {
	st := newMemStore()
	pr := &memProjector{}
	return &Service{Store: st, Decider: g.decider, Geoid: fakeGeoid{n: 20}, Policy: testPolicy, Projector: pr, Counters: g.counters}, st, pr
}
