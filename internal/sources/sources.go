// Package sources holds the source switches (docs/PLAN.md D11; LESSONS
// B-09, B-10, B-11) on the uspace-core sources model. This WP builds the
// writer side, which runs in api: Switch writes a switch with a version
// from a database sequence and the database's epoch under an advisory
// lock, projects the whole state to the KV bucket source_control inside
// the same transaction and refuses the switch (503-shaped) when the KV
// cannot take it; Republish rewrites the bucket from the database every
// RepublishInterval, repairing a bucket that was lost or got ahead of a
// rolled-back transaction; List reads the switches. The follower side
// (core sources.Follower behind a KV watch) is WP-6's.
package sources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// BucketSourceControl is the KV bucket the state is projected to (D6).
const BucketSourceControl = "source_control"

// DefaultRepublishInterval is how often Run republishes (B-09 repair).
const DefaultRepublishInterval = 60 * time.Second

// EventSourceSwitched is the audit event of a switch.
const EventSourceSwitched = "source_switched"

// Counter names of the writer.
const (
	CounterSwitched         = "source_switched"
	CounterProjectionFailed = "source_switch_projection_failed" // a switch refused because the KV projection failed
	CounterRepublished      = "source_republished"
	CounterRepublishFailed  = "source_republish_failed"
)

// Projector writes a source-control state to the KV bucket the hot path
// follows (WP-6 implements it on internal/bus).
type Projector interface {
	ProjectSources(ctx context.Context, s coresources.State) error
}

// Row is one stored switch with who set it and why.
type Row struct {
	Control   coresources.Control
	Reason    string
	Actor     string
	ChangedAt time.Time
	Version   int64
	Epoch     string
}

// Writer is api's side of the switches.
type Writer struct {
	Store     *store.Store
	Projector Projector
	Counters  *core.Counters
	Logger    *slog.Logger
	// RepublishInterval is Run's period; 0 is DefaultRepublishInterval.
	RepublishInterval time.Duration
}

func (w *Writer) counters() *core.Counters {
	if w.Counters == nil {
		w.Counters = &core.Counters{}
	}
	return w.Counters
}

// Switch sets one switch (a whole type when c.InstanceID is nil) and
// returns the state it published. The row, its audit event and the KV
// projection are written in one transaction under LockSources; the
// version comes from source_control_version_seq and the epoch from the
// database's source_control_epoch row. When the projection fails the
// transaction rolls back, nothing changes and the error is a
// *policy.ProjectionError (503-shaped; B-09). Callers that refuse a
// disabled source answer 503 with Retry-After, never 401 or 403 (B-10).
func (w *Writer) Switch(ctx context.Context, actor, reason string, c coresources.Control) (coresources.State, error) {
	var errs []error
	if c.SourceType == "" {
		errs = append(errs, &core.FieldError{Field: "source_type", Reason: "required"})
	}
	if c.InstanceID != nil && *c.InstanceID == "" {
		errs = append(errs, &core.FieldError{Field: "instance_id", Reason: "empty; omit it to switch the whole type"})
	}
	if actor == "" {
		errs = append(errs, &core.FieldError{Field: "actor", Reason: "required"})
	}
	if reason == "" {
		errs = append(errs, &core.FieldError{Field: "reason", Reason: "required"})
	}
	if err := errors.Join(errs...); err != nil {
		return coresources.State{}, err
	}
	var out coresources.State
	err := w.Store.Tx(ctx, func(q *relational.Queries) error {
		if err := store.Lock(ctx, q, store.LockSources); err != nil {
			return err
		}
		epoch, version, err := next(ctx, q)
		if err != nil {
			return err
		}
		if _, err := q.UpsertSourceControl(ctx, relational.UpsertSourceControlParams{
			SourceType: c.SourceType, InstanceID: c.InstanceID, Enabled: c.Enabled,
			Reason: reason, Actor: actor, Version: version, Epoch: epoch,
		}); err != nil {
			return fmt.Errorf("upsert source control: %w", err)
		}
		entity := c.SourceType
		if c.InstanceID != nil {
			entity += "/" + *c.InstanceID
		}
		if _, err := store.Audit(ctx, q, store.Event{
			ActorType: store.ActorStaff, ActorID: actor,
			EntityType: "source_control", EntityID: entity, EventType: EventSourceSwitched,
			Payload: map[string]any{"source_type": c.SourceType, "instance_id": c.InstanceID, "enabled": c.Enabled,
				"reason": reason, "version": version, "epoch": epoch},
		}); err != nil {
			return err
		}
		if out, err = state(ctx, q, epoch, version); err != nil {
			return err
		}
		if err := w.Projector.ProjectSources(ctx, out); err != nil {
			w.counters().Inc(CounterProjectionFailed)
			return &policy.ProjectionError{Bucket: BucketSourceControl, Err: err}
		}
		return nil
	})
	if err != nil {
		return coresources.State{}, fmt.Errorf("source switch: %w", err)
	}
	w.counters().Inc(CounterSwitched)
	return out, nil
}

