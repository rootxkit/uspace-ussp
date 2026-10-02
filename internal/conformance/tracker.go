package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"strconv"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// State is a flight's conformance state (spec 03 §3 conformance_states).
type State string

// The states.
const (
	StateConforming    State = "conforming"
	StateNonconforming State = "nonconforming"
	StateContingent    State = "contingent"
	StateLostLink      State = "lost_link"
	StateUnknown       State = "unknown"
)

// Reasons of the unknown state: why no judgement ran.
const (
	// UnknownNotJudged: no flying sample has been judged yet.
	UnknownNotJudged = "not_judged"
	// UnknownAuthorisationMissing: intent_active holds no authorisation
	// for the flight's intent.
	UnknownAuthorisationMissing = "authorisation_missing"
	// UnknownNoIntent: the flight has no intent; there is nothing to
	// conform to (a session outside the regulation's scope, Art. 1(3)).
	UnknownNoIntent = "no_intent"
	// UnknownProjectionUnavailable: intent_active has not been read, so
	// whether the flight has an authorisation is not known (SC-22).
	UnknownProjectionUnavailable = "intent_active_unavailable"
	// UnknownSourceDisabled: the flight's source is switched off (B-11).
	UnknownSourceDisabled = "source_disabled"
	// UnknownAuthorisationInvalid: the authorisation does not read or
	// refuses the judgement (no thresholds, a zero threshold: E-15).
	UnknownAuthorisationInvalid = "authorisation_invalid"
	// UnknownVerticalNotEvaluated: every judgement so far was
	// undetermined (no altitude, or alt_source none): the flight was
	// never shown inside its band, so it is not conforming (SC-22).
	UnknownVerticalNotEvaluated = "vertical_not_evaluated"
)

// Alert kinds (spec 04 §3.3).
const (
	KindNonconformance       = "nonconformance"
	KindLostLink             = "lost_link"
	KindNonconformanceNearby = "nonconformance_nearby"
)

// Alert states and clear reasons (spec 04 §3.3).
const (
	AlertRaised  = "raised"
	AlertUpdated = "updated"
	AlertCleared = "cleared"

	ClearResolved       = "resolved"
	ClearFlightEnded    = "flight_ended"
	ClearSourceDisabled = "source_disabled"
)

// Counter names (E-09): every sample is counted under exactly one of
// the rejected_*, not_flying, flying_unknown, authorisation_missing,
// judgement_failed and judged names.
const (
	CounterRejectedBacklog        = "conformance_rejected_backlog"
	CounterRejectedLate           = "conformance_rejected_late"
	CounterRejectedPlacedAhead    = "conformance_rejected_placed_ahead"
	CounterRejectedOutOfOrder     = "conformance_rejected_out_of_order"
	CounterRejectedSourceDisabled = "conformance_rejected_source_disabled"
	CounterRejectedInvalid        = "conformance_rejected_invalid"
	CounterNotFlying              = "conformance_not_flying"
	CounterFlyingUnknown          = "conformance_flying_unknown"
	CounterAuthorisationMissing   = "conformance_authorisation_missing"
	CounterJudgementFailed        = "conformance_judgement_failed"
	CounterJudged                 = "conformance_judged"
	CounterVerticalNotEvaluated   = "conformance_vertical_not_evaluated"
	CounterTransitions            = "conformance_transitions"
	CounterConfigInvalid          = "conformance_config_invalid"
)

// MaxRecoverableS is F3548's MaxRecoverableTimeInNonconformingStateSeconds:
// a nonconforming flight that has not returned within it is contingent.
// A standard's constant, never policy (CLAUDE.md rule 5).
const MaxRecoverableS = f3548.MaxRecoverableTimeInNonconformingStateSeconds

// Config is the state machine's thresholds, taken from the policy row
// (INV-03); PolicyVersion travels on every event.
type Config struct {
	ClearAfterS          float64
	LostLinkS            float64
	LiveMaxAgeS          float64
	AheadToleranceS      float64
	PressureUncertaintyM float64
	PolicyVersion        int64
}

// ConfigOf is the configuration of a policy version.
func ConfigOf(r policy.Record) Config {
	v := r.Values
	return Config{
		ClearAfterS: v.ConformanceClearAfterS, LostLinkS: v.LostLinkS, LiveMaxAgeS: v.MonitorLiveMaxAgeS,
		AheadToleranceS: v.TelemetryAheadToleranceS, PressureUncertaintyM: v.PressureUncertaintyM, PolicyVersion: r.Version,
	}
}

