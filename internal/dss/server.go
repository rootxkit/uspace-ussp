package dss

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	stdf3548 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3548"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Counter names of the F3548 USS endpoints.
const (
	CounterDetailsServed        = "uss_details_served"
	CounterDetailsNotFound      = "uss_details_not_found"
	CounterTelemetryServed      = "uss_telemetry_served"
	CounterTelemetryRefused     = "uss_telemetry_refused"
	CounterPeerNotification     = "uss_peer_notification"
	CounterPeerDeleted          = "uss_peer_intent_deleted"
	CounterPeerNotificationBad  = "uss_peer_notification_refused"
	CounterPeerNotOwnManager    = "uss_peer_notification_not_manager"
	CounterPeerOwnIgnored       = "uss_peer_notification_own_intent"
	CounterIndexBackwards       = "dss_notification_index_backwards"
	CounterIndexUnknown         = "dss_notification_unknown_subscription"
	CounterConflictReported     = "dss_conflict_reported"
	CounterPeerPrecedence       = "dss_peer_precedence"
	CounterConstraintNotified   = "uss_constraint_notification"
	CounterConstraintDeleted    = "uss_constraint_deleted"
	CounterConstraintBad        = "uss_constraint_notification_refused"
	CounterReportStored         = "uss_report_stored"
	CounterLogSetServed         = "uss_log_set_served"
	CounterPeerPublishFailed    = "peer_intent_publish_failed"
	CounterPeerConflictNotJudge = "dss_peer_conflict_not_judged"
)

// SchemaPeerIntent is the schema of the internal peer.intent.v1 message.
const SchemaPeerIntent = "peer/intent/v1"

// PeerIntentBody is peer/intent/v1: a peer's operational intent as its
// manager notified it (trust provider), or its deletion, for the console's
// view and the next decision (WP-12, WP-7).
type PeerIntentBody struct {
	EntityID   string                   `json:"entity_id"`
	Manager    string                   `json:"manager"`
	USSBaseURL string                   `json:"uss_base_url"`
	Trust      core.Trust               `json:"trust"`
	Deleted    bool                     `json:"deleted"`
	State      *string                  `json:"state"`
	Version    *int64                   `json:"version"`
	Priority   *int                     `json:"priority"`
	TimeStart  *time.Time               `json:"time_start"`
	TimeEnd    *time.Time               `json:"time_end"`
	ReceivedAt time.Time                `json:"received_at"`
	Details    *f3548.OperationalIntent `json:"operational_intent"`
}

// PeerPublisher publishes peer.intent.v1 (api's bus); nil publishes none.
type PeerPublisher interface {
	PublishPeerIntent(ctx context.Context, b PeerIntentBody) error
}

// Bounds of the endpoints.
const (
	// maxNotifiedSubscriptions bounds the subscriptions one notification
	// may name.
	maxNotifiedSubscriptions = 100
	// telemetryWindow is how old the newest sample served as an intent's
	// telemetry may be (rid-sp's window, F3411's 60 s).
	telemetryWindow = 60 * time.Second
	// telemetryNext is when a polling USS should ask again.
	telemetryNext = time.Second
	// recheckTimeout bounds the standing re-check a constraint
	// notification runs (CstrPublishedNotificationLatencySeconds, 5 s,
	// leaves room for the answer).
	recheckTimeout = 4 * time.Second
)

// Server is the F3548 USS endpoints api serves (docs/PLAN.md §6.2),
// replacing WP-3's 501s, behind stdapi.F3548Access.
type Server struct {
	Intents     Intents
	Store       Store
	Telemetry   Telemetry
	Constraints ConstraintRechecker
	Publisher   PeerPublisher
	// Manager and USSBaseURL are ours: a notification about our own
	// intent is not stored as a peer's.
	Manager    string
	USSBaseURL string
	Counters   *core.Counters
	Logger     *slog.Logger
	Now        func() time.Time
}

var _ stdf3548.StrictServerInterface = (*Server)(nil)

func (s *Server) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func (s *Server) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func msg(s string) *string { return &s }

// caller is the token's subject the guard admitted.
func caller(ctx context.Context) string {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		return p.Claims.Subject
	}
	return ""
}

func validUUID(s string) bool { return len(s) == 36 && uuidRE.MatchString(strings.ToLower(s)) }

