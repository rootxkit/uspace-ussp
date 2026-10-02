package cis

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed318"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/zones"
)

var inside = core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271}
var outside = core.LatLon{LatDeg: 41.60, LonDeg: 44.60}

func geodetic(altAMSLM float64) zones.Aircraft {
	return zones.Aircraft{AltAMSLM: &altAMSLM, AltSource: core.AltGeodetic}
}

// SC-13 in unit form (brief WP-4): a PROHIBITED 0-120 m AGL zone with
// no terrain configured warns limit_not_judged with not_judged ["AGL"];
// a CONDITIONAL one is not evaluated and counted. Both asserted.
func TestJudgePointSC13LimitNotJudged(t *testing.T) {
	clk := newClock()
	cond := prohibited("TZC001")
	cond.typ = "CONDITIONAL"
	e := loaded(t, clk, mustVersion(t, Zones, 1, prohibited("TZP001").json(), cond.json()))
	res := e.JudgePoint(inside, geodetic(500), zones.Env{Ground: zones.GroundNotConfigured}, clk.Now())
	if res.Error != "" || len(res.Zones) != 2 {
		t.Fatalf("got %+v", res)
	}
	byID := map[string]PointJudgement{}
	for _, z := range res.Zones {
		byID[z.Entry.Identifier] = z
	}
	p := byID["TZP001"]
	if p.Result.Raise == nil || p.Result.Raise.Severity != core.SeverityWarning ||
		p.Result.Raise.Detail.LimitNotJudged == nil || !*p.Result.Raise.Detail.LimitNotJudged ||
		!slices.Equal(p.Result.Raise.Detail.NotJudged, []string{"AGL"}) || !p.Result.Reasons.Has(zones.ReasonNoTerrain) {
		t.Fatalf("PROHIBITED: %+v", p.Result)
	}
	c := byID["TZC001"]
	if c.Result.Raise != nil || !c.Result.NotEvaluated {
		t.Fatalf("CONDITIONAL: %+v", c.Result)
	}
	if got := e.Counters().Get(zones.CounterZoneNotEvaluated); got != 1 {
		t.Fatalf("zone_checks_not_evaluated = %d, want 1", got)
	}
	if got := e.Counters().Get(zones.CounterZoneLimitNotJudged); got != 1 {
		t.Fatalf("zone_limits_not_judged = %d, want 1", got)
	}
}

// E-01 twin of SC-13: with the ground known the same aircraft (500 m
// AMSL over 400 m ground: 100 m AGL) is judged inside the PROHIBITED
// zone, critical, and clear of nothing; above it, nothing is raised.
func TestJudgePointGroundKnown(t *testing.T) {
	clk := newClock()
	e := loaded(t, clk, mustVersion(t, Zones, 1, prohibited("TZP001").json()))
	env := zones.Env{Ground: zones.GroundKnown, GroundM: 400}
	res := e.JudgePoint(inside, geodetic(500), env, clk.Now())
	if len(res.Zones) != 1 || res.Zones[0].Result.Raise == nil || res.Zones[0].Result.Raise.Severity != core.SeverityCritical {
		t.Fatalf("inside: %+v", res)
	}
	res = e.JudgePoint(inside, geodetic(600), env, clk.Now())
	if len(res.Zones) != 1 || res.Zones[0].Result.Raise != nil || res.Zones[0].Result.NotEvaluated {
		t.Fatalf("above (judged clear): %+v", res.Zones[0].Result)
	}
	if res := e.JudgePoint(outside, geodetic(500), env, clk.Now()); len(res.Zones) != 0 {
		t.Fatalf("outside: %+v", res)
	}
	if res := e.JudgePoint(core.LatLon{LatDeg: math.NaN()}, geodetic(500), env, clk.Now()); res.Error == "" {
		t.Fatal("an invalid point answered without an error")
	}
}

