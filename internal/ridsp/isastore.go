package ridsp

import (
	"context"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/store"
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
	// Lock runs fn holding the ISA's lock, which every worker in every
	// process takes before it handles a put or a delete of that ISA: a
	// delete never reads the ISA while a put of it is under way, nor the
	// other way round.
	Lock(ctx context.Context, isaID string, fn func() error) error
	// Claim leases up to n due items of the given kinds (ISAKinds or
	// NotifyKinds).
	Claim(ctx context.Context, kinds []string, n int) ([]store.OutboxItem, error)
	Done(ctx context.Context, id int64) error
	Fail(ctx context.Context, id int64, cause error, backoff time.Duration) error
	// ISA is one ISA with its flight's end; false when not recorded.
	ISA(ctx context.Context, isaID string) (ISARecord, bool, error)
	Now(ctx context.Context) (time.Time, error)
	// Written records what the DSS holds (version, window, extents) on
	// the ISA and its flight and queues the subscribers' notifications
	// (isa_notify, idempotent by ISANotify.Key), in one transaction.
	Written(ctx context.Context, r ISARecord, notes []ISANotify) error
	// Deleted records the ISA deleted and queues the notifications, in
	// one transaction.
	Deleted(ctx context.Context, isaID string, notes []ISANotify) error
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
