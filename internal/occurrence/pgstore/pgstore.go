// Package pgstore is internal/occurrence's Store on the relational
// database: occurrence_reports (migrations 00005, 00020) and the alerts
// and flights a report names, through the sqlc queries of
// internal/store/queries/relational/occurrences.sql. Every queued
// report and every outcome writes its events row in the same
// transaction. Its tests are the integration tests (test/integration).
package pgstore

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/occurrence"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is occurrence.Store on PostgreSQL.
type Store struct{ S *store.Store }

var _ occurrence.Store = Store{}

// EntityReport is the events entity of a report.
const EntityReport = "occurrence_report"

func secs(d time.Duration) float64 { return math.Max(d.Seconds(), 0) }

func n32(n int) int32 { return int32(min(max(n, 1), 10_000)) }

func clip(s string) *string {
	if len(s) > 1000 {
		s = s[:1000]
	}
	return &s
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func flightRef(id, intentID string, serial string, reg, number *string, emergency, inUSpace bool, started time.Time) occurrence.FlightRef {
	return occurrence.FlightRef{FlightID: id, Serial: serial, OperatorReg: reg, AuthorisationNumber: number, IntentID: opt(intentID),
		InUSpace: inUSpace, Emergency: emergency, StartedAt: started}
}

// AlertEvents implements occurrence.Store.
func (p Store) AlertEvents(ctx context.Context, lookback time.Duration, airproxHM, airproxVM float64, n int) ([]occurrence.AlertEvent, error) {
	rows, err := p.S.Queries().OccurrenceAlertEvents(ctx, relational.OccurrenceAlertEventsParams{LookbackS: secs(lookback),
		AirproxHM: airproxHM, AirproxVM: airproxVM, N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("occurrence alerts: %w", err)
	}
	out := make([]occurrence.AlertEvent, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, occurrence.AlertEvent{AlertID: store.UUIDText(r.ID), Kind: r.Kind, RaisedAt: r.RaisedAt, Detail: r.Detail, SourceRef: r.SourceRef,
			Flight: flightRef(store.UUIDText(r.FlightID), store.UUIDText(r.IntentID), r.UasSerial, r.OperatorReg, r.AuthorisationNumber, r.Emergency, r.InUspace, r.StartedAt)})
	}
	return out, nil
}

// EmergencyFlights implements occurrence.Store.
func (p Store) EmergencyFlights(ctx context.Context, lookback time.Duration, n int) ([]occurrence.FlightRef, error) {
	rows, err := p.S.Queries().OccurrenceEmergencyFlights(ctx, relational.OccurrenceEmergencyFlightsParams{LookbackS: secs(lookback), N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("emergency flights: %w", err)
	}
	out := make([]occurrence.FlightRef, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, flightRef(store.UUIDText(r.FlightID), store.UUIDText(r.IntentID), r.UasSerial, r.OperatorReg, r.AuthorisationNumber, r.Emergency, r.InUspace, r.StartedAt))
	}
	return out, nil
}

// Alert implements occurrence.Store.
func (p Store) Alert(ctx context.Context, alertID string) (occurrence.AlertEvent, error) {
	id, err := store.UUID("alert_id", alertID)
	if err != nil {
		return occurrence.AlertEvent{}, occurrence.ErrNotFound
	}
	r, err := p.S.Queries().OccurrenceAlert(ctx, id)
	if store.IsNoRows(err) {
		return occurrence.AlertEvent{}, occurrence.ErrNotFound
	}
	if err != nil {
		return occurrence.AlertEvent{}, fmt.Errorf("alert: %w", err)
	}
	return occurrence.AlertEvent{AlertID: store.UUIDText(r.ID), Kind: r.Kind, RaisedAt: r.RaisedAt, Detail: r.Detail, SourceRef: r.SourceRef,
		Flight: flightRef(store.UUIDText(r.FlightID), store.UUIDText(r.IntentID), r.UasSerial, r.OperatorReg, r.AuthorisationNumber, r.Emergency, r.InUspace, r.StartedAt)}, nil
}

// Flights implements occurrence.Store.
func (p Store) Flights(ctx context.Context, ids []string) ([]occurrence.FlightRef, error) {
	us, err := store.UUIDs("flight_id", ids)
	if err != nil {
		return nil, err
	}
	rows, err := p.S.Queries().OccurrenceFlights(ctx, us)
	if err != nil {
		return nil, fmt.Errorf("flights: %w", err)
	}
	out := make([]occurrence.FlightRef, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, flightRef(store.UUIDText(r.FlightID), store.UUIDText(r.IntentID), r.UasSerial, r.OperatorReg, r.AuthorisationNumber, r.Emergency, r.InUspace, r.StartedAt))
	}
	return out, nil
}

