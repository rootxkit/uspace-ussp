// Package pgstore is internal/status's Store on the relational database:
// operating_status_notices (migrations 00005, 00021, 00023) through the sqlc
// queries of internal/store/queries/relational/status.sql. Every stored
// notice and every outcome writes its events row in the same
// transaction. Its tests are the integration tests (test/integration).
package pgstore

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/status"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is status.Store on PostgreSQL.
type Store struct{ S *store.Store }

var _ status.Store = Store{}

// EntityNotice is the events entity of a notice.
const EntityNotice = "operating_status_notice"

func secs(d time.Duration) float64 { return math.Max(d.Seconds(), 0) }

func clip(s string) *string {
	if len(s) > 1000 {
		s = s[:1000]
	}
	return &s
}

func noticeOf(r relational.OperatingStatusNotice) status.Notice {
	n := status.Notice{ID: store.UUIDText(r.ID), Kind: r.Kind, At: r.At, CertificateID: r.CertificateID, Reference: r.Reference,
		RequestedBy: r.RequestedBy, State: r.State, Attempts: int(r.Attempts), LastError: r.LastError, SubmittedAt: r.SubmittedAt,
		AuthorityRef: r.AuthorityRef, FailedAt: r.FailedAt, CreatedAt: r.CreatedAt}
	if r.State == "pending" {
		next := r.NextAt
		n.NextAt = &next
	}
	return n
}

// Notices implements status.Store.
func (p Store) Notices(ctx context.Context, certificateID string, n int) ([]status.Notice, error) {
	rows, err := p.S.Queries().StatusNotices(ctx, relational.StatusNoticesParams{CertificateID: certificateID, N: int32(min(max(n, 1), 1000))})
	if err != nil {
		return nil, fmt.Errorf("status notices: %w", err)
	}
	out := make([]status.Notice, 0, len(rows))
	for i := range rows {
		out = append(out, noticeOf(rows[i]))
	}
	return out, nil
}

// The unique indexes Insert tells apart (00021, 00023).
const (
	indexOneStart     = "operating_status_notices_one_start_idx"
	indexOneSuccessor = "operating_status_notices_one_successor_idx"
)

// Insert implements status.Store: the notice and its events row commit
// together; a second start is status.ErrStartExists, a second notice
// after the same one status.ErrFollowed.
func (p Store) Insert(ctx context.Context, kind, certificateID, reference, requestedBy, follows string) (status.Notice, error) {
	params := relational.StatusInsertParams{Kind: kind, CertificateID: certificateID, Reference: reference, RequestedBy: requestedBy}
	if follows != "" {
		f, err := store.UUID("follows", follows)
		if err != nil {
			return status.Notice{}, err
		}
		params.Follows = f
	}
	var out status.Notice
	err := p.S.Tx(ctx, func(q *relational.Queries) error {
		r, err := q.StatusInsert(ctx, params)
		if store.SQLState(err) == store.StateUniqueViolation {
			switch store.ConstraintName(err) {
			case indexOneStart:
				return status.ErrStartExists
			case indexOneSuccessor:
				return status.ErrFollowed
			}
		}
		if err != nil {
			return fmt.Errorf("status notice: %w", err)
		}
		out = noticeOf(r)
		_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorStaff, ActorID: requestedBy, EntityType: EntityNotice, EntityID: reference,
			EventType: "operating_status_notice_requested", Payload: map[string]any{"kind": kind, "certificate_id": certificateID}})
		return err
	})
	return out, err
}

// Claim implements status.Store.
func (p Store) Claim(ctx context.Context, n int, lease time.Duration) ([]status.Queued, error) {
	rows, err := p.S.Queries().StatusClaim(ctx, relational.StatusClaimParams{LeaseS: secs(lease), N: int32(min(max(n, 1), 100))})
	if err != nil {
		return nil, fmt.Errorf("status claim: %w", err)
	}
	out := make([]status.Queued, 0, len(rows))
	for _, r := range rows {
		out = append(out, status.Queued{ID: store.UUIDText(r.ID), Kind: r.Kind, At: r.At, CertificateID: r.CertificateID, Reference: r.Reference, Attempts: int(r.Attempts)})
	}
	return out, nil
}

// Delivered implements status.Store.
func (p Store) Delivered(ctx context.Context, id, authorityRef string) error {
	uid, err := store.UUID("id", id)
	if err != nil {
		return err
	}
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		r, err := q.StatusDelivered(ctx, relational.StatusDeliveredParams{AuthorityRef: &authorityRef, ID: uid})
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("status delivered: %w", err)
		}
		_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "status", EntityType: EntityNotice, EntityID: r.Reference,
			EventType: "operating_status_notice_recorded", Payload: map[string]any{"kind": r.Kind, "authority_ref": authorityRef}})
		return err
	})
}

// Retry implements status.Store.
func (p Store) Retry(ctx context.Context, id, cause string, backoff time.Duration) error {
	uid, err := store.UUID("id", id)
	if err != nil {
		return err
	}
	if _, err := p.S.Queries().StatusRetry(ctx, relational.StatusRetryParams{BackoffS: secs(backoff), LastError: clip(cause), ID: uid}); err != nil {
		return fmt.Errorf("status retry: %w", err)
	}
	return nil
}

// Fail implements status.Store.
func (p Store) Fail(ctx context.Context, id, cause string) error {
	uid, err := store.UUID("id", id)
	if err != nil {
		return err
	}
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		r, err := q.StatusFail(ctx, relational.StatusFailParams{LastError: clip(cause), ID: uid})
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("status fail: %w", err)
		}
		_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "status", EntityType: EntityNotice, EntityID: r.Reference,
			EventType: "operating_status_notice_failed", Payload: map[string]any{"kind": r.Kind, "cause": cause}})
		return err
	})
}
