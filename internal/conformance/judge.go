package conformance

import (
	"fmt"
	"math"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
)

// Outcome is what one judgement found.
type Outcome string

// The outcomes.
const (
	// Inside: a volume active at the sample's time holds it horizontally
	// and vertically.
	Inside Outcome = "inside"
	// Outside: no volume holds it; the verdict's reason says why.
	Outside Outcome = "outside"
	// Undetermined: a volume active at the sample's time holds it
	// horizontally, but its vertical position is not known, so the
	// vertical judgement did not run. Never read as inside.
	Undetermined Outcome = "undetermined"
)

// Reasons (spec 03 §3 conformance_states.reason). The first five are a
// verdict's; ReasonThresholdExceeded and ReasonTelemetryLost are the
// state machine's.
const (
	ReasonOutsideVolumeH    = "outside_volume_h"
	ReasonAboveUpper        = "above_upper"
	ReasonBelowLower        = "below_lower"
	ReasonBeforeStart       = "before_start"
	ReasonAfterEnd          = "after_end"
	ReasonThresholdExceeded = "threshold_exceeded"
	ReasonTelemetryLost     = "telemetry_lost"
)

// MaxVolumes bounds the volumes of one authorisation (E-10; F3548 gives
// no figure, WP-7 accepts at most this many).
const MaxVolumes = 100

// Sample is one track sample as the judgement reads it.
type Sample struct {
	Position core.LatLon
	// AltAMSLM is the AMSL altitude, nil when unknown; AltSource says
	// which rule produced it (geodetic, network, pressure, none).
	AltAMSLM  *float64
	AltSource core.AltSource
	// CapturedAt is where the ingest placed the sample (T-01); the time
	// window is judged at it.
	CapturedAt time.Time
}

// Volume is one authorised volume: its outline (exactly one of a
// polygon and a circle, as WP-7 normalised it), its AMSL band and its
// time window, all closed.
type Volume struct {
	Shape      deconflict.Shape
	LowerAMSLM float64
	UpperAMSLM float64
	Start, End time.Time
}

// Thresholds are the deviation thresholds of Art. 10(2)(d): horizontal
// and vertical metres, and seconds.
type Thresholds struct {
	HM float64 `json:"h_m"`
	VM float64 `json:"v_m"`
	TS float64 `json:"t_s"`
}

// Condition is one Art. 6(1) condition the authorisation carries. The
// airspace's operational conditions were applied to the volumes by WP-7,
// so here a condition is carried for the record and judged through the
// volumes and the window.
type Condition struct {
	Code string `json:"code"`
	Ref  string `json:"ref,omitempty"`
}

// Authorisation is what a flight is judged against.
type Authorisation struct {
	IntentID            string
	AuthorisationNumber string
	Volumes             []Volume
	Thresholds          Thresholds
	Conditions          []Condition
}

// Policy holds the judgement's own thresholds (data, INV-03).
type Policy struct {
	// PressureUncertaintyM widens the band, each way, for a pressure
	// altitude (R-09). Zero is no margin.
	PressureUncertaintyM float64
}

// Verdict is the outcome of one judgement with its numbers.
type Verdict struct {
	Outcome Outcome `json:"outcome"`
	// Reason is empty for inside and undetermined.
	Reason string `json:"reason,omitempty"`
	// DistanceOutsideM is the horizontal distance to the nearest
	// authorised outline active at the sample's time (or, outside the
	// window, of the nearest window), 0 inside.
	DistanceOutsideM float64 `json:"distance_outside_m"`
	// HeightOverM is how far above the upper or below the lower AMSL
	// limit the sample is, 0 inside the band and when the vertical is not
	// known. For a pressure altitude it is measured from the widened band.
	HeightOverM float64 `json:"height_over_m"`
	// TimeOutsideS is how far before the start or after the end of the
	// nearest window the sample is, 0 within a window.
	TimeOutsideS float64 `json:"time_outside_s"`
	// WithinThreshold is true for an outside verdict whose every excess
	// is within the deviation thresholds (Art. 10(2)(d) tolerance).
	WithinThreshold bool `json:"within_threshold"`
	// VerticalKnown is false when the vertical judgement did not run.
	VerticalKnown bool `json:"vertical_known"`
	// WithinBand is false when the sample is in the band only through the
	// pressure margin, or outside it; true inside the band as given.
	WithinBand bool `json:"within_band"`
	// Volume is the index of the volume the numbers are measured against.
	Volume int `json:"volume"`
}

