package tsdbwriter

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Row is one row of one table.
type Row struct {
	Table  *store.CopyTable
	Values []any
}

// Decoded is what a message holds: its msg_id, its captured_at (for the
// time range of a gap), and its rows; none when the message is valid
// and holds nothing for the database (Skip says why).
type Decoded struct {
	MsgID      string
	CapturedAt time.Time
	Rows       []Row
	Skip       string
}

// Decoder reads one message of a stream.
type Decoder func(data []byte) (Decoded, error)

// SkipNotOwnFlight is the skip of a trk.v1 track that is not one of this
// USSP's flights (counted as skipped_not_own_flight).
const SkipNotOwnFlight = "not_own_flight"

type position struct {
	Lat *float64 `json:"lat"`
	Lng *float64 `json:"lng"`
}

func (p position) latLon() (core.LatLon, error) {
	if p.Lat == nil || p.Lng == nil {
		return core.LatLon{}, core.Fieldf("position", "lat and lng are required")
	}
	ll := core.LatLon{LatDeg: *p.Lat, LonDeg: *p.Lng}
	if !ll.Valid() {
		return core.LatLon{}, core.Fieldf("position", "out of range: %v", ll)
	}
	return ll, nil
}

// envelope reads and checks the envelope of a message of schema.
func envelope(data []byte, schema string) (bus.Envelope, json.RawMessage, error) {
	var m struct {
		bus.Envelope
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return bus.Envelope{}, nil, core.Fieldf("message", "not an enveloped message: %v", err)
	}
	if m.Schema != schema {
		return bus.Envelope{}, nil, core.Fieldf("schema", "%q where %q is expected", m.Schema, schema)
	}
	if err := m.Validate(); err != nil {
		return bus.Envelope{}, nil, err
	}
	if len(m.Body) == 0 || m.Body[0] != '{' {
		return bus.Envelope{}, nil, core.Fieldf("body", "not an object")
	}
	return m.Envelope, m.Body, nil
}

func tsOf(e *bus.Envelope) *time.Time {
	if e.TS == nil {
		return nil
	}
	t := e.TS.Time
	return &t
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func finite(name string, v *float64) error {
	if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
		return core.Fieldf(name, "not finite")
	}
	return nil
}

func trackDeg(v *float64) error {
	if v != nil && (*v < 0 || *v >= 360) {
		return core.Fieldf("track_deg", "must be in [0, 360), got %v", *v)
	}
	return nil
}

// trackBody is the part of track/telemetry/v1 (uspace-lab
// schemas/common/track/telemetry/v1) the writer stores.
type trackBody struct {
	TrackID        string   `json:"track_id"`
	Trust          string   `json:"trust"`
	Source         string   `json:"source"`
	SourceInstance string   `json:"source_instance"`
	Position       position `json:"position"`
	AltWGS84M      *float64 `json:"alt_wgs84_m"`
	AltAMSLM       *float64 `json:"alt_amsl_m"`
	AltPressureM   *float64 `json:"alt_pressure_m"`
	HeightM        *float64 `json:"height_m"`
	HeightRef      *string  `json:"height_ref"`
	SpeedMS        *float64 `json:"speed_ms"`
	TrackDeg       *float64 `json:"track_deg"`
	VSpeedMS       *float64 `json:"vspeed_ms"`
	AccuracyHM     *float64 `json:"accuracy_h_m"`
	AccuracyVM     *float64 `json:"accuracy_v_m"`
	Status         *string  `json:"status"`
	Emergency      bool     `json:"emergency"`
	FlightID       *string  `json:"flight_id"`
}

