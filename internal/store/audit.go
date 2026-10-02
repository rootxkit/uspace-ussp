package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Actor types of the events table.
const (
	ActorStaff  = "staff"  // a console user (staff_accounts.id)
	ActorClient = "client" // an operator machine client (oauth_clients.client_id)
	ActorPeer   = "peer"   // a peer USSP, the authority, the ANSP or the CISP (token subject)
	ActorSystem = "system" // this USSP itself (a job, a start-up default)
)

// Event is one audit row (docs/PLAN.md §8, spec 06 T7). ts is the
// transaction's time.
type Event struct {
	ActorType string
	ActorID   string
	// Purpose is why a PII-bearing record was read or exported; empty
	// for other events.
	Purpose    string
	EntityType string
	EntityID   string
	EventType  string
	// Payload is marshalled to JSON; nil is {}.
	Payload any
}

// Advisory lock keys of the relational database (transaction scoped).
const (
	LockPolicy  int64 = 0x7573737001 // policy versions
	LockSources int64 = 0x7573737002 // source switches and their republication
	LockIntents int64 = 0x7573737003 // intent decisions: the active intents read and one written
)

// Lock takes the transaction-scoped advisory lock key inside q's
// transaction; it is released at commit or rollback.
func Lock(ctx context.Context, q *relational.Queries, key int64) error {
	if err := q.AdvisoryXactLock(ctx, key); err != nil {
		return fmt.Errorf("advisory lock %#x: %w", key, err)
	}
	return nil
}

// Audit appends e to events inside the caller's transaction (q from
// Store.Tx) and returns its id. It is the only writer of events (a test
// greps for InsertEvent): every mutating path calls it, so the change
// and its audit row commit or roll back together. events is insert-only
// for ussp_app.
func Audit(ctx context.Context, q *relational.Queries, e Event) (int64, error) {
	var errs []error
	for _, f := range []struct{ name, v string }{
		{"actor_type", e.ActorType}, {"actor_id", e.ActorID}, {"entity_type", e.EntityType}, {"event_type", e.EventType},
	} {
		if f.v == "" {
			errs = append(errs, &core.FieldError{Field: f.name, Reason: "required"})
		}
	}
	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("audit: %w", err)
	}
	payload := []byte("{}")
	if e.Payload != nil {
		b, err := json.Marshal(e.Payload)
		if err != nil {
			return 0, fmt.Errorf("audit payload: %w", err)
		}
		payload = b
	}
	if _, err := q.EnsureEventsPartition(ctx); err != nil {
		return 0, fmt.Errorf("audit partition: %w", err)
	}
	row, err := q.InsertEvent(ctx, relational.InsertEventParams{
		ActorType:  e.ActorType,
		ActorID:    e.ActorID,
		Purpose:    nonEmpty(e.Purpose),
		EntityType: e.EntityType,
		EntityID:   nonEmpty(e.EntityID),
		EventType:  e.EventType,
		Payload:    payload,
	})
	if err != nil {
		return 0, fmt.Errorf("audit: %w", err)
	}
	return row.ID, nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
