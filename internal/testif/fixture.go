package testif

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/rootxkit/uspace-core/regnum"
	yaml "go.yaml.in/yaml/v3"
)

// FixtureFormat is the format line of deploy/conformance/test-operator.yaml.
const FixtureFormat = "ussp-test-operator/v1"

// maxFixtureBytes bounds the fixture file.
const maxFixtureBytes = 256 << 10

// Fixture is the test operator of the conformance profile
// (deploy/conformance/test-operator.yaml): its registration and client,
// which scripts/conformance.sh seeds through the national API, the serial
// pool, and the Annex IV items a flight plan of the InterUSS interface
// does not carry. Every value is the lab's reading of Annex IV for a test
// operator, pending GCAA; nothing here is a default in code.
type Fixture struct {
	Format   string `yaml:"format"`
	Operator struct {
		RegistrationNumber string `yaml:"registration_number"`
		DisplayName        string `yaml:"display_name"`
		ContactEmail       string `yaml:"contact_email"`
		AdminUsername      string `yaml:"admin_username"`
	} `yaml:"operator"`
	Client struct {
		Scopes []string `yaml:"scopes"`
	} `yaml:"client"`
	// Serials is the pool an injected flight without a serial flies under.
	Serials []string `yaml:"serials"`
	// AnnexIV holds the items of intent/request/v1 that the flight_planning
	// FlightPlan has no member for.
	AnnexIV struct {
		Contingency struct {
			Procedure    string    `yaml:"procedure"`
			LandingSites []LatLngY `yaml:"landing_sites"`
		} `yaml:"contingency"`
		// OpenSubcategoryByClass is the open-category subcategory
		// (Annex IV item 4) of a plan by its uas_class label, which
		// FlightAuthorisationData carries while it has no member for the
		// subcategory (pending GCAA; the spec's defaults: C0 and C1 A1,
		// C2 A2).
		OpenSubcategoryByClass map[string]string `yaml:"open_subcategory_by_class"`
		// OpenSubcategory is the subcategory (A1, A2 or A3) of an open
		// plan whose class the table does not name; empty, such a plan is
		// refused (pending GCAA: A3 admits every open class, UAS.OPEN.040,
		// and no Art. 1(3) exemption).
		OpenSubcategory     string   `yaml:"open_subcategory"`
		EmergencyContactRef string   `yaml:"emergency_contact_ref"`
		PilotRef            string   `yaml:"pilot_ref"`
		Takeoff             *LatLngY `yaml:"takeoff"`
		Landing             *LatLngY `yaml:"landing"`
	} `yaml:"annex_iv"`
	// IdentificationTechnologies maps a FlightAuthorisationData
	// identification_technologies entry onto intent/request/v1's
	// identification_technology (network, direct or both).
	IdentificationTechnologies map[string]string `yaml:"identification_technologies"`
}

// LatLngY is a point of the fixture.
type LatLngY struct {
	Lat float64 `yaml:"lat"`
	Lng float64 `yaml:"lng"`
}

var (
	// A test registration number the authority's registry can hold: GEOTEST
	// then letters and digits, and valid under uspace-core regnum's
	// default pattern (pending GCAA, spec 08 Q5), which refuses a hyphen
	// (only the public part is registered, spec 06 §5). The lab registers
	// it (uspace-lab conformance/fixtures/test-operators.yaml).
	fixtureRegRe    = regexp.MustCompile(`^GEOTEST[A-Z0-9]{1,13}$`)
	fixtureSerialRe = regexp.MustCompile(`^TEST[A-Z0-9-]{1,60}$`)
	idTechs         = []string{"network", "direct", "both"}
	// openSubcategories are intent/request/v1's subcategory enumeration
	// (TestAnnexIVMappingIsTheSchemas).
	openSubcategories = []string{"A1", "A2", "A3"}
	// classLabels are intent/request/v1's class_label enumeration.
	classLabels = []string{"C0", "C1", "C2", "C3", "C4", "C5", "C6"}
)

// LoadFixture reads and checks the fixture: GEOTEST* and TEST* values
// only (no production value in a fixture), a non-empty serial pool, the
// contingency procedure and emergency contact reference, and an
// identification technology table onto the request's enumeration.
func LoadFixture(path string) (Fixture, error) {
	var f Fixture
	file, err := os.Open(path) //nolint:gosec // G304: the path is configuration (USSP_TEST_INTERFACES_OPERATOR_FILE)
	if err != nil {
		return f, fmt.Errorf("%s cannot be read", path)
	}
	defer func() { _ = file.Close() }()
	b, err := io.ReadAll(io.LimitReader(file, maxFixtureBytes+1))
	if err != nil || len(b) > maxFixtureBytes {
		return f, fmt.Errorf("%s cannot be read or is larger than %d bytes", path, maxFixtureBytes)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, f.validate()
}

func (f Fixture) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if f.Format != FixtureFormat {
		bad("format is %q, want %s", f.Format, FixtureFormat)
	}
	if !fixtureRegRe.MatchString(f.Operator.RegistrationNumber) {
		bad("operator.registration_number %q is not a GEOTEST* number", f.Operator.RegistrationNumber)
	} else if v, err := regnum.NewValidator(""); err != nil || v.Validate(f.Operator.RegistrationNumber) != nil {
		bad("operator.registration_number %q is not a number the authority's registry admits (uspace-core regnum)", f.Operator.RegistrationNumber)
	}
	if len(f.Serials) == 0 || len(f.Serials) > 256 {
		bad("serials: 1 to 256 TEST* serials, got %d", len(f.Serials))
	}
	for _, s := range f.Serials {
		if !fixtureSerialRe.MatchString(s) {
			bad("serial %q is not a TEST* serial", s)
		}
	}
	if f.AnnexIV.Contingency.Procedure == "" || f.AnnexIV.EmergencyContactRef == "" {
		bad("annex_iv.contingency.procedure and annex_iv.emergency_contact_ref are required")
	}
	if f.AnnexIV.OpenSubcategory != "" && !slices.Contains(openSubcategories, f.AnnexIV.OpenSubcategory) {
		bad("annex_iv.open_subcategory is %q, not one of %v", f.AnnexIV.OpenSubcategory, openSubcategories)
	}
	for k, v := range f.AnnexIV.OpenSubcategoryByClass {
		if !slices.Contains(classLabels, k) {
			bad("annex_iv.open_subcategory_by_class names %q, not one of %v", k, classLabels)
		}
		if !slices.Contains(openSubcategories, v) {
			bad("annex_iv.open_subcategory_by_class[%s] is %q, not one of %v", k, v, openSubcategories)
		}
	}
	if len(f.IdentificationTechnologies) == 0 {
		bad("identification_technologies maps no entry")
	}
	for k, v := range f.IdentificationTechnologies {
		if !slices.Contains(idTechs, v) {
			bad("identification_technologies[%s] is %q, not one of %v", k, v, idTechs)
		}
	}
	return errors.Join(errs...)
}
