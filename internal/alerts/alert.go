// Package alerts is the alerts record of api (docs/PLAN.md §4; brief
// WP-11; spec 02 F5, 04 §3.3): every alert/v1 the monitor publishes on
// alrt.v1 recorded by its alert_id (Recorder), the operator's
// acknowledgement (Service.Ack, POST /v1/alerts/{alert_id}/ack), the
// escalation of a critical alert left unacknowledged for
// escalation_after_s (Escalator), and the delivery record per client
// (traffic-ws's alert/delivery/v1). api never re-judges an alert: it
// records what the monitor decided (§3.2).
//
// A fact api adds (an acknowledgement, an escalation) is written with
// the database clock, and republished on alrt.v1 only after the
// transaction that recorded it committed, so traffic-ws stops repeating
// an acknowledged alert and the console sees an escalation.
package alerts

import (
	"encoding/json"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// Schemas this package reads and writes.
const (
	// SchemaAlert is alert/v1 (schemas/alert/v1).
	SchemaAlert = "alert/v1"
	// SchemaDelivery is alert/delivery/v1: traffic-ws's record that it
	// first sent an alert to a client, on alrt.v1.delivery.<cell5>.<alert_id>
	// (internal: NATS never crosses a system).
	SchemaDelivery = "alert/delivery/v1"
	// KindDelivery is the subject token of a delivery record.
	KindDelivery = "delivery"
	// Producer is the envelope producer of api's republishes.
	Producer = "ussp/api"
)

// The states of an alert (04 §3.3).
const (
	StateRaised  = "raised"
	StateUpdated = "updated"
	StateCleared = "cleared"
)

// MaxMessageBytes bounds an alert message read (the ALRT stream's own
// bound).
const MaxMessageBytes = 256 << 10

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var kinds = map[string]bool{
	"proximity": true, "nonconformance": true, "nonconformance_nearby": true, "height_exceedance": true,
	"zone_incursion": true, "lost_link": true, "restriction_activated": true, "emergency_nearby": true,
}

var severities = map[core.Severity]bool{core.SeverityInfo: true, core.SeverityWarning: true, core.SeverityCritical: true}

var states = map[string]bool{StateRaised: true, StateUpdated: true, StateCleared: true}

var clearReasons = map[string]bool{
	"resolved": true, "stale": true, "source_disabled": true, "landed": true, "flight_ended": true,
	"not_reconfirmed": true, "acknowledged_timeout": true,
}

// Body is alert/v1's body as every producer writes it.
type Body struct {
	AlertID             string          `json:"alert_id"`
	Kind                string          `json:"kind"`
	Severity            core.Severity   `json:"severity"`
	State               string          `json:"state"`
	ClearReason         *string         `json:"clear_reason"`
	FlightID            string          `json:"flight_id"`
	IntentID            *string         `json:"intent_id"`
	AuthorisationNumber *string         `json:"authorisation_number"`
	CapturedAt          time.Time       `json:"captured_at"`
	RaisedAt            time.Time       `json:"raised_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	PolicyVersion       int64           `json:"policy_version"`
	Detail              json.RawMessage `json:"detail"`
	ClearingDetail      json.RawMessage `json:"clearing_detail,omitempty"`
	AckedAt             *time.Time      `json:"acked_at,omitempty"`
	EscalatedAt         *time.Time      `json:"escalated_at,omitempty"`
}

// Message is one alert/v1 message.
type Message struct {
	bus.Envelope
	Body Body `json:"body"`
}

// Peer is the peer of a proximity alert's detail, when it has one.
func (b *Body) Peer() (trackID string, ok bool) {
	var d struct {
		Peer *struct {
			TrackID string `json:"track_id"`
		} `json:"peer"`
	}
	if json.Unmarshal(b.Detail, &d) != nil || d.Peer == nil || d.Peer.TrackID == "" {
		return "", false
	}
	return d.Peer.TrackID, true
}

// Decode reads one alert/v1 message: the envelope, the schema, UUID
// ids, known kind, severity, state and clear reason, a clear reason
// exactly on a clear, an object detail and bounded strings. Anything
// else is an error naming the field; it never panics.
func Decode(data []byte) (Message, error) {
	var m Message
	if len(data) > MaxMessageBytes {
		return m, core.Fieldf("message", "longer than %d bytes", MaxMessageBytes)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, core.Fieldf("message", "not an alert/v1 message")
	}
	if m.Schema != SchemaAlert {
		return Message{}, core.Fieldf("schema", "%q where %q is expected", m.Schema, SchemaAlert)
	}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	b := &m.Body
	switch {
	case !uuidRe.MatchString(b.AlertID):
		return Message{}, core.Fieldf("body.alert_id", "not a version 4 UUID")
	case !kinds[b.Kind]:
		return Message{}, core.Fieldf("body.kind", "unknown kind")
	case !severities[b.Severity]:
		return Message{}, core.Fieldf("body.severity", "unknown severity")
	case !states[b.State]:
		return Message{}, core.Fieldf("body.state", "unknown state")
	case (b.State == StateCleared) != (b.ClearReason != nil):
		return Message{}, core.Fieldf("body.clear_reason", "set exactly on a clear")
	case b.ClearReason != nil && !clearReasons[*b.ClearReason]:
		return Message{}, core.Fieldf("body.clear_reason", "unknown clear reason")
	case !uuidRe.MatchString(b.FlightID):
		return Message{}, core.Fieldf("body.flight_id", "not a version 4 UUID")
	case b.IntentID != nil && !uuidRe.MatchString(*b.IntentID):
		return Message{}, core.Fieldf("body.intent_id", "not a version 4 UUID")
	case b.AuthorisationNumber != nil && (len(*b.AuthorisationNumber) > 64 || !utf8.ValidString(*b.AuthorisationNumber)):
		return Message{}, core.Fieldf("body.authorisation_number", "longer than 64 bytes")
	case b.RaisedAt.IsZero() || b.UpdatedAt.IsZero() || b.CapturedAt.IsZero():
		return Message{}, core.Fieldf("body.raised_at", "a time is missing")
	case b.PolicyVersion < 0:
		return Message{}, core.Fieldf("body.policy_version", "negative")
	case !isObject(b.Detail):
		return Message{}, core.Fieldf("body.detail", "not an object")
	case len(b.ClearingDetail) > 0 && !isObject(b.ClearingDetail) && string(b.ClearingDetail) != "null":
		return Message{}, core.Fieldf("body.clearing_detail", "not an object")
	}
	return m, nil
}

func isObject(raw json.RawMessage) bool {
	var v map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &v) == nil && v != nil
}

// DeliveryBody is alert/delivery/v1: traffic-ws first sent alert_id to
// client_id at sent_at.
type DeliveryBody struct {
	AlertID  string    `json:"alert_id"`
	ClientID string    `json:"client_id"`
	SentAt   time.Time `json:"sent_at"`
}

// Delivery is one alert/delivery/v1 message.
type Delivery struct {
	bus.Envelope
	Body DeliveryBody `json:"body"`
}

// DecodeDelivery reads one alert/delivery/v1 message; never panics.
func DecodeDelivery(data []byte) (Delivery, error) {
	var d Delivery
	if len(data) > MaxMessageBytes {
		return d, core.Fieldf("message", "longer than %d bytes", MaxMessageBytes)
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return Delivery{}, core.Fieldf("message", "not an alert/delivery/v1 message")
	}
	switch {
	case d.Schema != SchemaDelivery:
		return Delivery{}, core.Fieldf("schema", "%q where %q is expected", d.Schema, SchemaDelivery)
	case !uuidRe.MatchString(d.Body.AlertID):
		return Delivery{}, core.Fieldf("body.alert_id", "not a version 4 UUID")
	case d.Body.ClientID == "" || len(d.Body.ClientID) > bus.MaxTokenBytes || !utf8.ValidString(d.Body.ClientID):
		return Delivery{}, core.Fieldf("body.client_id", "empty or too long")
	case d.Body.SentAt.IsZero():
		return Delivery{}, core.Fieldf("body.sent_at", "missing")
	}
	if err := d.Validate(); err != nil {
		return Delivery{}, err
	}
	return d, nil
}

// SchemaOf is the schema a message on alrt.v1 names, "" when it names
// none (the dispatch of alrt.v1's consumers, M29).
func SchemaOf(data []byte) string {
	var probe struct {
		Schema string `json:"schema"`
	}
	if len(data) > MaxMessageBytes || json.Unmarshal(data, &probe) != nil {
		return ""
	}
	return probe.Schema
}
