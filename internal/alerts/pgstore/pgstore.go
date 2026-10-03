// Package pgstore is internal/alerts's Store and FactStore on the
// relational database: the alerts table (migrations 00002, 00016)
// through the sqlc queries of internal/store/queries/relational/alerts.sql.
// Its tests are the integration tests (test/integration).
package pgstore

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

// Store is alerts.Store and alerts.FactStore on PostgreSQL.
type Store struct{ S *store.Store }

var (
	_ alerts.Store     = Store{}
	_ alerts.FactStore = Store{}
)

// intentUUID is the intent id parameter; none when s is nil.
func intentUUID(s *string) (relational.RecordAlertParams, error) {
	var p relational.RecordAlertParams
	if s == nil {
		return p, nil
	}
	id, err := store.UUID("intent_id", *s)
	p.IntentID = id
	return p, err
}

// RecordAlert implements alerts.Store.
func (p Store) RecordAlert(ctx context.Context, r alerts.Record) (bool, error) {
	id, err := store.UUID("alert_id", r.AlertID)
	if err != nil {
		return false, err
	}
	params, err := intentUUID(r.IntentID)
	if err != nil {
		return false, err
	}
	// An alert of an intent without a flight (restriction_activated
	// before the activation, WP-12) has no flight id: NULL.
	if r.FlightID != "" {
		if params.FlightID, err = store.UUID("flight_id", r.FlightID); err != nil {
			return false, err
		}
	}
	var cleared *time.Time
	if r.State == alerts.StateCleared {
		t := r.UpdatedAt.UTC()
		cleared = &t
	}
	var cell *string
	if r.Cell5 != "" {
		cell = &r.Cell5
	}
	captured := r.CapturedAt.UTC()
	params.ID, params.Kind, params.AuthorisationNumber, params.PeerRef = id, r.Kind, r.AuthorisationNumber, r.PeerRef
	params.Severity, params.State, params.RaisedAt, params.UpdatedAt = string(r.Severity), r.State, r.RaisedAt.UTC(), r.UpdatedAt.UTC()
	params.ClearedAt, params.ClearReason, params.Detail, params.CapturedAt = cleared, r.ClearReason, r.Detail, &captured
	params.PolicyVersion, params.Cell5 = r.PolicyVersion, cell
	res, err := p.S.Queries().RecordAlert(ctx, params)
	if err != nil {
		return false, fmt.Errorf("alert record: %w", err)
	}
	return res.FlightKnown, nil
}

// RecordDelivery implements alerts.Store.
func (p Store) RecordDelivery(ctx context.Context, d alerts.DeliveryBody) (bool, error) {
	id, err := store.UUID("alert_id", d.AlertID)
	if err != nil {
		return false, err
	}
	n, err := p.S.Queries().RecordAlertDelivery(ctx, relational.RecordAlertDeliveryParams{ClientID: d.ClientID, SentAt: d.SentAt.UTC(), ID: id})
	if err != nil {
		return false, fmt.Errorf("alert delivery: %w", err)
	}
	return n == 1, nil
}

// row is the columns both fact queries return (EscalateAlertsRow
// converts to it).
type row relational.AckAlertRow

func (r row) stored() alerts.Stored {
	s := alerts.Stored{Body: alerts.Body{
		AlertID: store.UUIDText(r.ID), Kind: r.Kind, Severity: core.Severity(r.Severity), State: r.State, ClearReason: r.ClearReason,
		FlightID: store.UUIDText(r.FlightID), AuthorisationNumber: r.AuthorisationNumber, RaisedAt: r.RaisedAt.UTC(),
		UpdatedAt: r.UpdatedAt.UTC(), PolicyVersion: r.PolicyVersion, Detail: json.RawMessage(r.Detail),
		AckedAt: utc(r.AckedAt), EscalatedAt: utc(r.EscalatedAt),
	}, AckedBy: r.AckedBy}
	if r.IntentID.Valid {
		id := store.UUIDText(r.IntentID)
		s.IntentID = &id
	}
	s.CapturedAt = r.RaisedAt.UTC()
	if r.CapturedAt != nil {
		s.CapturedAt = r.CapturedAt.UTC()
	}
	if len(s.Detail) == 0 {
		s.Detail = json.RawMessage(`{}`)
	}
	if r.Cell5 != nil {
		s.Cell5 = *r.Cell5
	}
	return s
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Ack implements alerts.FactStore.
func (p Store) Ack(ctx context.Context, alertID, clientID string) (alerts.Stored, error) {
	id, err := store.UUID("alert_id", alertID)
	if err != nil {
		return alerts.Stored{}, alerts.ErrNotFound
	}
	r, err := p.S.Queries().AckAlert(ctx, relational.AckAlertParams{ClientID: clientID, ID: id})
	if store.IsNoRows(err) {
		return alerts.Stored{}, alerts.ErrNotFound
	}
	if err != nil {
		return alerts.Stored{}, fmt.Errorf("alert acknowledgement: %w", err)
	}
	return row(r).stored(), nil
}

// Escalate implements alerts.FactStore.
func (p Store) Escalate(ctx context.Context, afterS float64, maxRows int) ([]alerts.Stored, error) {
	rs, err := p.S.Queries().EscalateAlerts(ctx, relational.EscalateAlertsParams{AfterS: afterS, MaxRows: int32(min(maxRows, 10_000))})
	if err != nil {
		return nil, fmt.Errorf("alert escalation: %w", err)
	}
	out := make([]alerts.Stored, 0, len(rs))
	for i := range rs {
		out = append(out, row(rs[i]).stored())
	}
	return out, nil
}

// EndedNotices implements alerts.FactStore.
func (p Store) EndedNotices(ctx context.Context, maxRows int) ([]alerts.Stored, error) {
	rs, err := p.S.Queries().OpenIntentNoticesEnded(ctx, int32(min(maxRows, 10_000)))
	if err != nil {
		return nil, fmt.Errorf("ended notices: %w", err)
	}
	out := make([]alerts.Stored, 0, len(rs))
	for i := range rs {
		out = append(out, row(rs[i]).stored())
	}
	return out, nil
}
