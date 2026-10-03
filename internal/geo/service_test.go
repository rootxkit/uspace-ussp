package geo

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

type cisRig struct {
	fake  *cisp.Fake
	now   time.Time
	eval  *cis.Evaluator
	cache *cis.Cache
	stale float64
}

func (g *cisRig) clock() time.Time { return g.now }

func newCISRig(t *testing.T) *cisRig {
	t.Helper()
	fake, err := cisp.NewTLS()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	g := &cisRig{fake: fake, now: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), stale: 300}
	g.eval = cis.NewEvaluator(cis.EvaluatorConfig{StaleS: func() float64 { return g.stale }, Now: g.clock})
	client, err := cis.NewClient(cis.ClientConfig{BaseURL: fake.URL(), Tokens: cisp.Tokens{}, HTTPClient: fake.Client()})
	if err != nil {
		t.Fatal(err)
	}
	pubs, err := coreauth.NewDetachedVerifier(t.Context(), coreauth.DetachedConfig{Publishers: fake.PublisherKeys(), MaxAge: 366 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	g.cache = cis.NewCache(cis.CacheConfig{Client: client, Publishers: pubs, Evaluator: g.eval, Now: g.clock})
	return g
}

func (g *cisRig) publish(t *testing.T, d cis.Dataset, fs ...feat) {
	t.Helper()
	raws := make([]json.RawMessage, len(fs))
	for i := range fs {
		raws[i] = fs[i].json()
	}
	g.fake.Publish(string(d), raws...)
	if err := g.cache.Pull(t.Context(), d, nil, true); err != nil {
		t.Fatal(err)
	}
}

func requirementsBlock() map[string]any {
	return map[string]any{"uspace_requirements": map[string]any{
		"uas_requirements":       map[string]any{"geo_awareness": "required"},
		"service_performance":    map[string]any{"nid_update_hz": 1, "ti_update_hz": 1, "cis_latency_s": 1},
		"operational_conditions": map[string]any{},
		"airspace_constraints":   map[string]any{"max_height_agl_m": 120, "in_controlled_airspace": false},
		"services_required":      []string{"NID", "GEO", "FA", "TI"},
		"adjacent":               []string{"TSA002"},
	}}
}

// The done-when of /v1/geo in unit form: after the CISP publishes zones,
// an airspace and a restriction, a box query carries each with
// updated_at, version and valid_from/to; a window query returns a zone
// whose limitedApplicability opens next week and omits one that closed
// yesterday (E-01 both); stale says so with its age.
func TestGeoAnswersFromTheCache(t *testing.T) {
	g := newCISRig(t)
	nextWeek := amslZone("TZW001", "PROHIBITED")
	nextWeek.applicability = []any{map[string]any{"startDateTime": "2026-10-10T00:00:00Z", "endDateTime": "2026-10-11T00:00:00Z"}}
	yesterday := amslZone("TZW002", "PROHIBITED")
	yesterday.applicability = []any{map[string]any{"startDateTime": "2026-10-01T00:00:00Z", "endDateTime": "2026-10-02T00:00:00Z"}}
	g.publish(t, cis.Zones, amslZone("TZP001", "PROHIBITED"), nextWeek, yesterday)
	asp := amslZone("TSA001", "USPACE")
	asp.extended = requirementsBlock()
	g.publish(t, cis.USpaceAirspace, asp)
	r := amslZone("TRS001", "PROHIBITED")
	r.applicability = []any{map[string]any{"startDateTime": "2026-10-03T08:00:00Z", "endDateTime": "2026-10-03T12:00:00Z"}}
	r.extended = map[string]any{"cis_restriction": map[string]any{"id": "TRS001", "ansp_ref": "ANSP-1", "ansp_version": 1, "state": "active",
		"starts_at": "2026-10-03T08:00:00Z", "ends_at": "2026-10-03T12:00:00Z", "uspace_airspace_id": "TSA001", "ended_by": nil}}
	g.publish(t, cis.Restrictions, r)
	svc := &Service{CIS: g.eval, Now: g.clock}
	box, err := ParseBBox("44.79,41.69,44.86,41.74")
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Box(box, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Stale || a.CISVersion != "zones:1,uspace_airspace:1,restrictions:1" || a.At == nil || !a.At.Equal(g.now) {
		t.Fatalf("basis %+v", a)
	}
	if len(a.Zones) != 1 || a.Zones[0].Identifier != "TZP001" || a.Zones[0].Version != "zones:1" || a.Zones[0].UpdatedAt == nil ||
		a.Zones[0].Applicability.Kind != "always" || a.Zones[0].Applicability.AtState != "applies" || len(a.Zones[0].Parts) != 1 ||
		a.Zones[0].Parts[0].Upper == nil || a.Zones[0].Parts[0].Upper.Ref != "AMSL" || len(a.Zones[0].Feature) == 0 {
		t.Fatalf("zones %+v", a.Zones)
	}
	if len(a.USpaceAirspaces) != 1 || len(a.USpaceAirspaces[0].ServicesRequired) != 4 || a.USpaceAirspaces[0].Adjacent[0] != "TSA002" ||
		string(a.USpaceAirspaces[0].Requirements) == "null" {
		t.Fatalf("airspaces %+v", a.USpaceAirspaces)
	}
	if len(a.Restrictions) != 1 || a.Restrictions[0].State == nil || *a.Restrictions[0].State != "active" || a.Restrictions[0].StartsAt == nil ||
		a.Restrictions[0].ValidFrom == nil || a.Restrictions[0].ValidTo == nil || !a.Restrictions[0].ValidTo.Equal(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("restrictions %+v", a.Restrictions)
	}
	// The window of the coming fortnight: next week's zone is there,
	// yesterday's is not.
	w, err := svc.Windows([]Window{{Box: box, From: g.now, To: g.now.Add(14 * 24 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]Item{}
	for _, z := range w.Zones {
		ids[z.Identifier] = z
	}
	if z, ok := ids["TZW001"]; !ok || z.Applicability.Kind != "during" || z.ValidFrom == nil {
		t.Fatalf("next week's zone: %+v", w.Zones)
	}
	if _, ok := ids["TZW002"]; ok {
		t.Fatal("yesterday's zone listed")
	}
	// At an instant next week its zone applies.
	nw, err := svc.Box(box, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || len(nw.Zones) != 2 {
		t.Fatalf("next week %+v %v", nw.Zones, err)
	}
	// Stale beyond the bound: said, with the age, and still answered.
	g.now = g.now.Add(10 * time.Minute)
	st, err := svc.Box(box, time.Time{})
	if err != nil || !st.Stale || st.CISAgeS < 600 || len(st.Zones) != 1 {
		t.Fatalf("stale %+v %v", st, err)
	}
}

// A list longer than its bound says truncated (E-10); a window longer
// than MaxWindow is refused, never answered empty.
func TestGeoBounds(t *testing.T) {
	g := newCISRig(t)
	g.publish(t, cis.Zones, amslZone("TZP001", "PROHIBITED"), amslZone("TZP002", "PROHIBITED"), amslZone("TZP003", "PROHIBITED"))
	svc := &Service{CIS: g.eval, Now: g.clock, MaxItems: 2}
	box, _ := ParseBBox("44.79,41.69,44.86,41.74")
	a, err := svc.Box(box, time.Time{})
	if err != nil || !a.Truncated || len(a.Zones) != 2 {
		t.Fatalf("%+v %v", a, err)
	}
	_, err = svc.Windows([]Window{{Box: box, From: g.now, To: g.now.Add(MaxWindow + time.Hour)}})
	var na *NotAnsweredError
	if !errors.As(err, &na) || na.HTTPStatus() != 400 {
		t.Fatalf("long window: %v", err)
	}
	if _, err := svc.Windows(nil); err == nil {
		t.Fatal("no window answered")
	}
}

func TestParseBBox(t *testing.T) {
	for _, bad := range []string{"", "1,2,3", "a,1,2,3", "44,41,43,42", "40,40,50,50", "44,91,45,92", "NaN,1,2,3"} {
		if _, err := ParseBBox(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	b, err := ParseBBox("44.79, 41.69, 44.86, 41.74")
	if err != nil || b != (geodesy.BBox{MinLon: 44.79, MinLat: 41.69, MaxLon: 44.86, MaxLat: 41.74}) {
		t.Fatalf("%+v %v", b, err)
	}
}