// ours reads one of our intents the DSS holds; nil when there is none.
func (s *Server) ours(ctx context.Context, id string) (*intent.Record, *intent.DSSHeld, error) {
	if !validUUID(id) {
		return nil, nil, nil
	}
	r, err := s.Intents.Record(ctx, id)
	if err != nil || r == nil {
		return nil, nil, err
	}
	h, err := s.Intents.Held(ctx, id)
	if err != nil || h == nil {
		return nil, nil, err
	}
	return r, h, nil
}

// GetOperationalIntentDetails serves our intent as the DSS holds it
// (MaxRespondToOIDetailsRequestSeconds: one database read).
func (s *Server) GetOperationalIntentDetails(ctx context.Context, req stdf3548.GetOperationalIntentDetailsRequestObject) (stdf3548.GetOperationalIntentDetailsResponseObject, error) {
	r, h, err := s.ours(ctx, req.Entityid)
	if err != nil {
		return nil, err
	}
	if r == nil {
		s.count(CounterDetailsNotFound)
		return stdf3548.GetOperationalIntentDetails404JSONResponse{Message: msg("no operational intent " + req.Entityid + " is managed here")}, nil
	}
	s.count(CounterDetailsServed)
	return stdf3548.GetOperationalIntentDetails200JSONResponse{OperationalIntent: Details(r, h)}, nil
}

// GetOperationalIntentTelemetry serves the newest position of a
// Nonconforming or Contingent intent's flight from the time-series window
// (utm.conformance_monitoring_sa).
func (s *Server) GetOperationalIntentTelemetry(ctx context.Context, req stdf3548.GetOperationalIntentTelemetryRequestObject) (stdf3548.GetOperationalIntentTelemetryResponseObject, error) {
	r, h, err := s.ours(ctx, req.Entityid)
	if err != nil {
		return nil, err
	}
	switch {
	case r == nil:
		s.count(CounterTelemetryRefused)
		return stdf3548.GetOperationalIntentTelemetry404JSONResponse{Message: msg("no operational intent " + req.Entityid + " is managed here")}, nil
	case h.State != f3548.Nonconforming && h.State != f3548.Contingent:
		s.count(CounterTelemetryRefused)
		return stdf3548.GetOperationalIntentTelemetry409JSONResponse{Message: msg("the operational intent is " + string(h.State) + ", not off-nominal")}, nil
	case s.Telemetry == nil:
		s.count(CounterTelemetryRefused)
		return stdf3548.GetOperationalIntentTelemetry412JSONResponse{Message: msg("no telemetry store is configured")}, nil
	}
	now := s.now().UTC()
	t, ok, err := s.Telemetry.Latest(ctx, r.ID, now.Add(-telemetryWindow))
	if err != nil {
		return nil, err
	}
	if !ok {
		s.count(CounterTelemetryRefused)
		return stdf3548.GetOperationalIntentTelemetry412JSONResponse{Message: msg("no telemetry of the last 60 s")}, nil
	}
	next := f3548.Time{Format: f3548.RFC3339, Value: now.Add(telemetryNext)}
	s.count(CounterTelemetryServed)
	return stdf3548.GetOperationalIntentTelemetry200JSONResponse{OperationalIntentId: r.ID, Telemetry: t, NextTelemetryOpportunity: &next}, nil
}

// checkIndexes advances our subscriptions' notification indexes; an index
// going backwards is counted and the notification applied anyway (the
// DSS orders them, not us).
func (s *Server) checkIndexes(ctx context.Context, subs []f3548.SubscriptionState) {
	for _, st := range subs {
		prev, ours, err := s.Store.Notified(ctx, st.SubscriptionId, st.NotificationIndex)
		switch {
		case err != nil:
			s.logger().LogAttrs(ctx, slog.LevelWarn, "notification index not checked", slog.String("subscription_id", st.SubscriptionId), obs.Err(err))
		case !ours:
			s.count(CounterIndexUnknown)
		case st.NotificationIndex < prev:
			s.count(CounterIndexBackwards)
			s.logger().LogAttrs(ctx, slog.LevelWarn, "a notification index went backwards; applied anyway",
				slog.String("subscription_id", st.SubscriptionId), slog.Int("index", int(st.NotificationIndex)), slog.Int("previous", int(prev)))
		}
	}
}

func refuseNotification(reason string) stdf3548.NotifyOperationalIntentDetailsChanged400JSONResponse {
	return stdf3548.NotifyOperationalIntentDetailsChanged400JSONResponse{Message: msg(reason)}
}

