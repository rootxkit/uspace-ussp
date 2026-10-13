package coordination

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// testSystem is the code in the ANSP's examples this test compares
// against (schemas/examples/consumed, uspace-ansp's copy): they still
// carry the lab's former code, which the authority refuses (audit M-2).
const testSystem = "USSP-DEV"

var testIntentID = "6f1c0d2e-3b4a-4c5d-8e6f-7a8b9c0d1e2f"

// volumesJSON is one F3548 Volume4D as the intents table stores it, with
// a member the notice schema does not name (left out of the notice).
func volumesJSON(start, end time.Time) json.RawMessage {
	return json.RawMessage(`[{"volume":{"outline_polygon":{"vertices":[{"lat":41.71,"lng":44.78},{"lat":41.71,"lng":44.79},` +
		`{"lat":41.72,"lng":44.79},{"lat":41.72,"lng":44.78}]},"altitude_lower":{"value":520.5,"reference":"W84","units":"M"},` +
		`"altitude_upper":{"value":570.25,"reference":"W84","units":"M"}},"time_start":{"value":"` + start.Format(time.RFC3339) +
		`","format":"RFC3339"},"time_end":{"value":"` + end.Format(time.RFC3339) + `","format":"RFC3339"},"x_note":"not in the schema"}]`)
}

func testIntent() Intent {
	start := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Minute)
	return Intent{ID: testIntentID, AuthorisationNumber: "GE-USSP-DEV-20261004-0001", LocalState: "activated", DSSState: "Activated",
		TimeStart: start, TimeEnd: end, Volumes: volumesJSON(start, end)}
}

func ptr[T any](v T) *T { return &v }

func testDeviation(state, reason string) *Deviation {
	return &Deviation{StateID: 42, FlightID: "0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d", State: state, Reason: reason,
		At: time.Date(2026, 10, 4, 12, 10, 3, 250e6, time.UTC), DistanceOutsideM: ptr(73.5), HeightOverM: ptr(12.0),
		LastPosition: &core.LatLon{LatDeg: 41.7231234, LonDeg: 44.7912345}}
}

var sentAt = time.Date(2026, 10, 4, 12, 10, 4, 123456789, time.UTC)

// annexV compiles the pinned coordination/annex_v/v1 offline.
func annexV(t testing.TB) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "coordination", "annex_v", "v1", "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	const id = "https://schemas.uspace.ge/coordination/annex_v/v1.json"
	if err := c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validateBody(t *testing.T, s *jsonschema.Schema, body []byte) error {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return s.Validate(inst)
}

// E-03: every kind this USSP builds validates against the ANSP's pinned
// schema, and equals its example under schemas/examples/consumed/ (the
// examples are what this USSP sends; USSP_UPDATE_EXAMPLES=1 rewrites
// them). The stored volume's extra member is left out and the
// coordinates keep every digit (float64, not float32).
func TestBuiltNoticesValidateAndMatchTheirExamples(t *testing.T) {
	s := annexV(t)
	dir := filepath.Join("..", "..", "schemas", "examples", "consumed", "coordination", "annex_v", "v1")
	ended := testIntent()
	ended.LocalState, ended.DSSState = "ended", "Activated"
	cases := map[string]struct {
		k       Kind
		in      Intent
		dev     *Deviation
		remarks string
	}{
		"intent-notice":  {KindIntentNotice, testIntent(), nil, "in_controlled_airspace not stated by CIS version 7 for U-space airspace UA1; notified as controlled"},
		"nonconformance": {KindNonconformance, testIntent(), testDeviation("nonconforming", "above_upper"), ""},
		"lost-link":      {KindNonconformance, testIntent(), &Deviation{StateID: 43, State: "lost_link", Reason: "telemetry_lost", At: sentAt}, ""},
		"contingent":     {KindContingent, testIntent(), testDeviation("contingent", "outside_volume_h"), ""},
		"ended":          {KindEnded, ended, nil, ""},
	}
	for name, c := range cases {
		var id int64
		if c.dev != nil {
			id = c.dev.StateID
		}
		n, body, err := Build(c.k, Ref(testSystem, c.k, c.in.ID, id), testSystem, c.in, c.dev, c.remarks, sentAt)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := validateBody(t, s, body); err != nil {
			t.Errorf("%s does not validate: %v\n%s", name, err, body)
		}
		if strings.Contains(string(body), "x_note") || (c.dev != nil && c.dev.LastPosition != nil && !strings.Contains(string(body), "41.7231234")) {
			t.Errorf("%s: extra member kept or a coordinate rounded: %s", name, body)
		}
		if n.Kind != c.k || n.UsspId != testSystem || len(n.Intents) != 1 {
			t.Errorf("%s: %+v", name, n)
		}
		path := filepath.Join(dir, name+".json")
		if os.Getenv("USSP_UPDATE_EXAMPLES") == "1" {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, body, "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (USSP_UPDATE_EXAMPLES=1 writes it)", name, err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, want); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(compact.Bytes(), body) {
			t.Errorf("%s differs from %s:\n got %s\nwant %s", name, path, body, compact.Bytes())
		}
		if err := validateBody(t, s, want); err != nil {
			t.Errorf("%s does not validate: %v", path, err)
		}
	}
}

