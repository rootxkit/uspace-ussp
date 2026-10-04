package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// ErrNotFound is an alert that does not exist or is not the caller's
// operator's (never told apart: 404 either way, as for intents).
var ErrNotFound = errors.New("alert not found")

// Stored is one alert as the record holds it, with api's facts.
type Stored struct {
	Body
	Cell5   string
	AckedBy *string
}

// FactStore is the part of the record the acknowledgement and the
// escalation write, on the database clock.
type FactStore interface {
	// Ack records the acknowledgement of alertID by clientID, when the
	// alert is one of the client's operator's flights' (ErrNotFound
	// otherwise); a repeat keeps the first.
	Ack(ctx context.Context, alertID, clientID string) (Stored, error)
	// AckForOperator records the acknowledgement of alertID by a portal
	// user (actor operator_user:<account id>, brief WP-17) of
	// operatorID, when the alert is one of that operator's flights'
	// (ErrNotFound otherwise); a repeat keeps the first.
	AckForOperator(ctx context.Context, alertID, operatorID, actor string) (Stored, error)
	// Escalate marks every critical alert open and unacknowledged
	// afterS after its raise as escalated, at most maxRows, and returns
	// them.
	Escalate(ctx context.Context, afterS float64, maxRows int) ([]Stored, error)
	// EndedNotices are the open restriction_activated alerts whose
	// intent is over (ended, or past its time_end), at most maxRows
	// (WP-12).
	EndedNotices(ctx context.Context, maxRows int) ([]Stored, error)
	// OpenNotices are the open restriction_activated alerts whose intent
	// is not over, oldest first, at most maxRows (WP-12).
	OpenNotices(ctx context.Context, maxRows int) ([]Stored, error)
}

// Publisher is the bus (bus.Publisher).
type Publisher interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// Counters of the service.
const (
	CounterAcked             = "alerts_acknowledged"
	CounterAckNotFound       = "alerts_ack_not_found"
	CounterEscalated         = "alerts_escalated"
	CounterEscalateFailed    = "alerts_escalation_failed"
	CounterRepublished       = "alerts_republished"
	CounterRepublishFailed   = "alerts_republish_failed"
	CounterRepublishNoCell   = "alerts_republish_without_cell"
	DefaultEscalateEvery     = 2 * time.Second
	DefaultEscalateMaxRows   = 500
	defaultRepublishDeadline = 3 * time.Second
)

// Service is api's handle on the record's own facts.
type Service struct {
	Store    FactStore
	Bus      Publisher
	Policy   func() policy.Values
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
	// Restrictions says whether the restriction a notice names is
	// lifted (cis.Evaluator); nil: a notice clears only when its intent
	// is over.
	Restrictions RestrictionLifter

	once sync.Once
}

// RestrictionLifter is the CIS cache's answer on one restriction
// (cis.Evaluator.RestrictionLifted): lifted when it is no longer in
// force, judged false when the cache cannot say (stale).
type RestrictionLifter interface {
	RestrictionLifted(id string) (lifted, judged bool)
}

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

func (s *Service) counters() *core.Counters {
	s.once.Do(func() {
		if s.Counters == nil {
			s.Counters = &core.Counters{}
		}
	})
	return s.Counters
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// AckResult is the answer of POST /v1/alerts/{alert_id}/ack.
type AckResult struct {
	AlertID string    `json:"alert_id"`
	AckedAt time.Time `json:"acked_at"`
	AckedBy string    `json:"acked_by"`
}

// Ack records the acknowledgement (committed), then republishes the
// alert with acked_at on alrt.v1 so traffic-ws stops repeating it. A
// republish that fails is counted and logged: the record holds the
// acknowledgement, and the next one carries it.
func (s *Service) Ack(ctx context.Context, alertID, clientID string) (AckResult, error) {
	if !uuidRe.MatchString(alertID) {
		s.counters().Inc(CounterAckNotFound)
		return AckResult{}, ErrNotFound
	}
	st, err := s.Store.Ack(ctx, alertID, clientID)
	return s.acked(ctx, st, clientID, err)
}

// AckForOperator is Ack by a portal user of operatorID (brief WP-17),
// recorded as acked by actor (operator_user:<account id>).
func (s *Service) AckForOperator(ctx context.Context, alertID, operatorID, actor string) (AckResult, error) {
	if !uuidRe.MatchString(alertID) {
		s.counters().Inc(CounterAckNotFound)
		return AckResult{}, ErrNotFound
	}
	st, err := s.Store.AckForOperator(ctx, alertID, operatorID, actor)
	return s.acked(ctx, st, actor, err)
}

// acked counts and republishes a recorded acknowledgement.
func (s *Service) acked(ctx context.Context, st Stored, by string, err error) (AckResult, error) {
	if errors.Is(err, ErrNotFound) {
		s.counters().Inc(CounterAckNotFound)
		return AckResult{}, err
	}
	if err != nil {
		return AckResult{}, err
	}
	s.counters().Inc(CounterAcked)
	s.republish(ctx, st)
	if st.AckedBy != nil {
		by = *st.AckedBy
	}
	return AckResult{AlertID: st.AlertID, AckedAt: st.AckedAt.UTC(), AckedBy: by}, nil
}

// Republish sends a stored alert on alrt.v1 after the transaction that
// changed it committed (the console's escalation, WP-18); a failure is
// counted and logged, and the next fact carries it.
func (s *Service) Republish(ctx context.Context, st Stored) { s.republish(ctx, st) }

// republish sends the stored alert on alrt.v1.<kind>.<cell5>.<id>.
func (s *Service) republish(ctx context.Context, st Stored) {
	if st.Cell5 == "" {
		s.counters().Inc(CounterRepublishNoCell)
		return
	}
	subject, err := bus.Alrt(st.Kind, st.Cell5, st.AlertID)
	if err == nil {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultRepublishDeadline)
		err = s.Bus.Publish(pctx, subject, &Message{Envelope: bus.SystemEnvelope(SchemaAlert, Producer, s.now()), Body: st.Body})
		cancel()
	}
	if err != nil {
		s.counters().Inc(CounterRepublishFailed)
		s.logger().LogAttrs(ctx, slog.LevelError, "alert fact recorded but not republished; the next fact carries it",
			slog.String("alert_id", st.AlertID), obs.Err(err))
		return
	}
	s.counters().Inc(CounterRepublished)
}

