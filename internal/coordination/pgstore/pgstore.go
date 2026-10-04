// Package pgstore is internal/coordination's Store on the relational
// database: coordination_checks and coordination_notices (migration
// 00018) and the two facts a notice writes on its conformance state,
// through the sqlc queries of
// internal/store/queries/relational/coordination.sql. Every queued
// notice and every outcome (received, acknowledged, escalated, failed)
// writes its events row in the same transaction. Its tests are the
// integration tests (test/integration).
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/coordination"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is coordination.Store on PostgreSQL.
type Store struct{ S *store.Store }

var _ coordination.Store = Store{}

// EntityNotice is the events entity of a notice.
const EntityNotice = "coordination_notice"

// maxErrorLen bounds a stored last_error.
const maxErrorLen = 1000

func clip(s string) *string {
	if len(s) > maxErrorLen {
		s = s[:maxErrorLen]
	}
	return &s
}

func secs(d time.Duration) float64 { return math.Max(d.Seconds(), 0) }

func n32(n int) int32 { return int32(min(max(n, 1), 10_000)) }

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func position(lat, lng *float64) *core.LatLon {
	if lat == nil || lng == nil {
		return nil
	}
	return &core.LatLon{LatDeg: *lat, LonDeg: *lng}
}

func audit(ctx context.Context, q *relational.Queries, event, ref, kind, intentID string, extra map[string]any) error {
	p := map[string]any{"notice_ref": ref, "kind": kind, "intent_id": intentID}
	for k, v := range extra {
		p[k] = v
	}
	_, err := store.Audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "coordination", EntityType: EntityNotice,
		EntityID: ref, EventType: event, Payload: p})
	return err
}

// Transitions implements coordination.Store.
func (p Store) Transitions(ctx context.Context, lookback time.Duration, n int) ([]coordination.Transition, error) {
	rows, err := p.S.Queries().CoordinationTransitions(ctx, relational.CoordinationTransitionsParams{LookbackS: secs(lookback), N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("coordination transitions: %w", err)
	}
	out := make([]coordination.Transition, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, coordination.Transition{
			Deviation: coordination.Deviation{StateID: r.ID, FlightID: store.UUIDText(r.FlightID), State: r.State, Reason: str(r.Reason),
				At: r.At, DistanceOutsideM: r.DistanceOutsideM, HeightOverM: r.HeightOverM, LastPosition: position(r.LastLatDeg, r.LastLngDeg)},
			Intent: coordination.Intent{ID: store.UUIDText(r.IntentID), AuthorisationNumber: str(r.AuthorisationNumber),
				LocalState: r.LocalState, DSSState: str(r.DssState), TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, Volumes: r.Volumes},
		})
	}
	return out, nil
}

// Candidates implements coordination.Store.
func (p Store) Candidates(ctx context.Context, lookback time.Duration, n int) ([]coordination.Candidate, error) {
	rows, err := p.S.Queries().CoordinationCandidates(ctx, relational.CoordinationCandidatesParams{LookbackS: secs(lookback), N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("coordination candidates: %w", err)
	}
	out := make([]coordination.Candidate, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, coordination.Candidate{
			Intent: coordination.Intent{ID: store.UUIDText(r.ID), AuthorisationNumber: str(r.AuthorisationNumber), LocalState: r.LocalState,
				DSSState: str(r.DssState), TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, Volumes: r.Volumes},
			AirspaceIDs: r.UspaceAirspaceIds, Ended: r.Ended,
		})
	}
	return out, nil
}

// Ended implements coordination.Store.
func (p Store) Ended(ctx context.Context, n int) ([]coordination.Intent, error) {
	rows, err := p.S.Queries().CoordinationEnded(ctx, n32(n))
	if err != nil {
		return nil, fmt.Errorf("coordination ended: %w", err)
	}
	out := make([]coordination.Intent, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, coordination.Intent{ID: store.UUIDText(r.ID), AuthorisationNumber: str(r.AuthorisationNumber), LocalState: r.LocalState,
			DSSState: str(r.DssState), TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, Volumes: r.Volumes})
	}
	return out, nil
}

