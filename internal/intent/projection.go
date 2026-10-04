package intent

import (
	"context"
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Producer is the envelope producer of this package's messages.
const Producer = "ussp/api"

// StateBody is intent/state/v1 (spec 04 §3.5): what the hot path, the
// DSS sync and the console know of an intent, carried as the value of
// the KV bucket intent_active and as the body of intent.v1.<state>.<id>.
// Volumes are the F3548 volumes as authorised (W84, with the outlines
// conformance judges against), VolumesAMSL their bands in AMSL.
type StateBody struct {
	IntentID            string           `json:"intent_id"`
	Version             int              `json:"version"`
	LocalState          string           `json:"local_state"`
	DSSState            *string          `json:"dss_state"`
	Priority            int              `json:"priority"`
	ExemptArt13         bool             `json:"exempt_art_1_3"`
	AuthorisationNumber *string          `json:"authorisation_number"`
	OperatorReg         string           `json:"operator_reg"`
	UASSerial           string           `json:"uas_serial"`
	Volumes             []f3548.Volume4D `json:"volumes"`
	VolumesAMSL         []VolumeAMSL     `json:"volumes_amsl"`
	DeviationThresholds *Thresholds      `json:"deviation_thresholds"`
	FlightID            *string          `json:"flight_id"`
	CellSet             []string         `json:"cell_set"`
	TimeStart           time.Time        `json:"time_start"`
	TimeEnd             time.Time        `json:"time_end"`
	InUSpaceAirspace    bool             `json:"in_uspace_airspace"`
	PolicyVersion       int64            `json:"policy_version"`
	ChangeReason        *string          `json:"change_reason"`
	UpdatedAt           time.Time        `json:"updated_at"`
	// Category, ClassLabel and UARegistration are the Annex IV
	// declarations (items 4 and 10) the F3411 flight details carry
	// (WP-9); ClassLabel and UARegistration are null when not declared.
	Category       string  `json:"category,omitempty"`
	ClassLabel     *string `json:"class_label"`
	UARegistration *string `json:"ua_registration"`
	// OperatorID and ClientID are the intent's operator account and the
	// client it was filed under (brief WP-17): traffic-ws admits a portal
	// session of that operator to the intent's streams and serves it as
	// that client. Optional within v1.
	OperatorID string `json:"operator_id,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
}

// StateMessage is the message of intent.v1.<state>.<id>: the envelope
// and the body.
type StateMessage struct {
	bus.Envelope
	Body StateBody `json:"body"`
}

// StateOf is r's intent/state/v1 body.
func StateOf(r *Record) StateBody {
	cells := r.Cells
	if cells == nil {
		cells = []string{}
	}
	return StateBody{
		IntentID: r.ID, Version: r.Version, LocalState: r.LocalState, DSSState: r.Decision.DSSState,
		Priority: r.Priority, ExemptArt13: r.Exempt, AuthorisationNumber: r.Decision.AuthorisationNumber,
		OperatorReg: r.Request.OperatorReg, UASSerial: r.Request.UASSerial,
		Volumes: r.Request.Volumes, VolumesAMSL: r.VolumesAMSL, DeviationThresholds: r.Decision.DeviationThresholds,
		CellSet: cells, TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, InUSpaceAirspace: r.Decision.InUSpaceAirspace,
		PolicyVersion: r.Decision.PolicyVersion, ChangeReason: r.Decision.ChangeReason, UpdatedAt: r.Decision.UpdatedAt,
		Category: r.Request.Category, ClassLabel: optional(r.Request.ClassLabel), UARegistration: optional(r.Request.UARegistration),
		OperatorID: r.OperatorID, ClientID: r.ClientID,
	}
}

// optional is nil for an empty string.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// KVWriter is the KV side of internal/bus.Projector.
type KVWriter interface {
	PutJSON(ctx context.Context, bucket, key string, v any) error
	Delete(ctx context.Context, bucket, key string) error
}

// MessagePublisher is internal/bus.Publisher.
type MessagePublisher interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// BusProjector projects intents onto the bus: intent_active holds every
// intent in an active state under its id (a put) and nothing else (a
// delete), and every new version is published on
// intent.v1.<state>.<id>. Either failing is a *policy.ProjectionError
// (B-09: the transaction rolls back and the request answers 503).
type BusProjector struct {
	KV  KVWriter
	Pub MessagePublisher
	Now func() time.Time
	// Notices publishes the restriction_activated alert of a version a
	// re-check wrote (internal/geo); nil publishes none.
	Notices NoticePublisher
}

// NoticePublisher publishes the alert of a re-check's notice (brief
// WP-12): the operator is told before the intent leaves intent_active.
type NoticePublisher interface {
	PublishNotice(ctx context.Context, r *Record, n *Notice) error
}

// Project implements Projector. The version a re-check wrote first
// publishes its notice's alert (restriction_activated), then the
// projection: a failure of either leaves the version unprojected and
// the sweep publishes both again (the alert id is the notice's, so a
// repeat names the same alert).
func (p BusProjector) Project(ctx context.Context, r *Record) error {
	if n := NoticeOf(r); n != nil && n.Version == r.Version && p.Notices != nil {
		if err := p.Notices.PublishNotice(ctx, r, n); err != nil {
			return &policy.ProjectionError{Bucket: "alrt.v1", Err: err}
		}
	}
	body := StateOf(r)
	var err error
	if slices.Contains(ActiveStates, r.LocalState) {
		err = p.KV.PutJSON(ctx, bus.BucketIntentActive, r.ID, body)
	} else {
		err = p.KV.Delete(ctx, bus.BucketIntentActive, r.ID)
	}
	if err != nil {
		return &policy.ProjectionError{Bucket: bus.BucketIntentActive, Err: err}
	}
	subject, err := bus.Intent(r.LocalState, r.ID)
	if err != nil {
		return &policy.ProjectionError{Bucket: "intent.v1", Err: err}
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	m := &StateMessage{Envelope: bus.SystemEnvelope(SchemaState, Producer, now()), Body: body}
	if err := p.Pub.Publish(ctx, subject, m); err != nil {
		return &policy.ProjectionError{Bucket: "intent.v1", Err: err}
	}
	return nil
}
