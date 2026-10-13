package testif

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"

	fp "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/flightplanning"
)

// planJSON is an UpsertFlightPlanRequest in the shape of the suite's
// standard flight intents (flight1_planned moved to the lab's SITL home,
// about 622 m W84), with the standard flight authorisation but the test
// operator's registration.
func planJSON(requestID, usage, serial string, priority int, latShift float64) string {
	now := time.Now().UTC()
	start, end := now.Add(-time.Second).Format(time.RFC3339Nano), now.Add(45*time.Minute).Format(time.RFC3339Nano)
	lat := func(d float64) string { return fmt.Sprintf("%.9f", 41.7151+d+latShift) }
	return `{"request_id": "` + requestID + `", "execution_style": "IfAllowed", "flight_plan": {
	  "basic_information": {"usage_state": "` + usage + `", "uas_state": "Nominal", "area": [{
	    "volume": {"outline_polygon": {"vertices": [
	      {"lat": ` + lat(-0.000200001) + `, "lng": 44.82695},
	      {"lat": ` + lat(0.000100001) + `, "lng": 44.82695},
	      {"lat": ` + lat(0.000100001) + `, "lng": 44.8276},
	      {"lat": ` + lat(-0.000250001) + `, "lng": 44.82765}]},
	    "altitude_lower": {"value": 622.01, "reference": "W84", "units": "M"},
	    "altitude_upper": {"value": 652, "reference": "W84", "units": "M"}},
	    "time_start": {"value": "` + start + `", "format": "RFC3339"},
	    "time_end": {"value": "` + end + `", "format": "RFC3339"}}]},
	  "astm_f3548_21": {"priority": ` + fmt.Sprint(priority) + `},
	  "uspace_flight_authorisation": {"uas_serial_number": "` + serial + `", "operation_category": "Open", "operation_mode": "Vlos",
	    "uas_class": "C0", "identification_technologies": ["ASTMNetRID"], "connectivity_methods": ["cellular"],
	    "endurance_minutes": 50, "emergency_procedure_url": "https://testdummy.interuss.org/emergency",
	    "operator_id": "GEOTESTWP210001", "uas_id": "", "uas_type_certificate": ""}}}`
}

type upsertResult struct {
	Result string `json:"planning_result"`
	Status string `json:"flight_plan_status"`
	Notes  string `json:"notes"`
}

func upsert(t *testing.T, ts *testServer, id, body string) upsertResult {
	t.Helper()
	w := ts.do(t, http.MethodPut, BaseFlightPlanning+"/flight_plans/"+id, ScopePlan, body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT %s: %d %s", id, w.Code, w.Body)
	}
	var r upsertResult
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

const planID = "03e5572a-f733-49af-bc14-8a18bd53ee39"

// A plan the USSP authorises is Completed and Planned, on the real path
// with the Annex IV items mapped and the fixture's; InUse activates it
// (OkToFly); the test director's delete ends it (Closed). The area goes
// as the plan's JSON: no coordinate is rounded on the way.
func TestPlanAuthorisedActivatedClosed(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	r := upsert(t, ts, planID, planJSON("r1", "Planned", "TEST-WP21-0001", 0, 0))
	if r.Result != "Completed" || r.Status != "Planned" {
		t.Fatalf("%+v", r)
	}
	f.intents.mu.Lock()
	post := f.intents.posts[0]
	f.intents.mu.Unlock()
	var sent map[string]any
	if err := json.Unmarshal(post, &sent); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"client_ref": "fp:" + planID + ":1", "uas_serial": "TEST-WP21-0001", "mode": "VLOS",
		"category": "open", "class_label": "C0", "identification_technology": "network", "endurance_s": 3000.0,
		"loss_of_c2_procedure": "https://testdummy.interuss.org/emergency", "operator_reg": "GEOTESTWP210001",
		"flight_type": "normal", "emergency_contact_ref": "TEST-EMERGENCY-CONTACT-0001", "subcategory": "A3"} {
		if got := sent[k]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", k, got, want)
		}
	}
	if _, ok := sent["priority"]; ok {
		t.Error("a normal flight names a priority")
	}
	if !strings.Contains(string(post), `"lat":41.714899999`) {
		t.Errorf("the area's vertex was rounded on the way: %s", post)
	}
	if r := upsert(t, ts, planID, planJSON("r2", "InUse", "TEST-WP21-0001", 0, 0)); r.Result != "Completed" || r.Status != "OkToFly" {
		t.Fatalf("InUse: %+v", r)
	}
	w := ts.do(t, http.MethodDelete, BaseFlightPlanning+"/flight_plans/"+planID, ScopeDirectAutomatedTest, nil)
	var d upsertResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &d) != nil || d.Result != "Completed" || d.Status != "Closed" {
		t.Fatalf("DELETE %d %s", w.Code, w.Body)
	}
	f.intents.mu.Lock()
	patches := slices.Clone(f.intents.patches)
	f.intents.mu.Unlock()
	if len(patches) != 3 || !strings.HasPrefix(patches[0], "modify") || !strings.HasPrefix(patches[1], "activate") || !strings.HasPrefix(patches[2], "end") {
		t.Errorf("patches %v", patches)
	}
	if w := ts.do(t, http.MethodDelete, BaseFlightPlanning+"/flight_plans/1e1e1e1e-1e1e-4e1e-8e1e-1e1e1e1e1e1e", ScopeDirectAutomatedTest, nil); w.Code != http.StatusNotFound {
		t.Errorf("deleting an unknown plan: %d", w.Code)
	}
}

