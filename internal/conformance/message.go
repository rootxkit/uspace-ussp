package conformance

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Schemas of the messages the monitor publishes for conformance.
const (
	// SchemaState is conformance/state/v1 on conf.v1.<flight_id>: one per
	// transition (a sample's or a tick's), one per tick for a link-lost
	// flight, and otherwise a heartbeat of at most one per 10 s per
	// flight (monitor.DefaultConfHeartbeat), never one per sample.
	// tsdb-writer records each in conformance_samples; api
	// records the transitions in conformance_states and moves the intent
	// (internal subject; NATS never crosses a system, PLAN §7).
	SchemaState = "conformance/state/v1"
	// SchemaAlert is alert/v1 (spec 04 §3.3) on alrt.v1.<kind>.<cell5>.<id>.
	// WP-11 owns the schema file and the alerts records; until it merges
	// this package writes the fields 04 §3.3 names.
	SchemaAlert = "alert/v1"
	// Producer is the envelope producer of the monitor.
	Producer = "ussp/monitor"
)

// MaxMessageBytes bounds a conformance message read (E-10).
const MaxMessageBytes = 64 << 10

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Point is a WGS84 position as F3548 and the console write one.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// StateBody is conformance/state/v1.
type StateBody struct {
	FlightID            string  `json:"flight_id"`
	IntentID            *string `json:"intent_id"`
	AuthorisationNumber *string `json:"authorisation_number"`
	State               State   `json:"state"`
	BaseState           State   `json:"base_state"`
	Reason              *string `json:"reason"`
	// Transition is true when this message records a change of state;
	// PreviousState is then the state before it.
	Transition    bool   `json:"transition"`
	PreviousState *State `json:"previous_state"`
	// Judged is true when a judgement ran for this message's sample;
	// Unjudged names why one did not (not_flying, flying_unknown,
	// authorisation_missing, judgement_failed, tick).
	Judged           bool     `json:"judged"`
	Unjudged         *string  `json:"unjudged"`
	Outcome          *Outcome `json:"outcome"`
	VerdictReason    *string  `json:"verdict_reason"`
	DistanceOutsideM *float64 `json:"distance_outside_m"`
	HeightOverM      *float64 `json:"height_over_m"`
	TimeOutsideS     *float64 `json:"time_outside_s"`
	WithinThreshold  *bool    `json:"within_threshold"`
	// VerticalKnown is false when the vertical judgement did not run
	// (SC-22: "vertical not evaluated" is visible on every state).
	VerticalKnown *bool  `json:"vertical_known"`
	WithinBand    *bool  `json:"within_band"`
	LinkLost      bool   `json:"link_lost"`
	Position      *Point `json:"position"`
	PolicyVersion int64  `json:"policy_version"`
}

// StateMessage is one conformance/state/v1 message.
type StateMessage struct {
	bus.Envelope
	Body StateBody `json:"body"`
}

