//nolint:misspell // serial.Normalize is the uspace-core API name (Go spelling)
package intent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed269"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
)

// Bounds of a request (E-10, Z-06): a request past one is refused, never
// cut.
const (
	// MaxRequestBytes bounds the body of POST and PATCH.
	MaxRequestBytes = 256 << 10
	// MaxVolumes bounds the volumes of one intent.
	MaxVolumes = 16
	// MaxPolygonVertices bounds one outline of our own intents (the
	// standard allows 10 000; the body cap would refuse most of them).
	MaxPolygonVertices = 1000
	// MaxConnectivityMethods bounds item 7.
	MaxConnectivityMethods = 8
	// MaxLandingSites bounds the contingency's landing sites.
	MaxLandingSites = 16
	// MaxEnduranceS bounds item 8 (two days).
	MaxEnduranceS = 2 * 24 * 3600
	// MaxMTOMKg bounds the declared MTOM.
	MaxMTOMKg = 25000
	// MaxProblems caps the problems of one request (the problem body's
	// cap); the rest are counted.
	MaxProblems = 100
	// Art13MTOMKg is the mass below which a privately built UA is out of
	// the regulation's scope in A1 (Art. 1(3)).
	Art13MTOMKg = 0.25
)

// Annex IV item numbers by request field (spec 04 §3.5).
var items = map[string]int{
	"uas_serial": 1, "mode": 2, "flight_type": 3, "priority": 3,
	"category": 4, "subcategory": 4, "class_label": 4, "type_certificate": 4, "privately_built": 4, "mtom_kg": 4,
	"volumes": 5, "identification_technology": 6, "connectivity_methods": 7, "endurance_s": 8,
	"loss_of_c2_procedure": 9, "operator_reg": 10, "ua_registration": 10,
}

// ItemOf is the Annex IV item a field path belongs to, 0 for a field
// outside the ten items.
func ItemOf(field string) int {
	head := field
	if i := strings.IndexAny(head, ".["); i >= 0 {
		head = head[:i]
	}
	return items[head]
}

// Volume is one volume of a request as the decision uses it.
type Volume struct {
	Index    int
	Wire     f3548.Volume4D
	Shape    deconflict.Shape
	BBox     geodesy.BBox
	Centroid core.LatLon
	AMSL     VolumeAMSL
	Start    time.Time
	End      time.Time
}

// Deconflict is the volume as the deconfliction judges it.
func (v Volume) Deconflict() deconflict.Volume {
	return deconflict.Volume{Shape: v.Shape, LowerAMSLM: v.AMSL.LowerAMSLM, UpperAMSLM: v.AMSL.UpperAMSLM, Start: v.Start, End: v.End}
}

// Normalised is a request that passed Validate: the items checked, the
// volumes with their outlines, AMSL bands and windows, the derived
// priority, the Art. 1(3) exemption and the cells of intent_active.
type Normalised struct {
	Request Request
	Volumes []Volume
	// TimeStart and TimeEnd span every volume.
	TimeStart, TimeEnd time.Time
	Priority           int
	// Exempt is the Art. 1(3) scope exemption as claimed (open A1 with
	// C0, or privately built below 250 g); the decision keeps it only
	// when the registry's answer for the UAS confirms it.
	Exempt bool
	// SpecialUnverified is a flight_type special_operation judged at
	// priority 0: nothing this USSP can check verifies it.
	SpecialUnverified bool
	// OperatorPublic is the public part of the registration number and
	// OperatorKey its comparison key (regnum).
	OperatorPublic, OperatorKey string
	// Serial is the serial as stored (serial.Normalize).
	Serial string
	// Cells are the cell5 cells of intent_active (cell.CellsForEnvelope
	// over every volume's box).
	Cells []string
}

// ValidateEnv is what Validate needs besides the request.
type ValidateEnv struct {
	// Geoid converts the W84 limits to AMSL; nil refuses every request
	// with geoid_unavailable (never approximated).
	Geoid geoid.Undulator
	// Now is the database clock.
	Now time.Time
	// SpecialPriority is the policy's special_operation_priority.
	SpecialPriority int
}

// UnavailableError is a request that cannot be judged because a
// dependency is missing (the geoid, the database, the KV): 503 with the
// slug <dependency>_unavailable, nothing recorded.
type UnavailableError struct {
	Dependency string
	Detail     string
}

func (e *UnavailableError) Error() string { return e.Dependency + " unavailable: " + e.Detail }