func readTrack(data []byte) (bus.Envelope, trackBody, json.RawMessage, string, error) {
	env, raw, err := envelope(data, "track/telemetry/v1")
	if err != nil {
		return env, trackBody{}, nil, "", err
	}
	var b trackBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return env, b, nil, "", core.Fieldf("body", "%v", err)
	}
	if b.TrackID == "" || b.SourceInstance == "" {
		return env, b, nil, "", core.Fieldf("body", "track_id and source_instance are required")
	}
	ll, err := b.Position.latLon()
	if err != nil {
		return env, b, nil, "", err
	}
	for _, f := range []struct {
		name string
		v    *float64
	}{{"alt_wgs84_m", b.AltWGS84M}, {"alt_amsl_m", b.AltAMSLM}, {"alt_pressure_m", b.AltPressureM}, {"height_m", b.HeightM},
		{"speed_ms", b.SpeedMS}, {"track_deg", b.TrackDeg}, {"vspeed_ms", b.VSpeedMS}, {"accuracy_h_m", b.AccuracyHM}, {"accuracy_v_m", b.AccuracyVM}} {
		if err := finite(f.name, f.v); err != nil {
			return env, b, nil, "", err
		}
	}
	if err := trackDeg(b.TrackDeg); err != nil {
		return env, b, nil, "", err
	}
	if (b.HeightM == nil) != (b.HeightRef == nil) {
		return env, b, nil, "", core.Fieldf("height_ref", "required exactly when height_m is a number")
	}
	c5, _, err := cell.Key(ll)
	if err != nil {
		return env, b, nil, "", err
	}
	return env, b, raw, c5, nil
}

// DecodeTelemetry reads a TRK message: a track of one of this USSP's
// flights (a flight_id) is a telemetry row; a track without one (a peer
// or manned track on trk.v1) is skipped and counted, since its own
// subject (peer.v1, man.v1) carries it to its table.
func DecodeTelemetry(data []byte) (Decoded, error) {
	env, b, _, c5, err := readTrack(data)
	if err != nil {
		return Decoded{}, err
	}
	d := Decoded{MsgID: env.MsgID, CapturedAt: env.CapturedAt.Time}
	if b.FlightID == nil {
		d.Skip = SkipNotOwnFlight
		return d, nil
	}
	if !uuidRe.MatchString(*b.FlightID) {
		return Decoded{}, core.Fieldf("flight_id", "not a UUID")
	}
	d.Rows = []Row{{Table: &store.TableTelemetry, Values: []any{
		env.MsgID, *b.FlightID, env.CapturedAt.Time, tsOf(&env), env.RxTS.Time, env.Backlog, string(env.TimeSource),
		*b.Position.Lat, *b.Position.Lng, b.AltWGS84M, b.AltAMSLM, b.AltPressureM, b.HeightM, b.HeightRef, b.SpeedMS, b.TrackDeg,
		b.VSpeedMS, b.AccuracyHM, b.AccuracyVM, b.Status, b.Emergency, b.SourceInstance, c5,
	}}}
	return d, nil
}

// DecodePeer reads a PEER message: a peer USSP's flight as
// track/telemetry/v1 (trust provider), its body stored verbatim as the
// state.
func DecodePeer(data []byte) (Decoded, error) {
	env, b, raw, c5, err := readTrack(data)
	if err != nil {
		return Decoded{}, err
	}
	return Decoded{MsgID: env.MsgID, CapturedAt: env.CapturedAt.Time, Rows: []Row{{Table: &store.TablePeerFlights, Values: []any{
		env.MsgID, b.SourceInstance, b.TrackID, env.RxTS.Time, string(raw), c5,
	}}}}, nil
}

// mannedBody is the part of track/manned/v1 (the ANSP's schema) the
// writer stores.
type mannedBody struct {
	ICAO24         string          `json:"icao24"`
	Callsign       *string         `json:"callsign"`
	Position       position        `json:"position"`
	AltPressureM   *float64        `json:"alt_pressure_m"`
	AltWGS84M      *float64        `json:"alt_wgs84_m"`
	GSMS           *float64        `json:"gs_ms"`
	TrackDeg       *float64        `json:"track_deg"`
	VRateMS        *float64        `json:"vrate_ms"`
	Emergency      json.RawMessage `json:"emergency"`
	SourceClass    string          `json:"source_class"`
	Quality        json.RawMessage `json:"quality"`
	Trust          string          `json:"trust"`
	Source         string          `json:"source"`
	SourceInstance string          `json:"source_instance"`
}

var icao24Re = regexp.MustCompile(`^[0-9a-f]{6}$`)

// jsonText is a raw JSON value as text, nil for absent or null.
func jsonText(r json.RawMessage) *string {
	s := strings.TrimSpace(string(r))
	if s == "" || s == "null" {
		return nil
	}
	return &s
}

