package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
)

// SchemaTelemetry is the schema of what an operator client sends
// (schemas/telemetry/v1).
const SchemaTelemetry = "telemetry/v1"

// Bounds of what a client sends (E-10). A WebSocket message larger than
// MaxMessageBytes closes the socket (the library's 1009); a batch holds
// at most MaxBatchFrames samples in at most MaxBatchBytes.
const (
	MaxMessageBytes = 8 << 10
	MaxBatchFrames  = 2000
	MaxBatchBytes   = 1 << 20
	// maxFieldErrors bounds the errors one sample reports.
	maxFieldErrors = 20
)

// Position is a WGS84 position in decimal degrees.
type Position struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// LatLon is p for uspace-core.
func (p Position) LatLon() core.LatLon { return core.LatLon{LatDeg: p.Lat, LonDeg: p.Lng} }

// OperatorPosition is the remote pilot's or the take-off position
// (Art. 8(2)(e)).
type OperatorPosition struct {
	Lat       float64  `json:"lat"`
	Lng       float64  `json:"lng"`
	AltWGS84M *float64 `json:"alt_wgs84_m,omitempty"`
}

// Frame is one telemetry/v1 sample, decoded and checked: every field the
// schema bounds is within its bounds, every enumeration one of its
// values.
type Frame struct {
	TS                 time.Time
	Serial             string
	Seq                int64
	Backlog            bool
	End                bool
	IntentID           *string
	Position           Position
	AltWGS84M          *float64
	AltPressureM       *float64
	HeightM            *float64
	HeightRef          *string
	SpeedMS            *float64
	TrackDeg           *float64
	VSpeedMS           *float64
	Status             f3411.RIDOperationalStatus
	Emergency          bool
	OperatorPosition   *OperatorPosition
	AccuracyH          f3411.HorizontalAccuracy
	AccuracyV          f3411.VerticalAccuracy
	TimestampAccuracyS *float64
}

// wireFrame is telemetry/v1 as JSON reads it: every member a pointer, so
// a missing member is told from a null one.
type wireFrame struct {
	TS                 *string         `json:"ts"`
	Serial             *string         `json:"serial"`
	Seq                json.RawMessage `json:"seq"`
	Backlog            *bool           `json:"backlog"`
	End                *bool           `json:"end"`
	IntentID           json.RawMessage `json:"intent_id"`
	Position           *wirePosition   `json:"position"`
	AltWGS84M          json.RawMessage `json:"alt_wgs84_m"`
	AltPressureM       json.RawMessage `json:"alt_pressure_m"`
	HeightM            json.RawMessage `json:"height_m"`
	HeightRef          json.RawMessage `json:"height_ref"`
	SpeedMS            json.RawMessage `json:"speed_ms"`
	TrackDeg           json.RawMessage `json:"track_deg"`
	VSpeedMS           json.RawMessage `json:"vspeed_ms"`
	Status             *string         `json:"status"`
	Emergency          *bool           `json:"emergency"`
	OperatorPosition   json.RawMessage `json:"operator_position"`
	AccuracyH          *string         `json:"accuracy_h"`
	AccuracyV          *string         `json:"accuracy_v"`
	TimestampAccuracyS json.RawMessage `json:"timestamp_accuracy_s"`
}

type wirePosition struct {
	Lat       *float64        `json:"lat"`
	Lng       *float64        `json:"lng"`
	AltWGS84M json.RawMessage `json:"alt_wgs84_m"`
}

