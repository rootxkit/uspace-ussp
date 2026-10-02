// Package pgstore is internal/ridsp's PlanStore and WorkStore on the
// relational database: dss_isas, the flights' ISA columns, the intents'
// volumes and the dss_outbox items isa_put, isa_delete and isa_notify, through the
// sqlc queries of internal/store/queries/relational/isas.sql, on the
// database clock. Its tests are the integration tests (test/integration).
package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// PlanStore is ridsp.PlanStore on the queries of the caller's transaction.
type PlanStore struct{ Q *relational.Queries }

var _ ridsp.PlanStore = PlanStore{}

// Now implements ridsp.PlanStore.
func (p PlanStore) Now(ctx context.Context) (time.Time, error) { return p.Q.DBNow(ctx) }

// FlightISA implements ridsp.PlanStore.
func (p PlanStore) FlightISA(ctx context.Context, flightID string) (ridsp.ISARecord, bool, error) {
	fid, err := store.UUID("flight_id", flightID)
	if err != nil {
		return ridsp.ISARecord{}, false, err
	}
	r, err := p.Q.GetFlightISA(ctx, fid)
	if store.IsNoRows(err) {
		return ridsp.ISARecord{}, false, nil
	}
	if err != nil {
		return ridsp.ISARecord{}, false, err
	}
	return ridsp.ISARecord{ISAID: r.IsaID, FlightID: flightID, Kind: r.Kind, Version: r.Version, TimeStart: r.TimeStart,
		TimeEnd: r.TimeEnd, Extents: r.Extents, DeletedAt: r.DeletedAt}, true, nil
}

// FlightEnded implements ridsp.PlanStore.
func (p PlanStore) FlightEnded(ctx context.Context, flightID string) (bool, error) {
	fid, err := store.UUID("flight_id", flightID)
	if err != nil {
		return false, err
	}
	f, err := p.Q.GetFlight(ctx, fid)
	if err != nil {
		return false, fmt.Errorf("flight %s: %w", flightID, err)
	}
	return f.EndedAt != nil, nil
}

// IntentVolumes implements ridsp.PlanStore.
func (p PlanStore) IntentVolumes(ctx context.Context, intentID string) ([]byte, error) {
	iid, err := store.UUID("intent_id", intentID)
	if err != nil {
		return nil, err
	}
	r, err := p.Q.GetIntentExtent(ctx, iid)
	if err != nil {
		return nil, fmt.Errorf("intent %s: %w", intentID, err)
	}
	return r.Volumes, nil
}

// InsertISA implements ridsp.PlanStore.
func (p PlanStore) InsertISA(ctx context.Context, r ridsp.ISARecord) error {
	fid, err := store.UUID("flight_id", r.FlightID)
	if err != nil {
		return err
	}
	if _, err := p.Q.InsertISA(ctx, relational.InsertISAParams{IsaID: r.ISAID, FlightID: fid, Kind: r.Kind,
		TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, Extents: r.Extents}); err != nil {
		return fmt.Errorf("ISA %s: %w", r.ISAID, err)
	}
	id := r.ISAID
	return p.Q.SetFlightISA(ctx, relational.SetFlightISAParams{IsaID: &id, FlightID: fid})
}

// SetKind implements ridsp.PlanStore.
func (p PlanStore) SetKind(ctx context.Context, isaID, kind string) error {
	return p.Q.SetISAKind(ctx, relational.SetISAKindParams{Kind: kind, IsaID: isaID})
}

// Enqueue implements ridsp.PlanStore.
func (p PlanStore) Enqueue(ctx context.Context, kind, entityID string, version int64, payload any) (bool, error) {
	return store.Enqueue(ctx, p.Q, kind, entityID, version, payload)
}

// WorkStore is ridsp.WorkStore on the relational database.
type WorkStore struct{ S *store.Store }

var _ ridsp.WorkStore = WorkStore{}

// Claim implements ridsp.WorkStore.
func (p WorkStore) Claim(ctx context.Context, kinds []string, n int) ([]store.OutboxItem, error) {
	return p.S.Queries().ClaimOutboxKinds(ctx, relational.ClaimOutboxKindsParams{
		LeaseS: store.DefaultLease.Seconds(), Kinds: kinds, N: int32(max(0, min(n, store.MaxClaim))),
	})
}

// enqueueNotes queues the notifications inside q's transaction.
func enqueueNotes(ctx context.Context, q *relational.Queries, notes []ridsp.ISANotify) error {
	for _, n := range notes {
		id, v := n.Key()
		if _, err := store.Enqueue(ctx, q, store.OutboxISANotify, id, v, n); err != nil {
			return err
		}
	}
	return nil
}