// DecodeManned reads a MAN message: track/manned/v1 into manned_tracks,
// or into econspicuity_tracks when it comes from this USSP's own
// receiver (source adsb_rx, trust broadcast).
func DecodeManned(data []byte) (Decoded, error) {
	env, raw, err := envelope(data, "track/manned/v1")
	if err != nil {
		return Decoded{}, err
	}
	var b mannedBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return Decoded{}, core.Fieldf("body", "%v", err)
	}
	if !icao24Re.MatchString(b.ICAO24) || b.SourceClass == "" || b.Trust == "" || b.SourceInstance == "" {
		return Decoded{}, core.Fieldf("body", "icao24, source_class, trust and source_instance are required")
	}
	ll, err := b.Position.latLon()
	if err != nil {
		return Decoded{}, err
	}
	for _, f := range []struct {
		name string
		v    *float64
	}{{"alt_pressure_m", b.AltPressureM}, {"alt_wgs84_m", b.AltWGS84M}, {"gs_ms", b.GSMS}, {"track_deg", b.TrackDeg}, {"vrate_ms", b.VRateMS}} {
		if err := finite(f.name, f.v); err != nil {
			return Decoded{}, err
		}
	}
	if err := trackDeg(b.TrackDeg); err != nil {
		return Decoded{}, err
	}
	c5, _, err := cell.Key(ll)
	if err != nil {
		return Decoded{}, err
	}
	vals := []any{env.MsgID, b.ICAO24, b.Callsign, env.CapturedAt.Time, tsOf(&env), env.RxTS.Time, ll.LatDeg, ll.LonDeg,
		b.AltPressureM, b.AltWGS84M, b.GSMS, b.TrackDeg, b.VRateMS, jsonText(b.Emergency), b.SourceClass, jsonText(b.Quality),
		b.SourceInstance, b.Trust, c5}
	t := &store.TableManned
	if b.Source == "adsb_rx" || b.Trust == "broadcast" {
		if b.Trust != "broadcast" {
			return Decoded{}, core.Fieldf("trust", "an e-conspicuity track is broadcast, got %q", b.Trust)
		}
		t, vals = &store.TableEconspicuity, append(vals, b.SourceInstance)
	}
	return Decoded{MsgID: env.MsgID, CapturedAt: env.CapturedAt.Time, Rows: []Row{{Table: t, Values: vals}}}, nil
}

// trafficBody is the record of one sampled product (traffic/product/v1
// as traffic-ws samples it for the record, docs/PLAN.md §5.2): who was
// shown what, with the bbox as [min_lng, min_lat, max_lng, max_lat].
// trafficBody is what traffic_products keeps of a traffic/product/v1
// record sample (schemas/traffic/product/v1; traffic-ws publishes one
// per subscriber every traffic_record_every_s with client_id set): who
// it was for, what it covered, the tracks shown (id, trust, state and
// age) and the inputs degraded.
type trafficBody struct {
	ClientID string `json:"client_id"`
	For      struct {
		IntentID *string   `json:"intent_id"`
		BBox     []float64 `json:"bbox"`
	} `json:"for"`
	Tracks []struct {
		TrackID string   `json:"track_id"`
		Trust   string   `json:"trust"`
		State   string   `json:"state"`
		AgeS    *float64 `json:"age_s"`
	} `json:"tracks"`
	Degraded []struct {
		Input string `json:"input"`
	} `json:"degraded"`
	PolicyVersion *int64 `json:"policy_version"`
}

// shownTrack is one entry of tracks_shown.
type shownTrack struct {
	TrackID string   `json:"track_id"`
	Trust   string   `json:"trust"`
	State   string   `json:"state"`
	AgeS    *float64 `json:"age_s"`
}