// The deviation numbers: a nonconformance carries its numbers and
// position; a number that was not judged stays out (never 0).
func TestDeviationNumbers(t *testing.T) {
	d := testDeviation("nonconforming", "threshold_exceeded")
	d.HeightOverM, d.LastPosition = nil, nil
	n, body, err := Build(KindNonconformance, "r", testSystem, testIntent(), d, "", sentAt)
	if err != nil {
		t.Fatal(err)
	}
	nc := n.Nonconformance
	if nc == nil || nc.HeightOverM != nil || nc.Position != nil || nc.DistanceOutsideM == nil || *nc.DistanceOutsideM != 73.5 ||
		string(nc.Reason) != "threshold_exceeded" || !nc.DetectedAt.Equal(d.At) {
		t.Fatalf("%+v %s", nc, body)
	}
	if strings.Contains(string(body), "height_over_m") {
		t.Errorf("height_over_m present: %s", body)
	}
}

// Every notice that cannot be built is refused by name (E-01: each one
// differs from the accepted notice above by one thing).
func TestBuildRefusals(t *testing.T) {
	ok := testIntent()
	for name, c := range map[string]struct {
		k       Kind
		ref, id string
		in      func(*Intent)
		dev     *Deviation
		want    string
	}{
		"unknown kind":       {k: "warning", want: "unknown kind"},
		"no ref":             {ref: "-", want: "notice_ref"},
		"long system id":     {id: strings.Repeat("X", 65), want: "ussp_id"},
		"no number":          {in: func(i *Intent) { i.AuthorisationNumber = "" }, want: "authorisation number"},
		"empty window":       {in: func(i *Intent) { i.TimeEnd = i.TimeStart }, want: "window"},
		"no state":           {k: KindNonconformance, want: "without its conformance state"},
		"not a UUID":         {in: func(i *Intent) { i.ID = "intent-1" }, want: "intent does not read"},
		"volumes not JSON":   {in: func(i *Intent) { i.Volumes = json.RawMessage(`{`) }, want: "not JSON"},
		"no volumes":         {in: func(i *Intent) { i.Volumes = json.RawMessage(`[]`) }, want: "0 volumes"},
		"AMSL limit":         {in: func(i *Intent) { i.Volumes = json.RawMessage(strings.Replace(string(i.Volumes), `"W84"`, `"AMSL"`, 1)) }, want: "W84"},
		"two vertices":       {in: func(i *Intent) { i.Volumes = json.RawMessage(twoVertices) }, want: "2 vertices"},
		"no outline":         {in: func(i *Intent) { i.Volumes = json.RawMessage(`[{"volume":{}}]`) }, want: "exactly one outline"},
		"zero radius":        {in: func(i *Intent) { i.Volumes = json.RawMessage(zeroRadius) }, want: "radius"},
		"too many volumes":   {in: func(i *Intent) { i.Volumes = manyVolumes(MaxVolumes + 1) }, want: "101 volumes"},
		"NaN distance":       {k: KindNonconformance, dev: &Deviation{State: "nonconforming", DistanceOutsideM: ptr(math.NaN())}, want: "distance_outside_m"},
		"negative height":    {k: KindContingent, dev: &Deviation{State: "contingent", HeightOverM: ptr(-1.0)}, want: "height_over_m"},
		"position off earth": {k: KindNonconformance, dev: &Deviation{State: "nonconforming", LastPosition: &core.LatLon{LatDeg: 91}}, want: "WGS84"},
	} {
		in := ok
		if c.in != nil {
			c.in(&in)
		}
		k, ref, id := c.k, c.ref, c.id
		if k == "" {
			k = KindIntentNotice
		}
		switch ref {
		case "":
			ref = "ref-1"
		case "-":
			ref = ""
		}
		if id == "" {
			id = testSystem
		}
		_, _, err := Build(k, ref, id, in, c.dev, "", sentAt)
		var be *BuildError
		if !errors.As(err, &be) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a BuildError naming %q", name, err, c.want)
		}
	}
	if _, _, err := Build(KindIntentNotice, "ref-1", testSystem, ok, nil, strings.Repeat("r", 2000), sentAt); err != nil {
		t.Errorf("long remarks refused instead of clipped: %v", err)
	}
	if _, _, err := Build(KindIntentNotice, "ref-1", testSystem, func() Intent { i := ok; i.Volumes = manyVolumes(MaxVolumes); return i }(), nil, "", sentAt); err != nil {
		t.Errorf("%d volumes refused: %v", MaxVolumes, err)
	}
}

