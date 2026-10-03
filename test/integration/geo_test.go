//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/national"
)

// geoFeature is an ED-318 feature over box [lat0, lon0, lat1, lon1],
// lower to upper metres AMSL, with limitedApplicability and extended
// properties when given.
func geoFeature(id, typ string, box [4]float64, lower, upper float64, appl []any, ext map[string]any) json.RawMessage {
	p := map[string]any{"identifier": id, "country": "GEO", "type": typ, "variant": "COMMON",
		"name": []any{map[string]any{"text": "Integration " + id, "lang": "en-GB"}}, "reason": []string{"SENSITIVE"},
		"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test authority", "lang": "en-GB"}}, "purpose": "AUTHORIZATION"}},
		"dataSource":    map[string]any{"updateDateTime": "2026-10-01T08:00:00Z"}}
	if typ == "USPACE" {
		p["reason"] = []string{"AIR_TRAFFIC"}
	}
	if appl != nil {
		p["limitedApplicability"] = appl
	}
	if ext != nil {
		p["extendedProperties"] = ext
	}
	b, _ := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{"type": "Polygon",
			"coordinates": []any{[]any{[]any{box[1], box[0]}, []any{box[3], box[0]}, []any{box[3], box[2]}, []any{box[1], box[2]}, []any{box[1], box[0]}}},
			"layer":       map[string]any{"lower": lower, "lowerReference": "AMSL", "upper": upper, "upperReference": "AMSL", "uom": "m"}},
		"properties": p,
	})
	return b
}

// restrictionExt is the CISP's cis_restriction of a restriction.
func restrictionExt(id, state string, starts, ends time.Time) map[string]any {
	return map[string]any{cis.RestrictionMember: map[string]any{"id": id, "ansp_ref": "ANSP-" + id, "ansp_version": 1, "state": state,
		"starts_at": starts.UTC().Format(time.RFC3339), "ends_at": ends.UTC().Format(time.RFC3339), "uspace_airspace_id": "TSA001", "ended_by": nil}}
}

func period(from, to time.Time) []any {
	return []any{map[string]any{"startDateTime": from.UTC().Format(time.RFC3339), "endDateTime": to.UTC().Format(time.RFC3339)}}
}

// operatorWith is intentRig.operatorClient with the scopes given.
func (g *intentRig) operatorWith(scopes ...string) (number, serial, token string) {
	g.t.Helper()
	s := g.stack
	s.registry.set(accounts.RegistryValid)
	u := unique()
	number, serial = "GEO-TEST-"+u, "TEST"+u
	user, pass := "geo."+u, "geo-password-"+u
	r := s.call("POST", "/v1/accounts/operators", map[string]any{
		"registration_number": number, "display_name": "Geo operator " + u, "contact_email": "geo" + u + "@example.test",
		"admin_username": user, "admin_password": pass,
	}, nil)
	if r.status != 201 {
		g.t.Fatalf("register: %d %s", r.status, r.raw)
	}
	l := s.login("portal", user, pass, "")
	id, secret := s.client(r.str("id"), l.str("token"), scopes...)
	b := s.call("POST", "/v1/accounts/operators/"+r.str("id")+"/clients/"+id+"/serials", map[string]any{"serial": serial}, bearer(l.str("token")))
	if b.status != 201 {
		g.t.Fatalf("bind: %d %s", b.status, b.raw)
	}
	tok := s.token(id, secret)
	if tok.status != 200 {
		g.t.Fatalf("token: %d %s", tok.status, tok.raw)
	}
	g.auth.SetOperator(number, "active", nil)
	g.auth.SetUAS(serial, "active", "", "")
	return number, serial, tok.str("access_token")
}

// geoStack replaces the rig's national API with one that also serves
// /v1/geo* from the rig's CIS cache.
func (g *intentRig) geoStack() {
	g.t.Helper()
	svc := &geo.Service{CIS: g.cis.eval}
	g.stack = newStackWith(g.t, newClock(), &logBuffer{}, g.svc, func(s *national.Server) {
		s.Geo = &national.Geo{Service: svc, Intents: g.svc}
	})
}

