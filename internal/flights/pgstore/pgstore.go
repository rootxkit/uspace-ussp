// Package pgstore is internal/flights' Store on the relational database:
// the flights table (migration 00002) through the sqlc queries of
// internal/store/queries/relational/flights.sql, each fact with its audit
// row in one transaction. Its tests are the integration tests
// (test/integration).
package pgstore

import (
	"context"
	"fmt"

	"github.com/rootxkit/uspace-ussp/internal/flights"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is flights.Store on PostgreSQL.
type Store struct{ S *store.Store }

var _ flights.Store = Store{}

// The flights.last_state of each fact.
var lastState = map[string]string{
	flights.EventStarted:          "flying",
	flights.EventTelemetryLost:    "telemetry_lost",
	flights.EventTelemetryResumed: "flying",
	flights.EventEnded:            "ended",
}

// Record implements flights.Store: the row is created by the first fact
// to arrive, an ended flight is never reopened, a flight recorded
// without an intent takes the intent of a later fact (the binder bound
// it to one), and the fact is audited
// with telemetry-ingest as the system actor.
func (p Store) Record(ctx context.Context, b flights.Body) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error { return RecordIn(ctx, q, b) })
}

// RecordIn is Record inside the caller's transaction q, for a caller
// that commits more work with the fact (the F3411 ISA plan, WP-9).
func RecordIn(ctx context.Context, q *relational.Queries, b flights.Body) error {
	id, err := store.UUID("flight_id", b.FlightID)
	if err != nil {
		return err
	}
	state := lastState[b.Event]
	params := relational.RecordFlightEventParams{
		ID: id, AuthorisationNumber: b.AuthorisationNumber, UasSerial: b.UASSerial, OperatorReg: b.OperatorReg,
		ClientID: b.ClientID, StartedAt: b.StartedAt.Time, EndReason: b.EndReason, LastState: &state,
	}
	if b.IntentID != nil {
		if params.IntentID, err = store.UUID("intent_id", *b.IntentID); err != nil {
			return err
		}
	}
	if b.Event == flights.EventEnded {
		at := b.At.Time
		params.EndedAt = &at
	}
	if err := q.RecordFlightEvent(ctx, params); err != nil {
		return fmt.Errorf("flight %s %s: %w", b.FlightID, b.Event, err)
	}
	_, err = store.Audit(ctx, q, store.Event{
		ActorType: store.ActorSystem, ActorID: "telemetry-ingest", EntityType: "flight", EntityID: b.FlightID,
		EventType: "flight_" + b.Event, Payload: b,
	})
	return err
}