// The refusals, each the intent service's own and each beside its
// accepted twin in TestPlanAuthorisedActivatedClosed: a conflict is
// Rejected with the conflict in the notes; an invalid serial (a 400 of
// the USSP) is Rejected; another operator's registration (403) is
// Rejected; the DSS down (pending_dss) is Failed and says the DSS was
// not written; a priority above normal goes as a special operation.
func TestPlanRefusalsAreTheUSSPs(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	r := upsert(t, ts, "11111111-1111-4111-8111-111111111111", planJSON("c1", "Planned", "TEST-CONFLICT-1", 100, 0))
	if r.Result != "Rejected" || r.Status != "NotPlanned" || !strings.Contains(r.Notes, "higher_priority_intent") {
		t.Errorf("conflict: %+v", r)
	}
	f.intents.mu.Lock()
	var sent map[string]any
	_ = json.Unmarshal(f.intents.posts[0], &sent)
	f.intents.mu.Unlock()
	if sent["flight_type"] != "special_operation" || sent["priority"] != 100.0 {
		t.Errorf("priority 100: flight_type %v priority %v", sent["flight_type"], sent["priority"])
	}
	if r := upsert(t, ts, "22222222-2222-4222-8222-222222222222", planJSON("c2", "Planned", "BAD", 0, 0)); r.Result != "Rejected" || !strings.Contains(r.Notes, "annex_iv_invalid") {
		t.Errorf("invalid serial: %+v", r)
	}
	other := strings.Replace(planJSON("c3", "Planned", "TEST-WP21-0002", 0, 0), `"operator_id": "GEOTESTWP210001"`, `"operator_id": "CHEo5kut30e0mt01-qwe"`, 1)
	if r := upsert(t, ts, "33333333-3333-4333-8333-333333333333", other); r.Result != "Rejected" || !strings.Contains(r.Notes, "operator_mismatch") {
		t.Errorf("another operator: %+v", r)
	}
	f.intents.mu.Lock()
	f.intents.dssDown = true
	f.intents.mu.Unlock()
	if r := upsert(t, ts, "44444444-4444-4444-8444-444444444444", planJSON("c4", "Planned", "TEST-WP21-0003", 0, 0)); r.Result != "Failed" || !strings.Contains(r.Notes, "DSS was not written") {
		t.Errorf("DSS down: %+v", r)
	}
}