// The /v1/geo done-when (brief WP-12): after the fake CISP publishes
// zones, a U-space airspace and a restriction, the answer for a box
// carries each with updated_at, version and valid_from/to; a window
// query (the intent's) returns a zone whose limitedApplicability opens
// next week and omits one that closed yesterday (E-01 both). Another
// operator's intent is 404, and a token without ussp.geo is refused.
func TestIntegrationGeoAwareness(t *testing.T) {
	g := newIntentRig(t)
	g.geoStack()
	now := time.Now().UTC()
	whole := g.box(0, 0, 0.05)
	nextWeek := geoFeature("TZW001", "PROHIBITED", whole, 0, 3000, period(now.Add(6*24*time.Hour), now.Add(8*24*time.Hour)), nil)
	yesterday := geoFeature("TZW002", "PROHIBITED", whole, 0, 3000, period(now.Add(-48*time.Hour), now.Add(-24*time.Hour)), nil)
	always := geoFeature("TZC001", "CONDITIONAL", whole, 2000, 3000, nil, nil)
	airspace := geoFeature("TSA001", "USPACE", g.box(-1, -1, 2), 0, 5000, nil, requirementsExt(nil))
	rs := geoFeature("TRS009", "PROHIBITED", g.box(0.2, 0.2, 0.05), 0, 3000, period(now.Add(-time.Hour), now.Add(time.Hour)),
		restrictionExt("TRS009", "active", now.Add(-time.Hour), now.Add(time.Hour)))
	g.publishAll([]json.RawMessage{nextWeek, yesterday, always}, []json.RawMessage{airspace}, []json.RawMessage{rs})

	number, serial, token := g.operatorWith("ussp.intents", "ussp.geo")
	q := fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", g.o.LonDeg-0.1, g.o.LatDeg-0.1, g.o.LonDeg+0.5, g.o.LatDeg+0.5)
	r := g.stack.call("GET", "/v1/geo?bbox="+q, nil, bearer(token))
	if r.status != 200 {
		t.Fatalf("geo: %d %s", r.status, r.raw)
	}
	var a geo.Answer
	if err := json.Unmarshal([]byte(r.raw), &a); err != nil {
		t.Fatal(err)
	}
	if a.Stale || !strings.HasPrefix(a.CISVersion, "zones:") || a.CISAgeS < 0 {
		t.Fatalf("basis %+v", a)
	}
	if len(a.Zones) != 1 || a.Zones[0].Identifier != "TZC001" || a.Zones[0].UpdatedAt == nil || !strings.HasPrefix(a.Zones[0].Version, "zones:") {
		t.Fatalf("zones now %+v", a.Zones)
	}
	if len(a.USpaceAirspaces) != 1 || len(a.USpaceAirspaces[0].ServicesRequired) != 4 || a.USpaceAirspaces[0].UpdatedAt == nil {
		t.Fatalf("airspaces %+v", a.USpaceAirspaces)
	}
	if len(a.Restrictions) != 1 || a.Restrictions[0].State == nil || *a.Restrictions[0].State != "active" || a.Restrictions[0].ValidFrom == nil ||
		a.Restrictions[0].ValidTo == nil || a.Restrictions[0].UpdatedAt == nil {
		t.Fatalf("restrictions %+v", a.Restrictions)
	}
	t.Logf("GET /v1/geo: %d zone, %d airspace, %d restriction at %s, cis %s age %.1f s", len(a.Zones), len(a.USpaceAirspaces),
		len(a.Restrictions), a.At.Format(time.RFC3339), a.CISVersion, a.CISAgeS)

	// An intent flying next week, above the CONDITIONAL zone's floor is
	// irrelevant here: its window is the query. Its geo answer lists the
	// zone that opens next week and not the one that closed yesterday.
	start := now.Add(6*24*time.Hour + time.Hour).Truncate(time.Second)
	req := g.request(number, serial, "geo-"+unique(), g.box(0.01, 0.01, 0.005))
	req["volumes"] = []any{g.volume(g.box(0.01, 0.01, 0.005), 120, 170, start, start.Add(30*time.Minute))}
	d := g.file(token, req)
	if d.status != 201 {
		t.Fatalf("file: %d %s", d.status, d.raw)
	}
	wr := g.stack.call("GET", "/v1/geo/intents/"+d.str("intent_id"), nil, bearer(token))
	if wr.status != 200 {
		t.Fatalf("geo intent: %d %s", wr.status, wr.raw)
	}
	var w geo.Answer
	if err := json.Unmarshal([]byte(wr.raw), &w); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, z := range w.Zones {
		ids[z.Identifier] = true
	}
	if !ids["TZW001"] || ids["TZW002"] || w.IntentID == nil || w.From == nil || !w.From.Equal(start) {
		t.Fatalf("window answer %v from %v", ids, w.From)
	}
	// Another operator's intent is not found; a token without ussp.geo
	// is refused.
	_, _, other := g.operatorWith("ussp.intents", "ussp.geo")
	if r := g.stack.call("GET", "/v1/geo/intents/"+d.str("intent_id"), nil, bearer(other)); r.status != 404 {
		t.Fatalf("another operator's intent: %d", r.status)
	}
	_, _, noGeo := g.operatorWith("ussp.intents")
	if r := g.stack.call("GET", "/v1/geo?bbox="+q, nil, bearer(noGeo)); r.status != 403 {
		t.Fatalf("without ussp.geo: %d", r.status)
	}
	if r := g.stack.call("GET", "/v1/geo?bbox=1,2,3", nil, bearer(token)); r.status != 400 {
		t.Fatalf("bad bbox: %d", r.status)
	}
}