var (
	serialPattern = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// fieldErrs collects at most maxFieldErrors field errors under a prefix.
type fieldErrs struct {
	prefix string
	errs   []error
}

func (f *fieldErrs) add(field, format string, a ...any) {
	if len(f.errs) < maxFieldErrors {
		f.errs = append(f.errs, core.Fieldf(f.prefix+field, format, a...))
	}
}

func (f *fieldErrs) err() error { return errors.Join(f.errs...) }

// nullableNumber reads a member that is a number in [lo, hi] or null;
// required says whether it must be present. hiOpen excludes hi.
func (f *fieldErrs) nullableNumber(name string, raw json.RawMessage, required bool, lo, hi float64, hiOpen bool) *float64 {
	if raw == nil {
		if required {
			f.add(name, "required")
		}
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		f.add(name, "must be a number or null")
		return nil
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < lo || v > hi || (hiOpen && v == hi) {
		bound := "]"
		if hiOpen {
			bound = ")"
		}
		f.add(name, "must be in [%v, %v%s, got %v", lo, hi, bound, v)
		return nil
	}
	return &v
}

// DecodeFrame reads one telemetry/v1 sample. A member the schema does not
// know is refused (the trust class and the source are the ingest's, 06
// T11), as is anything out of its bounds; every refusal names its field
// under prefix. Nothing here panics on any input (FuzzDecodeFrame).
func DecodeFrame(raw []byte, prefix string) (Frame, error) {
	fe := &fieldErrs{prefix: prefix}
	var w wireFrame
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Frame{}, core.Fieldf(strings.TrimSuffix(prefix, ".")+bodyName(prefix), "not a telemetry/v1 sample: %s", clipErr(err))
	}
	if dec.More() {
		return Frame{}, core.Fieldf(strings.TrimSuffix(prefix, ".")+bodyName(prefix), "trailing data after the sample")
	}
	var f Frame
	switch {
	case w.TS == nil:
		fe.add("ts", "required")
	case len(*w.TS) > 40 || !strings.HasSuffix(*w.TS, "Z"):
		fe.add("ts", "must be RFC 3339 in UTC, ending in Z")
	default:
		t, err := time.Parse(time.RFC3339Nano, *w.TS)
		if err != nil {
			fe.add("ts", "must be RFC 3339 in UTC, ending in Z")
		} else {
			f.TS = t.UTC()
		}
	}
	switch {
	case w.Serial == nil:
		fe.add("serial", "required")
	case !serialPattern.MatchString(*w.Serial):
		fe.add("serial", "1 to 64 printable ASCII characters without spaces")
	default:
		f.Serial = *w.Serial
	}
	if w.Seq == nil {
		fe.add("seq", "required")
	} else if n, err := strconv.ParseInt(string(bytes.TrimSpace(w.Seq)), 10, 64); err != nil || n < 0 || n > 1<<53-1 {
		fe.add("seq", "must be an integer from 0 to 2^53-1")
	} else {
		f.Seq = n
	}
	f.Backlog = w.Backlog != nil && *w.Backlog
	f.End = w.End != nil && *w.End
	if w.IntentID != nil && !bytes.Equal(bytes.TrimSpace(w.IntentID), []byte("null")) {
		var id string
		if err := json.Unmarshal(w.IntentID, &id); err != nil || !uuidPattern.MatchString(id) {
			fe.add("intent_id", "must be a version 4 UUID in lower case, or null")
		} else {
			f.IntentID = &id
		}
	}
	switch p := w.Position; {
	case p == nil:
		fe.add("position", "required")
	case p.Lat == nil || p.Lng == nil:
		fe.add("position", "lat and lng are required")
	case p.AltWGS84M != nil:
		fe.add("position.alt_wgs84_m", "not a member of position; the altitude is alt_wgs84_m")
	case !(core.LatLon{LatDeg: *p.Lat, LonDeg: *p.Lng}).Valid() || *p.Lng < -180 || *p.Lng > 180:
		fe.add("position", "not a WGS84 position in degrees")
	default:
		f.Position = Position{Lat: *p.Lat, Lng: *p.Lng}
	}
	f.AltWGS84M = fe.nullableNumber("alt_wgs84_m", w.AltWGS84M, true, -1000, 20000, false)
	f.AltPressureM = fe.nullableNumber("alt_pressure_m", w.AltPressureM, false, -1000, 20000, false)
	f.HeightM = fe.nullableNumber("height_m", w.HeightM, true, -1000, 20000, false)
	f.SpeedMS = fe.nullableNumber("speed_ms", w.SpeedMS, true, 0, 300, false)
	f.TrackDeg = fe.nullableNumber("track_deg", w.TrackDeg, true, 0, 360, true)
	f.VSpeedMS = fe.nullableNumber("vspeed_ms", w.VSpeedMS, true, -100, 100, false)
	f.TimestampAccuracyS = fe.nullableNumber("timestamp_accuracy_s", w.TimestampAccuracyS, true, 0, 60, false)
	f.HeightRef = heightRef(fe, w.HeightRef, w.HeightM != nil && f.HeightM != nil)
	switch {
	case w.Status == nil:
		fe.add("status", "required")
	case !f3411.RIDOperationalStatus(*w.Status).Valid():
		fe.add("status", "must be an F3411 RIDOperationalStatus")
	default:
		f.Status = f3411.RIDOperationalStatus(*w.Status)
	}
	if w.Emergency == nil {
		fe.add("emergency", "required")
	} else {
		f.Emergency = *w.Emergency
	}
	switch {
	case w.AccuracyH == nil:
		fe.add("accuracy_h", "required")
	case !f3411.HorizontalAccuracy(*w.AccuracyH).Valid():
		fe.add("accuracy_h", "must be an F3411 HorizontalAccuracy")
	default:
		f.AccuracyH = f3411.HorizontalAccuracy(*w.AccuracyH)
	}
	switch {
	case w.AccuracyV == nil:
		fe.add("accuracy_v", "required")
	case !f3411.VerticalAccuracy(*w.AccuracyV).Valid():
		fe.add("accuracy_v", "must be an F3411 VerticalAccuracy")
	default:
		f.AccuracyV = f3411.VerticalAccuracy(*w.AccuracyV)
	}
	f.OperatorPosition = operatorPosition(fe, w.OperatorPosition)
	if err := fe.err(); err != nil {
		return Frame{}, err
	}
	return f, nil
}

func bodyName(prefix string) string {
	if prefix == "" {
		return "body"
	}
	return ""
}

func heightRef(fe *fieldErrs, raw json.RawMessage, heightGiven bool) *string {
	if raw == nil {
		fe.add("height_ref", "required")
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if heightGiven {
			fe.add("height_ref", "required when height_m is a number")
		}
		return nil
	}
	var ref string
	if err := json.Unmarshal(raw, &ref); err != nil || !f3411.RIDHeightReference(ref).Valid() {
		fe.add("height_ref", "must be TakeoffLocation, GroundLevel or null")
		return nil
	}
	if !heightGiven {
		fe.add("height_ref", "must be null when height_m is null")
		return nil
	}
	return &ref
}

func operatorPosition(fe *fieldErrs, raw json.RawMessage) *OperatorPosition {
	if raw == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var p wirePosition
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || p.Lat == nil || p.Lng == nil {
		fe.add("operator_position", "must be {lat, lng, alt_wgs84_m?} or null")
		return nil
	}
	if !(core.LatLon{LatDeg: *p.Lat, LonDeg: *p.Lng}).Valid() || *p.Lng < -180 || *p.Lng > 180 {
		fe.add("operator_position", "not a WGS84 position in degrees")
		return nil
	}
	out := &OperatorPosition{Lat: *p.Lat, Lng: *p.Lng}
	out.AltWGS84M = fe.nullableNumber("operator_position.alt_wgs84_m", p.AltWGS84M, false, -1000, 20000, false)
	return out
}

// clipErr is a decoder error as a short text: it may quote input.
func clipErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return strings.ToValidUTF8(s, "?")
}