// Enqueue implements coordination.Store. A check another replica wrote
// first leaves this one's notices to it (they carry the same refs).
func (p Store) Enqueue(ctx context.Context, intentID string, check *coordination.Check, ns []coordination.Notice) (int, error) {
	iid, err := store.UUID("intent_id", intentID)
	if err != nil {
		return 0, err
	}
	queued := 0
	err = p.S.Tx(ctx, func(q *relational.Queries) error {
		queued = 0
		if check != nil {
			var v *string
			if check.CISVersion != "" {
				v = &check.CISVersion
			}
			k, err := q.CoordinationInsertCheck(ctx, relational.CoordinationInsertCheckParams{IntentID: iid, Controlled: check.Controlled,
				AirspaceIds: nonNil(check.AirspaceIDs), UnstatedIds: nonNil(check.UnstatedIDs), CisVersion: v})
			if err != nil {
				return fmt.Errorf("coordination check: %w", err)
			}
			if k == 0 {
				return nil
			}
		}
		for _, n := range ns {
			params := relational.CoordinationInsertNoticeParams{NoticeRef: n.Ref, Kind: string(n.Kind), IntentID: iid,
				AckRequired: coordination.AckRequired(n.Kind), Payload: []byte("{}"), Body: []byte{}}
			if n.FlightID != "" {
				if params.FlightID, err = store.UUID("flight_id", n.FlightID); err != nil {
					return err
				}
			}
			if n.StateID > 0 {
				params.ConformanceStateID = &n.StateID
			}
			if n.FailReason != "" || len(n.Body) == 0 {
				reason := n.FailReason
				if reason == "" {
					reason = "no body"
				}
				params.FailReason = clip(reason)
			} else {
				params.Payload, params.Body = n.Body, n.Body
				if !json.Valid(n.Body) {
					return fmt.Errorf("notice %s: the body is not JSON", n.Ref)
				}
			}
			_, err := q.CoordinationInsertNotice(ctx, params)
			if store.IsNoRows(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("coordination notice: %w", err)
			}
			queued++
			ev := "coordination_notice_queued"
			if params.FailReason != nil {
				ev = "coordination_notice_failed"
			}
			if err := audit(ctx, q, ev, n.Ref, string(n.Kind), intentID, map[string]any{"flight_id": n.FlightID, "fail_reason": n.FailReason}); err != nil {
				return err
			}
		}
		return nil
	})
	return queued, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Claim implements coordination.Store.
func (p Store) Claim(ctx context.Context, n int, lease time.Duration) ([]coordination.Queued, error) {
	rows, err := p.S.Queries().CoordinationClaim(ctx, relational.CoordinationClaimParams{LeaseS: secs(lease), N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("coordination claim: %w", err)
	}
	out := make([]coordination.Queued, 0, len(rows))
	for _, r := range rows {
		q := coordination.Queued{ID: r.ID, Ref: r.NoticeRef, Kind: coordination.Kind(r.Kind), IntentID: store.UUIDText(r.IntentID),
			Body: r.Body, Attempts: int(r.Attempts)}
		if r.ConformanceStateID != nil {
			q.StateID = *r.ConformanceStateID
		}
		out = append(out, q)
	}
	return out, nil
}

// Received implements coordination.Store: the receipt, ats_notified_at
// on the conformance state and the events row commit together. A notice
// that is no longer pending (another replica stored it first) changes
// nothing.
func (p Store) Received(ctx context.Context, id int64, r coordination.Receipt, pollAfter time.Duration) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		var at *time.Time
		if !r.ReceivedAt.IsZero() {
			t := r.ReceivedAt.UTC()
			at = &t
		}
		row, err := q.CoordinationReceived(ctx, relational.CoordinationReceivedParams{AckID: &r.AckID, AnspReceivedAt: at, PollS: secs(pollAfter), ID: id})
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("coordination receipt: %w", err)
		}
		if row.ConformanceStateID != nil {
			if err := q.ConformanceStateNotified(ctx, *row.ConformanceStateID); err != nil {
				return fmt.Errorf("ats_notified_at: %w", err)
			}
		}
		return audit(ctx, q, "coordination_notice_received", row.NoticeRef, row.Kind, store.UUIDText(row.IntentID),
			map[string]any{"ack_id": r.AckID, "repeat": r.Repeat})
	})
}

// Retry implements coordination.Store.
func (p Store) Retry(ctx context.Context, id int64, cause string, backoff time.Duration) error {
	if _, err := p.S.Queries().CoordinationRetry(ctx, relational.CoordinationRetryParams{BackoffS: secs(backoff), LastError: clip(cause), ID: id}); err != nil {
		return fmt.Errorf("coordination retry: %w", err)
	}
	return nil
}

// Fail implements coordination.Store.
func (p Store) Fail(ctx context.Context, id int64, cause string) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		row, err := q.CoordinationFail(ctx, relational.CoordinationFailParams{LastError: clip(cause), ID: id})
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("coordination fail: %w", err)
		}
		return audit(ctx, q, "coordination_notice_failed", row.NoticeRef, row.Kind, store.UUIDText(row.IntentID), map[string]any{"cause": cause})
	})
}

// pollLease is how long a read of an acknowledgement is held by one
// replica.
const pollLease = 30 * time.Second