// finitePositive refuses a threshold no judgement can use (E-15).
func finitePositive(field string, v float64) error {
	if !core.IsFinite(v) || v <= 0 {
		return core.Fieldf(field, "must be a finite number greater than 0, got %v", v)
	}
	return nil
}

// Validate refuses an authorisation that cannot be judged: no volume or
// more than MaxVolumes, an outline that fails its check, a band that is
// not finite or upside down, a window that is empty or upside down, or a
// threshold that is zero, negative or not finite (E-15). Every refusal
// names its field.
func (a Authorisation) Validate() error {
	if err := finitePositive("deviation_thresholds.h_m", a.Thresholds.HM); err != nil {
		return err
	}
	if err := finitePositive("deviation_thresholds.v_m", a.Thresholds.VM); err != nil {
		return err
	}
	if err := finitePositive("deviation_thresholds.t_s", a.Thresholds.TS); err != nil {
		return err
	}
	if len(a.Volumes) == 0 || len(a.Volumes) > MaxVolumes {
		return core.Fieldf("volumes", "has %d volumes; 1 to %d", len(a.Volumes), MaxVolumes)
	}
	for i, v := range a.Volumes {
		f := fmt.Sprintf("volumes[%d]", i)
		if !core.IsFinite(v.LowerAMSLM) || !core.IsFinite(v.UpperAMSLM) || v.LowerAMSLM > v.UpperAMSLM {
			return core.Fieldf(f+".band", "lower %v and upper %v AMSL are not a band", v.LowerAMSLM, v.UpperAMSLM)
		}
		if v.Start.IsZero() || v.End.IsZero() || v.End.Before(v.Start) {
			return core.Fieldf(f+".window", "%v to %v is not a window", v.Start, v.End)
		}
		// The outline's own check, through a judgement at its first
		// point that cannot fail otherwise.
		if _, err := deconflict.PointDistanceM(anyPoint(v.Shape), v.Shape); err != nil {
			return core.Fieldf(f+".outline", "%v", err)
		}
	}
	return nil
}

// anyPoint is a valid point of the outline (or the zero point, which
// the outline's check then refuses).
func anyPoint(s deconflict.Shape) core.LatLon {
	switch {
	case s.Circle != nil:
		return s.Circle.Center
	case len(s.Polygon) > 0:
		return s.Polygon[0]
	}
	return core.LatLon{}
}

// verticalUsable reports whether the sample's vertical position may be
// judged and the margin it is judged with.
func verticalUsable(s Sample, pol Policy) (altM, marginM float64, ok bool) {
	if s.AltAMSLM == nil || !core.IsFinite(*s.AltAMSLM) {
		return 0, 0, false
	}
	switch s.AltSource {
	case core.AltGeodetic, core.AltNetwork:
		return *s.AltAMSLM, 0, true
	case core.AltPressure:
		return *s.AltAMSLM, pol.PressureUncertaintyM, true
	case core.AltNone:
		return 0, 0, false
	}
	return 0, 0, false
}

// judged is one volume's numbers for a sample.
type judged struct {
	index      int
	hM         float64 // horizontal excess, 0 inside the outline
	vM         float64 // vertical excess beyond the (widened) band, 0 inside
	above      bool    // the vertical excess is above the upper limit
	strictBand bool    // inside the band as given
	tS         float64 // time excess, 0 within the window
	before     bool    // the time excess is before the start
}

