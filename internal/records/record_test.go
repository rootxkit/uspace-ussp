package records

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

const (
	flightID = "0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d"
	intentID = "6f1c0d2e-3b4a-4c5d-8e6f-7a8b9c0d1e2f"
	clientID = "client-1"
)

var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// fixture is a flight with one hole (a silence the work queue explains),
// a proximity alert and a nonconformance alert both raised and cleared,
// the conformance timeline, a notice and two products shown.
func fixture() (*memReader, *memSeries) {
	m := newMemReader(t0.Add(26 * time.Hour))
	end := t0.Add(20 * time.Minute)
	m.flights[flightID] = FlightRow{ID: flightID, IntentID: ptr(intentID), AuthorisationNumber: ptr("GE-DEV01-1"), UASSerial: "TEST0001",
		OperatorReg: ptr("GEO87astrdge12k8-xyz"), ClientID: ptr(clientID), StartedAt: t0, EndedAt: &end, EndReason: ptr("landed")}
	m.intents[intentID] = IntentRow{IntentView: IntentView{IntentID: intentID, Version: 2, LocalState: "ended", Decision: ptr("authorised"),
		TimeStart: t0, TimeEnd: t0.Add(time.Hour), Volumes: rawJSON(`[]`), DeviationThresholds: rawJSON(`{"h_m":50,"v_m":15,"t_s":60}`),
		Conflicts: rawJSON(`[]`), PolicyVersion: ptr(int64(3)), UASSerial: "TEST0001"}, OperatorReg: ptr("GEO87astrdge12k8-xyz")}
	m.versions = []Version{{Version: 1, At: t0.Add(-time.Hour), Actor: clientID, ChangeReason: "submitted", Decision: rawJSON(`{"decision":"authorised","policy_version":3}`)},
		{Version: 2, At: t0, Actor: clientID, ChangeReason: "activated by the operator", Decision: rawJSON(`{"state":"activated","policy_version":3}`)}}
	m.alerts = []Alert{
		{AlertID: "5f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c", Kind: "proximity", Severity: "critical", State: "cleared", RaisedAt: t0.Add(5 * time.Minute),
			UpdatedAt: t0.Add(6 * time.Minute), ClearedAt: ptr(t0.Add(6 * time.Minute)), ClearReason: ptr("resolved"), PolicyVersion: 3,
			Detail: rawJSON(`{"d_cpa_h_m":40,"d_alt_m":10,"peer":{"track_id":"trk:x","trust":"authenticated"}}`)},
		{AlertID: "6f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c", Kind: "nonconformance", Severity: "critical", State: "cleared", RaisedAt: t0.Add(8 * time.Minute),
			UpdatedAt: t0.Add(10 * time.Minute), ClearedAt: ptr(t0.Add(10 * time.Minute)), ClearReason: ptr("resolved"), PolicyVersion: 4, Detail: rawJSON(`{}`)},
	}
	m.states = []ConformanceState{{At: t0, State: "conforming", PolicyVersion: 3},
		{At: t0.Add(8 * time.Minute), State: "nonconforming", Reason: ptr("above_upper"), HeightOverM: ptr(21.0), PolicyVersion: 4, ATSNotifiedAt: ptr(t0.Add(8 * time.Minute)), ATSAckRef: ptr("ACK1")},
		{At: t0.Add(10 * time.Minute), State: "conforming", PolicyVersion: 4}}
	m.notices = []Notice{{NoticeRef: "DEV01:" + intentID + ":nonconformance:2", Kind: "nonconformance", State: "acknowledged", CreatedAt: t0.Add(8 * time.Minute)}}
	m.gaps = []IngestGap{{Cause: "ingest_queue_age", Started: t0.Add(12*time.Minute + 2*time.Second), Ended: t0.Add(12*time.Minute + 8*time.Second), Dropped: 7}}
	m.policies = []PolicyVersion{{PolicyVersion: 3, CreatedAt: t0.Add(-48 * time.Hour), Values: rawJSON(`{"record_gap_s":3}`)},
		{PolicyVersion: 4, CreatedAt: t0.Add(-time.Hour), Values: rawJSON(`{"record_gap_s":3}`)}}
	s := &memSeries{errOf: map[string]error{},
		summary:  Summary{Samples: 1190, FirstAt: ptr(t0), LastAt: ptr(end), BBox: &[4]float64{44.78, 41.71, 44.79, 41.72}, MaxAltAMSLM: ptr(552.5)},
		silences: []Silence{{After: t0.Add(12*time.Minute + time.Second), Before: t0.Add(12*time.Minute + 9*time.Second)}},
		products: []Product{{At: t0.Add(5 * time.Minute), IntentID: ptr(intentID), TracksShown: rawJSON(`[{"id":"trk:x","trust":"authenticated","age_s":1}]`), Degraded: []string{}, PolicyVersion: 3},
			{At: t0.Add(5*time.Minute + 10*time.Second), TracksShown: rawJSON(`[]`), Degraded: []string{"manned: unavailable"}, PolicyVersion: 3}}}
	return m, s
}