// Escalate runs one escalation pass: the critical alerts left
// unacknowledged escalation_after_s are marked (committed), then
// republished with escalated_at for the supervisor console.
func (s *Service) Escalate(ctx context.Context) (int, error) {
	after := policy.Defaults().EscalationAfterS
	if s.Policy != nil {
		after = s.Policy().EscalationAfterS
	}
	rows, err := s.Store.Escalate(ctx, after, DefaultEscalateMaxRows)
	if err != nil {
		s.counters().Inc(CounterEscalateFailed)
		return 0, err
	}
	for i := range rows {
		s.counters().Inc(CounterEscalated)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "critical alert unacknowledged: escalated to the supervisor console",
			slog.String("alert_id", rows[i].AlertID), slog.String("kind", rows[i].Kind), obs.FlightID(rows[i].FlightID),
			slog.Float64("escalation_after_s", after))
		s.republish(ctx, rows[i])
	}
	return len(rows), nil
}

// CounterNoticesCleared counts the restriction_activated alerts cleared
// once their intent was over.
const CounterNoticesCleared = "alerts_notices_cleared"

// ClearEndedNotices clears the restriction_activated alerts (WP-12)
// whose intent is over: ended, or past its time_end (a withdrawn
// intent's notice stands while the window it would have flown lasts).
// The clear (flight_ended: the authorised flight is over) is published
// on alrt.v1 and recorded from there like every alert; one that is not
// published is found again by the next pass.
func (s *Service) ClearEndedNotices(ctx context.Context) (int, error) {
	rows, err := s.Store.EndedNotices(ctx, DefaultEscalateMaxRows)
	if err != nil {
		return 0, err
	}
	now := s.now().UTC()
	reason := "flight_ended"
	for i := range rows {
		st := rows[i]
		st.State, st.ClearReason, st.UpdatedAt = StateCleared, &reason, now
		s.counters().Inc(CounterNoticesCleared)
		s.republish(ctx, st)
	}
	return len(rows), nil
}

// CounterNoticesLifted counts the restriction_activated alerts cleared
// once their restriction was lifted.
const CounterNoticesLifted = "alerts_notices_lifted"

// noticeCause is the part of a notice's detail that names its cause
// (intent.Notice).
type noticeCause struct {
	Cause         string `json:"cause"`
	Ref           string `json:"ref"`
	RestrictionID string `json:"restriction_id"`
}

// liftedRestriction is the restriction of a notice caused by one that
// is no longer in force ("" otherwise, and while the CIS cannot say).
func (s *Service) liftedRestriction(st Stored) string {
	if s.Restrictions == nil || st.Kind != KindRestrictionActivated {
		return ""
	}
	var c noticeCause
	if json.Unmarshal(st.Detail, &c) != nil || c.Cause != "restriction" {
		return ""
	}
	id := c.RestrictionID
	if id == "" {
		id = c.Ref
	}
	if id == "" {
		return ""
	}
	if lifted, judged := s.Restrictions.RestrictionLifted(id); lifted && judged {
		return id
	}
	return ""
}

