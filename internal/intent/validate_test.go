package intent

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// problemsOf validates m and returns "field: reason" lines.
func problemsOf(t *testing.T, m map[string]any) []string {
	t.Helper()
	r, err := Decode(encode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	_, probs, err := Validate(r, ValidateEnv{Geoid: fakeGeoid{n: 20}, Now: testNow, SpecialPriority: 100})
	if err != nil {
		t.Fatal(err)
	}
	if probs == nil {
		return nil
	}
	var out []string
	for _, p := range probs.List {
		out = append(out, p.Field+": "+p.Reason)
	}
	return out
}

// The success path, read whole (E-02): every derived value of a valid
// request.
func TestValidateTheSuccessPath(t *testing.T) {
	n := normalise(t, baseRequest())
	if n.Exempt || n.Priority != 0 || n.OperatorPublic != testOperator || n.Serial != testSerial || len(n.Volumes) != 1 {
		t.Fatalf("%+v", n)
	}
	v := n.Volumes[0]
	if v.AMSL != (VolumeAMSL{LowerAMSLM: 480, UpperAMSLM: 530, UndulationM: 20, LowerW84M: 500, UpperW84M: 550}) {
		t.Fatalf("amsl %+v", v.AMSL)
	}
	if !n.TimeStart.Equal(t0) || !n.TimeEnd.Equal(t1) || math.Abs(v.Centroid.LatDeg-41.705) > 1e-9 || math.Abs(v.Centroid.LonDeg-44.805) > 1e-9 || len(n.Cells) != 9 {
		t.Fatalf("window %v %v centroid %+v cells %v", n.TimeStart, n.TimeEnd, v.Centroid, n.Cells)
	}
}

// Every Annex IV item refused names its number; each has its accepted
// twin in baseRequest (E-01).
func TestValidateNamesTheAnnexIVItem(t *testing.T) {
	cases := map[string]struct {
		m    map[string]any
		want string
	}{
		"1 serial missing":       {with(baseRequest(), "uas_serial", ""), "uas_serial: annex_iv.1: a serial number is required"},
		"1 serial not CTA in C1": {with(baseRequest(), "category", "open", "subcategory", "A2", "class_label", "C1"), "uas_serial: annex_iv.1: class C1 requires a CTA-2063-A serial"},
		"2 mode":                 {with(baseRequest(), "mode", "EVLOS"), "mode: annex_iv.2:"},
		"3 flight type":          {with(baseRequest(), "flight_type", "police"), "flight_type: annex_iv.3:"},
		"3 priority claimed":     {with(baseRequest(), "priority", 100), "priority: annex_iv.3: must be 0"},
		"4 category":             {with(baseRequest(), "category", "general"), "category: annex_iv.4:"},
		"4 open without sub":     {with(baseRequest(), "category", "open", "class_label", "C0"), "subcategory: annex_iv.4:"},
		"4 open without class":   {with(baseRequest(), "category", "open", "subcategory", "A1"), "class_label: annex_iv.4: required in the open category"},
		"4 certified no TC":      {with(baseRequest(), "category", "certified", "ua_registration", "UA-TEST-1"), "type_certificate: annex_iv.4:"},
		"4 class label":          {with(baseRequest(), "class_label", "C9"), "class_label: annex_iv.4:"},
		"4 private without mass": {with(baseRequest(), "category", "open", "subcategory", "A1", "privately_built", true), "mtom_kg: annex_iv.4: required"},
		"5 no volumes":           {with(baseRequest(), "volumes", []any{}), "volumes: annex_iv.5:"},
		"5 in the past":          {with(baseRequest(), "volumes", []any{wireVolumeJSON(squareWire(41.7, 44.8, 0.01), 1, 2, testNow.Add(-2*time.Hour), testNow.Add(-time.Hour))}), "volumes[0].time_end: annex_iv.5: is in the past"},
		"5 beyond horizon":       {with(baseRequest(), "volumes", []any{wireVolumeJSON(squareWire(41.7, 44.8, 0.01), 1, 2, testNow.Add(29*24*time.Hour), testNow.Add(31*24*time.Hour))}), "planning horizon of 30 days"},
		"5 upside down":          {with(baseRequest(), "volumes", []any{wireVolumeJSON(squareWire(41.7, 44.8, 0.01), 200, 100, t0, t1)}), "volumes[0].volume.altitude_upper: annex_iv.5:"},
		"5 not W84":              {with(baseRequest(), "volumes", []any{map[string]any{"volume": map[string]any{"outline_polygon": squareWire(41.7, 44.8, 0.01)["outline_polygon"], "altitude_lower": map[string]any{"value": 1, "reference": "SFC", "units": "M"}, "altitude_upper": map[string]any{"value": 2, "reference": "W84", "units": "M"}}, "time_start": map[string]any{"value": t0, "format": "RFC3339"}, "time_end": map[string]any{"value": t1, "format": "RFC3339"}}}), "is not W84"},
		"5 no window":            {with(baseRequest(), "volumes", []any{map[string]any{"volume": wireVolumeJSON(squareWire(41.7, 44.8, 0.01), 1, 2, t0, t1)["volume"]}}), "volumes[0].time_start: annex_iv.5: required"},
		"6 identification":       {with(baseRequest(), "identification_technology", "radio"), "identification_technology: annex_iv.6:"},
		"7 connectivity":         {with(baseRequest(), "connectivity_methods", []string{}), "connectivity_methods: annex_iv.7:"},
		"7 twice":                {with(baseRequest(), "connectivity_methods", []string{"lte", "lte"}), "connectivity_methods[1]: annex_iv.7:"},
		"8 endurance":            {with(baseRequest(), "endurance_s", 0), "endurance_s: annex_iv.8:"},
		"8 short endurance":      {with(baseRequest(), "endurance_s", 600), "endurance_s: annex_iv.8: 600 s does not cover the window of 1800 s"},
		"9 loss of C2":           {with(baseRequest(), "loss_of_c2_procedure", ""), "loss_of_c2_procedure: annex_iv.9: required"},
		"10 operator":            {with(baseRequest(), "operator_reg", "GEO-TEST-1"), "operator_reg: annex_iv.10:"},
		"10 UA registration":     {with(baseRequest(), "category", "certified", "type_certificate", "TC-TEST-1"), "ua_registration: annex_iv.10: required"},
		"client ref":             {with(baseRequest(), "client_ref", "a b"), "client_ref: required"},
		"contingency":            {with(baseRequest(), "contingency", map[string]any{"procedure": ""}), "contingency.procedure: required"},
		"emergency contact":      {with(baseRequest(), "emergency_contact_ref", nil), "emergency_contact_ref: required"},
		"control character":      {with(baseRequest(), "loss_of_c2_procedure", "land\u0007"), "control character"},
		"takeoff":                {with(baseRequest(), "takeoff", map[string]any{"lat": 91, "lng": 0}), "takeoff: is not a valid"},
	}
	for name, c := range cases {
		got := strings.Join(problemsOf(t, c.m), " | ")
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	if got := problemsOf(t, baseRequest()); got != nil {
		t.Fatalf("the base request: %v", got)
	}
}

// The special operation's priority is derived from the policy; a
// normal flight cannot claim it.
func TestValidatePriorityFollowsTheFlightType(t *testing.T) {
	n := normalise(t, with(baseRequest(), "flight_type", "special_operation"))
	if n.Priority != 100 {
		t.Fatalf("special priority %d", n.Priority)
	}
	n = normalise(t, with(baseRequest(), "flight_type", "special_operation", "priority", 100))
	if n.Priority != 100 {
		t.Fatalf("stated priority %d", n.Priority)
	}
	if got := problemsOf(t, with(baseRequest(), "flight_type", "special_operation", "priority", 0)); len(got) != 1 {
		t.Fatalf("a special operation at priority 0: %v", got)
	}
}

// The Art. 1(3) exemption: C0 or privately built under 250 g in A1 only.
func TestValidateExemption(t *testing.T) {
	for _, c := range []struct {
		m      map[string]any
		exempt bool
	}{
		{with(baseRequest(), "category", "open", "subcategory", "A1", "class_label", "C0"), true},
		{with(baseRequest(), "category", "open", "subcategory", "A3", "class_label", "C0"), false},
		{with(baseRequest(), "category", "open", "subcategory", "A1", "privately_built", true, "mtom_kg", 0.249), true},
		{with(baseRequest(), "category", "open", "subcategory", "A1", "privately_built", true, "mtom_kg", 0.25), false},
		{baseRequest(), false},
	} {
		if n := normalise(t, c.m); n.Exempt != c.exempt {
			t.Errorf("%v %v: exempt %v", c.m["subcategory"], c.m["mtom_kg"], n.Exempt)
		}
	}
}

// A missing geoid, or one that cannot answer, refuses the request: the
// AMSL band is never approximated (E-01 pair with the success path).
func TestValidateWithoutGeoidRefuses(t *testing.T) {
	r, err := Decode(encode(t, baseRequest()))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []ValidateEnv{{Now: testNow, SpecialPriority: 100}, {Geoid: fakeGeoid{err: errors.New("outside the grid")}, Now: testNow, SpecialPriority: 100}} {
		n, probs, err := Validate(r, g)
		var ue *UnavailableError
		if !errors.As(err, &ue) || ue.ProblemSlug() != "geoid_unavailable" || ue.HTTPStatus() != 503 || n != nil || probs != nil {
			t.Fatalf("%v %v %v", n, probs, err)
		}
	}
}

// Decode refuses an unknown field at any depth, trailing data and an
// oversized body.
func TestDecodeIsStrict(t *testing.T) {
	good := encode(t, baseRequest())
	if _, err := Decode(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]byte{
		"unknown field":        encode(t, with(baseRequest(), "colour", "red")),
		"unknown nested field": []byte(strings.Replace(string(good), `"altitude_lower":{`, `"altitude_lower":{"datum":"x",`, 1)),
		"trailing data":        append(append([]byte{}, good...), []byte(` {}`)...),
		"wrong type":           encode(t, with(baseRequest(), "endurance_s", "long")),
		"not JSON":             []byte(`{`),
		"too large":            []byte(`{"pad":"` + strings.Repeat("x", MaxRequestBytes) + `"}`),
	}
	for name, b := range bad {
		if _, err := Decode(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// E-10: the volume and vertex bounds refuse one past them.
func TestValidateBounds(t *testing.T) {
	vols := make([]any, MaxVolumes+1)
	for i := range vols {
		vols[i] = wireVolumeJSON(squareWire(41.7, 44.8, 0.01), 500, 550, t0, t1)
	}
	if got := problemsOf(t, with(baseRequest(), "volumes", vols)); len(got) == 0 {
		t.Fatal("MaxVolumes+1 accepted")
	}
	if got := problemsOf(t, with(baseRequest(), "volumes", vols[:MaxVolumes])); got != nil {
		t.Fatalf("MaxVolumes: %v", got)
	}
	verts := make([]map[string]float64, MaxPolygonVertices+1)
	for i := range verts {
		a := float64(i) / float64(len(verts)) * 6.283
		verts[i] = map[string]float64{"lat": 41.7 + 0.01*math.Sin(a), "lng": 44.8 + 0.01*math.Cos(a)}
	}
	big := wireVolumeJSON(map[string]any{"outline_polygon": map[string]any{"vertices": verts}}, 500, 550, t0, t1)
	if got := strings.Join(problemsOf(t, with(baseRequest(), "volumes", []any{big})), "|"); !strings.Contains(got, "at most 1000") {
		t.Fatalf("too many vertices: %s", got)
	}
}

// FuzzDecodeValidate: no input panics the decoder or the validation
// (E-03, LESSONS: never panic on untrusted input).
func FuzzDecodeValidate(f *testing.F) {
	f.Add(encode(f, baseRequest()))
	f.Add([]byte(`{"volumes":[{"volume":{"outline_circle":{"center":{"lat":1,"lng":2},"radius":{"value":-1,"units":"M"}}}}]}`))
	f.Add([]byte(`{"volumes":[{"volume":{"outline_polygon":{"vertices":[]}}}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := Decode(b)
		if err != nil {
			return
		}
		n, probs, err := Validate(r, ValidateEnv{Geoid: fakeGeoid{n: 20}, Now: testNow, SpecialPriority: 100})
		if err == nil && probs == nil && (n == nil || len(n.Volumes) == 0) {
			t.Fatalf("accepted without volumes: %s", b)
		}
		if n != nil {
			if _, err := json.Marshal(n.Request); err != nil {
				t.Fatal(err)
			}
		}
	})
}
