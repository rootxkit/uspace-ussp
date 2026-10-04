package peers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/identify"
	"github.com/rootxkit/uspace-core/rid"
	"github.com/rootxkit/uspace-core/timeplace"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// The refusals of FlightTrack.
var (
	// ErrNoState is a flight without a current state (only an operating
	// area): nothing to place.
	ErrNoState = errors.New("no current state")
	// ErrTooOld is a state older than F3411's 60 s behind its answer:
	// not shown as current (PlaceNetwork).
	ErrTooOld = errors.New("older than the near-real-time window")
)

// MaxFlightIDBytes bounds a peer's flight id (a track id).
const MaxFlightIDBytes = 128

// Conv is what FlightTrack reads besides the flight: the registry's
// resolution, the geoid and the policy.
type Conv struct {
	Resolver Resolver
	Geoid    geoid.Undulator
	Policy   policy.Values
}

// NetworkPolicy is the placement bound of a peer's state: uspace-core's
// network defaults (F3411's 60 s) with the ingest's ahead tolerance.
func NetworkPolicy(v policy.Values) timeplace.NetworkPolicy {
	p := timeplace.DefaultNetworkPolicy()
	p.MaxAgeS = f3411.NetMaxNearRealTimeDataPeriodSeconds
	p.ToleranceS = v.TelemetryAheadToleranceS
	return p
}

// Place is PlaceNetwork for one peer state: its own timestamp against
// the answer's, received at rx on our clock.
func Place(stateTS time.Time, respTS *time.Time, rx time.Time, v policy.Values) (timeplace.Placement, timeplace.NetworkNote, bool) {
	return timeplace.PlaceNetwork(stateTS, respTS, rx, NetworkPolicy(v))
}

// subjectToken is a flight id as a subject token: itself when it is one
// (no '.', '*', '>', space or control character, at most
// bus.MaxTokenBytes), else "_h" and 32 hex digits of its SHA-256.
func subjectToken(id string) string {
	ok := id != "" && len(id) <= bus.MaxTokenBytes && id[0] != '_' && utf8.ValidString(id)
	for _, r := range id {
		if r == '.' || r == '*' || r == '>' || unicode.IsSpace(r) || unicode.IsControl(r) {
			ok = false
			break
		}
	}
	if ok {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "_h" + hex.EncodeToString(sum[:16])
}

// FlightTrack maps one flight of a peer's GET /uss/flights answer onto
// track/telemetry/v1 and its subject peer.v1.<cell3>.<cell5>.<token>:
// trust provider (never authenticated, R-14), source network_rid, the
// peer's base URL as source_instance, the flight id as track id, no
// flight or intent of ours; the position, the speed, track and vertical
// speed with F3411's special values as unknown (uspace-core's
// accessors); the geodetic altitude through the geoid by
// rid.SelectAltitude (a vertical accuracy the code table does not know
// leaves only the pressure altitude); the height with its reference;
// the accuracy bounds; the operational status and the emergency it
// says; the identification ResolveBroadcast gives without a serial (the
// 1 Hz answer carries none: unidentified, no_serial, as broadcast). It is
// placed by PlaceNetwork against respTS; a state not shown is ErrTooOld.
func FlightTrack(base string, f *f3411.RIDFlight, respTS, rx time.Time, cv Conv) (*telemetry.Track, string, error) {
	if f.CurrentState == nil {
		return nil, "", ErrNoState
	}
	if f.Id == "" || len(f.Id) > MaxFlightIDBytes || !utf8.ValidString(f.Id) {
		return nil, "", core.Fieldf("id", "empty, invalid or longer than %d bytes", MaxFlightIDBytes)
	}
	st := f.CurrentState
	pos := st.Position.LatLon()
	if !pos.Valid() {
		return nil, "", core.Fieldf("current_state.position", "not a WGS84 position")
	}
	p, _, shown := Place(st.Timestamp.Value, &respTS, rx, cv.Policy)
	if !shown {
		return nil, "", ErrTooOld
	}
	c5, _, err := cell.Key(pos)
	if err != nil {
		return nil, "", err
	}
	b := telemetry.TrackBody{
		TrackID: f.Id, Trust: core.TrustProvider, Source: SourceNetworkRID, SourceInstance: base,
		Position: telemetry.Position{Lat: pos.LatDeg, Lng: pos.LonDeg}, AltWGS84M: st.Position.AltHAEM(),
		AltPressureM: st.Position.PressureAltM(), SpeedMS: st.SpeedMS(), TrackDeg: st.TrackDeg(), VSpeedMS: st.VerticalSpeedMS(),
	}
	if st.Position.AccuracyH != nil {
		b.AccuracyH, b.AccuracyHM = *st.Position.AccuracyH, telemetry.AccuracyHM(*st.Position.AccuracyH)
	}
	in := rid.AltInput{AltHAEM: b.AltWGS84M, AltPressureM: b.AltPressureM}
	if st.Position.AccuracyV != nil {
		b.AccuracyV, b.AccuracyVM = *st.Position.AccuracyV, telemetry.AccuracyVM(*st.Position.AccuracyV)
		code, known := telemetry.VerticalAccuracyCode(*st.Position.AccuracyV)
		in.VertAccuracyCode = code
		if !known {
			in.AltHAEM = nil
		}
	}
	if cv.Geoid != nil {
		if n, err := cv.Geoid.UndulationM(pos); err == nil && core.IsFinite(n) {
			in.UndulationM, b.UndulationM = &n, &n
		}
	}
	alt := rid.SelectAltitude(in, cv.Policy.AltPolicy())
	b.AltAMSLM, b.AltSource = alt.AltAMSLM, alt.Source
	if h := st.Position.Height; h != nil && h.Reference.Valid() {
		if v := h.DistanceM(); v != nil {
			ref := string(h.Reference)
			b.HeightM, b.HeightRef = v, &ref
		}
	}
	if st.OperationalStatus != nil {
		s := string(*st.OperationalStatus)
		b.Status = &s
		b.Emergency = *st.OperationalStatus == f3411.Emergency
	}
	if ta := float64(st.TimestampAccuracy); core.IsFinite(ta) && ta >= 0 {
		b.TimestampAccuracyS = &ta
	}
	if cv.Resolver != nil {
		b.Identification = cv.Resolver.ResolveBroadcast(nil, nil)
	} else {
		b.Identification = identify.ResolveBroadcast(nil, nil, nil)
	}
	m := &telemetry.Track{Envelope: bus.NewEnvelope(telemetry.SchemaTrack, Producer, timeplace.Times(p, rx, false)), Body: b}
	subject, err := bus.Peer(c5, subjectToken(f.Id))
	if err != nil {
		return nil, "", err
	}
	return m, subject, nil
}