// NotifyOperationalIntentDetailsChanged takes a peer's notification: the
// peer's intent stored by (entity id, version) as trust provider, or
// removed when deleted, published on peer.intent.v1, and judged against
// our active intents: one the peer takes precedence over by priority is
// re-checked (withdrawn or marked, Art. 10(10)); any other conflict is a
// conflict the DSS let through, audited with both references and ovns for
// the authority (spec 06 T9). A USS speaks only for its own intents: the
// manager named must be the token's subject.
func (s *Server) NotifyOperationalIntentDetailsChanged(ctx context.Context, req stdf3548.NotifyOperationalIntentDetailsChangedRequestObject) (stdf3548.NotifyOperationalIntentDetailsChangedResponseObject, error) {
	b := req.Body
	switch {
	case b == nil:
		s.count(CounterPeerNotificationBad)
		return refuseNotification("no body"), nil
	case !validUUID(b.OperationalIntentId):
		s.count(CounterPeerNotificationBad)
		return refuseNotification("operational_intent_id is not a UUID"), nil
	case len(b.Subscriptions) > maxNotifiedSubscriptions:
		s.count(CounterPeerNotificationBad)
		return refuseNotification("too many subscriptions"), nil
	}
	who := caller(ctx)
	s.checkIndexes(ctx, b.Subscriptions)
	if b.OperationalIntent == nil {
		gone, err := s.Store.DeletePeerIntent(ctx, b.OperationalIntentId, who)
		if err != nil {
			return nil, err
		}
		if gone {
			s.count(CounterPeerDeleted)
			s.publish(ctx, PeerIntentBody{EntityID: b.OperationalIntentId, Manager: who, Trust: core.TrustProvider, Deleted: true, ReceivedAt: s.now().UTC()})
		}
		return stdf3548.NotifyOperationalIntentDetailsChanged204Response{}, nil
	}
	raw, err := json.Marshal(b.OperationalIntent)
	if err != nil {
		return nil, err
	}
	oi, err := f3548.UnmarshalOperationalIntent(raw)
	switch {
	case err != nil:
		s.count(CounterPeerNotificationBad)
		return refuseNotification("operational_intent is not usable: " + err.Error()), nil //nolint:nilerr // the refusal is the answer (400)
	case oi.Reference.Id != b.OperationalIntentId:
		s.count(CounterPeerNotificationBad)
		return refuseNotification("operational_intent.reference.id is not operational_intent_id"), nil
	case oi.Reference.Manager == s.Manager || sameBase(oi.Reference.UssBaseUrl, s.USSBaseURL):
		s.count(CounterPeerOwnIgnored)
		return stdf3548.NotifyOperationalIntentDetailsChanged204Response{}, nil
	case oi.Reference.Manager != who:
		s.count(CounterPeerNotOwnManager)
		return stdf3548.NotifyOperationalIntentDetailsChanged403JSONResponse{Message: msg("a USS notifies only the operational intents it manages")}, nil
	}
	rec, err := peerRecordOf(oi, oi.Reference.UssBaseUrl)
	if err != nil {
		return nil, err
	}
	written, err := s.Store.UpsertPeerIntent(ctx, rec)
	if err != nil {
		return nil, err
	}
	s.count(CounterPeerNotification)
	if !written {
		// An older version than the one held: the newer stands.
		return stdf3548.NotifyOperationalIntentDetailsChanged204Response{}, nil
	}
	_, _ = s.Store.MarkPeerUnavailable(ctx, rec.USSBaseURL, false)
	st, v, pri := rec.State, rec.Version, rec.Priority
	ts, te := rec.TimeStart, rec.TimeEnd
	s.publish(ctx, PeerIntentBody{EntityID: rec.EntityID, Manager: rec.Manager, USSBaseURL: rec.USSBaseURL, Trust: core.TrustProvider,
		State: &st, Version: &v, Priority: &pri, TimeStart: &ts, TimeEnd: &te, ReceivedAt: s.now().UTC(), Details: oi})
	s.judgePeer(ctx, oi, rec)
	return stdf3548.NotifyOperationalIntentDetailsChanged204Response{}, nil
}

func (s *Server) publish(ctx context.Context, b PeerIntentBody) {
	if s.Publisher == nil {
		return
	}
	if err := s.Publisher.PublishPeerIntent(ctx, b); err != nil {
		s.count(CounterPeerPublishFailed)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "peer intent stored but not published", slog.String("peer_intent_id", b.EntityID), obs.Err(err))
	}
}

