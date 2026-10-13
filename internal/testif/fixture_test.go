package testif

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const fixturePath = "../../deploy/conformance/test-operator.yaml"

// The conformance profile's fixture loads: a GEOTEST* registration the
// authority's registry can hold (the one uspace-lab registers,
// conformance/fixtures/test-operators.yaml), TEST* serials, its Annex IV
// items and the identification technology table.
func TestTheProfilesFixtureLoads(t *testing.T) {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if f.Operator.RegistrationNumber != "GEOTESTWP210001" || len(f.Serials) == 0 || f.AnnexIV.EmergencyContactRef == "" || f.IdentificationTechnologies["ASTMNetRID"] != "network" {
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
		"production registration": {"GEOTESTWP210001", "GEOUSSPWP210001"},
		// The number the first WP-21 run used: a GEO-TEST-* number has
		// hyphens, which the authority's registry refuses, so the lab
		// could never register it and the operator stayed
		// pending_validation.
		"hyphenated registration": {"GEOTESTWP210001", "GEO-TEST-WP21-0001"},
		"registration too short":  {"GEOTESTWP210001", "GEOTEST1"},
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

// annex_iv.open_subcategory is one of A1, A2 and A3 or absent (an open
// plan is then refused, TestPlanOpenSubcategoryIsTheFixtures).
func TestFixtureOpenSubcategory(t *testing.T) {
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	// The profile's file without the member, whether it names one or not.
	bare := regexp.MustCompile(`(?m)^\s*open_subcategory:.*\n`).ReplaceAllString(string(b), "")
	if f, err := LoadFixture(writeFile(t, "bare.yaml", bare)); err != nil || f.AnnexIV.OpenSubcategory != "" {
		t.Fatalf("a fixture without open_subcategory: %+v %v", f.AnnexIV, err)
	}
	for v, ok := range map[string]bool{"A3": true, "A1": true, "A4": false, "a3": false, "STS-01": false} {
		s := strings.Replace(bare, "\nannex_iv:\n", "\nannex_iv:\n  open_subcategory: "+v+"\n", 1)
		p := filepath.Join(t.TempDir(), "f.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := LoadFixture(p)
		if (err == nil) != ok || ok && f.AnnexIV.OpenSubcategory != v {
			t.Errorf("open_subcategory %s: %v", v, err)
		}
	}
}

// annex_iv.open_subcategory_by_class names class labels of
// intent/request/v1 only, each onto A1, A2 or A3: the profile's table is
// read as declared, and a table naming another class or subcategory is
// refused at load, not at the first plan.
func TestFixtureOpenSubcategoryByClass(t *testing.T) {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.AnnexIV.OpenSubcategoryByClass; len(got) != 3 || got["C0"] != "A1" || got["C1"] != "A1" || got["C2"] != "A2" {
		t.Errorf("the profile's table: %v", got)
	}
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	for table, ok := range map[string]bool{"{C0: A1}": true, "{C7: A1}": false, "{C0: A4}": false, "{Other: A3}": false} {
		s := regexp.MustCompile(`(?m)^(\s*open_subcategory_by_class:).*$`).ReplaceAllString(string(b), "${1} "+table)
		if _, err := LoadFixture(writeFile(t, "f.yaml", s)); (err == nil) != ok {
			t.Errorf("open_subcategory_by_class %s: %v", table, err)
		}
	}
}
