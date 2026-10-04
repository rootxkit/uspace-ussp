package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Republisher sends an alert of the record on alrt.v1 (alerts.Service);
// it is called after the commit, and a failure is counted there.
type Republisher interface {
	Republish(ctx context.Context, st alerts.Stored)
}

// Event types of the console's alert writes.
const (
	EventAlertEscalated = "alert_escalated"
	EventAlertClosed    = "alert_closed"
	// EntityAlert is the events entity of an alert.
	EntityAlert = "alert"
)

// Counters of the console's alert writes.
const (
	CounterEscalated = "admin_alerts_escalated"
	CounterClosed    = "admin_alerts_closed"
)

// Alert is one alert as the console shows it.
type Alert struct {
	AlertID             string         `json:"alert_id"`
	Kind                string         `json:"kind"`
	Severity            string         `json:"severity"`
	State               string         `json:"state"`
	ClearReason         *string        `json:"clear_reason,omitempty"`
	FlightID            *string        `json:"flight_id,omitempty"`
	IntentID            *string        `json:"intent_id,omitempty"`
	AuthorisationNumber *string        `json:"authorisation_number,omitempty"`
	RaisedAt            time.Time      `json:"raised_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	ClearedAt           *time.Time     `json:"cleared_at,omitempty"`
	AckedAt             *time.Time     `json:"acked_at,omitempty"`
	AckedBy             *string        `json:"acked_by,omitempty"`
	EscalatedAt         *time.Time     `json:"escalated_at,omitempty"`
	EscalatedBy         *string        `json:"escalated_by,omitempty"`
	EscalationReason    *string        `json:"escalation_reason,omitempty"`
	ClosedAt            *time.Time     `json:"closed_at,omitempty"`
	ClosedBy            *string        `json:"closed_by,omitempty"`
	CloseReason         *string        `json:"close_reason,omitempty"`
	MessagesRecorded    int64          `json:"messages_recorded"`
	PolicyVersion       int64          `json:"policy_version"`
	Detail              map[string]any `json:"detail"`
}

// Alerts is GET /v1/admin/alerts and /v1/admin/escalations.
type Alerts struct {
	Alerts            []Alert `json:"alerts"`
	Truncated         bool    `json:"truncated"`
	EscalationAfterS  float64 `json:"escalation_after_s"`
	EscalationRepeatS float64 `json:"escalation_repeat_s"`
	PolicyVersion     int64   `json:"policy_version"`
}

// alertRow is the columns every alert query of the console reads.
type alertRow struct {
	ID                                                   string
	Kind, Severity, State                                string
	FlightID, IntentID                                   string
	AuthorisationNumber, ClearReason, AckedBy            *string
	EscalatedBy, EscalationReason, ClosedBy, CloseReason *string
	RaisedAt, UpdatedAt                                  time.Time
	ClearedAt, AckedAt, EscalatedAt, ClosedAt            *time.Time
	Detail                                               []byte
	PolicyVersion, MessagesRecorded                      int64
}

// view is the alert as the console shows it, the staff who escalated
// and closed it by their usernames (names, staffNames).
func (r *alertRow) view(names func(string) string) Alert {
	return Alert{AlertID: r.ID, Kind: r.Kind, Severity: r.Severity, State: r.State, ClearReason: r.ClearReason,
		FlightID: strp(r.FlightID), IntentID: strp(r.IntentID), AuthorisationNumber: r.AuthorisationNumber,
		RaisedAt: r.RaisedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), ClearedAt: utc(r.ClearedAt), AckedAt: utc(r.AckedAt), AckedBy: r.AckedBy,
		EscalatedAt: utc(r.EscalatedAt), EscalatedBy: named(names, r.EscalatedBy), EscalationReason: r.EscalationReason,
		ClosedAt: utc(r.ClosedAt), ClosedBy: named(names, r.ClosedBy), CloseReason: r.CloseReason,
		MessagesRecorded: max(r.MessagesRecorded, 1), PolicyVersion: r.PolicyVersion, Detail: object(r.Detail)}
}

// named is the display name of a staff id, nil kept nil.
func named(names func(string) string, id *string) *string {
	if id == nil {
		return nil
	}
	n := names(*id)
	return &n
}

// alertNames resolves the staff of rows (who escalated and closed each)
// to their usernames.
func (s *Service) alertNames(ctx context.Context, rows ...alertRow) func(string) string {
	ids := make([]string, 0, 2*len(rows))
	for i := range rows {
		for _, id := range []*string{rows[i].EscalatedBy, rows[i].ClosedBy} {
			if id != nil {
				ids = append(ids, *id)
			}
		}
	}
	return s.staffNames(ctx, ids)
}

func (s *Service) alertList(ctx context.Context, rows []alertRow) Alerts {
	pol := s.policy()
	out := Alerts{Alerts: make([]Alert, 0, min(len(rows), MaxAlerts)), EscalationAfterS: pol.Values.EscalationAfterS,
		EscalationRepeatS: pol.Values.EscalationRepeatS, PolicyVersion: pol.Version}
	if len(rows) > MaxAlerts {
		rows, out.Truncated = rows[:MaxAlerts], true
	}
	names := s.alertNames(ctx, rows...)
	for i := range rows {
		out.Alerts = append(out.Alerts, rows[i].view(names))
	}
	return out
}

// Alerts lists the alerts not cleared, and with recent those cleared in
// the last RecentAlerts, newest first, at most MaxAlerts.
func (s *Service) Alerts(ctx context.Context, recent bool) (Alerts, error) {
	rows, err := s.Store.Queries().AdminAlerts(ctx, relational.AdminAlertsParams{Recent: recent, RecentS: RecentAlerts.Seconds(), MaxRows: MaxAlerts + 1})
	if err != nil {
		return Alerts{}, fmt.Errorf("alerts: %w", err)
	}
	rs := make([]alertRow, len(rows))
	for i := range rows {
		r := &rows[i]
		rs[i] = alertRow{ID: store.UUIDText(r.ID), Kind: r.Kind, Severity: r.Severity, State: r.State,
			FlightID: store.UUIDText(r.FlightID), IntentID: store.UUIDText(r.IntentID), AuthorisationNumber: r.AuthorisationNumber,
			ClearReason: r.ClearReason, AckedBy: r.AckedBy, EscalatedBy: r.EscalatedBy, EscalationReason: r.EscalationReason,
			ClosedBy: r.ClosedBy, CloseReason: r.CloseReason, RaisedAt: r.RaisedAt, UpdatedAt: r.UpdatedAt, ClearedAt: r.ClearedAt,
			AckedAt: r.AckedAt, EscalatedAt: r.EscalatedAt, ClosedAt: r.ClosedAt, Detail: r.Detail, PolicyVersion: r.PolicyVersion,
			MessagesRecorded: r.MessagesRecorded}
	}
	return s.alertList(ctx, rs), nil
}

// Escalations lists the alerts escalated and not closed on the console,
// oldest escalation first, at most MaxAlerts.
func (s *Service) Escalations(ctx context.Context) (Alerts, error) {
	rows, err := s.Store.Queries().AdminEscalations(ctx, MaxAlerts+1)
	if err != nil {
		return Alerts{}, fmt.Errorf("escalations: %w", err)
	}
	rs := make([]alertRow, len(rows))
	for i := range rows {
		r := &rows[i]
		rs[i] = alertRow{ID: store.UUIDText(r.ID), Kind: r.Kind, Severity: r.Severity, State: r.State,
			FlightID: store.UUIDText(r.FlightID), IntentID: store.UUIDText(r.IntentID), AuthorisationNumber: r.AuthorisationNumber,
			ClearReason: r.ClearReason, AckedBy: r.AckedBy, EscalatedBy: r.EscalatedBy, EscalationReason: r.EscalationReason,
			ClosedBy: r.ClosedBy, CloseReason: r.CloseReason, RaisedAt: r.RaisedAt, UpdatedAt: r.UpdatedAt, ClearedAt: r.ClearedAt,
			AckedAt: r.AckedAt, EscalatedAt: r.EscalatedAt, ClosedAt: r.ClosedAt, Detail: r.Detail, PolicyVersion: r.PolicyVersion,
			MessagesRecorded: r.MessagesRecorded}
	}
	return s.alertList(ctx, rs), nil
}

func lockedRow(r relational.AdminAlertForUpdateRow) alertRow {
	return alertRow{ID: store.UUIDText(r.ID), Kind: r.Kind, Severity: r.Severity, State: r.State,
		FlightID: store.UUIDText(r.FlightID), IntentID: store.UUIDText(r.IntentID), AuthorisationNumber: r.AuthorisationNumber,
		ClearReason: r.ClearReason, AckedBy: r.AckedBy, EscalatedBy: r.EscalatedBy, EscalationReason: r.EscalationReason,
		ClosedBy: r.ClosedBy, CloseReason: r.CloseReason, RaisedAt: r.RaisedAt, UpdatedAt: r.UpdatedAt, ClearedAt: r.ClearedAt,
		AckedAt: r.AckedAt, EscalatedAt: r.EscalatedAt, ClosedAt: r.ClosedAt, Detail: r.Detail, PolicyVersion: r.PolicyVersion,
		MessagesRecorded: r.MessagesRecorded}
}

// stored is the alert as alrt.v1 carries it, for the republish.
func stored(r relational.AdminAlertForUpdateRow) alerts.Stored {
	b := alerts.Body{AlertID: store.UUIDText(r.ID), Kind: r.Kind, Severity: core.Severity(r.Severity), State: r.State, ClearReason: r.ClearReason,
		FlightID: store.UUIDText(r.FlightID), IntentID: strp(store.UUIDText(r.IntentID)), AuthorisationNumber: r.AuthorisationNumber,
		RaisedAt: r.RaisedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), PolicyVersion: r.PolicyVersion, Detail: json.RawMessage(r.Detail),
		AckedAt: utc(r.AckedAt), EscalatedAt: utc(r.EscalatedAt)}
	if r.CapturedAt != nil {
		b.CapturedAt = r.CapturedAt.UTC()
	} else {
		b.CapturedAt = b.UpdatedAt
	}
	if len(b.Detail) == 0 {
		b.Detail = json.RawMessage(`{}`)
	}
	st := alerts.Stored{Body: b, AckedBy: r.AckedBy}
	if r.Cell5 != nil {
		st.Cell5 = *r.Cell5
	}
	return st
}

func alertID(id string) error {
	if _, err := store.UUID("alert_id", id); err != nil {
		return err
	}
	return nil
}

// Escalate is a supervisor's escalation of an alert with a reason: the
// escalation (the database clock) and its events row in one
// transaction, then, after the commit, the alert republished on alrt.v1
// so every console and stream sees it. An alert escalated before is
// answered unchanged, with nothing written and nothing republished.
// Escalation never changes the alert's state.
func (s *Service) Escalate(ctx context.Context, staffID, id, why string) (Alert, error) {
	if err := alertID(id); err != nil {
		return Alert{}, err
	}
	if err := reason("reason", why, MaxReason); err != nil {
		return Alert{}, err
	}
	u, _ := store.UUID("alert_id", id)
	var row relational.AdminAlertForUpdateRow
	changed := false
	err := s.Store.Tx(ctx, func(q *relational.Queries) error {
		r, err := q.AdminAlertForUpdate(ctx, u)
		if store.IsNoRows(err) {
			return notFound("alert")
		}
		if err != nil {
			return err
		}
		if r.EscalatedAt != nil {
			row = r
			return nil
		}
		at, err := q.AdminEscalateAlert(ctx, relational.AdminEscalateAlertParams{StaffID: staffID, Reason: why, ID: u})
		if err != nil {
			return err
		}
		r.EscalatedAt, r.EscalatedBy, r.EscalationReason = at, &staffID, &why
		row, changed = r, true
		_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorStaff, ActorID: staffID, EntityType: EntityAlert, EntityID: id,
			EventType: EventAlertEscalated, Payload: map[string]any{"reason": why, "kind": r.Kind, "severity": r.Severity, "state": r.State}})
		return err
	})
	if err != nil {
		return Alert{}, err
	}
	if changed {
		s.count(CounterEscalated)
		if s.Republisher != nil {
			s.Republisher.Republish(ctx, stored(row))
		}
	}
	lr := lockedRow(row)
	return lr.view(s.alertNames(ctx, lr)), nil
}

// CloseAlert is the console's close of an alert (a supervisor handled
// it) with a reason and its events row in one transaction. It never
// clears the alert: an active one stays active. An alert closed before
// is 409, a permanent answer.
func (s *Service) CloseAlert(ctx context.Context, staffID, id, why string) (Alert, error) {
	if err := alertID(id); err != nil {
		return Alert{}, err
	}
	if err := reason("reason", why, MaxReason); err != nil {
		return Alert{}, err
	}
	u, _ := store.UUID("alert_id", id)
	var row relational.AdminAlertForUpdateRow
	err := s.Store.Tx(ctx, func(q *relational.Queries) error {
		r, err := q.AdminAlertForUpdate(ctx, u)
		if store.IsNoRows(err) {
			return notFound("alert")
		}
		if err != nil {
			return err
		}
		if r.ClosedAt != nil {
			return conflict(SlugAlreadyClosed, "the alert was closed on the console at "+r.ClosedAt.UTC().Format(time.RFC3339))
		}
		at, err := q.AdminCloseAlert(ctx, relational.AdminCloseAlertParams{StaffID: staffID, Reason: why, ID: u})
		if err != nil {
			return err
		}
		r.ClosedAt, r.ClosedBy, r.CloseReason = at, &staffID, &why
		row = r
		_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorStaff, ActorID: staffID, EntityType: EntityAlert, EntityID: id,
			EventType: EventAlertClosed, Payload: map[string]any{"reason": why, "state": r.State, "escalated": r.EscalatedAt != nil}})
		return err
	})
	if err != nil {
		return Alert{}, err
	}
	s.count(CounterClosed)
	lr := lockedRow(row)
	return lr.view(s.alertNames(ctx, lr)), nil
}