// HTTPStatus is 503.
func (e *UnavailableError) HTTPStatus() int { return 503 }

// ProblemSlug is <dependency>_unavailable.
func (e *UnavailableError) ProblemSlug() string { return e.Dependency + "_unavailable" }

// ProblemDetail is safe to send.
func (e *UnavailableError) ProblemDetail() string { return e.Detail }

// problems collects problems up to MaxProblems, prefixing each reason
// with the Annex IV item.
type problems struct{ p ed269.Problems }

func (ps *problems) add(field, format string, a ...any) {
	reason := fmt.Sprintf(format, a...)
	if n := ItemOf(field); n > 0 {
		reason = "annex_iv." + strconv.Itoa(n) + ": " + reason
	}
	if len(ps.p.List) >= MaxProblems {
		ps.p.Truncated++
		return
	}
	ps.p.List = append(ps.p.List, ed269.Problem{Field: field, Reason: reason})
}

func (ps *problems) result() *ed269.Problems {
	if len(ps.p.List) == 0 {
		return nil
	}
	out := ps.p
	return &out
}

// Decode reads exactly one intent/request/v1 object of at most
// MaxRequestBytes, refusing unknown fields (at any depth) and trailing
// data. Every refusal is a *core.FieldError; it never panics.
func Decode(raw []byte) (Request, error) {
	var r Request
	if len(raw) > MaxRequestBytes {
		return r, core.Fieldf("body", "longer than %d bytes", MaxRequestBytes)
	}
	if err := strictDecode(raw, &r); err != nil {
		return Request{}, err
	}
	return r, nil
}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return core.Fieldf(te.Field, "must be %s, not a JSON %s", te.Type.String(), te.Value)
		}
		msg := err.Error()
		if i := strings.Index(msg, "unknown field "); i >= 0 {
			return core.Fieldf("body", "%s", msg[i:])
		}
		return core.Fieldf("body", "not the expected JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return core.Fieldf("body", "trailing data after the JSON object")
	}
	return nil
}

// text checks a required free-text field: not blank, valid UTF-8, no
// control character, within its bound.
func (ps *problems) text(field, v string, maxLen int) {
	switch {
	case strings.TrimSpace(v) == "":
		ps.add(field, "required")
	case !utf8.ValidString(v):
		ps.add(field, "not valid UTF-8")
	case utf8.RuneCountInString(v) > maxLen:
		ps.add(field, "longer than %d characters", maxLen)
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		ps.add(field, "contains a control character")
	}
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func lowerOrDigit(r rune) bool { return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' }

func validClientRef(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case lowerOrDigit(r), r >= 'A' && r <= 'Z', r == '.', r == '_', r == ':', r == '-':
		default:
			return false
		}
	}
	return true
}