func builder(m *memReader, s Series) *Builder {
	return &Builder{Reader: m, Series: s, USSPID: "DEV01", Policy: func() policy.Record { return policy.Record{Version: 4, Values: policy.Defaults()} }}
}

// flightRecordSchema compiles FlightRecord of api/openapi.yaml.
func flightRecordSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("https://ussp.test/openapi.json", inst); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("https://ussp.test/openapi.json#/components/schemas/FlightRecord")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validateRecord(t *testing.T, s *jsonschema.Schema, r Record) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("the record does not validate against FlightRecord: %v\n%s", err, b)
	}
	return b
}

// forbidden are the member names of personal data (spec 06 §5).
var forbidden = []string{"name", "display_name", "username", "email", "contact_email", "phone", "address", "first_name", "last_name", "full_name", "holder_name"}

// walk fails on every forbidden member anywhere in v.
func walk(t *testing.T, path string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			for _, f := range forbidden {
				if strings.EqualFold(k, f) || strings.HasSuffix(strings.ToLower(k), "_"+f) {
					t.Errorf("%s.%s: a personal-data member in a record", path, k)
				}
			}
			walk(t, path+"."+k, c)
		}
	case []any:
		for i, c := range x {
			walk(t, path+"["+string(rune('0'+i%10))+"]", c)
		}
	}
}

// The done-when record: the hole with its cause, both alerts with raise
// and clear, the timeline with the ANSP's facts, the products shown,
// every policy version named with its values; it validates against
// FlightRecord and no member anywhere is a name (a test walks the JSON),
// and the registration number is only its public part.
func TestFlightRecord(t *testing.T) {
	m, s := fixture()
	r, err := builder(m, s).Flight(context.Background(), flightID)
	if err != nil {
		t.Fatal(err)
	}
	b := validateRecord(t, flightRecordSchema(t), r)
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	walk(t, "record", doc)
	if strings.Contains(string(b), "xyz") || r.Flight.OperatorRegPublic == nil || *r.Flight.OperatorRegPublic != "GEO87astrdge12k8" {
		t.Errorf("the registration number's secret part is in the record: %v", r.Flight.OperatorRegPublic)
	}
	tel := r.Telemetry
	if tel.State != StateIncluded || tel.Samples != 1190 || len(tel.Holes) != 1 || tel.Holes[0].Cause != "ingest_queue_age" ||
		tel.Holes[0].GapS != 8 || !strings.Contains(tel.Holes[0].Detail, "dropped 7 samples") {
		t.Fatalf("telemetry %+v", tel)
	}
	if r.Alerts.State != StateIncluded || len(r.Alerts.Items) != 2 || r.Alerts.Items[0].ClearedAt == nil || r.Alerts.Items[1].ClearReason == nil {
		t.Fatalf("alerts %+v", r.Alerts)
	}
	if len(r.Conformance.Items) != 3 || r.Conformance.Items[1].ATSAckRef == nil || len(r.Coordination.Items) != 1 {
		t.Fatalf("timeline %+v notices %+v", r.Conformance, r.Coordination)
	}
	if r.TrafficProducts.Total != 2 || len(r.TrafficProducts.Items) != 2 || r.TrafficProducts.Truncated {
		t.Fatalf("products %+v", r.TrafficProducts)
	}
	if len(r.PolicyVersions.Items) != 2 || len(r.PolicyVersions.Missing) != 0 {
		t.Fatalf("policies %+v", r.PolicyVersions)
	}
	if len(r.Intent.Versions) != 2 || r.Intent.Intent == nil || *r.Intent.Intent.OperatorRegPublic != "GEO87astrdge12k8" || !r.GeneratedAt.Equal(m.now) {
		t.Fatalf("intent %+v", r.Intent)
	}
}

