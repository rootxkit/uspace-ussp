package ridsp

import (
	"math"
	"time"

	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// The mapping from our track (track/telemetry/v1) to the F3411 wire
// types (spec 04 §3.1): every member name and type is uspace-core's
// f3411, generated from the pinned standard file. What our track does not
// know goes out as Table 1's special value through core's constants,
// never as zero: SpecialSpeed, SpecialTrackDirection,
// SpecialVerticalSpeed and SpecialHeight (altitude, pressure altitude,
// height). A speed above MaxSpeed is MaxSpeed ("254.25 m/s or more",
// the standard's rule); a vertical speed beyond MaxAbsVerticalSpeed is
// held at it.

// IntentFacts are what the F3411 answers read from an intent in
// intent_active (intent/state/v1).
type IntentFacts struct {
	IntentID            string  `json:"intent_id"`
	AuthorisationNumber *string `json:"authorisation_number"`
	OperatorReg         string  `json:"operator_reg"`
	UASSerial           string  `json:"uas_serial"`
	Category            string  `json:"category"`
	ClassLabel          *string `json:"class_label"`
	UARegistration      *string `json:"ua_registration"`
}

// Intents is the intent_active projection as rid-sp reads it.
type Intents interface {
	// Intent is the intent id's facts; false when intent_active does not
	// hold it.
	Intent(id string) (IntentFacts, bool)
}

func f32(v float64) *float32 {
	f := float32(v)
	return &f
}

func ptr[T any](v T) *T { return &v }

// wireTime is t as the standard's Time (RFC3339, UTC).
func wireTime(t time.Time) f3411.Time {
	return f3411.Time{Format: f3411.RFC3339, Value: t.UTC()}
}

// orSpecial is v, or special when v is nil or not finite.
func orSpecial(v *float64, special float64) *float32 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return f32(special)
	}
	return f32(*v)
}

// speedOf is the ground speed on the wire: SpecialSpeed when unknown,
// MaxSpeed when at or above it.
func speedOf(v *float64) *float32 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
		return f32(f3411.SpecialSpeed)
	}
	return f32(math.Min(*v, f3411.MaxSpeed))
}

// trackOf is the track on the wire: SpecialTrackDirection when unknown.
func trackOf(v *float64) *float32 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < f3411.MinTrackDirection || *v >= f3411.MaxTrackDirection {
		return f32(f3411.SpecialTrackDirection)
	}
	return f32(*v)
}

// verticalSpeedOf is the vertical speed on the wire:
// SpecialVerticalSpeed when unknown, held within MaxAbsVerticalSpeed.
func verticalSpeedOf(v *float64) *float32 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return f32(f3411.SpecialVerticalSpeed)
	}
	return f32(math.Max(-f3411.MaxAbsVerticalSpeed, math.Min(*v, f3411.MaxAbsVerticalSpeed)))
}

// statusOf is the operational status: Emergency when the track says
// emergency, the status the operator sent when it is one of the
// standard's, Undeclared otherwise.
func statusOf(b telemetry.TrackBody) f3411.RIDOperationalStatus {
	if b.Emergency {
		return f3411.Emergency
	}
	if b.Status != nil {
		if s := f3411.RIDOperationalStatus(*b.Status); s.Valid() {
			return s
		}
	}
	return f3411.Undeclared
}

// PositionOf is the F3411 position of one of our samples: lat and lng,
// alt (HAE, the WGS84 altitude the operator sent), the accuracy
// categories as sent, extrapolated false (we serve what was sent),
// pressure_altitude and height {distance, reference}.
func PositionOf(s Sample) f3411.RIDAircraftPosition {
	b := s.Body
	lat, lng := b.Position.Lat, b.Position.Lng
	accH, accV := b.AccuracyH, b.AccuracyV
	if !accH.Valid() {
		accH = f3411.HAUnknown
	}
	if !accV.Valid() {
		accV = f3411.VAUnknown
	}
	// Without a height the reference has no meaning; the standard still
	// requires one, and TakeoffLocation is the first of its enumeration.
	ref := f3411.TakeoffLocation
	if b.HeightRef != nil {
		if r := f3411.RIDHeightReference(*b.HeightRef); r.Valid() {
			ref = r
		}
	}
	h := b.HeightM
	if b.HeightRef == nil {
		h = nil
	}
	return f3411.RIDAircraftPosition{
		Lat: &lat, Lng: &lng,
		Alt:              orSpecial(b.AltWGS84M, f3411.SpecialHeight),
		AccuracyH:        &accH,
		AccuracyV:        &accV,
		Extrapolated:     ptr(false),
		PressureAltitude: orSpecial(b.AltPressureM, f3411.SpecialHeight),
		Height:           &f3411.RIDHeight{Distance: orSpecial(h, f3411.SpecialHeight), Reference: ref},
	}
}

// StateOf is the F3411 aircraft state of one of our samples. Our track
// carries no speed accuracy, so speed_accuracy is SAUnknown; an unknown
// timestamp accuracy is 0, the standard's default.
func StateOf(s Sample) f3411.RIDAircraftState {
	b := s.Body
	var tsAcc float32
	if b.TimestampAccuracyS != nil && *b.TimestampAccuracyS >= 0 && !math.IsInf(*b.TimestampAccuracyS, 0) {
		tsAcc = float32(*b.TimestampAccuracyS)
	}
	st := statusOf(b)
	return f3411.RIDAircraftState{
		Timestamp:         wireTime(s.CapturedAt),
		TimestampAccuracy: tsAcc,
		OperationalStatus: &st,
		Position:          PositionOf(s),
		Track:             trackOf(b.TrackDeg),
		Speed:             speedOf(b.SpeedMS),
		SpeedAccuracy:     f3411.SAUnknown,
		VerticalSpeed:     verticalSpeedOf(b.VSpeedMS),
	}
}

// FlightOf is the RIDFlight of one flight in the window: its id (our
// flight id), aircraft_type NotDeclared (spec gap: Annex IV and
// telemetry/v1 declare no UA type), the current state, the recent
// positions asked for, and simulated false.
func FlightOf(v View) f3411.RIDFlight {
	cur := StateOf(v.Current)
	f := f3411.RIDFlight{
		Id:           v.ID,
		AircraftType: f3411.NotDeclared,
		CurrentState: &cur,
		Simulated:    ptr(false),
	}
	if len(v.Recent) > 0 {
		rp := make([]f3411.RIDRecentAircraftPosition, len(v.Recent))
		for i := range v.Recent {
			s := &v.Recent[i]
			rp[i] = f3411.RIDRecentAircraftPosition{Time: wireTime(s.CapturedAt), Position: PositionOf(*s)}
		}
		f.RecentPositions = &rp
	}
	return f
}
