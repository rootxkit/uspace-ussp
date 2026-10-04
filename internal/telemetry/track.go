package telemetry

import (
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// Schemas this package publishes.
const (
	// SchemaTrack is track/telemetry/v1 (uspace-lab schemas/common, a
	// pinned copy under schemas/track/telemetry/v1).
	SchemaTrack = "track/telemetry/v1"
	// SchemaIdentChange is ident/change/v1 (schemas/ident/change/v1).
	SchemaIdentChange = "ident/change/v1"
	// SchemaSourceStatus is source/status/v1 (schemas/source/status/v1).
	SchemaSourceStatus = "source/status/v1"
	// SchemaConsoleStatus is console/status/v1 (schemas/console/status/v1):
	// the only frame this socket ever sends (CLAUDE.md rule 1).
	SchemaConsoleStatus = "console/status/v1"
)

// Producer is the envelope producer of this process's messages.
const Producer = "ussp/telemetry-ingest"

// SourceOperatorWS is the adapter type of operator telemetry (the
// envelope/v1 source enumeration and the source-control type). Samples
// that arrive by POST /v1/telemetry/batch carry it too: the shared
// enumeration has no batch value, and the switch is one for both.
const SourceOperatorWS = "operator_ws"

// TrackBody is the body of track/telemetry/v1 as this USSP publishes it
// for its own flights (spec 04 §3.1): trust authenticated, source
// operator_ws, the client as source_instance, the flight as track and
// flight id, the identification block of 04 §3.2. OperatorPosition and
// TelemetryLost are this USSP's members beside the shared ones (the body
// admits more): the operator position is for the F3411 details of
// authorised Display Providers only (06 §5), and telemetry_lost says the
// flight has been silent for telemetry_lost_s.
type TrackBody struct {
	TrackID          string              `json:"track_id"`
	Trust            core.Trust          `json:"trust"`
	Source           string              `json:"source"`
	SourceInstance   string              `json:"source_instance"`
	Position         Position            `json:"position"`
	AltWGS84M        *float64            `json:"alt_wgs84_m"`
	AltAMSLM         *float64            `json:"alt_amsl_m"`
	AltSource        core.AltSource      `json:"alt_source"`
	AltPressureM     *float64            `json:"alt_pressure_m"`
	HeightM          *float64            `json:"height_m"`
	HeightRef        *string             `json:"height_ref"`
	SpeedMS          *float64            `json:"speed_ms"`
	TrackDeg         *float64            `json:"track_deg"`
	VSpeedMS         *float64            `json:"vspeed_ms"`
	AccuracyHM       *float64            `json:"accuracy_h_m"`
	AccuracyVM       *float64            `json:"accuracy_v_m"`
	Status           *string             `json:"status"`
	Emergency        bool                `json:"emergency"`
	Identification   core.Identification `json:"identification"`
	FlightID         *string             `json:"flight_id"`
	IntentID         *string             `json:"intent_id"`
	UndulationM      *float64            `json:"undulation_m"`
	OperatorPosition *OperatorPosition   `json:"operator_position,omitempty"`
	// AccuracyH and AccuracyV are the F3411 categories as the client sent
	// them (WP-9 serves them back unchanged).
	AccuracyH          f3411.HorizontalAccuracy `json:"accuracy_h"`
	AccuracyV          f3411.VerticalAccuracy   `json:"accuracy_v"`
	TimestampAccuracyS *float64                 `json:"timestamp_accuracy_s"`
	Seq                int64                    `json:"seq"`
	// Anomaly is "teleport" when the aircraft moved faster than the
	// policy's teleport_speed_ms since its previous sample (06 T3):
	// flagged and counted, never dropped.
	Anomaly *string `json:"anomaly,omitempty"`
}

// AnomalyTeleport flags a sample implausibly far from the previous one.
const AnomalyTeleport = "teleport"

// Track is one track/telemetry/v1 message.
type Track struct {
	bus.Envelope
	Body TrackBody `json:"body"`
}

// IdentBody is ident/change/v1: the identification a track now carries
// and the one it replaces (nil for the track's first).
type IdentBody struct {
	TrackID        string               `json:"track_id"`
	FlightID       *string              `json:"flight_id"`
	Trust          core.Trust           `json:"trust"`
	Source         string               `json:"source"`
	SourceInstance string               `json:"source_instance"`
	Identification core.Identification  `json:"identification"`
	Previous       *core.Identification `json:"previous"`
}

// IdentChange is one ident/change/v1 message.
type IdentChange struct {
	bus.Envelope
	Body IdentBody `json:"body"`
}

// identChanged reports whether next differs from prev in what ident.v1
// announces: the status, the reason or the mismatch flag (04 §3.2). A
// nil prev is a change: the track's first identification.
func identChanged(prev *core.Identification, next core.Identification) bool {
	return prev == nil || prev.Status != next.Status || prev.Reason != next.Reason || prev.Mismatch != next.Mismatch
}

// The metre bounds of the F3411 accuracy categories, as the standard's
// file names them (uas_standards f3411 v22a, HorizontalAccuracy and
// VerticalAccuracy, "< X": the generated docs of uspace-core f3411). A
// category without an upper bound (unknown, or "more than") has none.
var (
	horizontalBoundM = map[f3411.HorizontalAccuracy]float64{
		f3411.HA10NM: 18520, f3411.HA4NM: 7408, f3411.HA2NM: 3704, f3411.HA1NM: 1852, f3411.HA05NM: 926,
		f3411.HA03NM: 555.6, f3411.HA01NM: 185.2, f3411.HA005NM: 92.6, f3411.HA30m: 30, f3411.HA10m: 10,
		f3411.HA3m: 3, f3411.HA1m: 1,
	}
	verticalBoundM = map[f3411.VerticalAccuracy]float64{
		f3411.VA150m: 150, f3411.VA45m: 45, f3411.VA25m: 25, f3411.VA10m: 10, f3411.VA3m: 3, f3411.VA1m: 1,
	}
	// verticalCode is MAV_ODID_VER_ACC of each category that has one:
	// 0 unknown, then 1 to 6 for under 150, 45, 25, 10, 3 and 1 m (the
	// codes uspace-core rid.AltInput reads). VA150mPlus has no code: it
	// is worse than every one of them.
	verticalCode = map[f3411.VerticalAccuracy]uint8{
		f3411.VAUnknown: 0, f3411.VA150m: 1, f3411.VA45m: 2, f3411.VA25m: 3, f3411.VA10m: 4, f3411.VA3m: 5, f3411.VA1m: 6,
	}
)

func horizontalM(a f3411.HorizontalAccuracy) *float64 {
	if v, ok := horizontalBoundM[a]; ok {
		return &v
	}
	return nil
}

func verticalM(a f3411.VerticalAccuracy) *float64 {
	if v, ok := verticalBoundM[a]; ok {
		return &v
	}
	return nil
}

// AccuracyHM is the metre bound of an F3411 horizontal accuracy
// category, nil for one without (unknown, or "more than").
func AccuracyHM(a f3411.HorizontalAccuracy) *float64 { return horizontalM(a) }

// AccuracyVM is the metre bound of an F3411 vertical accuracy category,
// nil for one without.
func AccuracyVM(a f3411.VerticalAccuracy) *float64 { return verticalM(a) }

// VerticalAccuracyCode is the MAV_ODID_VER_ACC code of an F3411 vertical
// accuracy category (what uspace-core rid.AltInput reads); false for one
// without a code (VA150mPlus, or a value outside the enumeration).
func VerticalAccuracyCode(a f3411.VerticalAccuracy) (uint8, bool) {
	c, ok := verticalCode[a]
	return c, ok
}
