//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	intentstore "github.com/rootxkit/uspace-ussp/internal/intent/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
	regstore "github.com/rootxkit/uspace-ussp/internal/registry/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

// geoidUndulationM is the undulation of the integration geoid grid.
const geoidUndulationM = 20

// geoidFile writes a constant geoid grid (GeographicLib PGM, N = 20 m
// everywhere) and returns its path: the real core geoid.Load reads it.
func geoidFile(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("P5\n# Description integration grid, N = 20 m\n# Offset " + fmt.Sprint(geoidUndulationM) + "\n# Scale 1\n2 3\n65535\n")
	b.Write(make([]byte, 2*2*3))
	path := filepath.Join(t.TempDir(), "geoid.pgm")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// withGeoid adds the integration geoid grid as USSP_GEOID_FILE.
func withGeoid(t *testing.T, vars map[string]string) {
	t.Helper()
	vars["USSP_GEOID_FILE"] = geoidFile(t)
}

// switchDSS is a DSS that is available or not (WP-13 brings the real one).
type switchDSS struct{ down atomic.Bool }

func (d *switchDSS) Available(context.Context) (bool, string) {
	if d.down.Load() {
		return false, "the DSS is down (integration)"
	}
	return true, ""
}

// origin is a random point far from every other test's, so intents left
// in the shared database by other tests and runs never meet this one's.
func origin(t *testing.T) core.LatLon {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	u := binary.LittleEndian.Uint64(b[:])
	return core.LatLon{LatDeg: -50 + float64(u%10000)/100, LonDeg: -170 + float64((u/10000)%34000)/100}
}

// edFeature is an ED-318 feature over the box [lat0, lon0, lat1, lon1]
// with a layer from lower to upper metres in ref.
func edFeature(id, typ string, box [4]float64, lower, upper float64, ref string, ext map[string]any) json.RawMessage {
	p := map[string]any{"identifier": id, "country": "GEO", "type": typ, "variant": "COMMON",
		"name": []any{map[string]any{"text": "Integration " + id, "lang": "en-GB"}}, "reason": []string{"SENSITIVE"},
		"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test authority", "lang": "en-GB"}}, "purpose": "AUTHORIZATION"}}}
	if ext != nil {
		p["extendedProperties"] = ext
	}
	b, _ := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{"type": "Polygon",
			"coordinates": []any{[]any{[]any{box[1], box[0]}, []any{box[3], box[0]}, []any{box[3], box[2]}, []any{box[1], box[2]}, []any{box[1], box[0]}}},
			"layer":       map[string]any{"lower": lower, "lowerReference": ref, "upper": upper, "upperReference": ref, "uom": "m"}},
		"properties": p,
	})
	return b
}

func requirementsExt(extra map[string]any) map[string]any {
	r := map[string]any{
		"adjacent": []string{}, "airspace_constraints": map[string]any{}, "operational_conditions": map[string]any{},
		"service_performance": map[string]any{"cis_latency_s": 5, "nid_update_hz": 1, "ti_update_hz": 1},
		"services_required":   []string{"NID", "GEO", "FA", "TI"}, "uas_requirements": map[string]any{},
	}
	for k, v := range extra {
		r[k] = v
	}
	return map[string]any{cis.RequirementsMember: r}
}

// intentRig is the S-M1 composition in process on the real database and
// NATS: the CIS cache against the fake CISP, the registry cache against
// the fake authority, the intent service with its store and the bus
// projector, and the national API with accounts and tokens.
type intentRig struct {
	t        *testing.T
	o        core.LatLon
	cis      *cisRig
	auth     *authority.Fake
	reg      *registry.Cache
	svc      *intent.Service
	dss      *switchDSS
	kv       *bus.Projector
	stack    *stack
	counters *core.Counters
	pol      atomic.Pointer[policy.Record]
}