// B-13 (E-01 pair with the record above): an alert table that cannot be
// read is "alerts: unavailable" with the reason, never an empty list;
// the other sections are still built.
func TestUnreadableAlertsAreUnavailableNotNone(t *testing.T) {
	m, s := fixture()
	m.errOf["alerts"] = errDown
	r, err := builder(m, s).Flight(context.Background(), flightID)
	if err != nil {
		t.Fatal(err)
	}
	b := validateRecord(t, flightRecordSchema(t), r)
	if r.Alerts.State != StateUnavailable || !strings.Contains(r.Alerts.Reason, "unreadable") || r.Alerts.Items != nil ||
		strings.Contains(string(b), `"alerts":{"state":"included"`) {
		t.Fatalf("alerts %+v", r.Alerts)
	}
	if r.Conformance.State != StateIncluded || r.Telemetry.State != StateIncluded {
		t.Fatal("one unreadable section took the others down")
	}
	// And an empty alert table reads as an included, empty list.
	m.errOf["alerts"], m.alerts = nil, nil
	r, _ = builder(m, s).Flight(context.Background(), flightID)
	if r.Alerts.State != StateIncluded || r.Alerts.Items == nil || len(r.Alerts.Items) != 0 {
		t.Fatalf("no alerts %+v", r.Alerts)
	}
}

// Every other section fails on its own the same way.
func TestEverySectionFailsOnItsOwn(t *testing.T) {
	for what, check := range map[string]func(Record) Section{
		"intent":      func(r Record) Section { return r.Intent.Section },
		"versions":    func(r Record) Section { return r.Intent.Section },
		"conformance": func(r Record) Section { return r.Conformance.Section },
		"notices":     func(r Record) Section { return r.Coordination.Section },
		"policies":    func(r Record) Section { return r.PolicyVersions.Section },
	} {
		m, s := fixture()
		m.errOf[what] = errDown
		r, err := builder(m, s).Flight(context.Background(), flightID)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if sec := check(r); sec.State != StateUnavailable || sec.Reason == "" {
			t.Errorf("%s: %+v", what, sec)
		}
		validateRecord(t, flightRecordSchema(t), r)
	}
	for what, check := range map[string]func(Record) Section{
		"summary":  func(r Record) Section { return r.Telemetry.Section },
		"silences": func(r Record) Section { return r.Telemetry.Section },
		"products": func(r Record) Section { return r.TrafficProducts.Section },
	} {
		m, s := fixture()
		s.errOf[what] = errDown
		r, _ := builder(m, s).Flight(context.Background(), flightID)
		if sec := check(r); sec.State != StateUnavailable {
			t.Errorf("%s: %+v", what, sec)
		}
	}
}

// Without TimescaleDB the telemetry and the products say so.
func TestNoSeries(t *testing.T) {
	m, _ := fixture()
	r, err := builder(m, nil).Flight(context.Background(), flightID)
	if err != nil {
		t.Fatal(err)
	}
	validateRecord(t, flightRecordSchema(t), r)
	if r.Telemetry.State != StateUnavailable || !strings.Contains(r.Telemetry.Reason, "USSP_TS_URL") || r.TrafficProducts.State != StateUnavailable {
		t.Fatalf("%+v %+v", r.Telemetry.Section, r.TrafficProducts.Section)
	}
}

// Holes: a declared gap is a hole however short; a silence nothing
// explains says no recorded cause; a tsdb-writer gap names itself as the
// stream's; unreadable gap records say so on every hole (B-13).
func TestHoleCauses(t *testing.T) {
	m, s := fixture()
	s.silences = append(s.silences,
		Silence{After: t0.Add(14 * time.Minute), Before: t0.Add(14*time.Minute + 5*time.Second)},
		Silence{After: t0.Add(16 * time.Minute), Before: t0.Add(16*time.Minute + 4*time.Second)})
	s.writerGaps = []WriterGap{{Cause: "stream_removed", Count: 12, CountUnit: "messages", AfterAt: ptr(t0.Add(16 * time.Minute)), BeforeAt: ptr(t0.Add(17 * time.Minute)), Detail: "aged out", At: t0}}
	m.gaps = append(m.gaps, IngestGap{Cause: "ingest_queue_corrupt", Started: t0.Add(18 * time.Minute), Ended: t0.Add(18*time.Minute + 500*time.Millisecond), Dropped: 1})
	r, _ := builder(m, s).Flight(context.Background(), flightID)
	h := r.Telemetry.Holes
	if len(h) != 4 || h[1].Cause != CauseNone || h[1].Detail != "" || h[2].Cause != "tsdb_writer_stream_removed" ||
		!strings.Contains(h[2].Detail, "not of this flight alone") || h[3].Cause != "ingest_queue_corrupt" || h[3].GapS != 0.5 {
		t.Fatalf("holes %+v", h)
	}
	m.errOf["gaps"] = errDown
	r, _ = builder(m, s).Flight(context.Background(), flightID)
	if r.Telemetry.Causes.State != StateUnavailable {
		t.Fatal("unreadable gap records not said")
	}
	for _, x := range r.Telemetry.Holes {
		if x.Cause == CauseNone && !strings.Contains(x.Detail, "could not be read") {
			t.Errorf("hole %+v does not say its causes could not be read", x)
		}
	}
}