// DecodeMessage reads one WS /v1/telemetry message: the console frame
// {"schema": "telemetry/v1", "body": <sample>} (reconciliation M29). The
// other envelope members a client may send are ignored: the ingest sets
// every time and the trust class itself.
func DecodeMessage(data []byte) (Frame, error) {
	var m struct {
		Schema *string         `json:"schema"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return Frame{}, core.Fieldf("message", "not a JSON object with schema and body: %s", clipErr(err))
	}
	if m.Schema == nil || *m.Schema != SchemaTelemetry {
		return Frame{}, core.Fieldf("schema", "must be %q", SchemaTelemetry)
	}
	if len(m.Body) == 0 || m.Body[0] != '{' {
		return Frame{}, core.Fieldf("body", "must be a telemetry/v1 object")
	}
	return DecodeFrame(m.Body, "body.")
}

// BatchRequest is a decoded POST /v1/telemetry/batch: each sample, or
// the reason it could not be read, and the client's send time.
type BatchRequest struct {
	// SentAt is the client's clock when it sent the batch; nil when it
	// did not say (the batch rule places the samples then).
	SentAt *time.Time
	Frames []Frame
	// Invalid holds, per index of the request, why the sample was not
	// read; Frames holds the others in their order, with Index.
	Invalid map[int]error
	Index   []int
}

// DecodeBatch reads {"sent_at": <RFC 3339 Z>?, "frames":
// [<telemetry/v1>, ...]} of at most MaxBatchFrames samples. A body that
// is not that shape is an error; a sample that does not decode is listed
// in Invalid and the others are kept.
func DecodeBatch(data []byte) (BatchRequest, error) {
	if len(data) > MaxBatchBytes {
		return BatchRequest{}, core.Fieldf("body", "longer than %d bytes", MaxBatchBytes)
	}
	var w struct {
		SentAt *string            `json:"sent_at"`
		Frames *[]json.RawMessage `json:"frames"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return BatchRequest{}, core.Fieldf("body", "not a telemetry batch: %s", clipErr(err))
	}
	if dec.More() {
		return BatchRequest{}, core.Fieldf("body", "trailing data after the batch")
	}
	switch {
	case w.Frames == nil:
		return BatchRequest{}, core.Fieldf("frames", "required")
	case len(*w.Frames) == 0:
		return BatchRequest{}, core.Fieldf("frames", "at least one sample")
	case len(*w.Frames) > MaxBatchFrames:
		return BatchRequest{}, core.Fieldf("frames", "%d samples, at most %d", len(*w.Frames), MaxBatchFrames)
	}
	out := BatchRequest{Invalid: map[int]error{}}
	if w.SentAt != nil {
		t, err := time.Parse(time.RFC3339Nano, *w.SentAt)
		if err != nil || len(*w.SentAt) > 40 || !strings.HasSuffix(*w.SentAt, "Z") {
			return BatchRequest{}, core.Fieldf("sent_at", "must be RFC 3339 in UTC, ending in Z")
		}
		t = t.UTC()
		out.SentAt = &t
	}
	for i, raw := range *w.Frames {
		f, err := DecodeFrame(raw, fmt.Sprintf("frames[%d].", i))
		if err != nil {
			out.Invalid[i] = err
			continue
		}
		out.Frames = append(out.Frames, f)
		out.Index = append(out.Index, i)
	}
	return out, nil
}