func TestJudgePointApplicability(t *testing.T) {
	clk := newClock()
	day := prohibited("TZD001")
	day.applicability = []any{map[string]any{"startDateTime": "2026-10-02T08:00:00Z", "endDateTime": "2026-10-02T10:00:00Z"}}
	dl := prohibited("TZL001")
	dl.applicability = []any{map[string]any{"schedule": []any{map[string]any{"day": []string{"ANY"}, "startEvent": "SR", "endEvent": "SS"}}}}
	v := mustVersion(t, Zones, 1, day.json(), dl.json())
	e := loaded(t, clk, v)
	env := zones.Env{Ground: zones.GroundKnown, GroundM: 400}
	got := e.JudgePoint(inside, geodetic(450), env, clk.Now()) // 09:00Z: the day window applies; it is day in Tbilisi
	ids := []string{}
	for _, z := range got.Zones {
		ids = append(ids, z.Entry.Identifier+":"+string(z.Applicability))
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"TZD001:applies", "TZL001:applies"}) {
		t.Fatalf("at 09:00Z: %v", ids)
	}
	got = e.JudgePoint(inside, geodetic(450), env, clk.Now().Add(12*time.Hour)) // 21:00Z: neither
	if len(got.Zones) != 0 {
		t.Fatalf("at 21:00Z: %+v", got.Zones)
	}
	// A zero instant is "when is not known": every zone applies.
	if got := e.JudgePoint(inside, geodetic(450), env, time.Time{}); len(got.Zones) != 2 {
		t.Fatalf("zero at: %d zones", len(got.Zones))
	}
	// A daylight event that cannot be resolved is unknown and kept,
	// never "does not apply".
	e.cfg.Daylight = ed318.FixedDaylight{}
	got = e.JudgePoint(inside, geodetic(450), env, clk.Now().Add(12*time.Hour))
	if len(got.Zones) != 1 || got.Zones[0].Applicability != Unknown || got.Zones[0].ApplicabilityError == "" {
		t.Fatalf("unresolvable daylight: %+v", got.Zones)
	}
	if e.Counters().Get(CounterApplicabilityUnknown) == 0 {
		t.Fatal("cis_applicability_unknown not counted")
	}
}

// A circle is judged on its published centre and radius (Z-11).
func TestJudgePointCircleAndLayers(t *testing.T) {
	clk := newClock()
	c := prohibited("TZR001")
	c.circle = &[3]float64{44.8271, 41.7151, 500}
	c.lowerRef, c.upperRef = "AMSL", "AMSL"
	c.lower, c.upper = f64(0), f64(1000)
	two := feat{id: "TZ2L001", typ: "REQ_AUTHORIZATION", rect: tbilisi, layers: []map[string]any{
		{"lower": 0, "lowerReference": "AMSL", "upper": 300, "upperReference": "AMSL", "uom": "m"},
		{"lower": 300, "lowerReference": "AMSL", "upper": 900, "upperReference": "AMSL", "uom": "m"},
	}}
	e := loaded(t, clk, mustVersion(t, Zones, 1, c.json(), two.json()))
	res := e.JudgePoint(inside, geodetic(100), zones.Env{}, clk.Now())
	got := []string{}
	for _, z := range res.Zones {
		if z.Result.Raise != nil {
			got = append(got, z.Zone.Identifier)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"TZ2L001/L0", "TZR001"}) {
		t.Fatalf("raised %v", got)
	}
	far := core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271 + 0.01} // about 830 m east: outside the circle
	for _, z := range e.JudgePoint(far, geodetic(100), zones.Env{}, clk.Now()).Zones {
		if z.Entry.Identifier == "TZR001" {
			t.Fatalf("the circle contains a point 830 m from its centre: %+v", z)
		}
	}
}

