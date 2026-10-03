package traffic

import (
	"math"
	"time"

	"github.com/rootxkit/uspace-core/alerting"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/rid"

	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// The namespaces of an aircraft id in the CPA monitor: one id is one
// aircraft for every check (alerting.Track.ID), so a track id of one
// source never meets the same string from another.
const (
	NSTrack  = "trk"  // trk.v1: this USSP's flights (and any track/telemetry/v1 on it)
	NSPeer   = "peer" // peer.v1: peer flights through F3411 (WP-14)
	NSManned = "man"  // man.v1: manned aircraft (WP-14)
)

// Track states of the product (04 §2: a disabled source's tracks age out
// as source_disabled, a silent one as stale; never removed silently).
const (
	StateLive           = "live"
	StateStale          = "stale"
	StateSourceDisabled = "source_disabled"
)

// SourceAdsbRx is the e-conspicuity receiver's adapter type (04 §2).
const SourceAdsbRx = "adsb_rx"

// SourceANSPFeed is the ANSP manned feed's adapter type (04 §2).
const SourceANSPFeed = "ansp_feed"

// Input is one sample of one aircraft as both halves of this package
// read it: a track/telemetry/v1 (TrackInputOf) or a track/manned/v1
// (MannedInputOf), with what the product shows and what TrackOf maps
// onto the core monitor.
type Input struct {
	// ID is the monitor's aircraft id, <namespace>:<track id>.
	ID string
	// TrackID is the id the product and a peer alert name: the flight id
	// of one of this USSP's flights, the peer's flight id, the icao24.
	TrackID string
	Trust   core.Trust
	Source  string
	// Instance is the source instance (client, provider or receiver id),
	// the monitor's station: the source share and the switches key on it.
	Instance string
	// FlightID, IntentID and AuthorisationNumber are set for this USSP's
	// own flights only (Own).
	FlightID, IntentID, AuthorisationNumber string
	Position                                core.LatLon
	Cell5                                   string
	AltAMSLM                                *float64
	AltSource                               core.AltSource
	SpeedMS, TrackDeg, VSpeedMS             *float64
	Emergency                               *bool
	// Flying is the C-05 reading of the status (nil: unknown).
	Flying *bool
	// Identification is the registry's verdict the track carries; nil for
	// a manned aircraft (none is resolved for it).
	Identification *core.Identification
	Times          core.Times
	// State is what the source said of the track (manned frames carry it:
	// live, stale, source_disabled); live for a telemetry track.
	State string
	// Callsign is a manned aircraft's callsign as broadcast (product only).
	Callsign *string
}

// Own reports whether the input is one of this USSP's operator flights:
// authenticated, from operator_ws, with a flight (the only tracks a
// proximity alert is sent for).
func (in *Input) Own() bool {
	return in.Trust == core.TrustAuthenticated && in.Source == telemetry.SourceOperatorWS && in.FlightID != ""
}

// FlyingOf reads the F3411 operational status (C-05): Airborne and
// Emergency fly, Ground does not, anything else (Undeclared,
// RemoteIDSystemFailure, none) is unknown and therefore not flying, but
// not a landing either. It reads the status as conformance.FlyingOf does.
func FlyingOf(status *string) *bool {
	if status == nil {
		return nil
	}
	var v bool
	switch f3411.RIDOperationalStatus(*status) {
	case f3411.Airborne, f3411.Emergency:
		v = true
	case f3411.Ground:
		v = false
	case f3411.Undeclared, f3411.RemoteIDSystemFailure:
		return nil
	default:
		return nil
	}
	return &v
}

func opt(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TrackInputOf maps a decoded track/telemetry/v1 (DecodeTrack) of
// namespace ns (NSTrack or NSPeer) onto an Input.
func TrackInputOf(ns string, tr *telemetry.Track) Input {
	b := &tr.Body
	p := b.Position.LatLon()
	ident := b.Identification
	emergency := b.Emergency
	in := Input{
		ID: ns + ":" + b.TrackID, TrackID: b.TrackID, Trust: b.Trust, Source: b.Source, Instance: b.SourceInstance,
		FlightID: opt(b.FlightID), IntentID: opt(b.IntentID),
		Position: p, AltAMSLM: b.AltAMSLM, AltSource: b.AltSource,
		SpeedMS: b.SpeedMS, TrackDeg: b.TrackDeg, VSpeedMS: b.VSpeedMS, Emergency: &emergency,
		Flying: FlyingOf(b.Status), Identification: &ident, Times: tr.Times(), State: StateLive,
	}
	// Only this USSP's operator flights, which its ingest names by UUIDs,
	// carry a flight and an intent here (Own).
	if ns != NSTrack || b.Trust != core.TrustAuthenticated || b.Source != telemetry.SourceOperatorWS || !uuidRe.MatchString(in.FlightID) {
		in.FlightID, in.IntentID = "", ""
	}
	if !uuidRe.MatchString(in.IntentID) {
		in.IntentID = ""
	}
	if c5, _, err := cell.Key(p); err == nil {
		in.Cell5 = c5
	}
	return in
}

// TrackOf is the one mapping of an Input onto core's alerting.Track
// (brief WP-11): the velocity north, east and down from the speed, the
// track and the vertical speed by rid.VelocityNED (down positive for
// cpa.State); the AMSL altitude with its source (core judges only a
// geodetic or network altitude vertically, R-09); flying from the
// status; the ingest's placement and receipt for the times and the
// source's own clock for the order within one source; the source type
// and instance as core's source key; no transmitter (no track here is a
// direct broadcast with a radio address of its own); identified from the
// identification block. A velocity that is not known is zero, which core
// reads as holding position: the "inside the minima now" clause still
// judges it (C-03). velocityKnown says which.
func TrackOf(in *Input) (tr alerting.Track, velocityKnown bool) {
	tr = alerting.Track{
		ID: in.ID, Pos: in.Position, AltAMSLM: in.AltAMSLM, AltSource: in.AltSource, Flying: in.Flying,
		CapturedAtS: unixS(in.Times.CapturedAt), RxAtS: unixS(in.Times.RxTS), Backlog: in.Times.Backlog,
		Source: in.Source, Station: in.Instance, Identification: in.Identification,
	}
	if in.Times.TS != nil {
		ts := unixS(*in.Times.TS)
		tr.SourceTS = &ts
	}
	if in.Identification != nil {
		identified := in.Identification.Status != core.IdentUnidentified
		tr.Identified = &identified
	}
	vn, ve, vd := rid.VelocityNED(in.SpeedMS, in.TrackDeg, in.VSpeedMS)
	if vn != nil && ve != nil {
		tr.VNMS, tr.VEMS, velocityKnown = *vn, *ve, true
	}
	if vd != nil {
		tr.VDMS = *vd
	}
	return tr, velocityKnown
}

// unixS is t in seconds since the Unix epoch (the ingest's time base,
// which the monitor's wall clock is on).
func unixS(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// timeOfS is the time of s seconds since the Unix epoch.
func timeOfS(s float64) time.Time {
	if !core.IsFinite(s) {
		return time.Time{}
	}
	sec, frac := math.Modf(s)
	return time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC()
}

// DefaultGridCellM is the neighbour grid's cell side (at least the 800 m
// search radius, C-15; core raises it to the radius when smaller).
const DefaultGridCellM = 1000

// ConfigOf is core's monitor configuration under the policy v: the CPA
// minima, window, search radius and largest age gap (cpa.Policy), the
// proximity hysteresis and stale time, the ingest-to-monitor bound and
// the ahead tolerance of the conformance path, core's bounds on aircraft
// and per-source share, and a grid at least one radius wide. No zones:
// the zone path is WP-12's.
func ConfigOf(v policy.Values) alerting.Config {
	c := alerting.DefaultConfig()
	c.Policy = v.CPA()
	c.ClearAfterS = v.CPAClearAfterS
	c.StaleAfterS = v.CPAStaleAfterS
	c.LiveMaxAgeS = v.MonitorLiveMaxAgeS
	c.AheadToleranceS = v.TelemetryAheadToleranceS
	c.GridCellM = math.Max(DefaultGridCellM, v.CPANeighbourRadiusM)
	return c
}
