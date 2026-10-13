package testif

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/national/client"
	inj "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/ridinjection"
)

// wire32 is a float32 member of the injection as the sender wrote it:
// the shortest decimal that reads back as the float32 (1.9 stays 1.9).
func wire32(f float32) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
	return v
}

// coreHeight is an injected height as uspace-core's F3411 type, whose
// DistanceM applies the special value (Table 1).
func coreHeight(h *inj.RIDHeight) *f3411.RIDHeight {
	if h == nil {
		return nil
	}
	d := h.Distance
	return &f3411.RIDHeight{Distance: &d, Reference: f3411.RIDHeightReference(h.Reference)}
}

// The enumerations, member by member from the generated types on both
// sides (E-03): an injected value outside them is refused, never guessed.
var (
	heightRefs = map[inj.RIDHeightReference]client.TelemetryFrameHeightRef{
		inj.RIDHeightReferenceGroundLevel:     client.GroundLevel,
		inj.RIDHeightReferenceTakeoffLocation: client.TakeoffLocation,
	}
	statuses = map[inj.RIDOperationalStatus]client.TelemetryFrameStatus{
		inj.RIDOperationalStatusUndeclared:            client.TelemetryFrameStatusUndeclared,
		inj.RIDOperationalStatusGround:                client.TelemetryFrameStatusGround,
		inj.RIDOperationalStatusAirborne:              client.TelemetryFrameStatusAirborne,
		inj.RIDOperationalStatusEmergency:             client.TelemetryFrameStatusEmergency,
		inj.RIDOperationalStatusRemoteIDSystemFailure: client.TelemetryFrameStatusRemoteIDSystemFailure,
	}
)

// FrameOf maps one injected RIDAircraftState onto a telemetry/v1 frame
// of serial at seq (brief WP-21, RID injection step 3): ts is the state's
// timestamp, the position its lat and lng, alt_wgs84_m its position.alt
// (W84, as the file says), height_m and height_ref its height (the
// state's, else the position's), speed_ms, track_deg and vspeed_ms with
// F3411's special values read by uspace-core as unknown (null), status
// its operational_status (absent: Undeclared, the file's default),
// emergency when that status is Emergency, the accuracies (absent:
// unknown) and timestamp_accuracy_s. A state without timestamp, lat or
// lng cannot be a sample and is refused, naming the member.
func FrameOf(s inj.RIDAircraftState, serial, epoch string, seq int64, op *client.TelemetryOperatorPosition) (client.TelemetryFrame, error) {
	var f client.TelemetryFrame
	if s.Timestamp == nil {
		return f, core.Fieldf("timestamp", "missing: a state without a time is not a sample")
	}
	if s.Position == nil || s.Position.Lat == nil || s.Position.Lng == nil {
		return f, core.Fieldf("position", "lat and lng are required for a sample")
	}
	p := s.Position
	cs := f3411.RIDAircraftState{Speed: s.Speed, Track: s.Track, VerticalSpeed: s.VerticalSpeed,
		Position: f3411.RIDAircraftPosition{Alt: p.Alt, PressureAltitude: p.PressureAltitude}}
	f = client.TelemetryFrame{
		Ts: s.Timestamp.UTC(), Serial: serial, Seq: seq, Epoch: &epoch,
		Position:         client.TelemetryPosition{Lat: *p.Lat, Lng: *p.Lng},
		AltWgs84M:        cs.Position.AltHAEM(),
		AltPressureM:     cs.Position.PressureAltM(),
		SpeedMs:          cs.SpeedMS(),
		TrackDeg:         cs.TrackDeg(),
		VspeedMs:         cs.VerticalSpeedMS(),
		AccuracyH:        client.HAUnknown,
		AccuracyV:        client.VAUnknown,
		Status:           client.TelemetryFrameStatusUndeclared,
		OperatorPosition: op,
	}
	h := s.Height
	if h == nil {
		h = p.Height
	}
	if ch := coreHeight(h); ch != nil {
		if d := ch.DistanceM(); d != nil {
			ref, ok := heightRefs[h.Reference]
			if !ok {
				return f, core.Fieldf("height.reference", "%q is not a reference of the file", string(h.Reference))
			}
			f.HeightM, f.HeightRef = d, &ref
		}
	}
	if s.OperationalStatus != nil {
		st, ok := statuses[*s.OperationalStatus]
		if !ok {
			return f, core.Fieldf("operational_status", "%q is not a status of the file", string(*s.OperationalStatus))
		}
		f.Status = st
	}
	f.Emergency = f.Status == client.TelemetryFrameStatusEmergency
	if p.AccuracyH != nil {
		a, err := p.AccuracyH.AsHorizontalAccuracy()
		if err != nil || !client.TelemetryFrameAccuracyH(a).Valid() {
			return f, core.Fieldf("position.accuracy_h", "not a horizontal accuracy of the file")
		}
		f.AccuracyH = client.TelemetryFrameAccuracyH(a)
	}
	if p.AccuracyV != nil {
		a, err := p.AccuracyV.AsVerticalAccuracy()
		if err != nil || !client.TelemetryFrameAccuracyV(a).Valid() {
			return f, core.Fieldf("position.accuracy_v", "not a vertical accuracy of the file")
		}
		f.AccuracyV = client.TelemetryFrameAccuracyV(a)
	}
	if s.TimestampAccuracy != nil {
		v := wire32(*s.TimestampAccuracy)
		f.TimestampAccuracyS = &v
	}
	return f, nil
}

