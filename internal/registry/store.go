package registry

import (
	"context"
	"slices"
	"sync"
	"time"
)

// BucketRegistryValidity is the KV bucket of the cached answers
// (docs/PLAN.md §7): Key -> Entry.
const BucketRegistryValidity = "registry_validity"

// Cached is a stored entry with its age on the database clock.
type Cached struct {
	Entry
	AgeS float64
}

// Invalidation is one key the change feed invalidates: the entity and
// its fold key (Entry.KeyFold), and the sequence of the change.
type Invalidation struct {
	Entity  EntityType
	KeyFold string
	Seq     int64
}

// FeedCursor is the change feed's position.
type FeedCursor struct {
	// Since is the authority's next_since of the last page applied.
	Since int64
	ETag  string
	// AgeS is the time since the feed last answered (database clock);
	// nil before the first answer.
	AgeS *float64
}

// Store is the database side of the cache (pgstore implements it on
// registry_validity, registry_invalidations and registry_feed).
type Store interface {
	// Entries reads the cached entries of keys with their age, and the
	// feed's cursor at the time of the read (the since an answer
	// fetched after this read is written under).
	Entries(ctx context.Context, keys []Key) ([]Cached, int64, error)
	// Save writes the entries in one transaction, except an entry whose
	// fold key the feed invalidated under a sequence above since, and
	// calls project with the entries written (FetchedAt set from the
	// database clock) before it commits: an error from project rolls the
	// writes back and is returned.
	Save(ctx context.Context, es []Entry, since int64, project func(context.Context, []Entry) error) ([]Entry, error)
	// Invalidate deletes the entries under the invalidations' fold keys,
	// records the invalidations, forgets those older than keep, and
	// moves the cursor to next with etag, in one transaction; project is
	// called with the deleted keys before it commits. With no
	// invalidation it records that the feed answered.
	Invalidate(ctx context.Context, inv []Invalidation, next int64, etag string, keep time.Duration, project func(context.Context, []Key) error) ([]Key, error)
	// Cursor is the feed's cursor (Since 0 before the first page).
	Cursor(ctx context.Context) (FeedCursor, error)
	// All is every cached entry, newest first, at most limit.
	All(ctx context.Context, limit int) ([]Cached, error)
}

// Projector writes the cached answers to the KV bucket the hot path
// reads (WP-6 implements it on internal/bus). It is called inside the
// transaction that changes registry_validity: an error rolls the change
// back (G-08, B-09).
type Projector interface {
	ProjectRegistry(ctx context.Context, put []Entry, del []Key) error
}

// MemoryProjector is the registry_validity projection in one process
// until WP-6's KV projector exists: a Projector, and the source of the
// hot path's Lookup (Snapshot). Safe for concurrent use.
type MemoryProjector struct {
	mu      sync.RWMutex
	entries map[Key]Entry
	loaded  bool
	// Fail, when set, makes every write fail (tests of the rollback).
	Fail error
}

// NewMemoryProjector is an empty projection that exists (loaded): an
// empty bucket knows nobody, which is not the same as no bucket.
func NewMemoryProjector() *MemoryProjector {
	return &MemoryProjector{entries: map[Key]Entry{}, loaded: true}
}

// ProjectRegistry applies put and del.
func (m *MemoryProjector) ProjectRegistry(_ context.Context, put []Entry, del []Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if m.entries == nil {
		m.entries = map[Key]Entry{}
	}
	m.loaded = true
	for _, k := range del {
		delete(m.entries, k)
	}
	for _, e := range put {
		e.Competencies = slices.Clone(e.Competencies)
		m.entries[e.Key] = e
	}
	return nil
}

// Snapshot is every entry and whether the projection exists; a zero
// MemoryProjector does not (the bucket is missing).
func (m *MemoryProjector) Snapshot() ([]Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	return out, m.loaded
}
