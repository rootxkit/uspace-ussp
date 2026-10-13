package testif

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixturePath = "../../deploy/conformance/test-operator.yaml"

// The conformance profile's fixture loads: GEO-TEST-* and TEST* values,
// its Annex IV items and the identification technology table.
func TestTheProfilesFixtureLoads(t *testing.T) {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Serials) == 0 || f.AnnexIV.EmergencyContactRef == "" || f.IdentificationTechnologies["ASTMNetRID"] != "network" {
		t.Errorf("%+v", f)
	}
}

// A production-looking value, an unknown member, a missing item or a
// technology outside the request's enumeration refuses the fixture (and
// the start).
func TestFixtureRefusesWhatIsNotATestOperator(t *testing.T) {
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	good := string(b)
	for name, edit := range map[string][2]string{
		"production registration": {"GEO-TEST-WP21-0001", "GEO-0001"},
		"production serial":       {"TEST-WP21-0003", "1AF49UL5CC5J6K"},
		"no contact reference":    {"emergency_contact_ref: TEST-EMERGENCY-CONTACT-0001", "emergency_contact_ref: \"\""},
		"technology":              {"ASTMDirectRID: direct", "ASTMDirectRID: radio"},
		"unknown member":          {"format: ussp-test-operator/v1", "format: ussp-test-operator/v1\nsecret: x"},
		"format":                  {"format: ussp-test-operator/v1", "format: ussp-test-operator/v2"},
	} {
		if !strings.Contains(good, edit[0]) {
			t.Fatalf("%s: the fixture has no %q", name, edit[0])
		}
		p := filepath.Join(t.TempDir(), "f.yaml")
		if err := os.WriteFile(p, []byte(strings.Replace(good, edit[0], edit[1], 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFixture(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