// operatorPosition is the operator's position of a flight: the first
// details response's operator_location (Art. 8(2)(e)), nil without one.
func operatorPosition(fl inj.TestFlight) (*client.TelemetryOperatorPosition, error) {
	for _, d := range fl.DetailsResponses {
		if d.Details.OperatorLocation == nil {
			continue
		}
		ll, err := d.Details.OperatorLocation.AsLatLngPoint()
		if err != nil {
			return nil, core.Fieldf("details_responses.details.operator_location", "not a LatLngPoint")
		}
		return &client.TelemetryOperatorPosition{Lat: ll.Lat, Lng: ll.Lng}, nil
	}
	return nil, nil
}

// injectedSerial is the serial a flight names: the first details
// response's uas_id.serial_number, else its deprecated serial_number;
// "" when it names none.
func injectedSerial(fl inj.TestFlight) string {
	for _, d := range fl.DetailsResponses {
		if d.Details.UasId != nil && d.Details.UasId.SerialNumber != nil && *d.Details.UasId.SerialNumber != "" {
			return *d.Details.UasId.SerialNumber
		}
		if d.Details.SerialNumber != nil && *d.Details.SerialNumber != "" {
			return *d.Details.SerialNumber
		}
	}
	return ""
}

// planned is one flight's frames, ready before any side effect.
type planned struct {
	injectionID string
	serial      string
	frames      []client.TelemetryFrame
}

// serialKey is the key two serials are one aircraft by: the binding
// route's fold of the normalised serial.
func serialKey(sn string) string {
	return serial.FoldKey(serial.Normalize(strings.TrimSpace(sn))) //nolint:misspell // uspace-core's name
}

// planFlights maps every flight of a test onto its frames, choosing each
// flight's serial (its own, or the first of pool no held test flies), and
// refuses the whole test on the first state that cannot be a sample or a
// serial a held test flies (held, by serialKey): validation before any
// side effect. Each test has its own serials, so two tests never stream
// one aircraft.
func planFlights(p inj.CreateTestParameters, version string, pool []string, held map[string]bool, maxFlights, maxStates int) ([]planned, error) {
	if len(p.RequestedFlights) == 0 {
		return nil, core.Fieldf("requested_flights", "at least one flight")
	}
	if len(p.RequestedFlights) > maxFlights {
		return nil, core.Fieldf("requested_flights", "at most %d flights per test", maxFlights)
	}
	out := make([]planned, 0, len(p.RequestedFlights))
	seen := map[string]bool{}
	next := 0
	for i, fl := range p.RequestedFlights {
		field := func(m string) string { return fmt.Sprintf("requested_flights[%d].%s", i, m) }
		if fl.InjectionId == "" {
			return nil, core.Fieldf(field("injection_id"), "required")
		}
		if len(fl.Telemetry) == 0 || len(fl.Telemetry) > maxStates {
			return nil, core.Fieldf(field("telemetry"), "1 to %d states", maxStates)
		}
		sn := injectedSerial(fl)
		if sn == "" {
			for next < len(pool) && (held[serialKey(pool[next])] || seen[serialKey(pool[next])]) {
				next++
			}
			if next >= len(pool) {
				return nil, core.Fieldf(field("details_responses"), "names no serial and the fixture's pool of %d is used up by this and the held tests", len(pool))
			}
			sn = pool[next]
			next++
		}
		key := serialKey(sn)
		if seen[key] {
			return nil, core.Fieldf(field("details_responses"), "serial %q flies another flight of this test", sn)
		}
		if held[key] {
			return nil, core.Fieldf(field("details_responses"), "serial %q flies in another held test; delete it first", sn)
		}
		seen[key] = true
		op, err := operatorPosition(fl)
		if err != nil {
			return nil, err
		}
		epoch := "t" + version + "-" + strconv.Itoa(i)
		frames := make([]client.TelemetryFrame, 0, len(fl.Telemetry))
		for j, st := range fl.Telemetry {
			f, err := FrameOf(st, sn, epoch, 0, op)
			if err != nil {
				var fe *core.FieldError
				if errors.As(err, &fe) {
					return nil, core.Fieldf(field(fmt.Sprintf("telemetry[%d].%s", j, fe.Field)), "%s", fe.Reason)
				}
				return nil, err
			}
			frames = append(frames, f)
		}
		// The file asks for ascending timestamps ("should"); the frames
		// go out in time order, numbered in it. Two states at one time
		// are not two samples.
		slices.SortStableFunc(frames, func(a, b client.TelemetryFrame) int { return a.Ts.Compare(b.Ts) })
		for j := range frames {
			if j > 0 && !frames[j].Ts.After(frames[j-1].Ts) {
				return nil, core.Fieldf(field("telemetry"), "two states at %s", frames[j].Ts.Format(time.RFC3339Nano))
			}
			frames[j].Seq = int64(j)
		}
		out = append(out, planned{injectionID: fl.InjectionId, serial: sn, frames: frames})
	}
	return out, nil
}