const twoVertices = `[{"volume":{"outline_polygon":{"vertices":[{"lat":41.7,"lng":44.7},{"lat":41.8,"lng":44.8}]}}}]`
const zeroRadius = `[{"volume":{"outline_circle":{"center":{"lat":41.7,"lng":44.7},"radius":{"value":0,"units":"M"}}}}]`

func manyVolumes(n int) json.RawMessage {
	v := `{"volume":{"outline_circle":{"center":{"lat":41.7,"lng":44.7},"radius":{"value":50,"units":"M"}}}}`
	return json.RawMessage("[" + strings.TrimSuffix(strings.Repeat(v+",", n), ",") + "]")
}

func TestReasonKindRefAndState(t *testing.T) {
	for _, c := range []struct{ state, reason, want string }{
		{"lost_link", "", "lost_link"}, {"nonconforming", "telemetry_lost", "lost_link"},
		{"nonconforming", "threshold_exceeded", "threshold_exceeded"}, {"nonconforming", "above_upper", "threshold_exceeded"},
		{"nonconforming", "outside_volume_h", "threshold_exceeded"}, {"nonconforming", "below_lower", "threshold_exceeded"},
		{"nonconforming", "before_start", "threshold_exceeded"}, {"contingent", "after_end", "threshold_exceeded"},
		{"contingent", "restored_from_intent", "other"}, {"nonconforming", "", "other"},
	} {
		if got := string(ReasonOf(c.state, c.reason)); got != c.want {
			t.Errorf("ReasonOf(%s, %s) = %s, want %s", c.state, c.reason, got, c.want)
		}
	}
	for state, want := range map[string]Kind{"nonconforming": KindNonconformance, "lost_link": KindNonconformance, "contingent": KindContingent, "conforming": "", "unknown": ""} {
		if KindOfState(state) != want {
			t.Errorf("KindOfState(%s)", state)
		}
	}
	if Ref("USSP-DEV", KindEnded, testIntentID, 0) != "USSP-DEV:"+testIntentID+":ended" || Ref("X", KindNonconformance, "i", 7) != "X:i:nonconformance:7" {
		t.Error("Ref")
	}
	if !AckRequired(KindNonconformance) || !AckRequired(KindContingent) || AckRequired(KindIntentNotice) || AckRequired(KindEnded) {
		t.Error("AckRequired")
	}
	for local, want := range map[string]string{"activated": "Activated", "nonconforming": "Nonconforming", "contingent": "Contingent", "ended": "Accepted"} {
		in := Intent{LocalState: local, DSSState: "Accepted"}
		if got := string(stateOf(KindEnded, in)); got != want {
			t.Errorf("stateOf(ended, %s) = %s", local, got)
		}
	}
	if string(stateOf(KindEnded, Intent{LocalState: "ended"})) != "Activated" {
		t.Error("an ended intent without a DSS state is Activated")
	}
}

// E-01 pair over Judge: controlled, not controlled, not said and no
// longer held.
func TestJudge(t *testing.T) {
	as := Airspaces{Version: "7", Controlled: map[string]*bool{"UA1": ptr(true), "UA2": ptr(false), "UA3": nil}}
	for _, c := range []struct {
		ids        []string
		controlled bool
		unstated   []string
	}{
		{nil, false, nil}, {[]string{"UA2"}, false, nil}, {[]string{"UA1"}, true, nil},
		{[]string{"UA2", "UA3"}, true, []string{"UA3"}}, {[]string{"GONE"}, true, []string{"GONE"}},
	} {
		got := Judge(c.ids, as)
		if got.Controlled != c.controlled || strings.Join(got.UnstatedIDs, ",") != strings.Join(c.unstated, ",") || got.CISVersion != "7" {
			t.Errorf("Judge(%v) = %+v", c.ids, got)
		}
	}
	if remarksOf(Check{}) != "" || !strings.Contains(remarksOf(Judge([]string{"UA3"}, as)), "UA3") {
		t.Error("remarks")
	}
}