// Validate refuses a configuration no state machine can run on: every
// time positive and finite (the ahead tolerance and the margin may be
// 0), and the hysteresis longer than twice the ahead tolerance (E-15).
func (c Config) Validate() error {
	for _, f := range []struct {
		name string
		v    float64
	}{{"conformance_clear_after_s", c.ClearAfterS}, {"lost_link_s", c.LostLinkS}, {"monitor_live_max_age_s", c.LiveMaxAgeS}} {
		if err := finitePositive(f.name, f.v); err != nil {
			return err
		}
	}
	if !core.IsFinite(c.AheadToleranceS) || c.AheadToleranceS < 0 {
		return core.Fieldf("telemetry_ahead_tolerance_s", "must be a finite number of at least 0, got %v", c.AheadToleranceS)
	}
	if !core.IsFinite(c.PressureUncertaintyM) || c.PressureUncertaintyM < 0 {
		return core.Fieldf("pressure_uncertainty_m", "must be a finite number of at least 0, got %v", c.PressureUncertaintyM)
	}
	if c.ClearAfterS <= 2*c.AheadToleranceS {
		return core.Fieldf("conformance_clear_after_s", "must be longer than twice the ahead tolerance")
	}
	return nil
}

// Input is one sample of one flight as the state machine admits it.
type Input struct {
	Sample Sample
	// Flying is true for an airborne (or emergency) status, false for a
	// ground status, nil when the status says neither (C-05).
	Flying *bool
	// RxAt is when the ingest received the sample (T-05).
	RxAt time.Time
	// Backlog is the ingest's verdict that the sample is history (T-04).
	Backlog bool
	// SourceDisabled is true when the sample's source is switched off.
	SourceDisabled bool
	// Auth is the flight's authorisation, nil when there is none to judge
	// against; MissingReason then says why (an Unknown* reason).
	Auth          *Authorisation
	MissingReason string
	// AuthErr is set when the flight's authorisation exists but does not
	// read as one (AuthorisationOf refused it): nothing is judged.
	AuthErr error
	// Cell5 is the sample's cell (the alert subject).
	Cell5 string
}

