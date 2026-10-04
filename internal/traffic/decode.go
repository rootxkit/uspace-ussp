package traffic

import (
	"encoding/json"
	"regexp"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/rid"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// SchemaManned is track/manned/v1 (the ANSP's, pinned under
// schemas/track/manned/v1).
const SchemaManned = "track/manned/v1"

// MaxIDBytes bounds a track id, an instance and a callsign read here.
const MaxIDBytes = bus.MaxTokenBytes

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	icao24Re = regexp.MustCompile(`^[0-9a-f]{6}$`)
	// cell5Re is the partition cell of track/telemetry/v1 (no leading zeros).
	cell5Re = regexp.MustCompile(`^c5:(0|[1-9][0-9]{0,2}|1[0-7][0-9]{2}):(0|[1-9][0-9]{0,2}|[12][0-9]{3}|3[0-5][0-9]{2})$`)
)

var bases = map[core.IdentBasis]bool{core.BasisAuthenticated: true, core.BasisAsBroadcast: true, core.BasisProvider: true}

var reasons = map[core.IdentReason]bool{
	"matched": true, "session_binding": true, "uas_suspended": true, "uas_revoked": true, "operator_suspended": true,
	"operator_revoked": true, "serial_unknown": true, "not_a_serial": true, "operator_absent": true, "operator_mismatch": true,
	"owner_unknown": true, "not_in_registry": true, "serial_conflict": true, "no_serial": true, "registry_unavailable": true,
}

var heightRefs = map[string]bool{"TakeoffLocation": true, "GroundLevel": true}

var trusts = map[core.Trust]bool{
	core.TrustAuthenticated: true, core.TrustProvider: true, core.TrustSurveillance: true,
	core.TrustBroadcast: true, core.TrustSensor: true, core.TrustSimulated: true,
}

var sourcesKnown = map[string]bool{
	telemetry.SourceOperatorWS: true, "network_rid": true, "direct_rid": true, SourceANSPFeed: true, SourceAdsbRx: true, "sitl": true,
}

var altSources = map[core.AltSource]bool{core.AltGeodetic: true, core.AltPressure: true, core.AltNetwork: true, core.AltNone: true}

var statuses = map[string]bool{"Undeclared": true, "Ground": true, "Airborne": true, "Emergency": true, "RemoteIDSystemFailure": true}

var identStatuses = map[core.IdentStatus]bool{
	core.IdentRegistered: true, core.IdentSuspended: true, core.IdentUnknownOperator: true, core.IdentUnidentified: true,
}

// trackRequired and mannedRequired are the members the schemas require
// of a body (a member may be null, never absent).
var (
	trackRequired = []string{
		"track_id", "trust", "source", "source_instance", "position", "alt_wgs84_m", "alt_amsl_m", "alt_source",
		"alt_pressure_m", "height_m", "height_ref", "speed_ms", "track_deg", "vspeed_ms", "accuracy_h_m", "accuracy_v_m",
		"status", "emergency", "identification", "flight_id", "intent_id",
	}
	mannedRequired = []string{
		"icao24", "position", "alt_pressure_m", "alt_wgs84_m", "gs_ms", "track_deg", "vrate_ms", "source_class",
		"trust", "source", "source_instance", "state",
	}
)