// Judge judges one sample against an authorisation (pure, stateless).
// An error is a judgement that did not run: an invalid authorisation
// (Validate), a margin that is negative or not finite, a sample whose
// position is not a valid WGS84 position or whose time is zero, or an
// outline the geometry could not measure. It never panics.
func Judge(s Sample, a Authorisation, pol Policy) (Verdict, error) {
	if !core.IsFinite(pol.PressureUncertaintyM) || pol.PressureUncertaintyM < 0 {
		return Verdict{}, core.Fieldf("pressure_uncertainty_m", "must be a finite number of at least 0, got %v", pol.PressureUncertaintyM)
	}
	if err := a.Validate(); err != nil {
		return Verdict{}, err
	}
	if !s.Position.Valid() {
		return Verdict{}, core.Fieldf("position", "is not a valid WGS84 position")
	}
	if s.CapturedAt.IsZero() {
		return Verdict{}, core.Fieldf("captured_at", "is required")
	}
	altM, marginM, vKnown := verticalUsable(s, pol)

	all := make([]judged, len(a.Volumes))
	for i, v := range a.Volumes {
		h, err := deconflict.PointDistanceM(s.Position, v.Shape)
		if err != nil {
			return Verdict{}, core.Fieldf(fmt.Sprintf("volumes[%d].outline", i), "%v", err)
		}
		if !core.IsFinite(h) {
			return Verdict{}, core.Fieldf(fmt.Sprintf("volumes[%d].outline", i), "distance not measurable")
		}
		j := judged{index: i, hM: h}
		if vKnown {
			lo, hi := v.LowerAMSLM-marginM, v.UpperAMSLM+marginM
			switch {
			case altM > hi:
				j.vM, j.above = altM-hi, true
			case altM < lo:
				j.vM = lo - altM
			}
			j.strictBand = altM >= v.LowerAMSLM && altM <= v.UpperAMSLM
		}
		switch t := s.CapturedAt; {
		case t.Before(v.Start):
			j.tS, j.before = v.Start.Sub(t).Seconds(), true
		case t.After(v.End):
			j.tS = t.Sub(v.End).Seconds()
		}
		all[i] = j
	}

	// The volumes active at the sample's time judge it; when none is, the
	// volumes of the nearest window do, and the reason is the time.
	var cands []judged
	minT := math.Inf(1)
	for _, j := range all {
		minT = math.Min(minT, j.tS)
	}
	for _, j := range all {
		if j.tS == minT {
			cands = append(cands, j)
		}
	}
	// Prefer a volume that holds the sample: horizontally, then
	// vertically; else the nearest horizontally, then vertically.
	best := cands[0]
	for _, j := range cands[1:] {
		if j.hM < best.hM || (j.hM == best.hM && j.vM < best.vM) ||
			(j.hM == best.hM && j.vM == best.vM && j.strictBand && !best.strictBand) {
			best = j
		}
	}
	out := Verdict{
		DistanceOutsideM: best.hM, HeightOverM: best.vM, TimeOutsideS: best.tS,
		VerticalKnown: vKnown, WithinBand: vKnown && best.strictBand, Volume: best.index,
	}
	switch {
	case best.tS > 0 && best.before:
		out.Outcome, out.Reason = Outside, ReasonBeforeStart
	case best.tS > 0:
		out.Outcome, out.Reason = Outside, ReasonAfterEnd
	case best.hM > 0:
		out.Outcome, out.Reason = Outside, ReasonOutsideVolumeH
	case !vKnown:
		out.Outcome = Undetermined
	case best.vM > 0 && best.above:
		out.Outcome, out.Reason = Outside, ReasonAboveUpper
	case best.vM > 0:
		out.Outcome, out.Reason = Outside, ReasonBelowLower
	default:
		out.Outcome = Inside
	}
	if out.Outcome == Outside {
		th := a.Thresholds
		out.WithinThreshold = out.DistanceOutsideM <= th.HM && out.HeightOverM <= th.VM && out.TimeOutsideS <= th.TS
	}
	return out, nil
}