// E-10: every bounded section past its bound says truncated (and the
// products their total), never thins silently.
func TestBoundsSayTruncated(t *testing.T) {
	m, s := fixture()
	for i := range MaxAlerts + 5 {
		m.alerts = append(m.alerts, Alert{AlertID: "a", Kind: "proximity", Severity: "info", State: "raised", RaisedAt: t0.Add(time.Duration(i) * time.Second),
			UpdatedAt: t0, PolicyVersion: 3, Detail: rawJSON(`{}`)})
	}
	for i := range MaxHoles + 5 {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		s.silences = append(s.silences, Silence{After: at, Before: at.Add(5 * time.Second)})
	}
	for range MaxProducts + 5 {
		s.products = append(s.products, s.products[0])
	}
	for i := range MaxVersions + 1 {
		m.versions = append(m.versions, Version{Version: i + 3, Decision: rawJSON(`{}`)})
	}
	for range MaxConformance + 1 {
		m.states = append(m.states, m.states[0])
	}
	for range MaxNotices + 1 {
		m.notices = append(m.notices, m.notices[0])
	}
	r, _ := builder(m, s).Flight(context.Background(), flightID)
	if !r.Alerts.Truncated || len(r.Alerts.Items) != MaxAlerts || !r.Telemetry.HolesTruncated || len(r.Telemetry.Holes) != MaxHoles ||
		!r.TrafficProducts.Truncated || len(r.TrafficProducts.Items) != MaxProducts || r.TrafficProducts.Total != int64(MaxProducts+7) ||
		!r.Intent.VersionsTruncated || !r.Conformance.Truncated || !r.Coordination.Truncated {
		t.Fatalf("alerts %v/%d holes %v/%d products %v/%d/%d versions %v conformance %v notices %v", r.Alerts.Truncated, len(r.Alerts.Items),
			r.Telemetry.HolesTruncated, len(r.Telemetry.Holes), r.TrafficProducts.Truncated, len(r.TrafficProducts.Items), r.TrafficProducts.Total,
			r.Intent.VersionsTruncated, r.Conformance.Truncated, r.Coordination.Truncated)
	}
}

// A flight the USSP does not hold is ErrNotFound; the flights table or
// the clock failing is the record's error.
func TestFlightErrors(t *testing.T) {
	m, s := fixture()
	if _, err := builder(m, s).Flight(context.Background(), "11111111-1111-4111-8111-111111111111"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown flight: %v", err)
	}
	m.errOf["now"] = errDown
	if _, err := builder(m, s).Flight(context.Background(), flightID); err == nil {
		t.Fatal("no clock accepted")
	}
	m.errFlight = errDown
	if _, err := builder(m, s).Flight(context.Background(), flightID); !errors.Is(err, errDown) {
		t.Fatalf("flights table down: %v", err)
	}
}

// A flight without an intent or a client: no intent, no products, and a
// policy version no table holds is listed missing.
func TestFlightWithoutIntent(t *testing.T) {
	m, s := fixture()
	f := m.flights[flightID]
	f.IntentID, f.ClientID, f.OperatorReg = nil, nil, nil
	m.flights[flightID] = f
	m.policies = nil
	r, err := (&Builder{Reader: m, Series: s}).Flight(context.Background(), flightID)
	if err != nil {
		t.Fatal(err)
	}
	validateRecord(t, flightRecordSchema(t), r)
	if r.Intent.Intent != nil || len(r.TrafficProducts.Items) != 0 || r.Flight.OperatorRegPublic != nil || len(r.PolicyVersions.Missing) == 0 {
		t.Fatalf("%+v %+v %+v", r.Intent, r.TrafficProducts, r.PolicyVersions)
	}
}