func newIntentRig(t *testing.T) *intentRig {
	t.Helper()
	g := &intentRig{t: t, o: origin(t), dss: &switchDSS{}, counters: &core.Counters{}}
	r := policy.Record{Version: 1, Values: policy.Defaults()}
	g.pol.Store(&r)
	// Registered first, run last: the CIS tables are left as the other
	// tests expect them (empty), after this rig's cache has stopped.
	t.Cleanup(func() { cleanCIS(t) })
	g.cis = newCISRig(t, time.Hour)
	g.auth = authority.New()
	t.Cleanup(g.auth.Close)
	client, err := registry.NewClient(registry.ClientConfig{BaseURL: g.auth.URL(), Tokens: authority.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	rs := regstore.Store{S: appStore(t)}
	g.reg = registry.NewCache(registry.CacheConfig{Client: client, Store: rs, Projector: registry.NewMemoryProjector(), Audit: rs})
	grid, err := geoid.Load(geoidFile(t))
	if err != nil {
		t.Fatal(err)
	}
	conn := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	// The streams and buckets of PLAN §7, as the api process keeps them.
	if _, err := bus.Ensure(context.Background(), conn.JetStream(), bus.DefaultTopology()); err != nil {
		t.Fatal(err)
	}
	g.kv = bus.NewProjector(conn, nil)
	g.svc = &intent.Service{
		Store: intentstore.Store{S: appStore(t)},
		Decider: &intent.Decider{CIS: g.cis.eval, Integrity: g.cis.cache, Registry: g.reg, DSS: g.dss,
			SystemID: "USSP-DEV", Counters: g.counters},
		Geoid: grid, Policy: func() policy.Record { return *g.pol.Load() }, Counters: g.counters, Logger: quiet(),
		Projector: intent.BusProjector{KV: g.kv, Pub: bus.NewPublisher(conn, g.counters)},
	}
	g.stack = newStackWith(t, newClock(), &logBuffer{}, g.svc)
	return g
}

// box is a square of size degrees at (dlat, dlon) from the rig's origin.
func (g *intentRig) box(dlat, dlon, size float64) [4]float64 {
	return [4]float64{g.o.LatDeg + dlat, g.o.LonDeg + dlon, g.o.LatDeg + dlat + size, g.o.LonDeg + dlon + size}
}

// publish makes features the next version of the dataset and pulls it.
func (g *intentRig) publish(d cis.Dataset, features ...json.RawMessage) {
	g.t.Helper()
	g.cis.fake.Publish(string(d), features...)
	if err := g.cis.cache.Pull(context.Background(), d, nil, false); err != nil {
		g.t.Fatalf("pull %s: %v", d, err)
	}
}

// publishAll publishes every ED-318 dataset (empty where not given).
func (g *intentRig) publishAll(zones, airspaces, restrictions []json.RawMessage) {
	g.t.Helper()
	g.publish(cis.Zones, zones...)
	g.publish(cis.USpaceAirspace, airspaces...)
	g.publish(cis.Restrictions, restrictions...)
	if _, _, stale := g.cis.eval.Age(); stale {
		g.t.Fatal("the CIS is stale after publishing")
	}
}

// operatorClient registers an operator (the accounts registry says
// valid), the fake authority holds its number and a UAS; it returns the
// registration number, the serial and a bearer token with ussp.intents
// for a client the serial is bound to.
func (g *intentRig) operatorClient() (number, serial, token string) {
	g.t.Helper()
	s := g.stack
	s.registry.set(accounts.RegistryValid)
	u := unique()
	number, serial = "GEO-TEST-"+u, "TEST"+u
	user, pass := "intents."+u, "intents-password-"+u
	r := s.call("POST", "/v1/accounts/operators", map[string]any{
		"registration_number": number, "display_name": "Intent operator " + u, "contact_email": "int" + u + "@example.test",
		"admin_username": user, "admin_password": pass,
	}, nil)
	if r.status != 201 {
		g.t.Fatalf("register: %d %s", r.status, r.raw)
	}
	l := s.login("portal", user, pass, "")
	id, secret := s.client(r.str("id"), l.str("token"), "ussp.intents")
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

// window is the volumes' window: an hour from now, half an hour long.
func window() (time.Time, time.Time) {
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	return start, start.Add(30 * time.Minute)
}

func (g *intentRig) volume(b [4]float64, loW84, hiW84 float64, start, end time.Time) map[string]any {
	return map[string]any{
		"volume": map[string]any{
			"outline_polygon": map[string]any{"vertices": []map[string]float64{
				{"lat": b[0], "lng": b[1]}, {"lat": b[0], "lng": b[3]}, {"lat": b[2], "lng": b[3]}, {"lat": b[2], "lng": b[1]}}},
			"altitude_lower": map[string]any{"value": loW84, "reference": "W84", "units": "M"},
			"altitude_upper": map[string]any{"value": hiW84, "reference": "W84", "units": "M"},
		},
		"time_start": map[string]any{"value": start.Format(time.RFC3339), "format": "RFC3339"},
		"time_end":   map[string]any{"value": end.Format(time.RFC3339), "format": "RFC3339"},
	}
}

// request is a request with all ten Annex IV items over box.
func (g *intentRig) request(number, serial, ref string, b [4]float64, kv ...any) map[string]any {
	start, end := window()
	m := map[string]any{
		"client_ref": ref, "uas_serial": serial, "mode": "BVLOS", "flight_type": "normal", "category": "specific",
		"volumes":                   []any{g.volume(b, 120, 170, start, end)},
		"identification_technology": "network", "connectivity_methods": []string{"lte", "radio"}, "endurance_s": 2700,
		"loss_of_c2_procedure": "return to the take-off point", "operator_reg": number,
		"contingency": map[string]any{"procedure": "land at the nearest landing site"}, "emergency_contact_ref": "EC-TEST-" + ref,
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func (g *intentRig) file(token string, m map[string]any) resp {
	g.t.Helper()
	return g.stack.call("POST", "/v1/intents", m, bearer(token))
}

func reasonsOf(r resp) []string {
	var out []string
	cs, _ := r.body["conflicts"].([]any)
	for _, c := range cs {
		m, _ := c.(map[string]any)
		s, _ := m["reason"].(string)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// activeKeys is the set of keys of intent_active.
func (g *intentRig) activeKeys() []string {
	g.t.Helper()
	ks, err := g.kv.Keys(context.Background(), bus.BucketIntentActive)
	if err != nil {
		g.t.Fatal(err)
	}
	return ks
}

// decisionSchema compiles schemas/intent/decision/v1 (and the request
// schema it shares definitions with) offline.
func decisionSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	for _, name := range []string{"request", "decision"} {
		raw, err := os.ReadFile("../../schemas/intent/" + name + "/v1/schema.json")
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource("https://schemas.uspace.ge/intent/"+name+"/v1.json", doc); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.Compile("https://schemas.uspace.ge/intent/decision/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// S-M1: an operator client files an intent with the ten Annex IV items;
// it is authorised with a number and the deviation thresholds; the
// response validates against intent/decision/v1 and has, member by
// member, the shape of the schema's authorised example (E-02); the
// registry was asked once with purpose authorisation and the answer is
// cached (a second intent asks nothing); intent_active holds it; its
// version and audit rows are written.
func TestIntegrationIntentAuthorisedWithTheTenItems(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	r := g.file(token, g.request(number, serial, "ten-items", g.box(0, 0, 0.01)))
	if r.status != 201 || r.str("decision") != "authorised" || r.str("state") != "accepted" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(r.raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := decisionSchema(t).Validate(inst); err != nil {
		t.Fatalf("the response does not validate against intent/decision/v1: %v\n%s", err, r.raw)
	}
	raw, err := os.ReadFile("../../schemas/intent/decision/v1/examples/authorised.json")
	if err != nil {
		t.Fatal(err)
	}
	var example map[string]any
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	for k, want := range example {
		got, ok := r.body[k]
		if !ok {
			t.Errorf("member %s missing", k)
			continue
		}
		if fmt.Sprintf("%T", got) != fmt.Sprintf("%T", want) {
			t.Errorf("member %s is %T, the example's %T", k, got, want)
		}
	}
	if len(r.body) != len(example) {
		t.Errorf("%d members, the example has %d", len(r.body), len(example))
	}
	num := r.str("authorisation_number")
	if !strings.HasPrefix(num, "USSP-DEV-"+number+"-") || len(num) != len("USSP-DEV-"+number+"-")+26 {
		t.Fatalf("authorisation number %q", num)
	}
	th, _ := r.body["deviation_thresholds"].(map[string]any)
	if th["h_m"] != 50.0 || th["v_m"] != 15.0 || th["t_s"] != 60.0 {
		t.Fatalf("thresholds %v", th)
	}
	vs, _ := r.body["volumes_amsl"].([]any)
	if v, _ := vs[0].(map[string]any); v["lower_amsl_m"] != 100.0 || v["upper_amsl_m"] != 150.0 || v["undulation_m"] != 20.0 {
		t.Fatalf("AMSL through the geoid: %v", vs)
	}
	if n := g.auth.Requests("validate"); n != 1 || !slices.Equal(g.auth.Purposes(), []string{"authorisation"}) {
		t.Fatalf("registry asked %d times for %v", n, g.auth.Purposes())
	}
	id := r.str("intent_id")
	if !slices.Contains(g.activeKeys(), id) {
		t.Fatal("not in intent_active")
	}
	if count(t, relOwner(t), "SELECT count(*) FROM intent_versions WHERE intent_id = $1", id) != 1 ||
		events(t, "operational_intent", id, intent.EventSubmitted) != 1 {
		t.Fatal("no version or audit row")
	}
	// The registry answer is cached: a second intent asks nothing.
	r2 := g.file(token, g.request(number, serial, "ten-items-2", g.box(0.1, 0, 0.01)))
	if r2.status != 201 || r2.str("decision") != "authorised" || g.auth.Requests("validate") != 1 {
		t.Fatalf("second: %d %s after %d registry requests", r2.status, r2.raw, g.auth.Requests("validate"))
	}
	if count(t, relOwner(t), "SELECT count(*) FROM registry_validity WHERE key = $1", serial) != 1 {
		t.Fatal("the UAS answer is not cached")
	}
	// GET answers the same body; another operator does not see it.
	got := g.stack.call("GET", "/v1/intents/"+id, nil, bearer(token))
	if got.status != 200 || got.raw != r.raw {
		t.Fatalf("GET: %d %s", got.status, got.raw)
	}
	_, _, other := g.operatorClient()
	if o := g.stack.call("GET", "/v1/intents/"+id, nil, bearer(other)); o.status != 404 {
		t.Fatalf("another operator: %d", o.status)
	}
	if l := g.stack.call("GET", "/v1/intents", nil, bearer(token)); l.status != 200 || !strings.Contains(l.raw, id) {
		t.Fatalf("list: %d", l.status)
	}
}

// S-M1: checked against the CIS cache. A PROHIBITED zone, an active ANSP
// restriction and a zone requiring an authorisation each name what
// refused or held; a volume clear of them inside U-space airspace is
// authorised (the DSS available) or waits pending_dss (the DSS down); a
// newer CIS version held untrusted refuses (cis_outdated) until a
// trusted one replaces it. Every refusal names the conflicting item.
func TestIntegrationIntentCISChecks(t *testing.T) {
	g := newIntentRig(t)
	zoneBox, darBox, reqBox, uspaceBox := g.box(0, 0, 0.02), g.box(0.1, 0, 0.02), g.box(0.2, 0, 0.02), g.box(0.3, 0, 0.2)
	g.publishAll(
		[]json.RawMessage{
			edFeature("TZP-INT", "PROHIBITED", zoneBox, 0, 1000, "AMSL", nil),
			edFeature("TZR-INT", "REQ_AUTHORIZATION", reqBox, 0, 1000, "AMSL", nil),
		},
		[]json.RawMessage{edFeature("TSA-INT", "USPACE", uspaceBox, 0, 3000, "AMSL", requirementsExt(nil))},
		[]json.RawMessage{edFeature("DAR-INT", "PROHIBITED", darBox, 0, 1000, "AMSL", map[string]any{cis.RestrictionMember: map[string]any{
			"id": "DAR-INT", "state": "active", "ansp_ref": "ANSP-INT", "ansp_version": 1, "uspace_airspace_id": "TSA-INT",
			"starts_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "ends_at": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)}})},
	)
	number, serial, token := g.operatorClient()
	cases := []struct {
		ref, decision string
		reasons       []string
		b             [4]float64
		extra         []any
	}{
		{"zone", "rejected", []string{"zone_prohibited"}, g.box(0.005, 0.005, 0.005), nil},
		{"restriction", "rejected", []string{"restriction_active"}, g.box(0.105, 0.005, 0.005), nil},
		{"req-auth", "pending_authority", []string{"zone_requires_authorisation"}, g.box(0.205, 0.005, 0.005), nil},
		{"req-auth-ref", "authorised", nil, g.box(0.205, 0.005, 0.005), []any{"authorisation_ref", "AUTH-TEST-1"}},
		{"uspace", "authorised", nil, g.box(0.35, 0.05, 0.005), nil},
		{"clear", "authorised", nil, g.box(0.6, 0.6, 0.005), nil},
	}
	for _, c := range cases {
		r := g.file(token, g.request(number, serial, c.ref, c.b, c.extra...))
		if r.status != 201 || r.str("decision") != c.decision || !slices.Equal(reasonsOf(r), c.reasons) {
			t.Fatalf("%s: %d %s", c.ref, r.status, r.raw)
		}
		if c.decision == "rejected" {
			cs, _ := r.body["conflicts"].([]any)
			m, _ := cs[0].(map[string]any)
			if m["ref"] == "" || m["item"] != 5.0 || m["overlap"] == nil {
				t.Fatalf("%s: the refusal names nothing: %v", c.ref, m)
			}
		}
		if c.ref == "uspace" && (r.body["in_uspace_airspace"] != true || !strings.Contains(r.raw, "TSA-INT")) {
			t.Fatalf("not inside U-space airspace: %s", r.raw)
		}
	}
	g.dss.down.Store(true)
	if r := g.file(token, g.request(number, serial, "uspace-dss-down", g.box(0.4, 0.05, 0.005))); r.str("decision") != "pending_dss" ||
		r.str("authorisation_number") != "" {
		t.Fatalf("DSS down: %s", r.raw)
	}
	g.dss.down.Store(false)

	// A newer zones version without its publisher's signature is held:
	// the picture is known to be old, nothing is authorised on it.
	g.cis.fake.Publish("zones", edFeature("TZP-INT", "PROHIBITED", zoneBox, 0, 1000, "AMSL", nil))
	g.cis.fake.SetPublisherSignature("zones", 2, "", "")
	if err := g.cis.cache.Pull(context.Background(), cis.Zones, nil, false); err == nil {
		t.Fatal("an unsigned version was accepted")
	}
	r := g.file(token, g.request(number, serial, "outdated", g.box(0.7, 0.7, 0.005)))
	if r.str("decision") != "rejected" || !slices.Equal(reasonsOf(r), []string{"cis_outdated"}) {
		t.Fatalf("held version: %s", r.raw)
	}
	g.publish(cis.Zones, edFeature("TZP-INT", "PROHIBITED", zoneBox, 0, 1000, "AMSL", nil))
	if r := g.file(token, g.request(number, serial, "trusted-again", g.box(0.7, 0.7, 0.005))); r.str("decision") != "authorised" {
		t.Fatalf("after a trusted version: %s", r.raw)
	}
}

// S-M1: two overlapping intents filed in either order give the same pair
// decision: the first filed is authorised, the second refused naming it.
func TestIntegrationIntentEitherOrder(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	for i, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
		boxes := map[string][4]float64{"a": g.box(float64(i), 0, 0.01), "b": g.box(float64(i)+0.005, 0.005, 0.01)}
		first := g.file(token, g.request(number, serial, fmt.Sprintf("order%d-%s", i, order[0]), boxes[order[0]]))
		second := g.file(token, g.request(number, serial, fmt.Sprintf("order%d-%s", i, order[1]), boxes[order[1]]))
		if first.str("decision") != "authorised" || second.str("decision") != "rejected" ||
			!slices.Equal(reasonsOf(second), []string{"intent_filed_first"}) || !strings.Contains(second.raw, first.str("intent_id")) {
			t.Fatalf("%s then %s: %s / %s", order[0], order[1], first.raw, second.raw)
		}
	}
}

// clientOf is the client a serial is bound to.
func clientOf(t *testing.T, serial string) string {
	t.Helper()
	id := ""
	if err := relOwner(t).QueryRow(context.Background(), "SELECT client_id FROM client_serial_bindings WHERE serial = $1", serial).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// First come, first served is ranked when the intents lock is held, not
// when the transaction began. Two overlapping requests race: the first
// begins its transaction first but waits (the hook holds it between
// BEGIN and the advisory lock) while the second begins later, takes the
// lock, decides and commits. The first then locks and decides: it is
// second in line, so it is refused naming the other; exactly one of the
// two is authorised, and nothing is flagged.
func TestIntegrationIntentFirstComeRanksAfterTheLock(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, _ := g.operatorClient()
	clientID := clientOf(t, serial)
	ctx := context.Background()
	b := g.box(0, 0, 0.01)
	early, late := mustJSON(t, g.request(number, serial, "race-early", b)), mustJSON(t, g.request(number, serial, "race-late", b))

	began, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := *g.svc
	slow.Store = intentstore.Store{S: appStore(t), BeforeLock: func(context.Context) {
		once.Do(func() { close(began) })
		<-release
	}}
	type result struct {
		d   intent.Decision
		err error
	}
	first := make(chan result, 1)
	go func() {
		d, _, err := slow.Submit(ctx, clientID, early)
		first <- result{d, err}
	}()
	<-began
	// The first transaction has begun; let the database clock move on so
	// the two cannot share a timestamp.
	time.Sleep(20 * time.Millisecond)
	second, _, err := g.svc.Submit(ctx, clientID, late)
	close(release)
	f := <-first
	if err != nil || f.err != nil {
		t.Fatalf("errors: %v / %v", err, f.err)
	}
	if second.Decision != intent.DecisionAuthorised || f.d.Decision != intent.DecisionRejected ||
		len(f.d.Conflicts) != 1 || f.d.Conflicts[0].Reason != intent.ReasonIntentFirstCome || f.d.Conflicts[0].Ref != second.IntentID {
		t.Fatalf("locked first %s %+v / began first %s %+v", second.Decision, second.Conflicts, f.d.Decision, f.d.Conflicts)
	}
	authorised := count(t, relOwner(t), "SELECT count(*) FROM operational_intents WHERE id = ANY($1::uuid[]) AND decision = 'authorised'",
		[]string{second.IntentID, f.d.IntentID})
	flagged := count(t, relOwner(t), "SELECT count(*) FROM operational_intents WHERE id = $1 AND update_required IS NOT NULL", second.IntentID)
	if authorised != 1 || flagged != 0 {
		t.Fatalf("%d authorised, %d flagged", authorised, flagged)
	}
}

// intentProjected reads an intent's version and projected version.
func intentProjected(t *testing.T, id string) (version, projected int64) {
	t.Helper()
	if err := relOwner(t).QueryRow(context.Background(), "SELECT version, projected_version FROM operational_intents WHERE id = $1", id).
		Scan(&version, &projected); err != nil {
		t.Fatal(err)
	}
	return version, projected
}

// failingProjector is a bus that cannot take a write.
type failingProjector struct{}

func (failingProjector) Project(context.Context, *intent.Record) error {
	return &policy.ProjectionError{Bucket: bus.BucketIntentActive, Err: errors.New("nats: timeout (integration)")}
}

// B-09 after the commit, on PostgreSQL and NATS: a transaction that
// fails at its commit leaves no intent_active key and no row; a bus that
// fails after the commit leaves the decision standing and the row
// unprojected, and the sweep's Republish projects it once the bus takes
// it. Twin: a normal submit is projected at once and marked so.
func TestIntegrationIntentProjectedAfterCommit(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, _ := g.operatorClient()
	clientID := clientOf(t, serial)
	ctx := context.Background()

	before := g.activeKeys()
	failing := *g.svc
	failing.Store = intentstore.Store{S: appStore(t), BeforeCommit: func(context.Context) error { return errors.New("commit refused (integration)") }}
	if _, _, err := failing.Submit(ctx, clientID, mustJSON(t, g.request(number, serial, "commit-fails", g.box(0, 0, 0.01)))); err == nil {
		t.Fatal("a failed commit answered a decision")
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM operational_intents WHERE client_id = $1 AND client_ref = 'commit-fails'", clientID); n != 0 {
		t.Fatalf("%d rows after a failed commit", n)
	}
	if after := g.activeKeys(); len(after) != len(before) {
		t.Fatalf("intent_active gained %d keys after a failed commit", len(after)-len(before))
	}

	down := *g.svc
	down.Projector = failingProjector{}
	d, created, err := down.Submit(ctx, clientID, mustJSON(t, g.request(number, serial, "bus-down", g.box(1, 0, 0.01))))
	if err != nil || !created || d.Decision != intent.DecisionAuthorised {
		t.Fatalf("bus down: %v %v %s", created, err, d.Decision)
	}
	if v, p := intentProjected(t, d.IntentID); v != 1 || p != 0 || slices.Contains(g.activeKeys(), d.IntentID) {
		t.Fatalf("bus down: version %d projected %d, in intent_active %v", v, p, slices.Contains(g.activeKeys(), d.IntentID))
	}
	if _, err := g.svc.Republish(ctx); err != nil {
		t.Fatal(err)
	}
	if v, p := intentProjected(t, d.IntentID); v != 1 || p != 1 || !slices.Contains(g.activeKeys(), d.IntentID) {
		t.Fatalf("after republish: version %d projected %d", v, p)
	}

	ok, _, err := g.svc.Submit(ctx, clientID, mustJSON(t, g.request(number, serial, "bus-up", g.box(2, 0, 0.01))))
	if err != nil || !slices.Contains(g.activeKeys(), ok.IntentID) {
		t.Fatalf("bus up: %v", err)
	}
	if v, p := intentProjected(t, ok.IntentID); v != 1 || p != 1 {
		t.Fatalf("bus up: version %d projected %d", v, p)
	}
}

// S-M1, as reviewed: flight_type special_operation is declared by the
// operator and nothing this USSP can check verifies it yet, so it is
// judged at priority 0 with the condition special_operation_unverified:
// filed over an authorised normal flight it is refused first come, first
// served and flags nothing. At equal priority the first filed wins
// either way: a special operation filed first is authorised and the
// normal flight after it is refused.
func TestIntegrationIntentPriorityAndFirstCome(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	b := g.box(0, 0, 0.01)
	normal := g.file(token, g.request(number, serial, "normal", b))
	special := g.file(token, g.request(number, serial, "special", b, "flight_type", "special_operation", "priority", 100))
	if normal.str("decision") != "authorised" || special.str("decision") != "rejected" || special.body["priority"] != 0.0 ||
		!slices.Equal(reasonsOf(special), []string{"intent_filed_first"}) || !strings.Contains(special.raw, "special_operation_unverified") {
		t.Fatalf("%s / %s", normal.raw, special.raw)
	}
	if count(t, relOwner(t), "SELECT count(*) FROM operational_intents WHERE id = $1 AND update_required IS NOT NULL", normal.str("intent_id")) != 0 {
		t.Fatal("an unverified special operation flagged the normal flight")
	}
	b2 := g.box(1, 0, 0.01)
	first := g.file(token, g.request(number, serial, "special-first", b2, "flight_type", "special_operation"))
	late := g.file(token, g.request(number, serial, "late-normal", b2))
	if first.str("decision") != "authorised" || !strings.Contains(first.raw, "special_operation_unverified") ||
		late.str("decision") != "rejected" || !slices.Equal(reasonsOf(late), []string{"intent_filed_first"}) {
		t.Fatalf("%s / %s", first.raw, late.raw)
	}
}

// S-M1: activation is confirmed in the response inside its window and
// refused before it; end removes the intent from intent_active.
func TestIntegrationIntentActivation(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	now := time.Now().UTC().Truncate(time.Second)
	soon := g.request(number, serial, "soon", g.box(0, 0, 0.01))
	soon["volumes"] = []any{g.volume(g.box(0, 0, 0.01), 120, 170, now.Add(5*time.Minute), now.Add(35*time.Minute))}
	later := g.request(number, serial, "later", g.box(1, 0, 0.01))
	later["volumes"] = []any{g.volume(g.box(1, 0, 0.01), 120, 170, now.Add(3*time.Hour), now.Add(3*time.Hour+30*time.Minute))}
	a, b := g.file(token, soon), g.file(token, later)
	if a.str("decision") != "authorised" || b.str("decision") != "authorised" {
		t.Fatalf("%s / %s", a.raw, b.raw)
	}
	if r := g.stack.call("PATCH", "/v1/intents/"+b.str("intent_id"), map[string]any{"action": "activate"}, bearer(token)); r.status != 409 ||
		r.slug() != "activation_refused" {
		t.Fatalf("three hours early: %d %s", r.status, r.raw)
	}
	r := g.stack.call("PATCH", "/v1/intents/"+a.str("intent_id"), map[string]any{"action": "activate"}, bearer(token))
	if r.status != 200 || r.str("state") != "activated" || r.str("dss_state") != "Activated" || r.str("authorisation_number") != a.str("authorisation_number") {
		t.Fatalf("activate: %d %s", r.status, r.raw)
	}
	e := g.stack.call("PATCH", "/v1/intents/"+a.str("intent_id"), map[string]any{"action": "end", "change_reason": "landed"}, bearer(token))
	if e.status != 200 || e.str("state") != "ended" || slices.Contains(g.activeKeys(), a.str("intent_id")) {
		t.Fatalf("end: %d %s", e.status, e.raw)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM intent_versions WHERE intent_id = $1", a.str("intent_id")); n != 3 {
		t.Fatalf("%d versions", n)
	}
}

// An authorisation whose row carries update_required (a later intent
// with precedence overlaps it, Art. 10(10)) is not activated: the store
// reads the column and activation answers 409 update_required, leaving
// the intent accepted. Twin: an unflagged intent in the same window
// activates.
func TestIntegrationIntentActivationRefusedWhileUpdateRequired(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	now := time.Now().UTC().Truncate(time.Second)
	soon := func(ref string, dlat float64) map[string]any {
		m := g.request(number, serial, ref, g.box(dlat, 0, 0.01))
		m["volumes"] = []any{g.volume(g.box(dlat, 0, 0.01), 120, 170, now.Add(5*time.Minute), now.Add(35*time.Minute))}
		return m
	}
	a, b := g.file(token, soon("flagged", 0)), g.file(token, soon("unflagged", 1))
	if a.str("decision") != "authorised" || b.str("decision") != "authorised" {
		t.Fatalf("%s / %s", a.raw, b.raw)
	}
	if _, err := relOwner(t).Exec(context.Background(),
		`UPDATE operational_intents SET update_required = '{"by_intent_id":"00000000-0000-4000-8000-0000000000ff","reason":"intent_flagged_for_update"}' WHERE id = $1`,
		a.str("intent_id")); err != nil {
		t.Fatal(err)
	}
	r := g.stack.call("PATCH", "/v1/intents/"+a.str("intent_id"), map[string]any{"action": "activate"}, bearer(token))
	if r.status != 409 || r.slug() != "update_required" {
		t.Fatalf("flagged: %d %s", r.status, r.raw)
	}
	if got := g.stack.call("GET", "/v1/intents/"+a.str("intent_id"), nil, bearer(token)); got.str("state") != "accepted" {
		t.Fatalf("flagged intent changed: %s", got.raw)
	}
	if r := g.stack.call("PATCH", "/v1/intents/"+b.str("intent_id"), map[string]any{"action": "activate"}, bearer(token)); r.status != 200 ||
		r.str("state") != "activated" {
		t.Fatalf("unflagged: %d %s", r.status, r.raw)
	}
}

// S-M1: a C0 A1 flight is accepted without an authorisation (Art. 1(3)).
func TestIntegrationIntentC0A1Voluntary(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	g.auth.SetUAS(serial, "active", "C0", "")
	r := g.file(token, g.request(number, serial, "c0", g.box(0, 0, 0.01), "category", "open", "subcategory", "A1", "class_label", "C0", "mode", "VLOS"))
	if r.status != 201 || r.str("decision") != "accepted_voluntary" || r.body["authorisation_number"] != nil || r.body["exempt_art_1_3"] != true {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	// An exempt intent never blocks an authorisation: it is not in the
	// deconfliction set.
	n := g.file(token, g.request(number, serial, "after-c0", g.box(0, 0, 0.01)))
	if n.str("decision") != "authorised" {
		t.Fatalf("after the exempt one: %s", n.raw)
	}
	// The exemption rests on the registry: a C0 the authority does not
	// hold for the UAS (it holds C2) is refused naming item 4.
	number2, serial2, token2 := g.operatorClient()
	g.auth.SetUAS(serial2, "active", "C2", "")
	m := g.file(token2, g.request(number2, serial2, "c0-not-held", g.box(1, 0, 0.01), "category", "open", "subcategory", "A1", "class_label", "C0", "mode", "VLOS"))
	if m.str("decision") != "rejected" || !slices.Equal(reasonsOf(m), []string{"class_label_mismatch"}) || !strings.Contains(m.raw, `"item":4`) {
		t.Fatalf("C0 not held: %s", m.raw)
	}
}

// Idempotency, ownership and the registry down, each with its twin.
func TestIntegrationIntentRefusals(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	m := g.request(number, serial, "idem", g.box(0, 0, 0.01))
	first := g.file(token, m)
	again := g.file(token, m)
	if first.status != 201 || again.status != 200 || again.str("intent_id") != first.str("intent_id") {
		t.Fatalf("replay: %d %d", first.status, again.status)
	}
	m["endurance_s"] = 2800
	if r := g.file(token, m); r.status != 409 || r.slug() != "idempotency_conflict" {
		t.Fatalf("another body: %d %s", r.status, r.raw)
	}
	if r := g.file(token, g.request(number, "TEST-NOT-BOUND", "unbound", g.box(1, 0, 0.01))); r.status != 403 || r.slug() != "serial_not_bound" {
		t.Fatalf("unbound serial: %d %s", r.status, r.raw)
	}
	if r := g.file(token, g.request("GEO-TEST-SOMEONE-ELSE", serial, "other-op", g.box(1, 0, 0.01))); r.status != 403 || r.slug() != "operator_mismatch" {
		t.Fatalf("another operator's number: %d %s", r.status, r.raw)
	}
	bad := g.request(number, serial, "bad", g.box(1, 0, 0.01))
	delete(bad, "loss_of_c2_procedure")
	if r := g.file(token, bad); r.status != 400 || r.slug() != "annex_iv_invalid" || !strings.Contains(r.raw, "annex_iv.9") {
		t.Fatalf("item 9 missing: %d %s", r.status, r.raw)
	}
	if r := g.stack.call("POST", "/v1/intents", m, nil); r.status != 401 {
		t.Fatalf("no token: %d", r.status)
	}
	// The registry cannot be asked and nothing is cached for a new
	// serial: held pending_validation, never authorised.
	number2, serial2, token2 := g.operatorClient()
	g.auth.Down()
	r := g.file(token2, g.request(number2, serial2, "reg-down", g.box(2, 0, 0.01)))
	g.auth.Up()
	if r.str("decision") != "pending_validation" || r.body["authorisation_number"] != nil || !slices.Contains(reasonsOf(r), "registry_unavailable") {
		t.Fatalf("registry down: %s", r.raw)
	}
}

// Decision latency with 1000 active local intents (brief WP-7): the
// 1000 share the outline and the window, stacked in 10 m bands, so the
// store's prefilter returns every one of them and the deconfliction
// judges all 1000 against each new request; p95 must stay within the
// 500 ms of PLAN §9 (printed).
func TestIntegrationIntentLatencyWith1000Active(t *testing.T) {
	g := newIntentRig(t)
	r := policy.Record{Version: 1, Values: policy.Defaults()}
	r.Values.IntentOpenMaxCount = 2000
	g.pol.Store(&r)
	g.publishAll(nil, nil, nil)
	number, serial, _ := g.operatorClient()
	ctx := context.Background()
	clientID := ""
	if err := relOwner(t).QueryRow(ctx, "SELECT client_id FROM client_serial_bindings WHERE serial = $1", serial).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	b := g.box(0, 0, 0.01)
	start, end := window()
	for i := range 1000 {
		m := g.request(number, serial, fmt.Sprintf("bulk-%04d", i), b)
		m["volumes"] = []any{g.volume(b, float64(i*10), float64(i*10+5), start, end)}
		d, _, err := g.svc.Submit(ctx, clientID, mustJSON(t, m))
		if err != nil || d.Decision != intent.DecisionAuthorised {
			t.Fatalf("bulk %d: %v %s", i, err, d.Decision)
		}
	}
	var took []time.Duration
	for i := range 40 {
		m := g.request(number, serial, fmt.Sprintf("probe-%02d", i), b)
		m["volumes"] = []any{g.volume(b, 20000+float64(i*10), 20000+float64(i*10+5), start, end)}
		t0 := time.Now()
		d, _, err := g.svc.Submit(ctx, clientID, mustJSON(t, m))
		took = append(took, time.Since(t0))
		if err != nil || d.Decision != intent.DecisionAuthorised {
			t.Fatalf("probe %d: %v %s %+v", i, err, d.Decision, d.Conflicts)
		}
	}
	slices.Sort(took)
	p95 := took[int(math.Ceil(0.95*float64(len(took))))-1]
	t.Logf("decision latency with 1000 active local intents in the prefilter: p50 %v, p95 %v, max %v (budget p95 500 ms)",
		took[len(took)/2].Round(time.Millisecond), p95.Round(time.Millisecond), took[len(took)-1].Round(time.Millisecond))
	if p95 > 500*time.Millisecond {
		t.Fatalf("p95 %v over 500 ms", p95)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIntegrationScenarioSM1 is the S-M1 scenario of brief WP-7 run in
// process, on the real database and NATS with the fake CISP and the fake
// authority (the lab's runner does not drive this USSP yet, so this run
// stands for the lab scenario and is recorded in docs/RUNBOOKS/WP-7.md).
// It walks the milestone in order and logs one line per step with the
// decision, the reasons and the time it took.
func TestIntegrationScenarioSM1(t *testing.T) {
	// The change feed's cursor is in the shared database: start it from
	// zero, so this fake authority's sequence numbers are read whatever
	// the tests before left there.
	cleanRegistry(t)
	g := newIntentRig(t)
	ctx := context.Background()
	zoneBox, uspaceBox, darBox := g.box(0, 0, 0.02), g.box(0.1, 0, 0.3), g.box(0.3, 0.1, 0.02)
	g.publishAll(
		[]json.RawMessage{edFeature("SC-ZONE", "PROHIBITED", zoneBox, 0, 1000, "AMSL", nil)},
		[]json.RawMessage{edFeature("SC-USP", "USPACE", uspaceBox, 0, 3000, "AMSL", requirementsExt(nil))},
		nil,
	)
	numA, serialA, tokA := g.operatorClient()
	numB, serialB, tokB := g.operatorClient()
	g.auth.SetUAS(serialA, "active", "C0", "") // A flies a C0 aircraft
	step := 0
	expect := func(what string, r resp, decision string, reasons ...string) resp {
		t.Helper()
		step++
		if reasons == nil {
			reasons = []string{}
		}
		got := reasonsOf(r)
		if got == nil {
			got = []string{}
		}
		if r.str("decision") != decision || !slices.Equal(got, reasons) {
			t.Fatalf("step %d %s: %d %s", step, what, r.status, r.raw)
		}
		t.Logf("step %2d %-58s %-18s %v", step, what, decision, got)
		return r
	}
	timed := func(f func() resp) (resp, time.Duration) { t0 := time.Now(); r := f(); return r, time.Since(t0) }

	inUspace := g.box(0.2, 0.1, 0.01)
	a, took := timed(func() resp { return g.file(tokA, g.request(numA, serialA, "sc-a", inUspace)) })
	expect("A files inside U-space airspace", a, "authorised")
	t.Logf("         A authorised in %v, number %s, in %v", took.Round(time.Millisecond), a.str("authorisation_number"), a.body["uspace_airspace_ids"])
	expect("B files the same volume later", g.file(tokB, g.request(numB, serialB, "sc-b", inUspace)), "rejected", "intent_filed_first")
	expect("A files into the PROHIBITED zone", g.file(tokA, g.request(numA, serialA, "sc-zone", g.box(0.005, 0.005, 0.005))), "rejected", "zone_prohibited")
	sp := expect("B files a special operation over A", g.file(tokB, g.request(numB, serialB, "sc-b-special", inUspace, "flight_type", "special_operation")),
		"rejected", "intent_filed_first")
	if !strings.Contains(sp.raw, "special_operation_unverified") ||
		count(t, relOwner(t), "SELECT count(*) FROM operational_intents WHERE id = $1 AND update_required IS NOT NULL", a.str("intent_id")) != 0 {
		t.Fatalf("an unverified special operation took precedence: %s", sp.raw)
	}
	t.Logf("         the special operation is unverified (priority 0): A keeps the space, nothing flagged")

	// The ANSP publishes a restriction: new intents over it are refused.
	g.publish(cis.Restrictions, edFeature("SC-DAR", "PROHIBITED", darBox, 0, 1000, "AMSL", map[string]any{cis.RestrictionMember: map[string]any{
		"id": "SC-DAR", "state": "active", "ansp_ref": "ANSP-SC", "ansp_version": 1, "uspace_airspace_id": "SC-USP",
		"starts_at": time.Now().UTC().Format(time.RFC3339), "ends_at": time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)}}))
	expect("A files over the new ANSP restriction", g.file(tokA, g.request(numA, serialA, "sc-dar", g.box(0.305, 0.105, 0.005))), "rejected", "restriction_active")

	// A newer CIS version arrives unsigned: nothing is authorised on the
	// old picture until a trusted version replaces it.
	g.cis.fake.Publish("uspace_airspace", edFeature("SC-USP", "USPACE", uspaceBox, 0, 3000, "AMSL", requirementsExt(nil)))
	g.cis.fake.SetPublisherSignature("uspace_airspace", 2, "", "")
	_ = g.cis.cache.Pull(ctx, cis.USpaceAirspace, nil, false)
	expect("A files while a newer CIS version is held untrusted", g.file(tokA, g.request(numA, serialA, "sc-held", g.box(0.25, 0.25, 0.005))), "rejected", "cis_outdated")
	g.publish(cis.USpaceAirspace, edFeature("SC-USP", "USPACE", uspaceBox, 0, 3000, "AMSL", requirementsExt(nil)))
	expect("A files again after a trusted version", g.file(tokA, g.request(numA, serialA, "sc-trusted", g.box(0.25, 0.25, 0.005))), "authorised")

	// The DSS goes down: inside U-space airspace the intent waits;
	// outside, local checks suffice.
	g.dss.down.Store(true)
	expect("A files inside U-space airspace with the DSS down", g.file(tokA, g.request(numA, serialA, "sc-dss-in", g.box(0.15, 0.25, 0.005))), "pending_dss", "dss_unavailable")
	expect("A files outside U-space airspace with the DSS down", g.file(tokA, g.request(numA, serialA, "sc-dss-out", g.box(1, 1, 0.005))), "authorised")
	g.dss.down.Store(false)

	// A C0 A1 flight is accepted without an authorisation.
	expect("A files a C0 A1 flight", g.file(tokA, g.request(numA, serialA, "sc-c0", g.box(2, 2, 0.005), "category", "open", "subcategory", "A1",
		"class_label", "C0", "mode", "VLOS")), "accepted_voluntary")

	// The authority suspends B: the change feed invalidates the cached
	// answer and B's next request is refused naming item 10.
	feed := registry.NewFeed(registry.FeedConfig{Client: mustRegistryClient(t, g.auth), Store: regstore.Store{S: appStore(t)},
		Projector: registry.NewMemoryProjector(), Counters: g.reg.Counters()})
	g.auth.SetOperator(numB, "suspended", nil)
	if err := feed.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	expect("B files after the authority suspends B", g.file(tokB, g.request(numB, serialB, "sc-b-suspended", g.box(3, 3, 0.005))), "rejected", "operator_suspended")

	// A cannot activate hours early; A ends its intent and leaves
	// intent_active.
	if r := g.stack.call("PATCH", "/v1/intents/"+a.str("intent_id"), map[string]any{"action": "activate"}, bearer(tokA)); r.status != 409 {
		t.Fatalf("early activation: %d %s", r.status, r.raw)
	}
	step++
	t.Logf("step %2d %-58s %-18s", step, "A activates an hour early (lead 600 s)", "409 activation_refused")
	e := g.stack.call("PATCH", "/v1/intents/"+a.str("intent_id"), map[string]any{"action": "end"}, bearer(tokA))
	if e.str("state") != "ended" || slices.Contains(g.activeKeys(), a.str("intent_id")) {
		t.Fatalf("end: %s", e.raw)
	}
	step++
	t.Logf("step %2d %-58s %-18s", step, "A ends its intent", "ended, out of intent_active")
	t.Logf("counters: %v", g.counters.Snapshot())
}

func mustRegistryClient(t *testing.T, a *authority.Fake) *registry.Client {
	t.Helper()
	c, err := registry.NewClient(registry.ClientConfig{BaseURL: a.URL(), Tokens: authority.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