// AlertBody is alert/v1 (spec 04 §3.3).
type AlertBody struct {
	AlertID             string         `json:"alert_id"`
	Kind                string         `json:"kind"`
	Severity            core.Severity  `json:"severity"`
	State               string         `json:"state"`
	ClearReason         *string        `json:"clear_reason"`
	FlightID            string         `json:"flight_id"`
	IntentID            *string        `json:"intent_id"`
	AuthorisationNumber *string        `json:"authorisation_number"`
	CapturedAt          time.Time      `json:"captured_at"`
	RaisedAt            time.Time      `json:"raised_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	PolicyVersion       int64          `json:"policy_version"`
	Detail              map[string]any `json:"detail"`
	ClearingDetail      map[string]any `json:"clearing_detail,omitempty"`
}

// AlertMessage is one alert/v1 message.
type AlertMessage struct {
	bus.Envelope
	Body AlertBody `json:"body"`
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// StateMessageOf is the conformance/state/v1 message of a snapshot after
// ev, with the envelope times of the sample (or the system's for a tick).
func StateMessageOf(s Snapshot, ev Events, times core.Times, policyVersion int64) *StateMessage {
	b := StateBody{
		FlightID: s.FlightID, IntentID: optString(s.IntentID), AuthorisationNumber: optString(s.AuthorisationNumber),
		State: s.State, BaseState: s.BaseState, Reason: optString(s.Reason), LinkLost: s.LinkLost,
		PolicyVersion: policyVersion, Unjudged: optString(ev.Unjudged),
	}
	if n := len(ev.Transitions); n > 0 {
		b.Transition = true
		prev := ev.Transitions[0].From
		b.PreviousState = &prev
	}
	if s.LastPosition.Valid() && !s.LastSeen.IsZero() {
		b.Position = &Point{Lat: s.LastPosition.LatDeg, Lng: s.LastPosition.LonDeg}
	}
	if v := ev.Verdict; v != nil {
		b.Judged = true
		o, d, h, wt, vk, wb := v.Outcome, v.DistanceOutsideM, v.HeightOverM, v.WithinThreshold, v.VerticalKnown, v.WithinBand
		run := s.TimeOutsideS
		b.Outcome, b.DistanceOutsideM, b.HeightOverM, b.WithinThreshold, b.VerticalKnown, b.WithinBand = &o, &d, &h, &wt, &vk, &wb
		b.VerdictReason, b.TimeOutsideS = optString(v.Reason), &run
		if v.Outcome == Undetermined || !v.VerticalKnown {
			b.HeightOverM = nil // not evaluated, never 0 by default
		}
	}
	return &StateMessage{Envelope: bus.NewEnvelope(SchemaState, Producer, times), Body: b}
}

// AlertMessageOf is the alert/v1 message of one event at now.
func AlertMessageOf(e AlertEvent, now time.Time) *AlertMessage {
	a := e.Alert
	b := AlertBody{
		AlertID: a.ID, Kind: a.Kind, Severity: a.Severity, State: e.State, ClearReason: optString(e.ClearReason),
		FlightID: a.FlightID, IntentID: optString(a.IntentID), AuthorisationNumber: optString(a.AuthorisationNumber),
		CapturedAt: a.CapturedAt.UTC(), RaisedAt: a.RaisedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(), PolicyVersion: a.PolicyVersion,
		Detail: a.Detail, ClearingDetail: e.ClearingDetail,
	}
	if b.Detail == nil {
		b.Detail = map[string]any{}
	}
	return &AlertMessage{Envelope: bus.SystemEnvelope(SchemaAlert, Producer, now), Body: b}
}

var states = map[State]bool{StateConforming: true, StateNonconforming: true, StateContingent: true, StateLostLink: true, StateUnknown: true}

// DecodeState reads one conformance/state/v1 message: the envelope, the
// schema, a UUID flight (and intent when present), known states and
// finite numbers. Anything else is an error naming the field; it never
// panics.
func DecodeState(data []byte) (StateMessage, error) {
	var m StateMessage
	if len(data) > MaxMessageBytes {
		return m, core.Fieldf("message", "longer than %d bytes", MaxMessageBytes)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return StateMessage{}, core.Fieldf("message", "not a conformance/state/v1 message")
	}
	b := &m.Body
	switch {
	case m.Schema != SchemaState:
		return StateMessage{}, core.Fieldf("schema", "%q where %q is expected", m.Schema, SchemaState)
	case !uuidRe.MatchString(b.FlightID):
		return StateMessage{}, core.Fieldf("body.flight_id", "not a version 4 UUID")
	case b.IntentID != nil && !uuidRe.MatchString(*b.IntentID):
		return StateMessage{}, core.Fieldf("body.intent_id", "not a version 4 UUID")
	case !states[b.State] || !states[b.BaseState] || b.BaseState == StateLostLink:
		return StateMessage{}, core.Fieldf("body.state", "unknown state")
	case b.PreviousState != nil && !states[*b.PreviousState]:
		return StateMessage{}, core.Fieldf("body.previous_state", "unknown state")
	case b.Reason != nil && len(*b.Reason) > 64:
		return StateMessage{}, core.Fieldf("body.reason", "longer than 64 bytes")
	}
	for _, f := range []struct {
		name string
		v    *float64
	}{{"distance_outside_m", b.DistanceOutsideM}, {"height_over_m", b.HeightOverM}, {"time_outside_s", b.TimeOutsideS}} {
		if f.v != nil && (!core.IsFinite(*f.v) || *f.v < 0) {
			return StateMessage{}, core.Fieldf("body."+f.name, "not a finite number of at least 0")
		}
	}
	if err := m.Validate(); err != nil {
		return StateMessage{}, err
	}
	return m, nil
}

// DecodeTrack reads one trk.v1 message for the monitor. ours is false
// (and err nil) for a track that is not one of this USSP's operator
// flights (another trust, another source, no flight id); a message that
// does not read as track/telemetry/v1 is an error. Never panics.
func DecodeTrack(data []byte) (tr *telemetry.Track, ours bool, err error) {
	if len(data) > bus.TrackMsgBytes {
		return nil, false, core.Fieldf("message", "longer than %d bytes", bus.TrackMsgBytes)
	}
	var t telemetry.Track
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, false, core.Fieldf("message", "not a track/telemetry/v1 message")
	}
	if t.Schema != telemetry.SchemaTrack {
		return nil, false, core.Fieldf("schema", "%q where %q is expected", t.Schema, telemetry.SchemaTrack)
	}
	if err := t.Validate(); err != nil {
		return nil, false, err
	}
	b := &t.Body
	if b.Trust != core.TrustAuthenticated || b.Source != telemetry.SourceOperatorWS || b.FlightID == nil {
		return nil, false, nil
	}
	if !uuidRe.MatchString(*b.FlightID) {
		return nil, false, core.Fieldf("body.flight_id", "not a version 4 UUID")
	}
	if b.IntentID != nil && !uuidRe.MatchString(*b.IntentID) {
		return nil, false, core.Fieldf("body.intent_id", "not a version 4 UUID")
	}
	if !b.Position.LatLon().Valid() {
		return nil, false, core.Fieldf("body.position", "not a WGS84 position")
	}
	if b.AltAMSLM != nil && !core.IsFinite(*b.AltAMSLM) {
		return nil, false, core.Fieldf("body.alt_amsl_m", "not a finite number")
	}
	return &t, true, nil
}
