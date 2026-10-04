package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// DepDSS is the name of api's DSS dependency on /readyz.
const DepDSS = "dss"

// DSSState is the dss_state singleton.
type DSSState struct {
	USSAvailability     *string    `json:"uss_availability,omitempty"`
	SetBy               *string    `json:"set_by,omitempty"`
	SetAt               *time.Time `json:"set_at,omitempty"`
	DSSReachableSince   *time.Time `json:"dss_reachable_since,omitempty"`
	DSSUnreachableSince *time.Time `json:"dss_unreachable_since,omitempty"`
}

// Outbox is the depth of the DSS outbox.
type Outbox struct {
	Pending         int64            `json:"pending"`
	ByKind          map[string]int64 `json:"by_kind"`
	OldestCreatedAt *time.Time       `json:"oldest_created_at,omitempty"`
	Retrying        int64            `json:"retrying"`
}

// OutboxError is one outbox item with its last error.
type OutboxError struct {
	Kind      string     `json:"kind"`
	EntityID  string     `json:"entity_id"`
	Attempts  int32      `json:"attempts"`
	LastError string     `json:"last_error"`
	NextAt    time.Time  `json:"next_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
}

// Subscription is one DSS subscription held.
type Subscription struct {
	SubscriptionID    string     `json:"subscription_id"`
	Kind              string     `json:"kind"`
	TimeEnd           time.Time  `json:"time_end"`
	RenewedAt         *time.Time `json:"renewed_at,omitempty"`
	NotificationIndex int32      `json:"notification_index"`
}

// DSS is GET /v1/admin/dss.
type DSS struct {
	DSSState      DSSState             `json:"dss_state"`
	Readiness     obs.DependencyStatus `json:"readiness"`
	Outbox        Outbox               `json:"outbox"`
	LastErrors    []OutboxError        `json:"last_errors"`
	Subscriptions []Subscription       `json:"subscriptions"`
}

// DSS is the DSS panel: dss_state, the readiness of the DSS dependency
// (unknown when this process does not report one), the outbox depth by
// kind, its last errors and the subscriptions.
func (s *Service) DSS(ctx context.Context) (DSS, error) {
	q := s.Store.Queries()
	st, err := q.DSSStateGet(ctx)
	if err != nil {
		return DSS{}, fmt.Errorf("dss_state: %w", err)
	}
	depth, err := q.AdminOutboxDepth(ctx)
	if err != nil {
		return DSS{}, fmt.Errorf("outbox depth: %w", err)
	}
	errs, err := q.AdminOutboxErrors(ctx, MaxOutboxErrors)
	if err != nil {
		return DSS{}, fmt.Errorf("outbox errors: %w", err)
	}
	subs, err := q.AdminSubscriptions(ctx, MaxSubs)
	if err != nil {
		return DSS{}, fmt.Errorf("subscriptions: %w", err)
	}
	out := DSS{
		DSSState: DSSState{USSAvailability: st.UssAvailability, SetBy: st.SetBy, SetAt: utc(st.SetAt),
			DSSReachableSince: utc(st.DssReachableSince), DSSUnreachableSince: utc(st.DssUnreachableSince)},
		Outbox:        Outbox{ByKind: map[string]int64{}},
		LastErrors:    make([]OutboxError, 0, len(errs)),
		Subscriptions: make([]Subscription, 0, len(subs)),
		Readiness:     obs.DependencyStatus{State: obs.StateUnknown, Since: s.now().UTC(), Detail: "this process reports no DSS dependency"},
	}
	for _, d := range depth {
		out.Outbox.Pending += d.Pending
		out.Outbox.Retrying += d.Retrying
		out.Outbox.ByKind[d.Kind] = d.Pending
		if o := d.OldestCreatedAt.UTC(); out.Outbox.OldestCreatedAt == nil || o.Before(*out.Outbox.OldestCreatedAt) {
			out.Outbox.OldestCreatedAt = &o
		}
	}
	for _, e := range errs {
		msg := ""
		if e.LastError != nil {
			msg = *e.LastError
		}
		out.LastErrors = append(out.LastErrors, OutboxError{Kind: e.Kind, EntityID: e.EntityID, Attempts: e.Attempts, LastError: msg,
			NextAt: e.NextAt.UTC(), DoneAt: utc(e.DoneAt)})
	}
	for _, sb := range subs {
		out.Subscriptions = append(out.Subscriptions, Subscription{SubscriptionID: sb.SubscriptionID, Kind: sb.Kind, TimeEnd: sb.TimeEnd.UTC(),
			RenewedAt: utc(sb.RenewedAt), NotificationIndex: sb.NotificationIndex})
	}
	if s.Health != nil {
		if d, ok := s.Health(ctx).Dependencies[DepDSS]; ok {
			out.Readiness = d
		}
	}
	return out, nil
}