// Now implements occurrence.Store.
func (p Store) Now(ctx context.Context) (time.Time, error) { return p.S.Queries().DBNow(ctx) }

func itemOf(ref, kind, state, channel, flaggedBy string, aware, deadline time.Time, attempts int32, lastErr *string, flights []string,
	submitted *time.Time, authorityRef *string, failed *time.Time, next time.Time, ttd float64) occurrence.Item {
	it := occurrence.Item{ReportRef: ref, Kind: kind, State: state, Channel: channel, FlaggedBy: flaggedBy, BecameAwareAt: aware, DeadlineAt: deadline,
		TimeToDeadlineS: ttd, Critical: state != "delivered" && ttd < 0, Attempts: int(attempts), LastError: lastErr, FlightIDs: flights,
		SubmittedAt: submitted, AuthorityRef: authorityRef, FailedAt: failed}
	if state == "pending" {
		n := next
		it.NextAt = &n
	}
	return it
}

// Enqueue implements occurrence.Store: the report and its events row
// commit together; a report of an event reported before is returned as
// stored.
func (p Store) Enqueue(ctx context.Context, r occurrence.Report) (occurrence.Item, bool, error) {
	flights, err := store.UUIDs("flight_id", r.FlightIDs)
	if err != nil {
		return occurrence.Item{}, false, err
	}
	intents, err := store.UUIDs("intent_id", r.IntentIDs)
	if err != nil {
		return occurrence.Item{}, false, err
	}
	created := false
	var it occurrence.Item
	err = p.S.Tx(ctx, func(q *relational.Queries) error {
		occurred := r.OccurredAt.UTC()
		_, err := q.OccurrenceInsert(ctx, relational.OccurrenceInsertParams{ReportRef: r.Ref, Kind: r.Kind, OccurredAt: &occurred,
			BecameAwareAt: r.AwareAt.UTC(), DeadlineS: occurrence.Deadline.Seconds(), Payload: r.Body, SourceKind: r.SourceKind,
			SourceRef: r.SourceRef, Channel: r.Channel, FlaggedBy: r.FlaggedBy, ReporterRef: r.Reporter, FlightIds: flights, IntentIds: intents})
		switch {
		case store.IsNoRows(err):
		case err != nil:
			return fmt.Errorf("occurrence report: %w", err)
		default:
			created = true
			if _, err := store.Audit(ctx, q, store.Event{ActorType: actorOf(r.FlaggedBy), ActorID: r.Reporter, Purpose: "occurrence_reporting",
				EntityType: EntityReport, EntityID: r.Ref, EventType: "occurrence_report_queued",
				Payload: map[string]any{"kind": r.Kind, "channel": r.Channel, "flight_ids": r.FlightIDs, "source": r.SourceKind + ":" + r.SourceRef}}); err != nil {
				return err
			}
		}
		row, err := q.OccurrenceBySource(ctx, relational.OccurrenceBySourceParams{SourceKind: r.SourceKind, SourceRef: r.SourceRef})
		if err != nil {
			return fmt.Errorf("occurrence report: %w", err)
		}
		it = itemOf(row.ReportRef, row.Kind, row.State, row.Channel, row.FlaggedBy, row.BecameAwareAt, row.DeadlineAt, row.Attempts, row.LastError,
			store.UUIDTexts(row.FlightIds), row.SubmittedAt, row.AuthorityRef, row.FailedAt, row.NextAt, row.TimeToDeadlineS)
		return nil
	})
	return it, created, err
}

func actorOf(flaggedBy string) string {
	if flaggedBy == "supervisor" {
		return store.ActorStaff
	}
	return store.ActorSystem
}