func TestAge(t *testing.T) {
	clk := newClock()
	e := NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: clk.Now})
	if v, age, stale := e.Age(); v != "" || age != 0 || !stale {
		t.Fatalf("nothing loaded: %q %v %v", v, age, stale)
	}
	for _, d := range ED318Datasets {
		v := mustVersion(t, d, 3)
		e.install(v, nil, clk.Now())
	}
	if v, age, stale := e.Age(); v != "zones:3,uspace_airspace:3,restrictions:3" || age != 0 || stale {
		t.Fatalf("loaded: %q %v %v", v, age, stale)
	}
	clk.advance(301 * time.Second)
	if _, age, stale := e.Age(); age != 301 || !stale {
		t.Fatalf("after 301 s: %v %v", age, stale)
	}
	for _, d := range ED318Datasets {
		e.confirm(d, clk.Now(), false)
	}
	if _, age, stale := e.Age(); age != 0 || stale {
		t.Fatalf("confirmed again: %v %v", age, stale)
	}
	// A dataset the CISP says has no version is a known empty set.
	e2 := NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: clk.Now})
	e2.install(mustVersion(t, Zones, 1), nil, clk.Now())
	if _, _, stale := e2.Age(); !stale {
		t.Fatal("two datasets never loaded, yet not stale")
	}
	e2.confirm(USpaceAirspace, clk.Now(), true)
	e2.confirm(Restrictions, clk.Now(), true)
	if v, _, stale := e2.Age(); v != "zones:1,uspace_airspace:0,restrictions:0" || stale {
		t.Fatalf("empty datasets: %q %v", v, stale)
	}
	// A 404 never empties a dataset whose version is held.
	e2.confirm(Zones, clk.Now(), true)
	if v, _, _ := e2.Age(); !strings.HasPrefix(v, "zones:1") {
		t.Fatalf("a 404 emptied the held zones: %q", v)
	}
}

func TestAirspacesAt(t *testing.T) {
	clk := newClock()
	agl := airspace("TSA002")
	agl.rect = box{44.83, 41.71, 44.86, 41.72}
	agl.lowerRef, agl.upperRef = "AGL", "AGL"
	z := prohibited("TZP001")
	e := loaded(t, clk, mustVersion(t, USpaceAirspace, 1, airspace("TSA001").json(), agl.json()), mustVersion(t, Zones, 1, z.json()))
	res := e.AirspacesAt(inside, 100, clk.Now())
	if res.Error != "" || len(res.Airspaces) != 1 || res.Airspaces[0].Entry.Identifier != "TSA001" || !res.Airspaces[0].VerticalKnown {
		t.Fatalf("at 100 m AMSL: %+v", res)
	}
	r := res.Airspaces[0].Entry.Requirements
	if r == nil || r.InControlledAirspace == nil || *r.InControlledAirspace || r.AirspaceConstraints.MaxHeightAglM == nil ||
		*r.AirspaceConstraints.MaxHeightAglM != 120 || len(r.ServicesRequired) != 4 {
		t.Fatalf("requirements: %+v", r)
	}
	if res := e.AirspacesAt(inside, 600, clk.Now()); len(res.Airspaces) != 0 {
		t.Fatalf("above the airspace: %+v", res.Airspaces)
	}
	// The AGL airspace cannot be judged without terrain: kept, with
	// VerticalKnown false (fail-safe).
	pt := core.LatLon{LatDeg: 41.715, LonDeg: 44.845}
	res = e.AirspacesAt(pt, 100, clk.Now())
	if len(res.Airspaces) != 2 {
		t.Fatalf("overlap: %+v", res.Airspaces)
	}
	for _, a := range res.Airspaces {
		if a.Entry.Identifier == "TSA002" && (a.VerticalKnown || a.Reasons == 0) {
			t.Fatalf("AGL airspace: %+v", a)
		}
	}
	if res := e.AirspacesAt(outside, 100, clk.Now()); len(res.Airspaces) != 0 {
		t.Fatalf("outside: %+v", res.Airspaces)
	}
	if res := e.AirspacesAt(inside, math.Inf(1), clk.Now()); res.Error == "" {
		t.Fatal("an infinite altitude answered")
	}
}

