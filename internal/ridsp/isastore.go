package ridsp

import (
	"context"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// ISARecord is one dss_isas row as the planner and the worker read it,
// with its flight's end.
type ISARecord struct {
	ISAID         string
	FlightID      string
	Kind          string
	Version       *string
	TimeStart     time.Time
	TimeEnd       time.Time
	Extents       []byte
	DeletedAt     *time.Time
	FlightEndedAt *time.Time
}

// PlanStore is the relational database as the planner sees it, inside
// the transaction that records a flight fact.
type PlanStore interface {
	// Now is the database clock.
	Now(ctx context.Context) (time.Time, error)
	// FlightISA is the flight's newest ISA; false when it has none.
	FlightISA(ctx context.Context, flightID string) (ISARecord, bool, error)
	// FlightEnded says whether the flight row is ended.
	FlightEnded(ctx context.Context, flightID string) (bool, error)
	// IntentVolumes is the intent's F3548 volumes as stored (JSON).
	IntentVolumes(ctx context.Context, intentID string) ([]byte, error)
	// InsertISA records a planned ISA and names it on its flight.
	InsertISA(ctx context.Context, r ISARecord) error
	// SetKind changes an ISA's kind.
	SetKind(ctx context.Context, isaID, kind string) error
	// Enqueue queues an outbox item, idempotent by kind, entity and
	// version (store.Enqueue).
	Enqueue(ctx context.Context, kind, entityID string, version int64, payload any) (bool, error)
}

// WorkStore is the relational database as the ISA worker sees it.
type WorkStore interface {
	// Claim leases up to n due isa_put and isa_delete items.
	Claim(ctx context.Context, n int) ([]store.OutboxItem, error)
	Done(ctx context.Context, id int64) error
	Fail(ctx context.Context, id int64, cause error, backoff time.Duration) error
	// ISA is one ISA with its flight's end; false when not recorded.
	ISA(ctx context.Context, isaID string) (ISARecord, bool, error)
	Now(ctx context.Context) (time.Time, error)
	// Written records what the DSS holds (version, window, extents) on
	// the ISA and its flight, in one transaction.
	Written(ctx context.Context, r ISARecord) error
	Deleted(ctx context.Context, isaID string) error
	// Failed records the last error of an ISA.
	Failed(ctx context.Context, isaID, msg string) error
	// Backlog is the number of undone ISA items and the oldest's age.
	Backlog(ctx context.Context) (pending int64, oldestAgeS float64, err error)
	// Renew queues, in one transaction, the item queue makes of every
	// session ISA whose flight goes on, whose time_end is within beforeS
	// and that no isa_put waits for, at most n; it returns how many were
	// queued.
	Renew(ctx context.Context, beforeS float64, n int, queue func(ISARecord) (ISAPut, error)) (int, error)
}

// PGPlanStore is PlanStore on the queries of the caller's transaction.
type PGPlanStore struct{ Q *relational.Queries }

var _ PlanStore = PGPlanStore{}

// Now implements PlanStore.
func (p PGPlanStore) Now(ctx context.Context) (time.Time, error) { return p.Q.DBNow(ctx) }

// FlightISA implements PlanStore.
func (p PGPlanStore) FlightISA(ctx context.Context, flightID string) (ISARecord, bool, error) {
	fid, err := store.UUID("flight_id", flightID)
	if err != nil {
		return ISARecord{}, false, err
	}
	r, err := p.Q.GetFlightISA(ctx, fid)
	if store.IsNoRows(err) {
		return ISARecord{}, false, nil
	}
	if err != nil {
		return ISARecord{}, false, err
	}
	return ISARecord{ISAID: r.IsaID, FlightID: flightID, Kind: r.Kind, Version: r.Version, TimeStart: r.TimeStart,
		TimeEnd: r.TimeEnd, Extents: r.Extents, DeletedAt: r.DeletedAt}, true, nil
}

// FlightEnded implements PlanStore.
func (p PGPlanStore) FlightEnded(ctx context.Context, flightID string) (bool, error) {
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

// IntentVolumes implements PlanStore.
func (p PGPlanStore) IntentVolumes(ctx context.Context, intentID string) ([]byte, error) {
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

// InsertISA implements PlanStore.
func (p PGPlanStore) InsertISA(ctx context.Context, r ISARecord) error {
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

// SetKind implements PlanStore.
func (p PGPlanStore) SetKind(ctx context.Context, isaID, kind string) error {
	return p.Q.SetISAKind(ctx, relational.SetISAKindParams{Kind: kind, IsaID: isaID})
}

// Enqueue implements PlanStore.
func (p PGPlanStore) Enqueue(ctx context.Context, kind, entityID string, version int64, payload any) (bool, error) {
	return store.Enqueue(ctx, p.Q, kind, entityID, version, payload)
}

// PGWorkStore is WorkStore on the relational database.
type PGWorkStore struct{ S *store.Store }

var _ WorkStore = PGWorkStore{}

// Claim implements WorkStore.
func (p PGWorkStore) Claim(ctx context.Context, n int) ([]store.OutboxItem, error) {
	return p.S.Queries().ClaimOutboxKinds(ctx, relational.ClaimOutboxKindsParams{
		LeaseS: store.DefaultLease.Seconds(), Kinds: ISAKinds, N: int32(max(0, min(n, store.MaxClaim))),
	})
}

// Done implements WorkStore.
func (p PGWorkStore) Done(ctx context.Context, id int64) error { return p.S.Outbox().Done(ctx, id) }

// Fail implements WorkStore.
func (p PGWorkStore) Fail(ctx context.Context, id int64, cause error, backoff time.Duration) error {
	return p.S.Outbox().Fail(ctx, id, cause, backoff)
}

// ISA implements WorkStore.
func (p PGWorkStore) ISA(ctx context.Context, isaID string) (ISARecord, bool, error) {
	r, err := p.S.Queries().GetISAForWrite(ctx, isaID)
	if store.IsNoRows(err) {
		return ISARecord{}, false, nil
	}
	if err != nil {
		return ISARecord{}, false, err
	}
	return ISARecord{ISAID: r.IsaID, FlightID: store.UUIDText(r.FlightID), Kind: r.Kind, Version: r.Version,
		TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, Extents: r.Extents, DeletedAt: r.DeletedAt, FlightEndedAt: r.FlightEndedAt}, true, nil
}

// Now implements WorkStore.
func (p PGWorkStore) Now(ctx context.Context) (time.Time, error) { return p.S.Queries().DBNow(ctx) }

// Written implements WorkStore.
func (p PGWorkStore) Written(ctx context.Context, r ISARecord) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		if err := q.SetISAWritten(ctx, relational.SetISAWrittenParams{Version: r.Version, TimeStart: r.TimeStart,
			TimeEnd: r.TimeEnd, Extents: r.Extents, IsaID: r.ISAID}); err != nil {
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

// Deleted implements WorkStore.
func (p PGWorkStore) Deleted(ctx context.Context, isaID string) error {
	return p.S.Queries().SetISADeleted(ctx, isaID)
}

// Failed implements WorkStore.
func (p PGWorkStore) Failed(ctx context.Context, isaID, msg string) error {
	return p.S.Queries().SetISAError(ctx, relational.SetISAErrorParams{IsaID: isaID, LastError: &msg})
}

// Backlog implements WorkStore.
func (p PGWorkStore) Backlog(ctx context.Context) (int64, float64, error) {
	b, err := p.S.Queries().OutboxBacklog(ctx, ISAKinds)
	return b.Pending, b.OldestAgeS, err
}

// Renew implements WorkStore.
func (p PGWorkStore) Renew(ctx context.Context, beforeS float64, n int, queue func(ISARecord) (ISAPut, error)) (int, error) {
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
			put, err := queue(ISARecord{ISAID: r.IsaID, FlightID: store.UUIDText(r.FlightID), Kind: ISAKindSession,
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