// ClearLiftedNotices clears the open restriction_activated alerts
// (WP-12) whose restriction the ANSP deactivated: ended or cancelled, or
// gone from the CIS (resolved, with the restriction in clearing_detail).
// The intent keeps its state and its change_reason: a withdrawn
// authorisation stays withdrawn (the operator files anew), only the
// alert that the restriction is in force ends. A stale CIS clears
// nothing. The clear is published on alrt.v1 and recorded from there;
// one that is not recorded yet is published again by the next pass.
func (s *Service) ClearLiftedNotices(ctx context.Context) (int, error) {
	if s.Restrictions == nil {
		return 0, nil
	}
	rows, err := s.Store.OpenNotices(ctx, MaxNoticeRepublish)
	if err != nil {
		return 0, err
	}
	now := s.now().UTC()
	reason := "resolved"
	n := 0
	for i := range rows {
		st := rows[i]
		id := s.liftedRestriction(st)
		if id == "" {
			continue
		}
		detail, err := json.Marshal(map[string]string{"cause": "restriction_lifted", "restriction_id": id})
		if err != nil {
			return n, err
		}
		st.State, st.ClearReason, st.UpdatedAt, st.ClearingDetail = StateCleared, &reason, now, detail
		s.counters().Inc(CounterNoticesLifted)
		s.republish(ctx, st)
		n++
	}
	return n, nil
}

// Notice republishing (WP-12).
const (
	// CounterNoticesRepublished counts the open notices republished from
	// the record.
	CounterNoticesRepublished = "alerts_notices_republished"
	// CounterNoticeRepublishOverBound counts the passes that found more
	// open notices than MaxNoticeRepublish (the oldest are republished,
	// the rest at a later pass once some are cleared).
	CounterNoticeRepublishOverBound = "alerts_notices_republish_over_bound"
	// MaxNoticeRepublish bounds the notices one pass republishes (E-10).
	MaxNoticeRepublish = DefaultEscalateMaxRows
	// NoticeRepublishEvery is how often the open notices are republished:
	// the longest a traffic-ws that restarted goes without one.
	NoticeRepublishEvery = 10 * time.Second
)

// RepublishOpenNotices republishes, unchanged, the open
// restriction_activated alerts whose intent is not over (WP-12). The
// intent's projection publishes a notice once and no monitor republishes
// it, so without this a traffic-ws that restarted would never hold it
// again. A republish that fails is counted and comes again at the next
// pass.
func (s *Service) RepublishOpenNotices(ctx context.Context) (int, error) {
	rows, err := s.Store.OpenNotices(ctx, MaxNoticeRepublish+1)
	if err != nil {
		return 0, err
	}
	if len(rows) > MaxNoticeRepublish {
		s.counters().Inc(CounterNoticeRepublishOverBound)
		s.logger().LogAttrs(ctx, slog.LevelError, "more open notices than one pass republishes; the oldest are republished",
			slog.Int("bound", MaxNoticeRepublish))
		rows = rows[:MaxNoticeRepublish]
	}
	n := 0
	for i := range rows {
		if s.liftedRestriction(rows[i]) != "" {
			// Its clear is ClearLiftedNotices'; republishing it raised
			// would undo the clear at traffic-ws.
			continue
		}
		s.counters().Inc(CounterNoticesRepublished)
		s.republish(ctx, rows[i])
		n++
	}
	return n, nil
}

// RunEscalation escalates every period until ctx ends, clears the
// notices of intents that are over, and republishes the open ones every
// NoticeRepublishEvery (the first at the first period); a failed pass
// (the database down) is logged and tried at the next one.
func (s *Service) RunEscalation(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultEscalateEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var republished time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Escalate(ctx); err != nil && ctx.Err() == nil {
				s.logger().LogAttrs(ctx, slog.LevelError, "escalation pass failed; tried again at the next", obs.Err(err))
			}
			if _, err := s.ClearEndedNotices(ctx); err != nil && ctx.Err() == nil {
				s.logger().LogAttrs(ctx, slog.LevelError, "notice clearing pass failed; tried again at the next", obs.Err(err))
			}
			if _, err := s.ClearLiftedNotices(ctx); err != nil && ctx.Err() == nil {
				s.logger().LogAttrs(ctx, slog.LevelError, "lifted notice clearing pass failed; tried again at the next", obs.Err(err))
			}
			if now := s.now(); republished.IsZero() || now.Sub(republished) >= NoticeRepublishEvery {
				republished = now
				if _, err := s.RepublishOpenNotices(ctx); err != nil && ctx.Err() == nil {
					s.logger().LogAttrs(ctx, slog.LevelError, "notice republish pass failed; tried again at the next", obs.Err(err))
				}
			}
		}
	}
}