// DecodeTraffic reads a TRAFFIC message into traffic_products; at is
// the envelope's captured_at.
func DecodeTraffic(data []byte) (Decoded, error) {
	env, raw, err := envelope(data, "traffic/product/v1")
	if err != nil {
		return Decoded{}, err
	}
	var b trafficBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return Decoded{}, core.Fieldf("body", "%v", err)
	}
	if b.ClientID == "" || b.PolicyVersion == nil || b.Tracks == nil {
		return Decoded{}, core.Fieldf("body", "client_id, tracks and policy_version are required")
	}
	if b.For.IntentID != nil && !uuidRe.MatchString(*b.For.IntentID) {
		return Decoded{}, core.Fieldf("intent_id", "not a UUID")
	}
	var box [4]*float64
	switch len(b.For.BBox) {
	case 0:
	case 4:
		bb := b.For.BBox
		if bb[1] < -90 || bb[3] > 90 || bb[1] > bb[3] || bb[0] < -180 || bb[2] > 180 {
			return Decoded{}, core.Fieldf("bbox", "not a box: %v", bb)
		}
		for i := range bb {
			if err := finite("bbox", &bb[i]); err != nil {
				return Decoded{}, err
			}
		}
		box = [4]*float64{&bb[1], &bb[0], &bb[3], &bb[2]}
	default:
		return Decoded{}, core.Fieldf("bbox", "four numbers, got %d", len(b.For.BBox))
	}
	shown := make([]shownTrack, len(b.Tracks))
	for i, t := range b.Tracks {
		shown[i] = shownTrack(t)
	}
	tracks, err := json.Marshal(shown)
	if err != nil {
		return Decoded{}, core.Fieldf("tracks", "%v", err)
	}
	degraded := make([]string, 0, len(b.Degraded))
	for _, d := range b.Degraded {
		degraded = append(degraded, d.Input)
	}
	return Decoded{MsgID: env.MsgID, CapturedAt: env.CapturedAt.Time, Rows: []Row{{Table: &store.TableTrafficProducts, Values: []any{
		env.MsgID, b.ClientID, env.CapturedAt.Time, b.For.IntentID, box[0], box[1], box[2], box[3], string(tracks),
		degraded, *b.PolicyVersion,
	}}}}, nil
}

// confBody is the conformance outcome of one sample (docs/PLAN.md §5.2
// conformance_samples).
type confBody struct {
	FlightID         string   `json:"flight_id"`
	State            string   `json:"state"`
	DistanceOutsideM *float64 `json:"distance_outside_m"`
	HeightOverM      *float64 `json:"height_over_m"`
}

// DecodeConformance reads a CONF message into conformance_samples;
// captured_at is the envelope's (the judged sample's).
func DecodeConformance(data []byte) (Decoded, error) {
	env, raw, err := envelope(data, "conformance/state/v1")
	if err != nil {
		return Decoded{}, err
	}
	var b confBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return Decoded{}, core.Fieldf("body", "%v", err)
	}
	if !uuidRe.MatchString(b.FlightID) || b.State == "" {
		return Decoded{}, core.Fieldf("body", "flight_id (a UUID) and state are required")
	}
	for _, f := range []struct {
		name string
		v    *float64
	}{{"distance_outside_m", b.DistanceOutsideM}, {"height_over_m", b.HeightOverM}} {
		if err := finite(f.name, f.v); err != nil {
			return Decoded{}, err
		}
	}
	return Decoded{MsgID: env.MsgID, CapturedAt: env.CapturedAt.Time, Rows: []Row{{Table: &store.TableConformanceSamples, Values: []any{
		env.MsgID, b.FlightID, env.CapturedAt.Time, b.State, b.DistanceOutsideM, b.HeightOverM,
	}}}}, nil
}

// Stream is one stream the writer consumes, with its decoder and the
// tables it fills.
type Stream struct {
	Name    string
	Subject string
	Decode  Decoder
	Tables  []*store.CopyTable
}

// Streams are the five streams of docs/PLAN.md §3.1's tsdb-writer row.
var Streams = []Stream{
	{Name: bus.StreamTRK, Subject: bus.SubjectTrkAll, Decode: DecodeTelemetry, Tables: []*store.CopyTable{&store.TableTelemetry}},
	{Name: bus.StreamMAN, Subject: bus.SubjectManAll, Decode: DecodeManned, Tables: []*store.CopyTable{&store.TableManned, &store.TableEconspicuity}},
	{Name: bus.StreamPEER, Subject: bus.SubjectPeerAll, Decode: DecodePeer, Tables: []*store.CopyTable{&store.TablePeerFlights}},
	{Name: bus.StreamTRAFFIC, Subject: bus.SubjectTrafficAll, Decode: DecodeTraffic, Tables: []*store.CopyTable{&store.TableTrafficProducts}},
	{Name: bus.StreamCONF, Subject: bus.SubjectConfAll, Decode: DecodeConformance, Tables: []*store.CopyTable{&store.TableConformanceSamples}},
}