func TestRequirementsProblemKept(t *testing.T) {
	clk := newClock()
	bad := airspace("TSA003")
	bad.extended = map[string]any{"uspace_requirements": "not an object"}
	none := airspace("TSA004")
	none.extended = nil
	e := loaded(t, clk, mustVersion(t, USpaceAirspace, 1, bad.json(), none.json()))
	es := e.Snapshot().Entries(USpaceAirspace)
	if len(es) != 2 {
		t.Fatalf("entries: %d", len(es))
	}
	for _, en := range es {
		if en.Requirements != nil || en.RequirementsProblem == "" {
			t.Fatalf("%s: %+v", en.Identifier, en)
		}
	}
}

func TestZonesFor(t *testing.T) {
	clk := newClock()
	now := clk.Now()
	during := prohibited("TZW001")
	during.applicability = []any{map[string]any{"startDateTime": "2026-10-02T10:00:00Z", "endDateTime": "2026-10-02T12:00:00Z"}}
	sched := prohibited("TZS001")
	sched.applicability = []any{map[string]any{"startDateTime": "2026-10-01T00:00:00Z",
		"schedule": []any{map[string]any{"day": []string{"MON"}, "startTime": "09:00:00Z", "endTime": "17:00:00Z"}}}}
	past := prohibited("TZX001")
	past.applicability = []any{map[string]any{"startDateTime": "2026-09-01T00:00:00Z", "endDateTime": "2026-09-02T00:00:00Z"}}
	far := prohibited("TZF001")
	far.rect = box{45.5, 42.5, 45.6, 42.6}
	e := loaded(t, clk, mustVersion(t, Zones, 1, prohibited("TZA001").json(), during.json(), sched.json(), past.json(), far.json()))
	env := geodesy.BBox{MinLat: 41.71, MinLon: 44.81, MaxLat: 41.72, MaxLon: 44.82}
	res := e.ZonesFor(env, now, now.Add(4*time.Hour))
	got := map[string]ZoneCandidate{}
	for _, z := range res.Zones {
		got[z.Entry.Identifier] = z
	}
	if len(got) != 3 {
		t.Fatalf("candidates: %v", res.Zones)
	}
	if got["TZA001"].Kind != WindowAlways || got["TZS001"].Kind != WindowScheduled || got["TZW001"].Kind != WindowDuring {
		t.Fatalf("kinds: %+v", got)
	}
	w := got["TZW001"]
	if w.From == nil || !w.From.Equal(now.Add(time.Hour)) || w.To == nil || !w.To.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("during %v..%v", w.From, w.To)
	}
	if s := got["TZS001"]; s.From != nil && !s.From.Equal(now) {
		t.Fatalf("scheduled from %v", s.From)
	}
	if res := e.ZonesFor(env, now.Add(time.Hour), now); res.Error == "" {
		t.Fatal("a window ending before it starts answered")
	}
	if res := e.ZonesFor(geodesy.BBox{MinLat: 1, MaxLat: 0}, now, now); res.Error == "" {
		t.Fatal("an empty envelope answered")
	}
	// An envelope across the antimeridian is two spans.
	if !overlaps(geodesy.BBox{MinLat: 0, MaxLat: 1, MinLon: 179, MaxLon: -179}, geodesy.BBox{MinLat: 0, MaxLat: 1, MinLon: -179.5, MaxLon: -179.2}) {
		t.Fatal("antimeridian span missed")
	}
	if overlaps(geodesy.BBox{MinLat: 0, MaxLat: 1, MinLon: 179, MaxLon: -179}, geodesy.BBox{MinLat: 0, MaxLat: 1, MinLon: 0, MaxLon: 1}) {
		t.Fatal("antimeridian span overlaps the prime meridian")
	}
}