func validToken(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case lowerOrDigit(r), r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (p Point) latLon() core.LatLon { return core.LatLon{LatDeg: p.Lat, LonDeg: p.Lng} }

// Validate checks the ten Annex IV items (problems name the item as
// annex_iv.N in the reason and the JSON path in the field), runs the
// volumes through uspace-core's F3548 validation, derives each volume's
// AMSL band through the geoid at its outline's centroid, the priority,
// the Art. 1(3) exemption and the cells. A missing geoid, or one that
// cannot answer at a valid point, is an *UnavailableError: the request
// is not judged, never approximated.
func Validate(r Request, env ValidateEnv) (*Normalised, *ed269.Problems, error) {
	ps := &problems{}
	if !validClientRef(r.ClientRef) {
		ps.add("client_ref", "required: 1 to 64 characters of letters, digits and . _ : -")
	}
	n := &Normalised{Request: r}

	// (1) the serial, checked for the class (core/serial).
	n.Serial = serial.Normalize(r.UASSerial)
	if len(n.Serial) > 64 {
		ps.add("uas_serial", "longer than 64 characters")
	} else if err := serial.ValidateForClass(n.Serial, r.ClassLabel); err != nil {
		var fe *core.FieldError
		field := "uas_serial"
		if errors.As(err, &fe) && fe.Field == serial.FieldClassLabel {
			field = "class_label"
		}
		ps.add(field, "%s", reasonOf(err))
	}
	// (2)
	if !oneOf(r.Mode, ModeVLOS, ModeBVLOS) {
		ps.add("mode", "must be VLOS or BVLOS")
	}
	// (3) and the priority it implies.
	switch r.FlightType {
	case FlightNormal:
		n.Priority = 0
	case FlightSpecial:
		// A special operation's priority would beat every other intent,
		// and nothing this USSP can check verifies one yet (no registry
		// flag, no scope granted by the authority): it is recorded as
		// declared and judged at priority 0 (docs/PLAN.md §15.2 Q19).
		n.Priority = 0
		n.SpecialUnverified = true
	default:
		ps.add("flight_type", "must be normal or special_operation")
	}
	switch {
	case r.Priority == nil:
	case r.FlightType == FlightSpecial && (*r.Priority == 0 || *r.Priority == env.SpecialPriority):
		// The class a special operation would have; it is not applied.
	case *r.Priority != n.Priority:
		ps.add("priority", "must be 0 for flight_type %q (or the special_operation_priority %d for special_operation)", r.FlightType, env.SpecialPriority)
	}
	// (4)
	validateCategory(ps, r)
	n.Exempt = r.Category == CategoryOpen && r.Subcategory == "A1" &&
		(r.ClassLabel == "C0" || r.PrivatelyBuilt && r.MTOMKg != nil && *r.MTOMKg < Art13MTOMKg)
	// (6), (7), (8), (9)
	if !oneOf(r.IdentificationTechnology, IdentNetwork, IdentDirect, IdentBoth) {
		ps.add("identification_technology", "must be network, direct or both")
	}
	validateConnectivity(ps, r.ConnectivityMethods)
	if r.EnduranceS <= 0 || r.EnduranceS > MaxEnduranceS {
		ps.add("endurance_s", "must be from 1 to %d seconds", MaxEnduranceS)
	}
	ps.text("loss_of_c2_procedure", r.LossOfC2Procedure, 256)
	// (10)
	// The number's format is the registry's (F8 answers unknown for one
	// it does not hold); here it is text without white space, compared on
	// its public part as uspace-core's regnum compares it, as the
	// operator account stores it.
	switch op := strings.TrimSpace(r.OperatorReg); {
	case op == "":
		ps.add("operator_reg", "required")
	case len(op) > regnum.MaxLen:
		ps.add("operator_reg", "longer than %d characters", regnum.MaxLen)
	case strings.IndexFunc(op, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0:
		ps.add("operator_reg", "contains white space or a control character")
	default:
		n.OperatorPublic, n.OperatorKey = regnum.Public(op)
	}
	if r.Category == CategoryCertified && strings.TrimSpace(r.UARegistration) == "" {
		ps.add("ua_registration", "required in the certified category")
	} else if r.UARegistration != "" {
		ps.text("ua_registration", r.UARegistration, 64)
	}
	// Beside the ten items.
	if r.PilotRef != "" {
		ps.text("pilot_ref", r.PilotRef, 32)
	}
	for _, p := range []struct {
		field string
		pt    *Point
	}{{"takeoff", r.Takeoff}, {"landing", r.Landing}} {
		if p.pt != nil && !p.pt.latLon().Valid() {
			ps.add(p.field, "is not a valid WGS84 position")
		}
	}
	ps.text("contingency.procedure", r.Contingency.Procedure, 512)
	if len(r.Contingency.LandingSites) > MaxLandingSites {
		ps.add("contingency.landing_sites", "has %d sites; at most %d", len(r.Contingency.LandingSites), MaxLandingSites)
	} else {
		for i, s := range r.Contingency.LandingSites {
			if !s.latLon().Valid() {
				ps.add(fmt.Sprintf("contingency.landing_sites[%d]", i), "is not a valid WGS84 position")
			}
		}
	}
	ps.text("emergency_contact_ref", r.EmergencyContactRef, 128)
	if r.AuthorisationRef != "" {
		ps.text("authorisation_ref", r.AuthorisationRef, 128)
	}
	// (5) the volumes, last: they need the geoid.
	if err := validateVolumes(ps, n, env); err != nil {
		return nil, nil, err
	}
	if n.TimeEnd.After(n.TimeStart) && r.EnduranceS > 0 && float64(r.EnduranceS) < n.TimeEnd.Sub(n.TimeStart).Seconds() {
		ps.add("endurance_s", "%d s does not cover the window of %.0f s", r.EnduranceS, n.TimeEnd.Sub(n.TimeStart).Seconds())
	}
	if p := ps.result(); p != nil {
		return nil, p, nil
	}
	return n, nil, nil
}

func reasonOf(err error) string {
	var fe *core.FieldError
	if errors.As(err, &fe) {
		return fe.Reason
	}
	return err.Error()
}

func validateCategory(ps *problems, r Request) {
	if r.ClassLabel != "" && !oneOf(r.ClassLabel, "C0", "C1", "C2", "C3", "C4", "C5", "C6") {
		ps.add("class_label", "must be C0 to C6")
	}
	if r.MTOMKg != nil && (!core.IsFinite(*r.MTOMKg) || *r.MTOMKg <= 0 || *r.MTOMKg > MaxMTOMKg) {
		ps.add("mtom_kg", "must be a positive mass of at most %d kg", MaxMTOMKg)
	}
	if r.PrivatelyBuilt && r.MTOMKg == nil {
		ps.add("mtom_kg", "required for a privately built UA")
	}
	if r.TypeCertificate != "" {
		ps.text("type_certificate", r.TypeCertificate, 64)
	}
	switch r.Category {
	case CategoryOpen:
		if !oneOf(r.Subcategory, "A1", "A2", "A3") {
			ps.add("subcategory", "must be A1, A2 or A3 in the open category")
		}
		if r.ClassLabel == "" && !r.PrivatelyBuilt {
			ps.add("class_label", "required in the open category unless the UA is privately built")
		}
	case CategorySpecific:
		if r.Subcategory != "" {
			ps.add("subcategory", "only the open category has subcategories")
		}
	case CategoryCertified:
		if r.Subcategory != "" {
			ps.add("subcategory", "only the open category has subcategories")
		}
		if strings.TrimSpace(r.TypeCertificate) == "" {
			ps.add("type_certificate", "required in the certified category")
		}
	default:
		ps.add("category", "must be open, specific or certified")
	}
}

func validateConnectivity(ps *problems, ms []string) {
	if len(ms) == 0 || len(ms) > MaxConnectivityMethods {
		ps.add("connectivity_methods", "list 1 to %d methods", MaxConnectivityMethods)
		return
	}
	seen := map[string]bool{}
	for i, m := range ms {
		if !validToken(m) {
			ps.add(fmt.Sprintf("connectivity_methods[%d]", i), "1 to 32 characters of a-z, 0-9, _ and -")
			continue
		}
		if seen[m] {
			ps.add(fmt.Sprintf("connectivity_methods[%d]", i), "%q is listed twice", m)
		}
		seen[m] = true
	}
}

// wireRadius is a radius as the F3548 file gives it (a float32 read as
// its shortest decimal, as uspace-core reads it).
func wireRadius(f float32) float64 {
	v, err := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

// coreCheck runs uspace-core's F3548 validation over the volumes, as
// they are the details of an operational intent
// (f3548.UnmarshalOperationalIntent, brief WP-7), and returns its first
// problem with the path relative to the request.
func coreCheck(vs []f3548.Volume4D, at time.Time) error {
	raw, err := json.Marshal(vs)
	if err != nil {
		return core.Fieldf("volumes", "do not encode: %v", err)
	}
	ts := at.UTC().Format(time.RFC3339)
	doc := `{"reference":{"id":"x","manager":"x","uss_base_url":"x","subscription_id":"x","state":"Accepted",` +
		`"uss_availability":"Unknown","version":1,"ovn":"x","time_start":{"value":"` + ts + `","format":"RFC3339"},` +
		`"time_end":{"value":"` + ts + `","format":"RFC3339"}},"details":{"volumes":` + string(raw) + `}}`
	if _, err := f3548.UnmarshalOperationalIntent([]byte(doc)); err != nil {
		var fe *core.FieldError
		if errors.As(err, &fe) {
			return core.Fieldf(strings.TrimPrefix(fe.Field, "operational_intent.details."), "%s", fe.Reason)
		}
		return core.Fieldf("volumes", "%v", err)
	}
	return nil
}

func validateVolumes(ps *problems, n *Normalised, env ValidateEnv) error {
	vs := n.Request.Volumes
	if len(vs) == 0 || len(vs) > MaxVolumes {
		ps.add("volumes", "list 1 to %d volumes", MaxVolumes)
		return nil
	}
	for i, v := range vs {
		if v.Volume.OutlinePolygon != nil && len(v.Volume.OutlinePolygon.Vertices) > MaxPolygonVertices {
			ps.add(fmt.Sprintf("volumes[%d].volume.outline_polygon.vertices", i), "has %d vertices; at most %d", len(v.Volume.OutlinePolygon.Vertices), MaxPolygonVertices)
			return nil
		}
	}
	if err := coreCheck(vs, env.Now); err != nil {
		var fe *core.FieldError
		if errors.As(err, &fe) {
			ps.add(fe.Field, "%s", fe.Reason)
		}
		return nil
	}
	horizon := env.Now.Add(time.Duration(f3548.OiMaxPlanHorizonDays) * 24 * time.Hour)
	cellSet := map[string]bool{}
	for i, v := range vs {
		field := fmt.Sprintf("volumes[%d]", i)
		ok := true
		for _, need := range []struct {
			name    string
			present bool
		}{
			{"time_start", v.TimeStart != nil}, {"time_end", v.TimeEnd != nil},
			{"volume.altitude_lower", v.Volume.AltitudeLower != nil}, {"volume.altitude_upper", v.Volume.AltitudeUpper != nil},
		} {
			if !need.present {
				ps.add(field+"."+need.name, "required for every volume")
				ok = false
			}
		}
		if !ok {
			continue
		}
		start, end := v.TimeStart.Value.UTC(), v.TimeEnd.Value.UTC()
		switch {
		case !end.After(start):
			ps.add(field+".time_end", "must be after time_start")
			continue
		case !end.After(env.Now):
			ps.add(field+".time_end", "is in the past")
			continue
		case end.After(horizon):
			ps.add(field+".time_end", "is beyond the planning horizon of %d days (F3548 OiMaxPlanHorizonDays)", f3548.OiMaxPlanHorizonDays)
			continue
		}
		vol := Volume{Index: i, Wire: v, Start: start, End: end}
		if c := v.Volume.OutlineCircle; c != nil {
			vol.Shape = deconflict.Shape{Circle: &geodesy.Circle{Center: c.Center.LatLon(), RadiusM: wireRadius(c.Radius.Value)}}
			vol.Centroid = c.Center.LatLon()
		} else {
			pts := make([]core.LatLon, len(v.Volume.OutlinePolygon.Vertices))
			for k, p := range v.Volume.OutlinePolygon.Vertices {
				pts[k] = p.LatLon()
			}
			vol.Shape = deconflict.Shape{Polygon: pts}
			vol.Centroid = centroid(pts)
		}
		vol.BBox = vol.Shape.BBox()
		lo, _ := v.Volume.AltitudeLower.HAEM() // checked by coreCheck
		hi, _ := v.Volume.AltitudeUpper.HAEM()
		if env.Geoid == nil {
			return &UnavailableError{Dependency: "geoid", Detail: "no geoid grid is configured (USSP_GEOID_FILE): the AMSL band of a volume is never approximated"}
		}
		und, err := env.Geoid.UndulationM(vol.Centroid)
		if err != nil || !core.IsFinite(und) {
			return &UnavailableError{Dependency: "geoid", Detail: "the geoid did not answer at the centroid of volumes[" + strconv.Itoa(i) + "]"}
		}
		vol.AMSL = VolumeAMSL{
			LowerAMSLM: geoid.AMSLFromHAE(lo, und), UpperAMSLM: geoid.AMSLFromHAE(hi, und),
			UndulationM: und, LowerW84M: lo, UpperW84M: hi,
		}
		cs, err := cell.CellsForEnvelope(vol.BBox)
		if err != nil {
			ps.add(field+".volume", "the outline covers too large an area: %s", reasonOf(err))
			continue
		}
		for _, c := range cs {
			cellSet[c] = true
		}
		if len(cellSet) > cell.MaxCells {
			ps.add("volumes", "the volumes cover more than %d cells", cell.MaxCells)
			return nil
		}
		if n.TimeStart.IsZero() || start.Before(n.TimeStart) {
			n.TimeStart = start
		}
		if end.After(n.TimeEnd) {
			n.TimeEnd = end
		}
		n.Volumes = append(n.Volumes, vol)
	}
	n.Cells = sortedKeys(cellSet)
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// centroid is the mean of the vertices with longitudes unwrapped about
// the first (the AMSL reference point of a polygon outline).
func centroid(pts []core.LatLon) core.LatLon {
	ref := pts[0].LonDeg
	var lat, lon float64
	for _, p := range pts {
		lat += p.LatDeg
		lon += core.WrapLonDeg(p.LonDeg - ref)
	}
	k := float64(len(pts))
	return core.LatLon{LatDeg: lat / k, LonDeg: core.WrapLonDeg(ref + lon/k)}
}