// Republish writes the database's state to the KV bucket under a fresh
// version from the sequence, so a follower takes it even when the
// bucket was lost or holds a version from a transaction that rolled
// back after projecting. It writes no audit event: nothing changed.
func (w *Writer) Republish(ctx context.Context) (coresources.State, error) {
	var out coresources.State
	err := w.Store.Tx(ctx, func(q *relational.Queries) error {
		if err := store.Lock(ctx, q, store.LockSources); err != nil {
			return err
		}
		epoch, version, err := next(ctx, q)
		if err != nil {
			return err
		}
		if out, err = state(ctx, q, epoch, version); err != nil {
			return err
		}
		if err := w.Projector.ProjectSources(ctx, out); err != nil {
			return &policy.ProjectionError{Bucket: BucketSourceControl, Err: err}
		}
		return nil
	})
	if err != nil {
		w.counters().Inc(CounterRepublishFailed)
		return coresources.State{}, fmt.Errorf("source republish: %w", err)
	}
	w.counters().Inc(CounterRepublished)
	return out, nil
}

// Run republishes at once and then every RepublishInterval until ctx
// ends; a failure is counted and logged and the next tick retries.
func (w *Writer) Run(ctx context.Context) {
	interval := w.RepublishInterval
	if interval <= 0 {
		interval = DefaultRepublishInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := w.Republish(ctx); err != nil && ctx.Err() == nil && w.Logger != nil {
			w.Logger.LogAttrs(ctx, slog.LevelWarn, "source control republish failed",
				slog.String("bucket", BucketSourceControl), slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// List is every stored switch with its reason and actor, and the state
// they make (Version is the highest row version).
func (w *Writer) List(ctx context.Context) ([]Row, coresources.State, error) {
	q := w.Store.Queries()
	epoch, err := q.SourceControlEpoch(ctx)
	if err != nil {
		return nil, coresources.State{}, fmt.Errorf("source control epoch: %w", err)
	}
	rows, err := q.ListSourceControls(ctx)
	if err != nil {
		return nil, coresources.State{}, fmt.Errorf("list source controls: %w", err)
	}
	out := make([]Row, 0, len(rows))
	st := coresources.State{Epoch: epoch, Controls: make([]coresources.Control, 0, len(rows))}
	for _, r := range rows {
		c := coresources.Control{SourceType: r.SourceType, InstanceID: r.InstanceID, Enabled: r.Enabled}
		out = append(out, Row{Control: c, Reason: r.Reason, Actor: r.Actor, ChangedAt: r.ChangedAt, Version: r.Version, Epoch: r.Epoch})
		st.Controls = append(st.Controls, c)
		st.Version = max(st.Version, uint64(r.Version))
	}
	return out, st, nil
}

// next is the database's epoch and a fresh version.
func next(ctx context.Context, q *relational.Queries) (string, int64, error) {
	epoch, err := q.SourceControlEpoch(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("source control epoch: %w", err)
	}
	version, err := q.NextSourceControlVersion(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("source control version: %w", err)
	}
	return epoch, version, nil
}

// state is every stored switch as a core state at version. DefaultDeny
// is false: an instance with no row is enabled (B-09: never fail closed
// by omission).
func state(ctx context.Context, q *relational.Queries, epoch string, version int64) (coresources.State, error) {
	rows, err := q.ListSourceControls(ctx)
	if err != nil {
		return coresources.State{}, fmt.Errorf("list source controls: %w", err)
	}
	st := coresources.State{Version: uint64(version), Epoch: epoch, Controls: make([]coresources.Control, 0, len(rows))}
	for _, r := range rows {
		st.Controls = append(st.Controls, coresources.Control{SourceType: r.SourceType, InstanceID: r.InstanceID, Enabled: r.Enabled})
	}
	return st, nil
}
