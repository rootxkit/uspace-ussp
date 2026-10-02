package conformance

import (
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// ReasonRestored is the reason of a nonconforming or contingent state a
// tracker took from its intent's state rather than from a judgement: a
// fresh tracker (a restart, a handover with no saved state, an
// intent_active read late) never shows conforming a flight its intent
// says is not, and needs the full hysteresis to return (SC-22).
const ReasonRestored = "restored_from_intent"

// The intent states a fresh tracker takes over (intent.StateNonconforming
// and intent.StateContingent; this package does not import the intent
// service's state machine).
const (
	IntentNonconforming = "nonconforming"
	IntentContingent    = "contingent"
)

// SavedAlert is an alert as the monitor persists it: the published
// fields and the cell the alert is published under.
type SavedAlert struct {
	Alert
	Cell5 string `json:"cell5"`
}

func saveAlert(a *Alert) *SavedAlert {
	if a == nil {
		return nil
	}
	return &SavedAlert{Alert: a.clone(), Cell5: a.Cell5}
}

// AlertOf is the alert of a saved one.
func (s *SavedAlert) AlertOf() *Alert {
	if s == nil {
		return nil
	}
	a := s.clone()
	a.Cell5 = s.Cell5
	if a.Detail == nil {
		a.Detail = map[string]any{}
	}
	return &a
}

// SaveAlerts are alerts as the monitor persists them.
func SaveAlerts(as []Alert) []SavedAlert {
	out := make([]SavedAlert, 0, len(as))
	for i := range as {
		out = append(out, *saveAlert(&as[i]))
	}
	return out
}

// TrackerState is a tracker's whole state machine as the monitor
// persists it (the conformance_state bucket), so that a restart or a
// handover to another instance continues the flight instead of starting
// it again from unknown.
type TrackerState struct {
	FlightID            string `json:"flight_id"`
	IntentID            string `json:"intent_id,omitempty"`
	AuthorisationNumber string `json:"authorisation_number,omitempty"`

	Base      State    `json:"base"`
	Reason    string   `json:"reason,omitempty"`
	UnknownBy string   `json:"unknown_by,omitempty"`
	Verdict   *Verdict `json:"verdict,omitempty"`
	LastOut   *Verdict `json:"last_out,omitempty"`

	Authorised  bool `json:"authorised"`
	AuthMissing bool `json:"auth_missing,omitempty"`
	Disabled    bool `json:"disabled,omitempty"`

	LastCaptured time.Time   `json:"last_captured"`
	LastLive     time.Time   `json:"last_live"`
	HasLive      bool        `json:"has_live"`
	LastFlying   bool        `json:"last_flying"`
	LastPos      core.LatLon `json:"last_pos"`
	LastCell5    string      `json:"last_cell5,omitempty"`

	LinkLost     bool      `json:"link_lost,omitempty"`
	LostAt       time.Time `json:"lost_at"`
	OutsideSince time.Time `json:"outside_since"`
	LastOutside  time.Time `json:"last_outside"`
	NCSince      time.Time `json:"nc_since"`

	NC       *SavedAlert `json:"nc,omitempty"`
	LL       *SavedAlert `json:"ll,omitempty"`
	NCReason string      `json:"nc_reason,omitempty"`
}

// State is the tracker's state machine to persist.
func (t *Tracker) State() TrackerState {
	cp := func(v *Verdict) *Verdict {
		if v == nil {
			return nil
		}
		c := *v
		return &c
	}
	return TrackerState{
		FlightID: t.FlightID, IntentID: t.IntentID, AuthorisationNumber: t.AuthorisationNumber,
		Base: t.base, Reason: t.reason, UnknownBy: t.unknownBy, Verdict: cp(t.verdict), LastOut: cp(t.lastOut),
		Authorised: t.authorised, AuthMissing: t.authMissing, Disabled: t.disabled,
		LastCaptured: t.lastCaptured, LastLive: t.lastLive, HasLive: t.hasLive, LastFlying: t.lastFlying,
		LastPos: t.lastPos, LastCell5: t.lastCell5,
		LinkLost: t.linkLost, LostAt: t.lostAt, OutsideSince: t.outsideSince, LastOutside: t.lastOutside, NCSince: t.ncSince,
		NC: saveAlert(t.nc), LL: saveAlert(t.ll), NCReason: t.ncReason,
	}
}

// RestoreTracker is the tracker of a persisted state, taken over at wall.
// It is conservative where the state may be older than the flight: a
// flight that is not conforming counts its time back inside from wall
// at the earliest, so a restored tracker never returns a flight to
// conforming (nor clears its nonconformance) without the full
// hysteresis seen by itself.
func RestoreTracker(s TrackerState, counters *core.Counters, wall time.Time) *Tracker {
	t := NewTracker(s.FlightID, s.IntentID, s.AuthorisationNumber, counters)
	if !states[s.Base] || s.Base == StateLostLink {
		return t // a state that does not read starts from unknown
	}
	t.base, t.reason, t.unknownBy = s.Base, s.Reason, s.UnknownBy
	if t.base == StateUnknown && t.unknownBy == "" {
		t.unknownBy = UnknownNotJudged
	}
	t.verdict, t.lastOut = s.Verdict, s.LastOut
	t.authorised, t.authMissing, t.disabled = s.Authorised, s.AuthMissing, s.Disabled
	t.lastCaptured, t.lastLive, t.hasLive, t.lastFlying = s.LastCaptured, s.LastLive, s.HasLive, s.LastFlying
	t.lastPos, t.lastCell5 = s.LastPos, s.LastCell5
	t.linkLost, t.lostAt, t.outsideSince, t.lastOutside, t.ncSince = s.LinkLost, s.LostAt, s.OutsideSince, s.LastOutside, s.NCSince
	t.nc, t.ll, t.ncReason = s.NC.AlertOf(), s.LL.AlertOf(), s.NCReason
	if t.ll != nil && !t.linkLost {
		t.ll = nil
	}
	if t.base != StateConforming && t.base != StateUnknown && t.lastOutside.Before(wall) {
		t.lastOutside = wall
	}
	return t
}

// SeedFromIntent takes the intent's state into a tracker that has not
// judged the flight yet: a nonconforming or contingent intent makes it
// nonconforming or contingent (ReasonRestored) from at, so its return
// needs the full hysteresis. Any other intent state, or a tracker that
// has judged, changes nothing. It returns what it changed.
func (t *Tracker) SeedFromIntent(intentState string, at time.Time) Events {
	var ev Events
	t.seedFromIntent(&ev, intentState, at)
	return ev
}

func (t *Tracker) seedFromIntent(ev *Events, intentState string, at time.Time) {
	if t.authorised || t.base != StateUnknown {
		return
	}
	var to State
	switch intentState {
	case IntentNonconforming:
		to = StateNonconforming
	case IntentContingent:
		to = StateContingent
	default:
		return
	}
	t.change(ev, at, func() {
		t.base, t.reason, t.ncSince, t.lastOutside = to, ReasonRestored, at, at
	})
}
