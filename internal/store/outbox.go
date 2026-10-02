package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Outbox kinds (dss_outbox.kind): what dss-sync does with an item.
const (
	OutboxOIRPut     = "oir_put"
	OutboxOIRDelete  = "oir_delete"
	OutboxISAPut     = "isa_put"
	OutboxISADelete  = "isa_delete"
	OutboxISANotify  = "isa_notify"
	OutboxPeerNotify = "peer_notify"
	OutboxUSSReport  = "uss_report"
)

// OutboxItem is one queued item.
type OutboxItem = relational.DssOutbox

// Bounds of the outbox.
const (
	// MaxClaim bounds one Claim: a larger n is cut to it (E-10).
	MaxClaim = 100
	// DefaultLease is how long a claimed item stays with its worker
	// before another may claim it again.
	DefaultLease = 60 * time.Second
	// maxErrorLen bounds the stored last_error.
	maxErrorLen = 1000
)

// ErrNotPending is returned by Done and Fail for an item that does not
// exist or is already done.
var ErrNotPending = errors.New("outbox item is not pending")

// Enqueue queues one change inside the caller's transaction, so the
// change and the work it implies commit together. It is idempotent by
// (kind, entityID, entityVersion): it returns false when that change is
// already queued (or done).
func Enqueue(ctx context.Context, q *relational.Queries, kind, entityID string, entityVersion int64, payload any) (bool, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("outbox payload: %w", err)
	}
	_, err = q.EnqueueOutbox(ctx, relational.EnqueueOutboxParams{Kind: kind, EntityID: entityID, EntityVersion: entityVersion, Payload: b})
	if IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("outbox enqueue: %w", err)
	}
	return true, nil
}

// Outbox is the worker side of dss_outbox.
type Outbox struct {
	q *relational.Queries
	// Lease is how long a claimed item stays with its worker; 0 is
	// DefaultLease.
	Lease time.Duration
}

// Outbox is the worker side of the relational store's outbox.
func (s *Store) Outbox() *Outbox { return &Outbox{q: s.Queries()} }

// Claim takes up to n due items (at most MaxClaim), oldest due first,
// skipping items another worker holds; each is leased for Lease and its
// attempt counted.
func (o *Outbox) Claim(ctx context.Context, n int) ([]OutboxItem, error) {
	if n <= 0 {
		return nil, nil
	}
	n = min(n, MaxClaim)
	lease := o.Lease
	if lease <= 0 {
		lease = DefaultLease
	}
	items, err := o.q.ClaimOutbox(ctx, relational.ClaimOutboxParams{LeaseS: lease.Seconds(), N: int32(n)})
	if err != nil {
		return nil, fmt.Errorf("outbox claim: %w", err)
	}
	return items, nil
}

// Done marks the item done.
func (o *Outbox) Done(ctx context.Context, id int64) error {
	n, err := o.q.MarkOutboxDone(ctx, id)
	if err != nil {
		return fmt.Errorf("outbox done: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("outbox item %d: %w", id, ErrNotPending)
	}
	return nil
}

// Fail records cause and makes the item due again after backoff.
func (o *Outbox) Fail(ctx context.Context, id int64, cause error, backoff time.Duration) error {
	msg := "unknown error"
	if cause != nil {
		msg = cause.Error()
	}
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen]
	}
	n, err := o.q.MarkOutboxFailed(ctx, relational.MarkOutboxFailedParams{ID: id, BackoffS: max(backoff, 0).Seconds(), LastError: &msg})
	if err != nil {
		return fmt.Errorf("outbox fail: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("outbox item %d: %w", id, ErrNotPending)
	}
	return nil
}
