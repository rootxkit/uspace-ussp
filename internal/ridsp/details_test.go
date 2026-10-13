package ridsp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Every Art. 8(2) item mapped as spec 04 §3.1 says: (a) operator_id, the
// public part of the registration number only; (b) uas_id.serial_number;
// (e) operator_location with its WGS84 altitude; the authorisation
// number as operation_description, the Annex IV category and class, the
// UA registration and our flight id as utm_id. A flight without an
// intent has undefined classification and no description (E-01 pair).
func TestDetailsMapArticle8(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	withIntent := sample(1, origin, t0)
	intentID := "8c1f3f2e-7d0e-4a8b-9a51-0e4b7d6f2c11"
	withIntent.Body.IntentID = &intentID
	withIntent.Body.OperatorPosition = nil
	earlier := sample(1, origin, t0.Add(-time.Second))
	earlier.Body.OperatorPosition = &telemetry.OperatorPosition{Lat: 41.71, Lng: 44.82, AltWGS84M: fptr(470)}
	w.Add(earlier)
	w.Add(withIntent)
	w.Add(sample(2, origin, t0))
	cls, reg := "C2", "GEO-TEST-UA-0001"
	auth := "DEV01-GEOTESTOP0001-01"
	s := &Server{Window: w, Now: c.now, Counters: &core.Counters{}, Intents: fakeIntents{intentID: {
		IntentID: intentID, AuthorisationNumber: &auth, OperatorReg: "GEOTESTOP0001-xyz", UASSerial: "TEST0001",
		Category: "specific", ClassLabel: &cls, UARegistration: &reg,
	}}}
	base := serve(t, s)

	code, b := get(t, base+"/uss/flights/"+flightN(1)+"/details")
	var r f3411.GetFlightDetailsResponse
	if code != 200 || json.Unmarshal(b, &r) != nil {
		t.Fatalf("%d %s", code, b)
	}
	d := r.Details
	switch {
	case d.Id != flightN(1):
		t.Errorf("id %q", d.Id)
	case d.UasId == nil || *d.UasId.SerialNumber != "TEST0001" || *d.UasId.RegistrationId != reg || *d.UasId.UtmId != flightN(1) || d.UasId.SpecificSessionId != nil:
		t.Errorf("uas_id %s", b)
	case d.OperatorId == nil || *d.OperatorId != "GEOTESTOP0001":
		t.Errorf("operator_id %v: the public part only", d.OperatorId)
	case d.OperationDescription == nil || *d.OperationDescription != auth:
		t.Errorf("operation_description %v", d.OperationDescription)
	case *d.EuClassification.Category != f3411.Specific || *d.EuClassification.Class != f3411.Class2:
		t.Errorf("eu_classification %s", b)
	case d.OperatorLocation == nil || d.OperatorLocation.Position.Lat != 41.71 || d.OperatorLocation.Altitude == nil ||
		d.OperatorLocation.Altitude.Value != 470 || d.OperatorLocation.Altitude.Reference != f3411.W84:
		t.Errorf("operator_location from the newest sample that has one: %s", b)
	}
	if strings.Contains(string(b), "xyz") {
		t.Errorf("the secret part of the registration number is served: %s", b)
	}

	code, b = get(t, base+"/uss/flights/"+flightN(2)+"/details")
	r = f3411.GetFlightDetailsResponse{}
	if code != 200 || json.Unmarshal(b, &r) != nil {
		t.Fatalf("%d %s", code, b)
	}
	d = r.Details
	if *d.EuClassification.Category != f3411.EUCategoryUndefined || *d.EuClassification.Class != f3411.EUClassUndefined ||
		d.OperationDescription != nil || d.OperatorLocation != nil || d.UasId.RegistrationId != nil || *d.OperatorId != "GEO-TEST-0001" {
		t.Errorf("without an intent: %s", b)
	}

	code, b = get(t, base+"/uss/flights/"+flightN(3)+"/details")
	var e f3411.ErrorResponse
	if code != http.StatusNotFound || json.Unmarshal(b, &e) != nil || e.Message == nil || s.Counters.Get(CounterDetailsNotFound) != 1 {
		t.Fatalf("unknown flight: %d %s", code, b)
	}
	c.add(Horizon + time.Second)
	if code, _ := get(t, base+"/uss/flights/"+flightN(1)+"/details"); code != http.StatusNotFound {
		t.Fatalf("a flight silent for more than 60 s: %d", code)
	}
	if s.Counters.Get(CounterDetailsRequests) != 4 {
		t.Errorf("%v", s.Counters.Snapshot())
	}
}

// Every class label maps to its F3411 class, and an unknown one to
// EUClassUndefined; every category likewise.
func TestClassificationTable(t *testing.T) {
	for label, want := range map[string]f3411.UAClassificationEUClass{
		"C0": f3411.Class0, "C1": f3411.Class1, "C2": f3411.Class2, "C3": f3411.Class3, "C4": f3411.Class4,
		"C5": f3411.Class5, "C6": f3411.Class6, "C7": f3411.EUClassUndefined,
	} {
		l := label
		if got := ClassificationOf(&IntentFacts{Category: "open", ClassLabel: &l}); *got.Class != want || *got.Category != f3411.Open {
			t.Errorf("%s: %v %v", label, *got.Class, *got.Category)
		}
	}
	if got := ClassificationOf(&IntentFacts{Category: "certified"}); *got.Category != f3411.Certified || *got.Class != f3411.EUClassUndefined {
		t.Errorf("%v %v", *got.Category, *got.Class)
	}
	if got := ClassificationOf(&IntentFacts{Category: "other"}); *got.Category != f3411.EUCategoryUndefined {
		t.Errorf("%v", *got.Category)
	}
}