// judgePeer judges a peer's intent against our active ones.
func (s *Server) judgePeer(ctx context.Context, oi *f3548.OperationalIntent, rec PeerRecord) {
	if oi.Details.Volumes == nil || len(*oi.Details.Volumes) == 0 {
		return
	}
	switch oi.Reference.State {
	case f3548.Accepted, f3548.Activated, f3548.Nonconforming:
	case f3548.Contingent:
		// Its volumes are off-nominal only: nothing is judged on them.
		return
	default:
		return
	}
	cs, err := s.Intents.PeerConflicts(ctx, intent.PeerIntent{EntityID: rec.EntityID, Priority: rec.Priority, FetchedAt: s.now().UTC(), Volumes: *oi.Details.Volumes})
	if err != nil {
		s.count(CounterPeerConflictNotJudge)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "peer intent not judged against our intents", slog.String("peer_intent_id", rec.EntityID), obs.Err(err))
		return
	}
	seen := map[string]bool{}
	for _, c := range cs {
		if seen[c.IntentID] {
			continue
		}
		seen[c.IntentID] = true
		if c.PeerWinsByPriority {
			s.count(CounterPeerPrecedence)
			if _, err := s.Intents.PeerIntentDisplaced(ctx, c.IntentID, rec.EntityID); err != nil {
				s.logger().LogAttrs(ctx, slog.LevelWarn, "authorisation a peer takes precedence over not re-checked; the sweep runs it",
					slog.String("intent_id", c.IntentID), slog.String("peer_intent_id", rec.EntityID), obs.Err(err))
			}
			continue
		}
		s.count(CounterConflictReported)
		h, _ := s.Intents.Held(ctx, c.IntentID)
		payload := map[string]any{"peer_intent_id": rec.EntityID, "peer_manager": rec.Manager, "peer_uss_base_url": rec.USSBaseURL,
			"peer_ovn": rec.OVN, "peer_version": rec.Version, "peer_priority": rec.Priority,
			"overlap": map[string]float64{"h_m": c.Overlap.HM, "v_m": c.Overlap.VM, "t_s": c.Overlap.TS}}
		if h != nil {
			payload["ovn"], payload["dss_version"] = h.OVN, h.Version
		}
		if err := s.Store.Audit(ctx, store.Event{ActorType: store.ActorPeer, ActorID: rec.Manager, EntityType: "operational_intent",
			EntityID: c.IntentID, EventType: "dss_conflict_reported", Payload: payload}); err != nil {
			s.logger().LogAttrs(ctx, slog.LevelError, "a conflict the DSS let through was not audited", slog.String("intent_id", c.IntentID), obs.Err(err))
			continue
		}
		s.logger().LogAttrs(ctx, slog.LevelWarn, "a peer's intent conflicts with ours and the DSS let it through; reported to the authority",
			slog.String("intent_id", c.IntentID), slog.String("peer_intent_id", rec.EntityID))
	}
}

// GetConstraintDetails answers 404: this USSP manages no constraint.
func (s *Server) GetConstraintDetails(_ context.Context, req stdf3548.GetConstraintDetailsRequestObject) (stdf3548.GetConstraintDetailsResponseObject, error) {
	return stdf3548.GetConstraintDetails404JSONResponse{Message: msg("this USSP manages no constraints; " + req.Entityid + " is not here")}, nil
}