// requireMembers refuses a body that lacks one of names.
func requireMembers(data []byte, names []string) error {
	var probe struct {
		Body map[string]json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Body == nil {
		return core.Fieldf("body", "not an object")
	}
	for _, n := range names {
		if _, ok := probe.Body[n]; !ok {
			return core.Fieldf("body."+n, "missing")
		}
	}
	return nil
}

func finiteOpt(field string, v *float64) error {
	if v != nil && !core.IsFinite(*v) {
		return core.Fieldf(field, "not a finite number")
	}
	return nil
}

func nonNegOpt(field string, v *float64) error {
	if v != nil && (!core.IsFinite(*v) || *v < 0) {
		return core.Fieldf(field, "not a finite number of at least 0")
	}
	return nil
}

func trackDegOpt(field string, v *float64) error {
	if v != nil && (!core.IsFinite(*v) || *v < 0 || *v >= 360) {
		return core.Fieldf(field, "not in [0, 360)")
	}
	return nil
}

func idOK(field, s string) error {
	switch {
	case s == "":
		return core.Fieldf(field, "empty")
	case len(s) > MaxIDBytes:
		return core.Fieldf(field, "longer than %d bytes", MaxIDBytes)
	case !utf8.ValidString(s):
		return core.Fieldf(field, "not valid UTF-8")
	}
	return nil
}

// DecodeTrack reads one track/telemetry/v1 message (trk.v1 or peer.v1)
// for the picture and the CPA path: the envelope, the schema, a known
// trust, source, altitude source, status and identification status, a
// WGS84 position, finite numbers within their ranges, bounded ids and a
// flight and intent id when present (one of this USSP's flights is one
// with UUIDs, TrackInputOf). Anything else is an error naming
// the field; it never panics.
func DecodeTrack(data []byte) (*telemetry.Track, error) {
	if len(data) > bus.TrackMsgBytes {
		return nil, core.Fieldf("message", "longer than %d bytes", bus.TrackMsgBytes)
	}
	var t telemetry.Track
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, core.Fieldf("message", "not a track/telemetry/v1 message")
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if err := requireMembers(data, trackRequired); err != nil {
		return nil, err
	}
	var extra struct {
		Body struct {
			Cell *string `json:"cell"`
		} `json:"body"`
	}
	if json.Unmarshal(data, &extra) != nil || (extra.Body.Cell != nil && !cell5Re.MatchString(*extra.Body.Cell)) {
		return nil, core.Fieldf("body.cell", "not a c5 cell")
	}
	b := &t.Body
	checks := []error{
		idOK("body.track_id", b.TrackID), idOK("body.source_instance", b.SourceInstance),
		finiteOpt("body.alt_amsl_m", b.AltAMSLM), nonNegOpt("body.speed_ms", b.SpeedMS),
		trackDegOpt("body.track_deg", b.TrackDeg), finiteOpt("body.vspeed_ms", b.VSpeedMS),
	}
	for _, err := range checks {
		if err != nil {
			return nil, err
		}
	}
	switch {
	case !trusts[b.Trust]:
		return nil, core.Fieldf("body.trust", "unknown trust class")
	case !sourcesKnown[b.Source]:
		return nil, core.Fieldf("body.source", "unknown source")
	case !altSources[b.AltSource]:
		return nil, core.Fieldf("body.alt_source", "unknown altitude source")
	case b.Status != nil && !statuses[*b.Status]:
		return nil, core.Fieldf("body.status", "unknown operational status")
	case !identStatuses[b.Identification.Status]:
		return nil, core.Fieldf("body.identification.status", "unknown identification status")
	case !reasons[b.Identification.Reason]:
		return nil, core.Fieldf("body.identification.reason", "unknown identification reason")
	case !bases[b.Identification.Basis]:
		return nil, core.Fieldf("body.identification.basis", "unknown identification basis")
	case b.HeightRef != nil && !heightRefs[*b.HeightRef]:
		return nil, core.Fieldf("body.height_ref", "unknown height reference")
	case b.HeightM != nil && (b.HeightRef == nil || !core.IsFinite(*b.HeightM)):
		return nil, core.Fieldf("body.height_m", "a height needs its reference and a finite value")
	case !b.Position.LatLon().Valid():
		return nil, core.Fieldf("body.position", "not a WGS84 position")
	case b.Identification.Reason == core.ReasonSerialConflict && !b.Identification.Mismatch:
		return nil, core.Fieldf("body.identification.mismatch", "serial_conflict is a mismatch (04 §3.2)")
	case b.FlightID != nil && idOK("body.flight_id", *b.FlightID) != nil:
		return nil, core.Fieldf("body.flight_id", "empty or longer than %d bytes", MaxIDBytes)
	case b.IntentID != nil && idOK("body.intent_id", *b.IntentID) != nil:
		return nil, core.Fieldf("body.intent_id", "empty or longer than %d bytes", MaxIDBytes)
	}
	return &t, nil
}

// MannedPosition is a manned track's WGS84 position.
type MannedPosition struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// MannedBody is the body of track/manned/v1 (uspace-ansp, pinned): the
// members this system reads.
type MannedBody struct {
	ICAO24         string         `json:"icao24"`
	Callsign       *string        `json:"callsign"`
	Position       MannedPosition `json:"position"`
	AltPressureM   *float64       `json:"alt_pressure_m"`
	AltWGS84M      *float64       `json:"alt_wgs84_m"`
	GSMS           *float64       `json:"gs_ms"`
	TrackDeg       *float64       `json:"track_deg"`
	VRateMS        *float64       `json:"vrate_ms"`
	Emergency      *bool          `json:"emergency"`
	SourceClass    string         `json:"source_class"`
	Trust          core.Trust     `json:"trust"`
	Source         string         `json:"source"`
	SourceInstance string         `json:"source_instance"`
	State          string         `json:"state"`
}

// MannedTrack is one track/manned/v1 message.
type MannedTrack struct {
	bus.Envelope
	Body MannedBody `json:"body"`
}

var sourceClasses = map[string]bool{"ads_b": true, "mode_s": true, "ssr": true, "atm_feed": true, "ads_l": true}

var mannedStates = map[string]bool{StateLive: true, StateStale: true, StateSourceDisabled: true}

// DecodeManned reads one track/manned/v1 message (man.v1). The ANSP's
// schema names trust surveillance and source ansp_feed; the
// e-conspicuity receiver of WP-14 publishes the same shape with trust
// broadcast and source adsb_rx (R-05: always shown as broadcast), which
// is read too. Anything else is an error naming the field; it never
// panics.
func DecodeManned(data []byte) (*MannedTrack, error) {
	if len(data) > bus.TrackMsgBytes {
		return nil, core.Fieldf("message", "longer than %d bytes", bus.TrackMsgBytes)
	}
	var m MannedTrack
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, core.Fieldf("message", "not a track/manned/v1 message")
	}
	if m.Schema != SchemaManned {
		return nil, core.Fieldf("schema", "%q where %q is expected", m.Schema, SchemaManned)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := requireMembers(data, mannedRequired); err != nil {
		return nil, err
	}
	b := &m.Body
	pair := (b.Trust == core.TrustSurveillance && b.Source == SourceANSPFeed) || (b.Trust == core.TrustBroadcast && b.Source == SourceAdsbRx)
	checks := []error{
		finiteOpt("body.alt_pressure_m", b.AltPressureM), finiteOpt("body.alt_wgs84_m", b.AltWGS84M),
		nonNegOpt("body.gs_ms", b.GSMS), trackDegOpt("body.track_deg", b.TrackDeg), finiteOpt("body.vrate_ms", b.VRateMS),
		idOK("body.source_instance", b.SourceInstance),
	}
	for _, err := range checks {
		if err != nil {
			return nil, err
		}
	}
	switch {
	case !icao24Re.MatchString(b.ICAO24):
		return nil, core.Fieldf("body.icao24", "not six lower-case hex digits")
	case b.Callsign != nil && (len(*b.Callsign) > 8 || !utf8.ValidString(*b.Callsign)):
		return nil, core.Fieldf("body.callsign", "longer than 8 characters")
	case !b.Position.latLon().Valid():
		return nil, core.Fieldf("body.position", "not a WGS84 position")
	case !sourceClasses[b.SourceClass]:
		return nil, core.Fieldf("body.source_class", "unknown source class")
	case !pair:
		return nil, core.Fieldf("body.trust", "%q from %q: a manned track is surveillance from ansp_feed or broadcast from adsb_rx", b.Trust, b.Source)
	case !mannedStates[b.State]:
		return nil, core.Fieldf("body.state", "unknown state")
	}
	return &m, nil
}

func (p MannedPosition) latLon() core.LatLon { return core.LatLon{LatDeg: p.Lat, LonDeg: p.Lng} }

// MannedInputOf maps a decoded manned track onto an Input. Its AMSL
// altitude is uspace-core's rid.SelectAltitude of the geometric (WGS84)
// altitude through the geoid and the pressure altitude (R-07, R-08): the
// geometric one through the geoid when both are known (alt_source
// geodetic); without a geometric altitude the pressure altitude as
// broadcast, which is not AMSL, so the aircraft is judged on the
// horizontal alone (alt_source pressure, R-09); a geometric altitude
// without a geoid is none (pressure never stands in for a missing
// geoid). The selection's thresholds are ap, the policy row's
// (policy.Values.AltPolicy), as for every other source. A live manned
// aircraft flies; a stale or switched-off one is not a new sample at all
// (the caller does not feed it to the monitor).
func MannedInputOf(m *MannedTrack, g geoid.Undulator, ap rid.AltPolicy) Input {
	b := &m.Body
	p := b.Position.latLon()
	flying := true
	in := Input{
		ID: NSManned + ":" + b.ICAO24, TrackID: b.ICAO24, Trust: b.Trust, Source: b.Source, Instance: b.SourceInstance,
		Position: p, AltSource: core.AltNone, SpeedMS: b.GSMS, TrackDeg: b.TrackDeg, VSpeedMS: b.VRateMS,
		Emergency: b.Emergency, Flying: &flying, Times: m.Times(), State: b.State, Callsign: b.Callsign,
	}
	ai := rid.AltInput{AltHAEM: b.AltWGS84M, AltPressureM: b.AltPressureM}
	if b.AltWGS84M != nil && g != nil {
		if n, err := g.UndulationM(p); err == nil && core.IsFinite(n) {
			ai.UndulationM = &n
		}
	}
	alt := rid.SelectAltitude(ai, ap)
	in.AltAMSLM, in.AltSource = alt.AltAMSLM, alt.Source
	if c5, _, err := cell.Key(p); err == nil {
		in.Cell5 = c5
	}
	return in
}
