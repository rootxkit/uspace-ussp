package dss

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// PeerRecord is one peer_intents row: a peer's operational intent as its
// manager gave it (trust provider, never altered).
type PeerRecord struct {
	EntityID        string
	Manager         string
	USSBaseURL      string
	State           string
	OVN             string
	Version         int64
	TimeStart       time.Time
	TimeEnd         time.Time
	Priority        int
	Details         json.RawMessage
	FetchedAt       time.Time
	PeerUnavailable bool
}

// ConstraintRecord is one constraints row.
type ConstraintRecord struct {
	EntityID         string
	Manager          string
	USSBaseURL       string
	OVN              string
	Version          int64
	TimeStart        time.Time
	TimeEnd          time.Time
	Details          json.RawMessage
	CISRestrictionID string
	FetchedAt        time.Time
}

// Area is one area of interest: a U-space airspace's id and box.
type Area struct {
	ID  string       `json:"id"`
	Box geodesy.BBox `json:"box"`
}

// SubscriptionRecord is one dss_subscriptions row of kind utm: the
// subscription of one area of interest (its area is the box and the id
// of the U-space airspace it covers).
type SubscriptionRecord struct {
	ID                string
	Area              Area
	Version           string
	NotificationIndex int32
	TimeEnd           time.Time
	USSBaseURL        string
	RenewedAt         *time.Time
}

// StateRecord is the dss_state singleton.
type StateRecord struct {
	Availability        string
	SetBy               string
	SetAt               *time.Time
	ReachableSince      *time.Time
	UnreachableSince    *time.Time
	AvailabilityUnknown bool
}

// PurgeCounts is what one purge removed.
type PurgeCounts struct {
	PeerIntents, Constraints, Exchanges, Reports int64
}

// Store is the relational database as internal/dss uses it
// (internal/dss/pgstore in api, the only relational writer, D5).
type Store interface {
	ExchangeStore
	// Now is the database clock.
	Now(ctx context.Context) (time.Time, error)
	// Lock runs fn holding the transaction-scoped advisory lock of the
	// entity in class (store.LockClassOIR, store.LockClassSub), which
	// every writer in every process takes first: two writes of one
	// entity never interleave.
	Lock(ctx context.Context, class int32, entityID string, fn func() error) error
	// Claim leases up to n due outbox items of the kinds.
	Claim(ctx context.Context, kinds []string, n int) ([]store.OutboxItem, error)
	Done(ctx context.Context, id int64) error
	Fail(ctx context.Context, id int64, cause error, backoff time.Duration) error
	// DoneByKey marks the item of the key done (a notification posted
	// inline); false when none was pending.
	DoneByKey(ctx context.Context, kind, entityID string, version int64) (bool, error)
	// Backlog is the number of undone items of the kinds, the oldest's
	// age and the most attempts one has taken.
	Backlog(ctx context.Context, kinds []string) (n int64, oldestS float64, maxAttempts int32, err error)

	// UpsertPeerIntent stores a peer's intent unless the row holds a newer
	// version; true when written.
	UpsertPeerIntent(ctx context.Context, p PeerRecord) (bool, error)
	// PeerIntent is the stored peer intent; nil when none.
	PeerIntent(ctx context.Context, entityID string) (*PeerRecord, error)
	// DeletePeerIntent removes the manager's peer intent; true when one was.
	DeletePeerIntent(ctx context.Context, entityID, manager string) (bool, error)
	// MarkPeerUnavailable marks (or clears) every stored intent of a peer.
	MarkPeerUnavailable(ctx context.Context, ussBaseURL string, unavailable bool) (int64, error)
	UpsertConstraint(ctx context.Context, c ConstraintRecord) (bool, error)
	Constraint(ctx context.Context, entityID string) (*ConstraintRecord, error)
	DeleteConstraint(ctx context.Context, entityID, manager string) (bool, error)

	Subscriptions(ctx context.Context) ([]SubscriptionRecord, error)
	UpsertSubscription(ctx context.Context, s SubscriptionRecord) error
	DeleteSubscription(ctx context.Context, id string) error
	// Notified advances the notification index of one of our
	// subscriptions to idx and returns the one it held; ours false when
	// the subscription is not ours.
	Notified(ctx context.Context, subscriptionID string, idx int32) (previous int32, ours bool, err error)

	State(ctx context.Context) (StateRecord, error)
	// SetAvailability records this USSP's availability as the DSS gave
	// it; true when it changed.
	SetAvailability(ctx context.Context, availability, setBy string) (bool, error)
	SetReachable(ctx context.Context, up bool) error

	// Exchanges are the recorded exchanges of an entity, oldest first.
	Exchanges(ctx context.Context, entityID string, limit int) ([]Exchange, error)
	// InsertReport stores a report a peer sent.
	InsertReport(ctx context.Context, reportID, reporter string, exchange json.RawMessage) error
	// Purge removes the peers' intents and constraints fetched before
	// peersBefore that no decision names, the exchanges and reports
	// recorded before logBefore, and the exchanges beyond maxExchanges.
	Purge(ctx context.Context, peersBefore, logBefore time.Time, maxExchanges int64) (PurgeCounts, error)
	// Audit appends an audit event (store.Audit) in its own transaction.
	Audit(ctx context.Context, e store.Event) error
}

// Intents is the intent service as internal/dss uses it
// (*intent.Service).
type Intents interface {
	Record(ctx context.Context, id string) (*intent.Record, error)
	Held(ctx context.Context, id string) (*intent.DSSHeld, error)
	PeerCheck(ctx context.Context, id string, version int) (intent.PeerCheckResult, error)
	RecreateCheck(ctx context.Context, id string, version int) (intent.PeerCheckResult, error)
	DSSHold(ctx context.Context, id string, version int, reason, detail string) error
	DSSAuthorise(ctx context.Context, id string, version int, held intent.DSSHeld, notes []intent.OutboxSpec) (bool, error)
	DSSRecord(ctx context.Context, id string, held *intent.DSSHeld, notes []intent.OutboxSpec) error
	PeerConflicts(ctx context.Context, p intent.PeerIntent) ([]intent.PeerConflict, error)
	PeerIntentDisplaced(ctx context.Context, id, peerEntityID string) (intent.RecheckResult, error)
}

var _ Intents = (*intent.Service)(nil)

// ConstraintRechecker is the standing re-check of WP-12 for a constraint
// notification (*geo.Rechecker).
type ConstraintRechecker interface {
	Constraint(ctx context.Context, ref string, boxes []geodesy.BBox, from, to *time.Time) []intent.RecheckResult
}

// Telemetry is the newest position of an intent's flight from the
// time-series window (GET .../telemetry).
type Telemetry interface {
	// Latest is the newest sample of the intent's newest flight captured
	// after since; false when there is none.
	Latest(ctx context.Context, intentID string, since time.Time) (*f3548.VehicleTelemetry, bool, error)
}