func TestWindowOpenEnds(t *testing.T) {
	from := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	start := ed318.DateTime{Time: from.Add(-time.Hour)}
	kind, wf, wt, ok := window([]ed318.TimePeriod{{StartDateTime: &start}}, from, to)
	if !ok || kind != WindowDuring || wf == nil || !wf.Equal(from) || wt != nil {
		t.Fatalf("open end: %v %v %v %v", kind, wf, wt, ok)
	}
	end := ed318.DateTime{Time: to.Add(time.Hour)}
	kind, wf, wt, ok = window([]ed318.TimePeriod{{EndDateTime: &end}}, from, to)
	if !ok || kind != WindowDuring || wf != nil || wt == nil || !wt.Equal(to) {
		t.Fatalf("open start: %v %v %v %v", kind, wf, wt, ok)
	}
}

func TestProjection(t *testing.T) {
	clk := newClock()
	big := prohibited("TZB001")
	big.rect = box{40.0, 40.0, 46.0, 44.0} // 60 x 40 cell5 cells: beyond MaxCellsPerZone
	r := prohibited("DAR0A1F")
	r.reason = []string{"DAR"}
	r.extended = map[string]any{"cis_restriction": map[string]any{"id": "r1", "ansp_ref": "A1", "ansp_version": 1, "state": "active",
		"starts_at": "2026-10-02T08:00:00Z", "ends_at": "2026-10-02T12:00:00Z", "ended_by": nil, "uspace_airspace_id": "TSA001"}}
	r.applicability = []any{map[string]any{"startDateTime": "2026-10-02T08:00:00Z", "endDateTime": "2026-10-02T12:00:00Z"}}
	e := loaded(t, clk, mustVersion(t, Zones, 2, prohibited("TZP001").json(), big.json()), mustVersion(t, Restrictions, 7, r.json()))
	p := e.Project(clk.Now())
	all, ok := p.Cells[AllCells]
	if !ok || len(all.Zones) != 1 || all.Zones[0].Identifier != "TZB001" {
		t.Fatalf("all: %+v", all)
	}
	var cellKey string
	for k, ce := range p.Cells {
		for _, z := range ce.Zones {
			if z.Identifier == "DAR0A1F" {
				cellKey = k
				if !z.Applies || z.CISApplicability != "applies" || z.RestrictionState != "active" || z.Version != "restrictions:7" ||
					z.ValidFrom == nil || z.ValidTo == nil || len(z.Feature) == 0 || z.Dataset != "restrictions" {
					t.Fatalf("restriction entry: %+v", z)
				}
			}
		}
	}
	if !strings.HasPrefix(cellKey, "c5:") {
		t.Fatalf("restriction not listed per cell: %q", cellKey)
	}
	ce := p.Cells[cellKey]
	if ce.Cell != cellKey || ce.CISVersion == "" || !ce.Stale { // uspace_airspace never loaded: stale
		t.Fatalf("cell entry: %+v", ce)
	}
	// After it ends, the restriction does not apply.
	p = e.Project(clk.Now().Add(4 * time.Hour))
	for _, z := range p.Cells[cellKey].Zones {
		if z.Identifier == "DAR0A1F" && (z.Applies || z.CISApplicability != "not_applicable") {
			t.Fatalf("ended restriction: %+v", z)
		}
	}
	var m MemoryProjector
	if last, n := m.Last(); last != nil || n != 0 {
		t.Fatal("empty projector holds something")
	}
	_ = m.ProjectCIS(t.Context(), p)
	if last, n := m.Last(); last != p || n != 1 {
		t.Fatal("projector did not keep the projection")
	}
}

func TestValidity(t *testing.T) {
	if f, to := validity(nil); f != nil || to != nil {
		t.Fatal("no periods have bounds")
	}
	a := ed318.DateTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	b := ed318.DateTime{Time: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
	f, to := validity([]ed318.TimePeriod{{StartDateTime: &b, EndDateTime: &b}, {StartDateTime: &a, EndDateTime: &a}})
	if !f.Equal(a.Time) || !to.Equal(b.Time) {
		t.Fatalf("%v %v", f, to)
	}
	f, to = validity([]ed318.TimePeriod{{StartDateTime: &a}, {EndDateTime: &b}})
	if f != nil || to != nil {
		t.Fatalf("open ends: %v %v", f, to)
	}
}
