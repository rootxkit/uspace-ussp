package registry

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// clock is a settable clock standing for the database's.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type invalidation struct {
	seq int64
	at  time.Time
}

// memStore is Store in memory, with the semantics pgstore's SQL has:
// ages on its clock, the invalidation guard on Save, the cursor never
// moving backwards, and project inside the "transaction" (an error
// leaves nothing changed).
type memStore struct {
	mu       sync.Mutex
	clock    *clock
	rows     map[Key]Entry
	inv      map[Key]invalidation // Key{entity, fold}
	since    int64
	etag     string
	polledAt *time.Time
	// failRead / failWrite make the calls fail.
	failRead, failWrite error
	// beforeSave runs at the start of Save (a feed racing the write).
	beforeSave func()
}

func newMemStore(c *clock) *memStore {
	return &memStore{clock: c, rows: map[Key]Entry{}, inv: map[Key]invalidation{}}
}

func (m *memStore) Entries(_ context.Context, keys []Key) ([]Cached, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRead != nil {
		return nil, 0, m.failRead
	}
	var out []Cached
	for _, k := range keys {
		if e, ok := m.rows[k]; ok {
			out = append(out, Cached{Entry: e, AgeS: m.clock.Now().Sub(e.FetchedAt).Seconds()})
		}
	}
	return out, m.since, nil
}

func (m *memStore) Save(ctx context.Context, es []Entry, since int64, project func(context.Context, []Entry) error) ([]Entry, error) {
	if m.beforeSave != nil {
		m.beforeSave()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite != nil {
		return nil, m.failWrite
	}
	var written []Entry
	for i := range es {
		e := es[i]
		if in, ok := m.inv[Key{e.Entity, e.KeyFold}]; ok && in.seq > since {
			continue
		}
		e.FetchedAt = m.clock.Now()
		written = append(written, e)
	}
	if err := project(ctx, written); err != nil {
		return nil, err
	}
	for i := range written {
		m.rows[written[i].Key] = written[i]
	}
	return written, nil
}

func (m *memStore) Invalidate(ctx context.Context, inv []Invalidation, next int64, etag string, keep time.Duration, project func(context.Context, []Key) error) ([]Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite != nil {
		return nil, m.failWrite
	}
	var deleted []Key
	for _, in := range inv {
		for k := range m.rows {
			if k.Entity == in.Entity && m.rows[k].KeyFold == in.KeyFold {
				deleted = append(deleted, k)
			}
		}
	}
	if err := project(ctx, deleted); err != nil {
		return nil, err
	}
	for _, k := range deleted {
		delete(m.rows, k)
	}
	now := m.clock.Now()
	for _, in := range inv {
		k := Key{in.Entity, in.KeyFold}
		prev := m.inv[k]
		m.inv[k] = invalidation{seq: max(prev.seq, in.Seq), at: now}
	}
	for k, in := range m.inv {
		if in.at.Before(now.Add(-keep)) {
			delete(m.inv, k)
		}
	}
	m.since, m.etag, m.polledAt = max(m.since, next), etag, &now
	return deleted, nil
}

func (m *memStore) Cursor(context.Context) (FeedCursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRead != nil {
		return FeedCursor{}, m.failRead
	}
	c := FeedCursor{Since: m.since, ETag: m.etag}
	if m.polledAt != nil {
		a := m.clock.Now().Sub(*m.polledAt).Seconds()
		c.AgeS = &a
	}
	return c, nil
}

func (m *memStore) All(_ context.Context, limit int) ([]Cached, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRead != nil {
		return nil, m.failRead
	}
	var out []Cached
	for k := range m.rows {
		out = append(out, Cached{Entry: m.rows[k], AgeS: m.clock.Now().Sub(m.rows[k].FetchedAt).Seconds()})
	}
	slices.SortFunc(out, func(a, b Cached) int { return b.FetchedAt.Compare(a.FetchedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) row(k Key) (Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[k]
	return e, ok
}

func (m *memStore) RecordValidated(context.Context, Validated) error { return nil }

// auditRecorder records the audited lookups, or fails.
type auditRecorder struct {
	mu   sync.Mutex
	got  []Validated
	fail error
}

func (a *auditRecorder) RecordValidated(_ context.Context, v Validated) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return a.fail
	}
	a.got = append(a.got, v)
	return nil
}

var errDown = errors.New("database down")
