package testif

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

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
	fixtureRegRe    = regexp.MustCompile(`^GEO-TEST-[A-Z0-9-]{1,40}$`)
	fixtureSerialRe = regexp.MustCompile(`^TEST[A-Z0-9-]{1,60}$`)
	idTechs         = []string{"network", "direct", "both"}
)

// LoadFixture reads and checks the fixture: GEO-TEST-* and TEST* values
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
		bad("operator.registration_number %q is not a GEO-TEST-* number", f.Operator.RegistrationNumber)
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