// Lock implements ridsp.WorkStore: fn runs inside a transaction holding
// the ISA's advisory lock (store.LockClassISA), released when it ends.
func (p WorkStore) Lock(ctx context.Context, isaID string, fn func() error) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		if err := store.LockEntity(ctx, q, store.LockClassISA, isaID); err != nil {
			return err
		}
		return fn()
	})
}

// Done implements ridsp.WorkStore.
func (p WorkStore) Done(ctx context.Context, id int64) error { return p.S.Outbox().Done(ctx, id) }

// Fail implements ridsp.WorkStore.
func (p WorkStore) Fail(ctx context.Context, id int64, cause error, backoff time.Duration) error {
	return p.S.Outbox().Fail(ctx, id, cause, backoff)
}

// ISA implements ridsp.WorkStore.
func (p WorkStore) ISA(ctx context.Context, isaID string) (ridsp.ISARecord, bool, error) {
	r, err := p.S.Queries().GetISAForWrite(ctx, isaID)
	if store.IsNoRows(err) {
		return ridsp.ISARecord{}, false, nil
	}
	if err != nil {
		return ridsp.ISARecord{}, false, err
	}
	return ridsp.ISARecord{ISAID: r.IsaID, FlightID: store.UUIDText(r.FlightID), Kind: r.Kind, Version: r.Version,
		TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, Extents: r.Extents, DeletedAt: r.DeletedAt, FlightEndedAt: r.FlightEndedAt}, true, nil
}

// Now implements ridsp.WorkStore.
func (p WorkStore) Now(ctx context.Context) (time.Time, error) { return p.S.Queries().DBNow(ctx) }

// Written implements ridsp.WorkStore.
func (p WorkStore) Written(ctx context.Context, r ridsp.ISARecord, notes []ridsp.ISANotify) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		if err := q.SetISAWritten(ctx, relational.SetISAWrittenParams{Version: r.Version, TimeStart: r.TimeStart,
			TimeEnd: r.TimeEnd, Extents: r.Extents, IsaID: r.ISAID}); err != nil {
			return err
		}
		if err := enqueueNotes(ctx, q, notes); err != nil {
			return err
		}
		if r.FlightID == "" {
			return nil
		}
		fid, err := store.UUID("flight_id", r.FlightID)
		if err != nil {
			return err
		}
		id := r.ISAID
		return q.SetFlightISA(ctx, relational.SetFlightISAParams{IsaID: &id, IsaVersion: r.Version, FlightID: fid})
	})
}

// Deleted implements ridsp.WorkStore.
func (p WorkStore) Deleted(ctx context.Context, isaID string, notes []ridsp.ISANotify) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		if err := q.SetISADeleted(ctx, isaID); err != nil {
			return err
		}
		return enqueueNotes(ctx, q, notes)
	})
}

// Failed implements ridsp.WorkStore.
func (p WorkStore) Failed(ctx context.Context, isaID, msg string) error {
	return p.S.Queries().SetISAError(ctx, relational.SetISAErrorParams{IsaID: isaID, LastError: &msg})
}

// Backlog implements ridsp.WorkStore.
func (p WorkStore) Backlog(ctx context.Context) (int64, float64, error) {
	b, err := p.S.Queries().OutboxBacklog(ctx, ridsp.ISAKinds)
	return b.Pending, b.OldestAgeS, err
}

// Renew implements ridsp.WorkStore.
func (p WorkStore) Renew(ctx context.Context, beforeS float64, n int, queue func(ridsp.ISARecord) (ridsp.ISAPut, error)) (int, error) {
	queued := 0
	err := p.S.Tx(ctx, func(q *relational.Queries) error {
		queued = 0
		rows, err := q.SessionISAsDue(ctx, relational.SessionISAsDueParams{BeforeS: beforeS, N: int32(max(0, min(n, store.MaxClaim)))})
		if err != nil {
			return err
		}
		now, err := q.DBNow(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			put, err := queue(ridsp.ISARecord{ISAID: r.IsaID, FlightID: store.UUIDText(r.FlightID), Kind: ridsp.ISAKindSession,
				Version: r.Version, TimeEnd: r.TimeEnd, Extents: r.Extents})
			if err != nil {
				return err
			}
			ok, err := store.Enqueue(ctx, q, store.OutboxISAPut, r.IsaID, now.UnixMilli(), put)
			if err != nil {
				return err
			}
			if ok {
				queued++
			}
		}
		return nil
	})
	return queued, err
}