// DuePolls implements coordination.Store.
func (p Store) DuePolls(ctx context.Context, n int) ([]coordination.Polled, error) {
	rows, err := p.S.Queries().CoordinationDuePolls(ctx, relational.CoordinationDuePollsParams{LeaseS: pollLease.Seconds(), N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("coordination polls: %w", err)
	}
	out := make([]coordination.Polled, 0, len(rows))
	for _, r := range rows {
		x := coordination.Polled{ID: r.ID, Ref: r.NoticeRef, Kind: coordination.Kind(r.Kind), AckID: str(r.AckID), State: r.State,
			Polls: int(r.Polls), SinceReceived: time.Duration(r.SinceReceivedS * float64(time.Second))}
		if r.ConformanceStateID != nil {
			x.StateID = *r.ConformanceStateID
		}
		if r.ReceivedAt != nil {
			x.ReceivedAt = *r.ReceivedAt
		}
		out = append(out, x)
	}
	return out, nil
}

// Polled implements coordination.Store.
func (p Store) Polled(ctx context.Context, id int64, next time.Duration) error {
	if _, err := p.S.Queries().CoordinationPolled(ctx, relational.CoordinationPolledParams{NextS: secs(next), ID: id}); err != nil {
		return fmt.Errorf("coordination poll: %w", err)
	}
	return nil
}

// Acknowledged implements coordination.Store: the acknowledgement,
// ats_ack_ref (the notice's ack_id) on the conformance state and the
// events row commit together.
func (p Store) Acknowledged(ctx context.Context, id int64, s coordination.NoticeState) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		var by *string
		if s.AcknowledgedBy != "" {
			by = &s.AcknowledgedBy
		}
		row, err := q.CoordinationAcknowledged(ctx, relational.CoordinationAcknowledgedParams{AnspAcknowledgedAt: s.AcknowledgedAt, AcknowledgedBy: by, ID: id})
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("coordination acknowledgement: %w", err)
		}
		if row.ConformanceStateID != nil && row.AckID != nil {
			if err := q.ConformanceStateAcknowledged(ctx, relational.ConformanceStateAcknowledgedParams{AtsAckRef: row.AckID, ID: *row.ConformanceStateID}); err != nil {
				return fmt.Errorf("ats_ack_ref: %w", err)
			}
		}
		return audit(ctx, q, "coordination_notice_acknowledged", row.NoticeRef, row.Kind, store.UUIDText(row.IntentID),
			map[string]any{"ack_id": str(row.AckID), "acknowledged_by": s.AcknowledgedBy})
	})
}

// item is a console row; flightID is "" for a notice without a flight.
func item(id int64, ref, kind, intentID, flightID, state string, created time.Time, attempts int32,
	lastErr, ackID *string, received, escalated, failed *time.Time, next time.Time, age float64) coordination.Item {
	it := coordination.Item{ID: id, NoticeRef: ref, Kind: kind, IntentID: intentID, State: state, CreatedAt: created,
		Attempts: int(attempts), LastError: lastErr, AckID: ackID, ReceivedAt: received, EscalatedAt: escalated, FailedAt: failed, AgeS: math.Max(age, 0)}
	if flightID != "" {
		it.FlightID = &flightID
	}
	if state == "pending" {
		n := next
		it.NextAt = &n
	}
	return it
}

// Escalate implements coordination.Store.
func (p Store) Escalate(ctx context.Context, after time.Duration) ([]coordination.Item, error) {
	var out []coordination.Item
	err := p.S.Tx(ctx, func(q *relational.Queries) error {
		out = nil
		rows, err := q.CoordinationEscalate(ctx, secs(after))
		if err != nil {
			return fmt.Errorf("coordination escalation: %w", err)
		}
		for i := range rows {
			r := &rows[i]
			it := item(r.ID, r.NoticeRef, r.Kind, store.UUIDText(r.IntentID), store.UUIDText(r.FlightID), r.State, r.CreatedAt, r.Attempts, r.LastError, r.AckID,
				r.ReceivedAt, r.EscalatedAt, r.FailedAt, r.NextAt, r.AgeS)
			if err := audit(ctx, q, "coordination_notice_escalated", r.NoticeRef, r.Kind, it.IntentID, map[string]any{"age_s": it.AgeS}); err != nil {
				return err
			}
			out = append(out, it)
		}
		return nil
	})
	return out, err
}

// Open implements coordination.Store.
func (p Store) Open(ctx context.Context, n int) ([]coordination.Item, bool, error) {
	n = min(max(n, 1), coordination.MaxListed)
	rows, err := p.S.Queries().CoordinationOpen(ctx, int32(n+1))
	if err != nil {
		return nil, false, fmt.Errorf("coordination open: %w", err)
	}
	truncated := len(rows) > n
	if truncated {
		rows = rows[:n]
	}
	out := make([]coordination.Item, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, item(r.ID, r.NoticeRef, r.Kind, store.UUIDText(r.IntentID), store.UUIDText(r.FlightID), r.State, r.CreatedAt, r.Attempts, r.LastError, r.AckID,
			r.ReceivedAt, r.EscalatedAt, r.FailedAt, r.NextAt, r.AgeS))
	}
	return out, truncated, nil
}

// Summarise implements coordination.Store.
func (p Store) Summarise(ctx context.Context) (coordination.Summary, error) {
	r, err := p.S.Queries().CoordinationSummary(ctx)
	if err != nil {
		return coordination.Summary{}, fmt.Errorf("coordination summary: %w", err)
	}
	return coordination.Summary{Pending: int(r.Pending), Retrying: int(r.Retrying), OldestPendingAgeS: r.OldestPendingS, Escalated: int(r.Escalated), Failed: int(r.Failed)}, nil
}