// NotifyConstraintDetailsChanged takes a constraint notification: the
// constraint stored (joined to the CIS restriction its geozone names), or
// removed when deleted, then the standing re-check of WP-12 over its
// volumes and window (restriction_activated).
func (s *Server) NotifyConstraintDetailsChanged(ctx context.Context, req stdf3548.NotifyConstraintDetailsChangedRequestObject) (stdf3548.NotifyConstraintDetailsChangedResponseObject, error) {
	b := req.Body
	refuse := func(reason string) (stdf3548.NotifyConstraintDetailsChangedResponseObject, error) {
		s.count(CounterConstraintBad)
		return stdf3548.NotifyConstraintDetailsChanged400JSONResponse{Message: msg(reason)}, nil
	}
	switch {
	case b == nil:
		return refuse("no body")
	case !validUUID(b.ConstraintId):
		return refuse("constraint_id is not a UUID")
	case len(b.Subscriptions) > maxNotifiedSubscriptions:
		return refuse("too many subscriptions")
	}
	who := caller(ctx)
	s.checkIndexes(ctx, b.Subscriptions)
	if b.Constraint == nil {
		gone, err := s.Store.DeleteConstraint(ctx, b.ConstraintId, who)
		if err != nil {
			return nil, err
		}
		if gone {
			s.count(CounterConstraintDeleted)
		}
		return stdf3548.NotifyConstraintDetailsChanged204Response{}, nil
	}
	c := b.Constraint
	switch {
	case c.Reference.Id != b.ConstraintId:
		return refuse("constraint.reference.id is not constraint_id")
	case c.Reference.Manager != who:
		s.count(CounterPeerNotOwnManager)
		return stdf3548.NotifyConstraintDetailsChanged403JSONResponse{Message: msg("a USS notifies only the constraints it manages")}, nil
	case len(c.Details.Volumes) == 0 || len(c.Details.Volumes) > 64:
		return refuse("constraint.details.volumes must hold 1 to 64 volumes")
	}
	boxes := make([]geodesy.BBox, 0, len(c.Details.Volumes))
	var from, to time.Time
	for i, v := range c.Details.Volumes {
		box, start, end, err := f3548.Volume4DToZonesEnvelope(v)
		if err != nil || start.IsZero() || end.IsZero() {
			return refuse("constraint.details.volumes cannot be bounded")
		}
		boxes = append(boxes, box)
		if i == 0 || start.Before(from) {
			from = start
		}
		if i == 0 || end.After(to) {
			to = end
		}
	}
	rec, err := constraintRecordOf(c, c.Reference.UssBaseUrl)
	if err != nil {
		return nil, err
	}
	if _, err := s.Store.UpsertConstraint(ctx, rec); err != nil {
		return nil, err
	}
	s.count(CounterConstraintNotified)
	if s.Constraints != nil {
		ref := rec.EntityID
		if rec.CISRestrictionID != "" {
			ref = rec.CISRestrictionID
		}
		rctx, cancel := context.WithTimeout(ctx, recheckTimeout)
		rs := s.Constraints.Constraint(rctx, ref, boxes, &from, &to)
		cancel()
		s.logger().LogAttrs(ctx, slog.LevelInfo, "constraint notification re-checked", slog.String("constraint_id", rec.EntityID),
			slog.String("ref", ref), slog.String("outcomes", intent.Summary(rs)))
	}
	return stdf3548.NotifyConstraintDetailsChanged204Response{}, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// MakeUssReport stores a peer's report with the id it is given.
func (s *Server) MakeUssReport(ctx context.Context, req stdf3548.MakeUssReportRequestObject) (stdf3548.MakeUssReportResponseObject, error) {
	if req.Body == nil {
		return stdf3548.MakeUssReport400JSONResponse{Message: msg("no body")}, nil
	}
	raw, err := json.Marshal(req.Body.Exchange)
	if err != nil {
		return nil, err
	}
	id := newUUID()
	if err := s.Store.InsertReport(ctx, id, caller(ctx), raw); err != nil {
		return nil, err
	}
	s.count(CounterReportStored)
	out := *req.Body
	out.ReportId = &id
	return stdf3548.MakeUssReport201JSONResponse(out), nil
}

// GetLogSet serves the exchange log of one entity (log_set_id is an
// operational intent's id): the exchanges recorded with it, oldest first,
// at most MaxLogSetMessages; an unknown id is an empty set.
func (s *Server) GetLogSet(ctx context.Context, req stdf3548.GetLogSetRequestObject) (stdf3548.GetLogSetResponseObject, error) {
	msgs := []f3548.ExchangeRecord{}
	if validUUID(req.LogSetId) {
		es, err := s.Store.Exchanges(ctx, strings.ToLower(req.LogSetId), MaxLogSetMessages)
		if err != nil {
			return nil, err
		}
		for i := range es {
			msgs = append(msgs, recordOf(es[i]))
		}
	}
	s.count(CounterLogSetServed)
	return stdf3548.GetLogSet200JSONResponse{Messages: &msgs}, nil
}

func recordOf(e Exchange) f3548.ExchangeRecord {
	r := f3548.ExchangeRecord{Method: e.Method, Url: e.URL, RecorderRole: f3548.ExchangeRecordRecorderRole(e.Role),
		RequestTime: f3548.Time{Format: f3548.RFC3339, Value: e.RequestTime.UTC()}}
	if e.RequestBody != "" {
		r.RequestBody = &e.RequestBody
	}
	if e.ResponseBody != "" {
		r.ResponseBody = &e.ResponseBody
	}
	if e.ResponseCode != 0 {
		c := int32(e.ResponseCode)
		r.ResponseCode = &c
	}
	if !e.ResponseTime.IsZero() {
		r.ResponseTime = &f3548.Time{Format: f3548.RFC3339, Value: e.ResponseTime.UTC()}
	}
	if e.Problem != "" {
		r.Problem = &e.Problem
	}
	return r
}