// Claim implements occurrence.Store.
func (p Store) Claim(ctx context.Context, n int, lease time.Duration) ([]occurrence.Queued, error) {
	rows, err := p.S.Queries().OccurrenceClaim(ctx, relational.OccurrenceClaimParams{LeaseS: secs(lease), N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("occurrence claim: %w", err)
	}
	out := make([]occurrence.Queued, 0, len(rows))
	for _, r := range rows {
		out = append(out, occurrence.Queued{ID: store.UUIDText(r.ID), Ref: r.ReportRef, Body: r.Payload, Attempts: int(r.Attempts)})
	}
	return out, nil
}

// Delivered implements occurrence.Store.
func (p Store) Delivered(ctx context.Context, id, authorityRef string) error {
	uid, err := store.UUID("id", id)
	if err != nil {
		return err
	}
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		row, err := q.OccurrenceDelivered(ctx, relational.OccurrenceDeliveredParams{AuthorityRef: opt(authorityRef), ID: uid})
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("occurrence delivered: %w", err)
		}
		_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "occurrence", EntityType: EntityReport, EntityID: row.ReportRef,
			EventType: "occurrence_report_delivered", Payload: map[string]any{"authority_ref": authorityRef}})
		return err
	})
}

// Retry implements occurrence.Store.
func (p Store) Retry(ctx context.Context, id, cause string, backoff time.Duration) error {
	uid, err := store.UUID("id", id)
	if err != nil {
		return err
	}
	if _, err := p.S.Queries().OccurrenceRetry(ctx, relational.OccurrenceRetryParams{BackoffS: secs(backoff), LastError: clip(cause), ID: uid}); err != nil {
		return fmt.Errorf("occurrence retry: %w", err)
	}
	return nil
}

// Fail implements occurrence.Store.
func (p Store) Fail(ctx context.Context, id, cause string) error {
	uid, err := store.UUID("id", id)
	if err != nil {
		return err
	}
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		row, err := q.OccurrenceFail(ctx, relational.OccurrenceFailParams{LastError: clip(cause), ID: uid})
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("occurrence fail: %w", err)
		}
		_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "occurrence", EntityType: EntityReport, EntityID: row.ReportRef,
			EventType: "occurrence_report_failed", Payload: map[string]any{"cause": cause}})
		return err
	})
}

// Open implements occurrence.Store.
func (p Store) Open(ctx context.Context, n int) ([]occurrence.Item, bool, error) {
	n = min(max(n, 1), occurrence.MaxListed)
	rows, err := p.S.Queries().OccurrenceOpen(ctx, int32(n+1))
	if err != nil {
		return nil, false, fmt.Errorf("occurrence open: %w", err)
	}
	truncated := len(rows) > n
	if truncated {
		rows = rows[:n]
	}
	out := make([]occurrence.Item, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, itemOf(r.ReportRef, r.Kind, r.State, r.Channel, r.FlaggedBy, r.BecameAwareAt, r.DeadlineAt, r.Attempts, r.LastError,
			store.UUIDTexts(r.FlightIds), r.SubmittedAt, r.AuthorityRef, r.FailedAt, r.NextAt, r.TimeToDeadlineS))
	}
	return out, truncated, nil
}

// Summarise implements occurrence.Store.
func (p Store) Summarise(ctx context.Context) (occurrence.Summary, error) {
	r, err := p.S.Queries().OccurrenceSummary(ctx)
	if err != nil {
		return occurrence.Summary{}, fmt.Errorf("occurrence summary: %w", err)
	}
	return occurrence.Summary{Pending: int(r.Pending), Failed: int(r.Failed), Critical: int(r.Critical), NearestDeadlineS: r.NearestDeadlineS}, nil
}

// Held implements occurrence.Store.
func (p Store) Held(ctx context.Context, after string, n int) ([]string, error) {
	if after == "" {
		after = "00000000-0000-0000-0000-000000000000"
	}
	a, err := store.UUID("after", after)
	if err != nil {
		return nil, err
	}
	ids, err := p.S.Queries().OccurrenceHeld(ctx, relational.OccurrenceHeldParams{After: a, N: int32(min(max(n, 1), occurrence.HoldsBatch))})
	if err != nil {
		return nil, fmt.Errorf("held flights: %w", err)
	}
	return ids, nil
}