// What the USSP's request cannot carry is refused before any call:
// Hypothetical is NotSupported, a plan without U-space flight
// authorisation data or with a category or technology the tables do
// not map is Rejected naming the member.
func TestPlanRefusedBeforeAnyCall(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	hyp := strings.Replace(planJSON("h1", "Planned", "TEST-WP21-0001", 0, 0), `"IfAllowed"`, `"Hypothetical"`, 1)
	if r := upsert(t, ts, planID, hyp); r.Result != "NotSupported" {
		t.Errorf("Hypothetical: %+v", r)
	}
	for name, edit := range map[string][2]string{
		"no authorisation": {`"uspace_flight_authorisation"`, `"not_an_authorisation"`},
		"category":         {`"operation_category": "Open"`, `"operation_category": "Unknown"`},
		"mode":             {`"operation_mode": "Vlos"`, `"operation_mode": "Undeclared"`},
		"technology":       {`["ASTMNetRID"]`, `["Bluetooth5"]`},
	} {
		body := strings.Replace(planJSON("x-"+name, "Planned", "TEST-WP21-0001", 0, 0), edit[0], edit[1], 1)
		if r := upsert(t, ts, planID, body); r.Result != "Rejected" || !strings.Contains(r.Notes, "uspace_flight_authorisation") && name != "no authorisation" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	f.intents.mu.Lock()
	n := len(f.intents.posts)
	f.intents.mu.Unlock()
	if n != 0 {
		t.Errorf("%d intents filed for plans refused before any call", n)
	}
}

// A request id seen before answers the first response again without a
// second call; a plan upserted after a rejection files a new intent
// under the next client_ref; a process that restarted finds the plan's
// intent again through GET /v1/intents and modifies it instead of filing
// a second one (restart persistence through the USSP's record).
func TestPlanReplayRetryAndRestart(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	first := upsert(t, ts, planID, planJSON("same", "Planned", "TEST-CONFLICT-1", 0, 0))
	again := upsert(t, ts, planID, planJSON("same", "Planned", "TEST-WP21-0001", 0, 0))
	if first != again || first.Result != "Rejected" {
		t.Errorf("replay %+v vs %+v", first, again)
	}
	if r := upsert(t, ts, planID, planJSON("next", "Planned", "TEST-WP21-0001", 0, 0)); r.Result != "Completed" {
		t.Fatalf("after the rejection: %+v", r)
	}
	f.intents.mu.Lock()
	refs := []string{}
	for _, p := range f.intents.posts {
		var b struct {
			ClientRef string `json:"client_ref"`
		}
		_ = json.Unmarshal(p, &b)
		refs = append(refs, b.ClientRef)
	}
	f.intents.mu.Unlock()
	if !slices.Equal(refs, []string{"fp:" + planID + ":1", "fp:" + planID + ":2"}) {
		t.Errorf("client refs %v", refs)
	}
	restarted := newTestServer(t, f, nil)
	if r := upsert(t, restarted, planID, planJSON("after-restart", "Planned", "TEST-WP21-0001", 0, 0.00001)); r.Result != "Completed" {
		t.Fatalf("after a restart: %+v", r)
	}
	f.intents.mu.Lock()
	posts, patches := len(f.intents.posts), slices.Clone(f.intents.patches)
	f.intents.mu.Unlock()
	if posts != 2 || len(patches) != 1 || !strings.HasPrefix(patches[0], "modify") {
		t.Errorf("after a restart the plan was filed again: posts %d patches %v", posts, patches)
	}
}

// clear_area ends the open intents whose volumes meet the extent and
// leaves the others; one whose volumes this process does not know is
// ended and named as such.
func TestClearArea(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	upsert(t, ts, "55555555-5555-4555-8555-555555555555", planJSON("in", "Planned", "TEST-WP21-0001", 0, 0))
	upsert(t, ts, "66666666-6666-4666-8666-666666666666", planJSON("out", "Planned", "TEST-WP21-0002", 0, 0.5))
	other := newTestServer(t, f, nil)
	upsert(t, other, "77777777-7777-4777-8777-777777777777", planJSON("unknown", "Planned", "TEST-WP21-0003", 0, 0.5))
	now := time.Now().UTC()
	body := `{"request_id": "clear-1", "extent": {"volume": {"outline_circle": {"center": {"lat": 41.7151, "lng": 44.8273}, "radius": {"value": 500, "units": "M"}},
	  "altitude_lower": {"value": 0, "reference": "W84", "units": "M"}, "altitude_upper": {"value": 3000, "reference": "W84", "units": "M"}},
	  "time_start": {"value": "` + now.Format(time.RFC3339) + `", "format": "RFC3339"}, "time_end": {"value": "` + now.Add(time.Hour).Format(time.RFC3339) + `", "format": "RFC3339"}}}`
	w := ts.do(t, http.MethodPost, BaseFlightPlanning+"/clear_area_requests", ScopeDirectAutomatedTest, body)
	var res struct {
		Outcome struct {
			Success bool                `json:"success"`
			Details map[string][]string `json:"details"`
		} `json:"outcome"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &res) != nil || !res.Outcome.Success {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(res.Outcome.Details["ended"]) != 2 || len(res.Outcome.Details["ended_volumes_unknown"]) != 1 {
		t.Errorf("ended %v (want the one inside and the unknown one)", res.Outcome.Details)
	}
	f.intents.mu.Lock()
	defer f.intents.mu.Unlock()
	states := map[string]string{}
	for _, in := range f.intents.byID {
		states[in.d.ClientRef], _ = in.d.State.(string)
	}
	if states["fp:66666666-6666-4666-8666-666666666666:1"] != stateAccepted || states["fp:55555555-5555-4555-8555-555555555555:1"] != stateEnded {
		t.Errorf("states %v", states)
	}
	if w := ts.do(t, http.MethodPost, BaseFlightPlanning+"/clear_area_requests", ScopePlan, body); w.Code != http.StatusForbidden {
		t.Errorf("clear_area with the plan scope: %d", w.Code)
	}
}

// The status is Ready while the USSP's api is ready, Starting while it
// is not, and names the API and version the pinned file requires.
func TestStatusNamesThePinnedVersion(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "api", "standards", "interuss", "flight_planning", "v1", "flight_planning.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Info struct {
			Title   string `yaml:"title"`
			Version string `yaml:"version"`
		} `yaml:"info"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if fpAPIName != doc.Info.Title || fpAPIVersion != "v"+doc.Info.Version {
		t.Fatalf("status names %s %s, the file %s v%s", fpAPIName, fpAPIVersion, doc.Info.Title, doc.Info.Version)
	}
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	var st fp.StatusResponse
	w := ts.do(t, http.MethodGet, BaseFlightPlanning+"/status", ScopeDirectAutomatedTest, nil)
	if json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Status != fp.StatusResponseStatusReady || *st.ApiVersion != fpAPIVersion {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	f.srv.Close()
	w = ts.do(t, http.MethodGet, BaseFlightPlanning+"/status", ScopeDirectAutomatedTest, nil)
	if json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Status != fp.StatusResponseStatusStarting {
		t.Errorf("the USSP down: %d %s", w.Code, w.Body)
	}
}

// One plan past the bound forgets the oldest, which is found again
// through the USSP (E-10).
func TestPlansAreBounded(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, func(c *Config) { c.MaxPlans = 16 })
	for i := range 17 {
		id := fmt.Sprintf("%08d-0000-4000-8000-000000000000", i)
		upsert(t, ts, id, planJSON("b"+fmt.Sprint(i), "Planned", "TEST-WP21-0001", 0, 0))
	}
	if n := ts.counters.Snapshot()[CounterPlanForgotten]; n != 1 {
		t.Errorf("forgotten %d, want 1", n)
	}
	if ts.plan.recall("00000000-0000-4000-8000-000000000000") != nil {
		t.Error("the oldest plan is still remembered")
	}
}

// Annex IV item 4 asks an open-category flight for its subcategory,
// which FlightAuthorisationData does not carry: the test operator
// declares it (annex_iv.open_subcategory, pending GCAA). A fixture that
// declares none refuses an open plan before any call, naming the item;
// a specific plan needs none and is filed without one (E-01 pair with
// TestPlanAuthorisedActivatedClosed, where the fixture's A3 is sent).
func TestPlanOpenSubcategoryIsTheFixtures(t *testing.T) {
	f := newFakeUSSP(t)
	raw := filepath.Join("..", "..", "deploy", "conformance", "test-operator.yaml")
	b, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	without := writeFile(t, "no-subcategory.yaml", regexp.MustCompile(`(?m)^\s*open_subcategory:.*\n`).ReplaceAllString(string(b), ""))
	ts := newTestServer(t, f, func(c *Config) { c.OperatorFile = without })
	r := upsert(t, ts, planID, planJSON("open", "Planned", "TEST-WP21-0001", 0, 0))
	if r.Result != "Rejected" || !strings.Contains(r.Notes, "annex_iv.open_subcategory") {
		t.Fatalf("an open plan without a declared subcategory: %+v", r)
	}
	f.intents.mu.Lock()
	n := len(f.intents.posts)
	f.intents.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d intents filed for an open plan without a subcategory", n)
	}
	specific := strings.Replace(planJSON("specific", "Planned", "TEST-WP21-0001", 0, 0), `"operation_category": "Open"`, `"operation_category": "Specific"`, 1)
	if r := upsert(t, ts, "1e1e1e1e-1e1e-4e1e-8e1e-1e1e1e1e1e1e", specific); r.Result != "Completed" {
		t.Fatalf("a specific plan: %+v", r)
	}
	f.intents.mu.Lock()
	post := f.intents.posts[0]
	f.intents.mu.Unlock()
	var sent map[string]any
	if err := json.Unmarshal(post, &sent); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["subcategory"]; ok || sent["category"] != "specific" {
		t.Errorf("a specific plan names a subcategory: %s", post)
	}
}
