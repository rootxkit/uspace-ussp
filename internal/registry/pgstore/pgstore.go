// Package pgstore is internal/registry's Store on the relational
// database: registry_validity, registry_invalidations and registry_feed
// (migrations 00004 and 00010), through the sqlc queries of
// internal/store/queries/relational/registry.sql. Every age and every
// timestamp is the database clock. Its tests are the integration tests
// (test/integration), against PostgreSQL.
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/registry"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is registry.Store on the relational database.
type Store struct{ S *store.Store }

var _ registry.Store = Store{}

// Entries reads the cached entries of keys and the feed's cursor.
func (p Store) Entries(ctx context.Context, keys []registry.Key) ([]registry.Cached, int64, error) {
	q := p.S.Queries()
	cur, err := q.RegistryFeedCursor(ctx)
	switch {
	case store.IsNoRows(err):
		cur.Since = 0
	case err != nil:
		return nil, 0, fmt.Errorf("registry_feed: %w", err)
	}
	if len(keys) == 0 {
		return nil, cur.Since, nil
	}
	params := relational.RegistryValidityByKeysParams{EntityTypes: make([]string, len(keys)), Keys: make([]string, len(keys))}
	for i, k := range keys {
		params.EntityTypes[i], params.Keys[i] = string(k.Entity), k.Key
	}
	rows, err := q.RegistryValidityByKeys(ctx, params)
	if err != nil {
		return nil, 0, fmt.Errorf("registry_validity: %w", err)
	}
	out := make([]registry.Cached, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		c, err := cached(r.EntityType, r.Key, r.KeyFold, r.Status, r.ValidUntil, r.ClassLabel, r.MtomBand, r.Competencies, r.FetchedAt, r.AgeS)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, cur.Since, nil
}

// All reads every cached entry, newest first, at most limit.
func (p Store) All(ctx context.Context, limit int) ([]registry.Cached, error) {
	if limit <= 0 || limit > 1_000_000 {
		limit = 1_000_000
	}
	rows, err := p.S.Queries().RegistryValidityAll(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("registry_validity: %w", err)
	}
	out := make([]registry.Cached, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		c, err := cached(r.EntityType, r.Key, r.KeyFold, r.Status, r.ValidUntil, r.ClassLabel, r.MtomBand, r.Competencies, r.FetchedAt, r.AgeS)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func cached(entity, key, fold, status string, validUntil *time.Time, class, band *string, comps []byte, fetched time.Time, ageS float64) (registry.Cached, error) {
	e := registry.Entry{Key: registry.Key{Entity: registry.EntityType(entity), Key: key}, KeyFold: fold, Status: registry.Status(status),
		ValidUntil: validUntil, FetchedAt: fetched.UTC()}
	if class != nil {
		e.ClassLabel = *class
	}
	if band != nil {
		e.MTOMBand = *band
	}
	if len(comps) > 0 {
		if err := json.Unmarshal(comps, &e.Competencies); err != nil {
			return registry.Cached{}, fmt.Errorf("registry_validity %s/%s competencies: %w", entity, key, err)
		}
	}
	return registry.Cached{Entry: e, AgeS: ageS}, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Save writes the entries in one transaction, skipping an entry whose
// key the feed invalidated after since, and projects the written ones
// before the commit.
func (p Store) Save(ctx context.Context, es []registry.Entry, since int64, project func(context.Context, []registry.Entry) error) ([]registry.Entry, error) {
	var written []registry.Entry
	err := p.S.Tx(ctx, func(q *relational.Queries) error {
		written = written[:0]
		for i := range es {
			e := es[i]
			var comps []byte
			if e.Entity == registry.EntityPilot {
				c := e.Competencies
				if c == nil {
					c = []registry.Competency{}
				}
				b, err := json.Marshal(c)
				if err != nil {
					return err
				}
				comps = b
			}
			at, err := q.UpsertRegistryValidity(ctx, relational.UpsertRegistryValidityParams{
				EntityType: string(e.Entity), Key: e.Key.Key, KeyFold: e.KeyFold, Status: string(e.Status), ValidUntil: e.ValidUntil,
				ClassLabel: optional(e.ClassLabel), MtomBand: optional(e.MTOMBand), Competencies: comps, Negative: e.Negative(), Since: since,
			})
			if store.IsNoRows(err) {
				continue // invalidated while it was fetched
			}
			if err != nil {
				return fmt.Errorf("registry_validity %s: %w", e.Entity, err)
			}
			e.FetchedAt = at.UTC()
			written = append(written, e)
		}
		return project(ctx, written)
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// Invalidate deletes, remembers and moves the cursor in one
// transaction.
func (p Store) Invalidate(ctx context.Context, inv []registry.Invalidation, next int64, etag string, keep time.Duration, project func(context.Context, []registry.Key) error) ([]registry.Key, error) {
	var deleted []registry.Key
	err := p.S.Tx(ctx, func(q *relational.Queries) error {
		deleted = deleted[:0]
		for _, in := range inv {
			rows, err := q.DeleteRegistryValidityByFold(ctx, relational.DeleteRegistryValidityByFoldParams{EntityType: string(in.Entity), KeyFold: in.KeyFold})
			if err != nil {
				return fmt.Errorf("registry_validity: %w", err)
			}
			for _, r := range rows {
				deleted = append(deleted, registry.Key{Entity: registry.EntityType(r.EntityType), Key: r.Key})
			}
			if err := q.RecordRegistryInvalidation(ctx, relational.RecordRegistryInvalidationParams{
				EntityType: string(in.Entity), KeyFold: in.KeyFold, Seq: in.Seq,
			}); err != nil {
				return fmt.Errorf("registry_invalidations: %w", err)
			}
		}
		if _, err := q.PruneRegistryInvalidations(ctx, keep.Seconds()); err != nil {
			return fmt.Errorf("registry_invalidations: %w", err)
		}
		if err := q.SetRegistryFeedCursor(ctx, relational.SetRegistryFeedCursorParams{Since: next, Etag: optional(etag)}); err != nil {
			return fmt.Errorf("registry_feed: %w", err)
		}
		return project(ctx, deleted)
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// Cursor reads the feed's cursor.
func (p Store) Cursor(ctx context.Context) (registry.FeedCursor, error) {
	r, err := p.S.Queries().RegistryFeedCursor(ctx)
	if store.IsNoRows(err) {
		return registry.FeedCursor{}, nil
	}
	if err != nil {
		return registry.FeedCursor{}, fmt.Errorf("registry_feed: %w", err)
	}
	c := registry.FeedCursor{Since: r.Since, AgeS: &r.AgeS}
	if r.Etag != nil {
		c.ETag = *r.Etag
	}
	return c, nil
}

var _ registry.Auditor = Store{}

// RecordValidated writes the events row of a lookup.
func (p Store) RecordValidated(ctx context.Context, v registry.Validated) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		_, err := store.Audit(ctx, q, store.Event{
			ActorType: v.ActorType, ActorID: v.ActorID, Purpose: string(v.Purpose), EntityType: "registry",
			EventType: registry.EventRegistryValidated,
			Payload:   map[string]any{"operators": v.Operators, "serials": v.Serials, "pilots": v.Pilots, "unknown": v.Unknown},
		})
		return err
	})
}
