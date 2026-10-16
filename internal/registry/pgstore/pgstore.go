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
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/registry"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is registry.Store on the relational database, and the
// registry.BoundKeys of its flights and intents.
type Store struct {
	S *store.Store
	// BoundLimit bounds the rows Bound reads (MaxBoundRows when 0).
	BoundLimit int
	// Counters, when set, count a Bound cut at its limit
	// (CounterBoundTruncated).
	Counters *core.Counters
}

var (
	_ registry.Store     = Store{}
	_ registry.BoundKeys = Store{}
)

// MaxBoundRows bounds the open flights and active intents Bound reads
// (E-10); each names at most three keys.
const MaxBoundRows = 20_000

// CounterBoundTruncated counts reads of the bound keys cut at
// BoundLimit: the keys beyond it are neither re-fetched nor swept.
const CounterBoundTruncated = "registry_bound_truncated"

// Bound implements registry.BoundKeys: the cache keys of the serial and
// operator of every open flight and the serial, operator and pilot of
// every active intent, each once.
func (p Store) Bound(ctx context.Context) ([]registry.Key, error) {
	limit := p.BoundLimit
	if limit <= 0 || limit > MaxBoundRows {
		limit = MaxBoundRows
	}
	rows, err := p.S.Queries().RegistryBoundKeys(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("bound keys: %w", err)
	}
	if len(rows) >= limit && p.Counters != nil {
		p.Counters.Inc(CounterBoundTruncated)
	}
	var out []registry.Key
	seen := map[registry.Key]bool{}
	for _, r := range rows {
		q := registry.Query{Serial: r.UasSerial}
		if r.OperatorReg != nil {
			q.Operator = *r.OperatorReg
		}
		if r.PilotRef != nil {
			q.Pilot = *r.PilotRef
		}
		for _, k := range registry.KeysOf(q) {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out, nil
}

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
		c, err := cached(r.EntityType, r.Key, r.KeyFold, r.Status, r.ValidUntil, r.ClassLabel, r.MtomBand, r.Competencies, r.FetchedAt, r.FetchedSince, r.AgeS)
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
		c, err := cached(r.EntityType, r.Key, r.KeyFold, r.Status, r.ValidUntil, r.ClassLabel, r.MtomBand, r.Competencies, r.FetchedAt, r.FetchedSince, r.AgeS)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func cached(entity, key, fold, status string, validUntil *time.Time, class, band *string, comps []byte, fetched time.Time, since int64, ageS float64) (registry.Cached, error) {
	e := registry.Entry{Key: registry.Key{Entity: registry.EntityType(entity), Key: key}, KeyFold: fold, Status: registry.Status(status),
		ValidUntil: validUntil, FetchedAt: fetched.UTC(), FetchedSince: since}
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

// Save writes the entries in one transaction under since (their
// fetched_since), skipping an entry whose key the feed invalidated after
// since, and projects the written ones before the commit.
func (p Store) Save(ctx context.Context, es []registry.Entry, since int64, project func(context.Context, []registry.Entry) error) ([]registry.Entry, error) {
	var written []registry.Entry
	err := p.S.Tx(ctx, func(q *relational.Queries) error {
		var err error
		if written, err = upsert(ctx, q, es, since); err != nil {
			return err
		}
		return project(ctx, written)
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// upsert writes es under since inside a transaction and returns the
// entries written: an entry whose key the feed invalidated under a
// sequence above since is skipped.
func upsert(ctx context.Context, q *relational.Queries, es []registry.Entry, since int64) ([]registry.Entry, error) {
	var written []registry.Entry
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
				return nil, err
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
			return nil, fmt.Errorf("registry_validity %s: %w", e.Entity, err)
		}
		e.FetchedAt, e.FetchedSince = at.UTC(), since
		written = append(written, e)
	}
	return written, nil
}

// Invalidate deletes the rows fetched under a cursor below the change
// that names them, retains the others, writes the re-fetched answers
// under next, remembers and moves the cursor in one transaction.
func (p Store) Invalidate(ctx context.Context, inv []registry.Invalidation, put []registry.Entry, next int64, etag string, keep time.Duration, project func(context.Context, registry.Applied) error) (registry.Applied, error) {
	var out registry.Applied
	err := p.S.Tx(ctx, func(q *relational.Queries) error {
		out = registry.Applied{}
		var retained []registry.Key
		for _, in := range inv {
			rows, err := q.DeleteRegistryValidityByFold(ctx, relational.DeleteRegistryValidityByFoldParams{
				EntityType: string(in.Entity), KeyFold: in.KeyFold, Seq: in.Seq,
			})
			if err != nil {
				return fmt.Errorf("registry_validity: %w", err)
			}
			for _, r := range rows {
				out.Deleted = append(out.Deleted, registry.Key{Entity: registry.EntityType(r.EntityType), Key: r.Key})
			}
			kept, err := q.RegistryValidityRetainedByFold(ctx, relational.RegistryValidityRetainedByFoldParams{
				EntityType: string(in.Entity), KeyFold: in.KeyFold, Seq: in.Seq,
			})
			if err != nil {
				return fmt.Errorf("registry_validity: %w", err)
			}
			for _, r := range kept {
				retained = append(retained, registry.Key{Entity: registry.EntityType(r.EntityType), Key: r.Key})
			}
		}
		// The re-fetched answers, before this page's invalidations are
		// recorded: the guard skips one only when another instance
		// applied a later page meanwhile (an invalidation above next).
		written, err := upsert(ctx, q, put, next)
		if err != nil {
			return err
		}
		out.Put = written
		out.Deleted = slices.DeleteFunc(out.Deleted, func(k registry.Key) bool {
			if slices.ContainsFunc(written, func(e registry.Entry) bool { return e.Key == k }) {
				out.Replaced = append(out.Replaced, k)
				return true
			}
			return false
		})
		for _, in := range inv {
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
		// A row one change of the page retained and a later one deleted
		// (two changes to one key) is deleted; one written again is put.
		for _, k := range retained {
			if !slices.Contains(out.Deleted, k) && !slices.Contains(out.Retained, k) &&
				!slices.ContainsFunc(out.Put, func(e registry.Entry) bool { return e.Key == k }) {
				out.Retained = append(out.Retained, k)
			}
		}
		return project(ctx, out)
	})
	if err != nil {
		return registry.Applied{}, err
	}
	return out, nil
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