// Alert is one active alert.
type Alert struct {
	ID       string        `json:"alert_id"`
	Kind     string        `json:"kind"`
	Severity core.Severity `json:"severity"`
	FlightID string        `json:"flight_id"`
	IntentID string        `json:"intent_id,omitempty"`
	// AuthorisationNumber is the public authorisation number (Art.
	// 10(11)), empty when the flight has none.
	AuthorisationNumber string `json:"authorisation_number,omitempty"`
	// Cell5 is where the aircraft was last (the subject's cell).
	Cell5 string `json:"-"`
	// Detail holds the current numbers (C-08).
	Detail map[string]any `json:"detail"`
	// RaisedAt is when the condition was raised, UpdatedAt when its
	// numbers last changed, CapturedAt the triggering sample's placement.
	RaisedAt   time.Time `json:"raised_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	CapturedAt time.Time `json:"captured_at"`
	// PolicyVersion is the policy in force at the last update.
	PolicyVersion int64 `json:"policy_version"`
}

func (a *Alert) clone() Alert {
	c := *a
	c.Detail = maps.Clone(a.Detail)
	return c
}

// AlertEvent is a raise, an update or a clear.
type AlertEvent struct {
	State       string
	ClearReason string
	Alert       Alert
	// ClearingDetail is the numbers of the judgement that cleared a
	// resolved alert (C-14); nil for every other event.
	ClearingDetail map[string]any
}

// Transition is a change of the reported state.
type Transition struct {
	From, To State
	Reason   string
	At       time.Time
}

// Events is what one call did.
type Events struct {
	// Verdict is the judgement of the sample, nil when none ran.
	Verdict *Verdict
	// Admitted is false for a sample the admission refused (it judged
	// nothing and is recorded nowhere as judged); Refusal names why.
	Admitted bool
	Refusal  string
	// Unjudged names why an admitted sample was not judged (not_flying,
	// flying_unknown, authorisation_missing, judgement_failed).
	Unjudged    string
	Transitions []Transition
	Alerts      []AlertEvent
}

// Snapshot is a flight's conformance as it stands.
type Snapshot struct {
	FlightID, IntentID, AuthorisationNumber string
	State                                   State
	// BaseState is the judgement's state, without the link: conforming,
	// nonconforming, contingent or unknown.
	BaseState State
	Reason    string
	LinkLost  bool
	// Verdict is the last judgement, nil before the first.
	Verdict *Verdict
	// TimeOutsideS is how long the current run outside has lasted.
	TimeOutsideS float64
	// LastPosition and LastCell5 are of the last admitted sample;
	// LastSeen its placement (zero before any).
	LastPosition core.LatLon
	LastCell5    string
	LastSeen     time.Time
	LastFlying   bool
}

// Tracker is one flight's state machine.
type Tracker struct {
	FlightID            string
	IntentID            string
	AuthorisationNumber string
	Counters            *core.Counters

	base      State
	reason    string
	unknownBy string
	verdict   *Verdict
	lastOut   *Verdict // the last outside verdict, for a resolved clear

	authorised  bool // a sample was ever judged against an authorisation
	authMissing bool
	disabled    bool

	lastCaptured time.Time // the last admitted sample's placement
	lastLive     time.Time // the same, capped at the wall time
	hasLive      bool
	lastFlying   bool
	lastPos      core.LatLon
	lastCell5    string

	linkLost     bool
	lostAt       time.Time
	outsideSince time.Time
	lastOutside  time.Time
	ncSince      time.Time

	nc, ll   *Alert
	ncReason string // the reason the active nonconformance was raised with
}

// NewTracker is the state machine of one flight, unknown until its
// first judgement.
func NewTracker(flightID, intentID, authNo string, counters *core.Counters) *Tracker {
	if counters == nil {
		counters = &core.Counters{}
	}
	return &Tracker{FlightID: flightID, IntentID: intentID, AuthorisationNumber: authNo, Counters: counters,
		base: StateUnknown, unknownBy: UnknownNotJudged}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// reported is the state a reader sees.
func (t *Tracker) reported() State {
	switch {
	case t.disabled:
		return StateUnknown
	case t.base == StateContingent:
		return StateContingent
	case t.linkLost:
		return StateLostLink
	case t.authMissing && t.base == StateUnknown:
		return StateUnknown
	}
	return t.base
}

func (t *Tracker) reportedReason() string {
	switch s := t.reported(); {
	case t.disabled:
		return UnknownSourceDisabled
	case s == StateLostLink:
		return ReasonTelemetryLost
	case s == StateUnknown:
		return t.unknownBy
	case s == StateConforming:
		return ""
	}
	return t.reason
}

// change runs fn and records a transition when the reported state or
// its reason moved.
func (t *Tracker) change(ev *Events, at time.Time, fn func()) {
	from, fromReason := t.reported(), t.reportedReason()
	fn()
	if to, reason := t.reported(), t.reportedReason(); to != from || reason != fromReason {
		ev.Transitions = append(ev.Transitions, Transition{From: from, To: to, Reason: reason, At: at})
		t.Counters.Inc(CounterTransitions)
	}
}

// Snapshot is the flight's conformance now (wall for the time outside).
func (t *Tracker) Snapshot() Snapshot {
	s := Snapshot{
		FlightID: t.FlightID, IntentID: t.IntentID, AuthorisationNumber: t.AuthorisationNumber,
		State: t.reported(), BaseState: t.base, Reason: t.reportedReason(), LinkLost: t.linkLost,
		LastPosition: t.lastPos, LastCell5: t.lastCell5, LastSeen: t.lastLive, LastFlying: t.lastFlying,
	}
	if t.verdict != nil {
		v := *t.verdict
		s.Verdict = &v
	}
	if !t.outsideSince.IsZero() {
		s.TimeOutsideS = t.lastOutside.Sub(t.outsideSince).Seconds()
	}
	return s
}

// Active are the flight's active alerts, nonconformance first.
func (t *Tracker) Active() []Alert {
	var out []Alert
	for _, a := range []*Alert{t.nc, t.ll} {
		if a != nil {
			out = append(out, a.clone())
		}
	}
	return out
}

// alertID is the id of an alert raised for key at raisedAt: the same
// raise always has the same id (a republish or a replay names one
// alert), a later raise of the same condition another. UUID-shaped
// (version 4 bits) because the alerts table keys on a UUID.
func alertID(key string, raisedAt time.Time) string {
	sum := sha256.Sum256([]byte(key + "|" + strconv.FormatInt(raisedAt.UnixNano(), 10)))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (t *Tracker) newAlert(kind string, at, captured time.Time, cfg Config, detail map[string]any) *Alert {
	return &Alert{
		ID: alertID(kind+"|"+t.FlightID, at), Kind: kind, Severity: core.SeverityCritical,
		FlightID: t.FlightID, IntentID: t.IntentID, AuthorisationNumber: t.AuthorisationNumber, Cell5: t.lastCell5,
		Detail: detail, RaisedAt: at, UpdatedAt: at, CapturedAt: captured, PolicyVersion: cfg.PolicyVersion,
	}
}

func (t *Tracker) ncDetail(v Verdict, entry string) map[string]any {
	d := map[string]any{
		"reason": entry, "verdict_reason": v.Reason, "distance_outside_m": v.DistanceOutsideM,
		"height_over_m": v.HeightOverM, "time_outside_s": v.TimeOutsideS, "within_threshold": v.WithinThreshold,
		"vertical_known": v.VerticalKnown, "within_band": v.WithinBand, "volume": v.Volume,
		"state": string(t.reported()), "position": map[string]float64{"lat": t.lastPos.LatDeg, "lng": t.lastPos.LonDeg},
	}
	if !t.outsideSince.IsZero() {
		d["time_outside_run_s"] = t.lastOutside.Sub(t.outsideSince).Seconds()
	}
	return d
}

func clearEvent(a *Alert, reason string, at time.Time, clearing map[string]any) AlertEvent {
	c := a.clone()
	c.UpdatedAt = at
	return AlertEvent{State: AlertCleared, ClearReason: reason, Alert: c, ClearingDetail: clearing}
}

// clearAll clears every active alert with reason.
func (t *Tracker) clearAll(ev *Events, reason string, at time.Time) {
	for _, p := range []**Alert{&t.nc, &t.ll} {
		if *p != nil {
			ev.Alerts = append(ev.Alerts, clearEvent(*p, reason, at, nil))
			*p = nil
		}
	}
}

// Observe admits one sample at wall time wall, judges it when it can and
// runs the state machine. cfg must validate; one that does not judges
// nothing and is counted (E-15).
func (t *Tracker) Observe(in Input, cfg Config, wall time.Time) Events {
	var ev Events
	if err := cfg.Validate(); err != nil {
		t.Counters.Inc(CounterConfigInvalid)
		ev.Refusal = "config_invalid"
		return ev
	}
	s := in.Sample
	refuse := func(counter, why string) Events {
		t.Counters.Inc(counter)
		ev.Refusal = why
		return ev
	}
	ahead := secs(cfg.AheadToleranceS)
	switch {
	case s.CapturedAt.IsZero() || in.RxAt.IsZero() || wall.IsZero() || !s.Position.Valid():
		return refuse(CounterRejectedInvalid, "invalid")
	case in.Backlog:
		return refuse(CounterRejectedBacklog, "backlog")
	case in.SourceDisabled:
		t.Counters.Inc(CounterRejectedSourceDisabled)
		ev.Refusal = "source_disabled"
		t.disable(&ev, wall)
		return ev
	case wall.Sub(in.RxAt) > secs(cfg.LiveMaxAgeS):
		return refuse(CounterRejectedLate, "late")
	case s.CapturedAt.Sub(in.RxAt) > ahead || s.CapturedAt.Sub(wall) > ahead || in.RxAt.Sub(wall) > ahead:
		return refuse(CounterRejectedPlacedAhead, "placed_ahead")
	case !t.lastCaptured.IsZero() && !s.CapturedAt.After(t.lastCaptured):
		return refuse(CounterRejectedOutOfOrder, "out_of_order")
	}
	ev.Admitted = true
	placed := s.CapturedAt
	if placed.After(wall) {
		placed = wall
	}
	prevLive := t.lastLive
	t.lastCaptured, t.lastLive, t.hasLive = s.CapturedAt, placed, true
	t.lastPos, t.lastCell5 = s.Position, in.Cell5
	if in.Flying != nil {
		// An undeclared status says nothing: the last known one holds
		// (C-05), so a flight seen airborne still loses its link.
		t.lastFlying = *in.Flying
	}
	t.change(&ev, placed, func() {
		t.disabled = false
		if t.linkLost {
			// The link is back: lost_link clears on this live sample; the
			// silence was not shown inside, so the hysteresis back to
			// conforming counts from here.
			t.linkLost = false
			if t.ll != nil {
				ev.Alerts = append(ev.Alerts, clearEvent(t.ll, ClearResolved, placed, map[string]any{
					"clearing_at": placed, "silence_s": placed.Sub(prevLive).Seconds(),
				}))
				t.ll = nil
			}
			if t.base != StateConforming {
				t.lastOutside = placed
			}
		}
	})
	switch {
	case in.Flying == nil:
		t.Counters.Inc(CounterFlyingUnknown)
		ev.Unjudged = "flying_unknown"
		return ev
	case !*in.Flying:
		t.Counters.Inc(CounterNotFlying)
		ev.Unjudged = "not_flying"
		return ev
	case in.AuthErr != nil:
		t.Counters.Inc(CounterJudgementFailed)
		ev.Unjudged = "judgement_failed"
		t.change(&ev, placed, func() {
			if t.base == StateUnknown {
				t.unknownBy = UnknownAuthorisationInvalid
			}
		})
		return ev
	case in.Auth == nil:
		t.Counters.Inc(CounterAuthorisationMissing)
		ev.Unjudged = "authorisation_missing"
		t.change(&ev, placed, func() {
			t.authMissing = true
			if t.base == StateUnknown {
				t.unknownBy = in.MissingReason
				if t.unknownBy == "" {
					t.unknownBy = UnknownAuthorisationMissing
				}
			}
		})
		return ev
	}
	v, err := Judge(s, *in.Auth, Policy{PressureUncertaintyM: cfg.PressureUncertaintyM})
	if err != nil {
		// C-09: a judgement that did not run neither refreshes nor clears.
		t.Counters.Inc(CounterJudgementFailed)
		ev.Unjudged = "judgement_failed"
		return ev
	}
	t.Counters.Inc(CounterJudged)
	if !v.VerticalKnown {
		t.Counters.Inc(CounterVerticalNotEvaluated)
	}
	t.authorised, t.authMissing = true, false
	if in.Auth.AuthorisationNumber != "" {
		t.AuthorisationNumber = in.Auth.AuthorisationNumber
	}
	ev.Verdict = &v
	t.verdict = &v
	t.apply(&ev, v, placed, wall, cfg, in.Auth.Thresholds)
	t.contingent(&ev, wall)
	return ev
}

// apply runs the state machine on one verdict placed at placed.
func (t *Tracker) apply(ev *Events, v Verdict, placed, wall time.Time, cfg Config, th Thresholds) {
	switch v.Outcome {
	case Inside:
		t.outsideSince = time.Time{}
		t.change(ev, placed, func() {
			if t.base == StateUnknown {
				t.base, t.reason = StateConforming, ""
			}
			if t.base == StateConforming || placed.Sub(t.lastOutside) <= secs(cfg.ClearAfterS) {
				return
			}
			if t.nc != nil {
				ev.Alerts = append(ev.Alerts, clearEvent(t.nc, ClearResolved, placed, map[string]any{
					"clearing_distance_outside_m": v.DistanceOutsideM, "clearing_height_over_m": v.HeightOverM,
					"clearing_time_outside_s": v.TimeOutsideS, "clearing_vertical_known": v.VerticalKnown,
					"clearing_within_band": v.WithinBand, "clearing_at": placed,
				}))
				t.nc = nil
			}
			if t.base == StateNonconforming {
				t.base, t.reason, t.ncSince = StateConforming, "", time.Time{}
			}
		})
	case Undetermined:
		// The horizontal and the window ran and hold it; the vertical did
		// not. It neither refreshes nor clears (C-09); a flight with no
		// complete judgement yet stays unknown, never conforming by
		// default (SC-22, E-15).
		t.change(ev, placed, func() {
			if t.base == StateUnknown {
				t.unknownBy = UnknownVerticalNotEvaluated
			}
		})
	case Outside:
		t.lastOutside = placed
		if t.outsideSince.IsZero() {
			t.outsideSince = placed
		}
		vv := v
		t.lastOut = &vv
		persisted := placed.Sub(t.outsideSince) > secs(th.TS)
		enter := !v.WithinThreshold || persisted
		reason := v.Reason
		if !v.WithinThreshold {
			reason = ReasonThresholdExceeded
		}
		t.change(ev, placed, func() {
			if t.base == StateUnknown {
				// Outside within the tolerance and not yet for t_s:
				// conforming (Art. 10(2)(d)).
				t.base = StateConforming
			}
			if t.base == StateConforming && enter {
				t.base, t.reason, t.ncSince = StateNonconforming, reason, wall
			}
		})
		switch {
		case t.base == StateConforming:
		case t.nc == nil && enter:
			// Raised once on entering (or again, outside after a resolved
			// clear while contingent or after a lost link).
			t.nc, t.ncReason = t.newAlert(KindNonconformance, placed, placed, cfg, t.ncDetail(v, reason)), reason
			ev.Alerts = append(ev.Alerts, AlertEvent{State: AlertRaised, Alert: t.nc.clone()})
		case t.nc != nil:
			// Refreshed silently with the current numbers (C-06, C-08).
			t.nc.Detail = t.ncDetail(v, t.ncReason)
			t.nc.UpdatedAt, t.nc.CapturedAt, t.nc.PolicyVersion, t.nc.Cell5 = placed, placed, cfg.PolicyVersion, t.lastCell5
		}
	}
}

// contingent moves a flight nonconforming for MaxRecoverableS without
// returning to contingent.
func (t *Tracker) contingent(ev *Events, wall time.Time) {
	if t.base != StateNonconforming || t.ncSince.IsZero() || wall.Sub(t.ncSince) < MaxRecoverableS*time.Second {
		return
	}
	t.change(ev, wall, func() { t.base = StateContingent })
	if t.nc != nil {
		t.nc.Detail["state"] = string(StateContingent)
		t.nc.UpdatedAt = wall
	}
}

// Disable is the switch-off of the flight's source seen without a
// sample (the ingest refuses a disabled source's samples, so the monitor
// learns it from the source-control state): every alert clears as
// source_disabled and the flight is unknown until a sample of an enabled
// source arrives. A flight already disabled changes nothing.
func (t *Tracker) Disable(wall time.Time) Events {
	var ev Events
	if !t.disabled {
		t.disable(&ev, wall)
	}
	return ev
}

// disable clears every alert as source_disabled and holds the flight
// unknown until a sample of an enabled source arrives (B-11): the
// judgement starts again from that sample, so a flight still outside is
// raised again.
func (t *Tracker) disable(ev *Events, wall time.Time) {
	t.change(ev, wall, func() {
		t.disabled, t.linkLost = true, false
		t.base, t.reason, t.unknownBy = StateUnknown, "", UnknownSourceDisabled
		t.ncSince, t.outsideSince, t.lastOutside = time.Time{}, time.Time{}, time.Time{}
		t.clearAll(ev, ClearSourceDisabled, wall)
	})
}

// Tick runs what the absence of samples decides at wall: lost_link after
// LostLinkS without a live sample from a flying aircraft with an
// authorisation, and contingent after MaxRecoverableS nonconforming.
// The lost_link alert is refreshed with the current silence (C-08).
func (t *Tracker) Tick(cfg Config, wall time.Time) Events {
	var ev Events
	if err := cfg.Validate(); err != nil {
		t.Counters.Inc(CounterConfigInvalid)
		return ev
	}
	if t.linkLost && t.ll != nil {
		t.ll.Detail["silence_s"] = wall.Sub(t.lastLive).Seconds()
		t.ll.UpdatedAt, t.ll.PolicyVersion = wall, cfg.PolicyVersion
		t.lastOutside = wall
	}
	if !t.disabled && t.authorised && t.hasLive && t.lastFlying && !t.linkLost && wall.Sub(t.lastLive) >= secs(cfg.LostLinkS) {
		t.change(&ev, wall, func() {
			t.linkLost, t.lostAt = true, wall
			t.lastOutside = wall
			if t.base == StateConforming || t.base == StateUnknown {
				t.base, t.reason, t.ncSince = StateNonconforming, ReasonTelemetryLost, wall
			}
			t.outsideSince = time.Time{}
			t.ll = t.newAlert(KindLostLink, wall, t.lastLive, cfg, map[string]any{
				"silence_s": wall.Sub(t.lastLive).Seconds(), "lost_link_s": cfg.LostLinkS, "last_captured_at": t.lastLive,
				"position": map[string]float64{"lat": t.lastPos.LatDeg, "lng": t.lastPos.LonDeg},
			})
			ev.Alerts = append(ev.Alerts, AlertEvent{State: AlertRaised, Alert: t.ll.clone()})
		})
	}
	t.contingent(&ev, wall)
	return ev
}

// Drop ends the flight: every active alert clears with reason
// (flight_ended), never resolved, which only a judgement gives.
func (t *Tracker) Drop(reason string, wall time.Time) Events {
	var ev Events
	if reason == "" || reason == ClearResolved {
		reason = ClearFlightEnded
	}
	t.clearAll(&ev, reason, wall)
	return ev
}
