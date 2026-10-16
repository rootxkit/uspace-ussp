package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Defaults of FeedConfig.
const (
	// DefaultFeedInterval is the poll period of spec 02 F8.
	DefaultFeedInterval = 30 * time.Second
	// DefaultFeedPageLimit is the page size asked.
	DefaultFeedPageLimit = 500
	// DefaultFeedMaxPages bounds the pages one poll applies (E-10); the
	// rest wait for the next poll.
	DefaultFeedMaxPages = 20
	// InvalidationKeep is how long an applied invalidation is remembered
	// against an answer whose fetch raced it: far beyond DefaultTimeout.
	InvalidationKeep = time.Hour
	// DefaultMaxRefetchPerPoll bounds the bound keys one poll re-fetches
	// (E-10): ten batches of MaxQueries. The rest are deleted, counted
	// registry_refetch_deferred, and left to the sweep.
	DefaultMaxRefetchPerPoll = 1000
)

// Revalidate is the sweep's period and bound (policy
// registry_revalidate_interval_s and registry_revalidate_max_count).
type Revalidate struct {
	Interval time.Duration
	MaxKeys  int
}

// RevalidateFromPolicy reads the sweep's period and bound from a policy
// version.
func RevalidateFromPolicy(v policy.Values) Revalidate {
	return Revalidate{Interval: time.Duration(v.RegistryRevalidateIntervalS * float64(time.Second)), MaxKeys: v.RegistryRevalidateMaxCount}
}

// BoundKeys names the cache keys currently bound to an open flight or
// an active intent (pgstore): the keys whose answer the hot path needs
// now, so a change to one is re-fetched in the poll that applies it
// instead of leaving a hole.
type BoundKeys interface {
	Bound(ctx context.Context) ([]Key, error)
}

// FeedConfig configures a Feed.
type FeedConfig struct {
	Client    *Client
	Store     Store
	Projector Projector
	Interval  time.Duration
	PageLimit int
	MaxPages  int
	// Bound names the keys bound to an open flight or an active intent;
	// nil: none is, and every answer a change names is deleted.
	Bound BoundKeys
	// Cache re-fetches the bound keys a change names; nil: none is
	// re-fetched.
	Cache *Cache
	// MaxRefetchPerPoll bounds the keys one poll re-fetches
	// (DefaultMaxRefetchPerPoll).
	MaxRefetchPerPoll int
	// Revalidate is the sweep's period and bound, read before every
	// sweep (the policy's); nil: the policy defaults.
	Revalidate func() Revalidate
	// Counters are the cache's (Cache.Counters).
	Counters *core.Counters
	Logger   *slog.Logger
	// Now is the process clock of the last success shown on /readyz.
	Now func() time.Time
}

// Feed polls the authority's change feed and invalidates the cached
// answers a change names (SC-17): from registry_validity and from the
// projection, in the transaction that moves the cursor.
//
// The invariant: a cached answer survives a change to its key iff it
// was fetched under a feed cursor at or beyond the change's seq
// (Entry.FetchedSince); every other answer the change names is
// invalidated. An answer fetched under a cursor at or beyond the change
// was fetched after this USSP received the page naming it, which the
// authority served after the change committed, so it reflects the
// change by causality. Time (the change's at, fetched_at) never decides
// retention: they are two clocks of two systems, and at precedes the
// authority's commit.
//
// Every other answer the change names is replaced in the same
// transaction when its key is bound to an open flight or an active
// intent (FeedConfig.Bound), and deleted otherwise: the re-fetched
// answer overwrites the projected key, which is never deleted first, so
// the hot path never sees a hole between the change and its re-fetch. A
// key that cannot be re-fetched is deleted and said to be unavailable
// (registry_refetch_failed, /readyz), never kept: an answer the feed
// named as changed is known to be stale, and the typical change is a
// suspension.
type Feed struct {
	cfg FeedConfig

	mu          sync.Mutex
	lastSuccess time.Time
	lastErr     string
	// failed are the bound keys a re-fetch left without an answer, until
	// a later re-fetch answers them.
	failed map[Key]bool
}

// NewFeed builds a Feed with the defaults applied.
func NewFeed(cfg FeedConfig) *Feed {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultFeedInterval
	}
	if cfg.PageLimit <= 0 || cfg.PageLimit > MaxChangesLimit {
		cfg.PageLimit = DefaultFeedPageLimit
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = DefaultFeedMaxPages
	}
	if cfg.MaxRefetchPerPoll <= 0 {
		cfg.MaxRefetchPerPoll = DefaultMaxRefetchPerPoll
	}
	if cfg.Revalidate == nil {
		def := RevalidateFromPolicy(policy.Defaults())
		cfg.Revalidate = func() Revalidate { return def }
	}
	if cfg.Counters == nil {
		cfg.Counters = &core.Counters{}
	}
	if cfg.Logger == nil {
		cfg.Logger = obs.Discard()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Feed{cfg: cfg, failed: map[Key]bool{}}
}

// Run polls at once and then every Interval, and sweeps after the first
// poll and then every Revalidate().Interval, until ctx ends.
func (f *Feed) Run(ctx context.Context) {
	t := time.NewTicker(f.cfg.Interval)
	defer t.Stop()
	poll := func() {
		if err := f.Poll(ctx); err != nil && ctx.Err() == nil {
			obs.Error(ctx, f.cfg.Logger, "registry change feed not polled", err)
		}
	}
	poll()
	sweep := time.NewTimer(0)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			poll()
		case <-sweep.C:
			if err := f.Sweep(ctx); err != nil && ctx.Err() == nil {
				obs.Error(ctx, f.cfg.Logger, "registry bound keys not revalidated", err)
			}
			sweep.Reset(f.revalidate().Interval)
		}
	}
}

// revalidate is the sweep's period and bound, the defaults in place of
// a value that is not positive (the policy refuses one; E-15: never a
// disarmed sweep).
func (f *Feed) revalidate() Revalidate {
	r, def := f.cfg.Revalidate(), RevalidateFromPolicy(policy.Defaults())
	if r.Interval <= 0 {
		r.Interval = def.Interval
	}
	if r.MaxKeys <= 0 {
		r.MaxKeys = def.MaxKeys
	}
	return r
}

// Sweep asks the authority (purpose identification) again about every
// key bound to an open flight or an active intent whose cached answer
// is missing (never fetched, deferred by a poll, failed) or older than
// the period, missing first and then oldest first, at most MaxKeys
// (the rest counted registry_revalidate_deferred and left to the next
// sweep), and caches the answers as a lookup does (Save's guard, the
// projection overwritten). It bounds in time a change the feed never
// names, and restores the keys a poll could not re-fetch. A key that is
// no longer bound is no longer reported failed.
func (f *Feed) Sweep(ctx context.Context) error {
	if f.cfg.Bound == nil || f.cfg.Cache == nil || f.cfg.Store == nil {
		return nil
	}
	err := f.sweep(ctx)
	if err != nil {
		f.cfg.Counters.Inc(CounterSweepFailed)
	}
	return err
}

func (f *Feed) sweep(ctx context.Context) error {
	r := f.revalidate()
	bound, err := f.cfg.Bound.Bound(ctx)
	if err != nil {
		return fmt.Errorf("registry bound keys: %w", err)
	}
	f.mu.Lock()
	for k := range f.failed {
		if !slices.Contains(bound, k) {
			delete(f.failed, k)
		}
	}
	f.mu.Unlock()
	if len(bound) == 0 {
		return nil
	}
	rows, since, err := f.cfg.Store.Entries(ctx, bound)
	if err != nil {
		return fmt.Errorf("registry_validity: %w", err)
	}
	ageOf := map[Key]float64{}
	for i := range rows {
		ageOf[rows[i].Key] = rows[i].AgeS
	}
	var due []Key
	for _, k := range bound {
		if a, ok := ageOf[k]; !ok || a < 0 || time.Duration(a*float64(time.Second)) >= r.Interval {
			due = append(due, k)
		}
	}
	slices.SortStableFunc(due, func(a, b Key) int {
		ma, ok := ageOf[a]
		mb, okb := ageOf[b]
		switch {
		case !ok && !okb:
			return 0
		case !ok:
			return -1
		case !okb:
			return 1
		}
		return cmp.Compare(mb, ma)
	})
	if n := len(due) - r.MaxKeys; n > 0 {
		f.cfg.Counters.Add(CounterRevalidateDeferred, uint64(n))
		due = due[:r.MaxKeys]
	}
	for start := 0; start < len(due); start += MaxQueries {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch := due[start:min(start+MaxQueries, len(due))]
		got, _ := f.cfg.Cache.refetch(context.WithoutCancel(ctx), batch)
		f.cfg.Counters.Add(CounterRevalidateFailed, uint64(len(batch)-len(got)))
		if len(got) == 0 {
			continue
		}
		written, err := f.cfg.Cache.store(ctx, got, since)
		if err != nil {
			return err
		}
		f.cfg.Counters.Add(CounterRevalidated, uint64(len(written)))
		f.mu.Lock()
		for i := range written {
			delete(f.failed, written[i].Key)
		}
		f.mu.Unlock()
	}
	return nil
}

// Poll applies the pages after the stored cursor, at most MaxPages.
// Each page is one transaction: the deletions, the re-fetched answers of
// the bound keys it names, the invalidations remembered and the cursor
// move together, with the projection's deletions and puts inside it.
func (f *Feed) Poll(ctx context.Context) error {
	err := f.poll(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.cfg.Counters.Inc(CounterFeedFailed)
		var re *RefusedError
		if errors.As(err, &re) {
			f.cfg.Counters.Inc(re.Counter)
		}
		f.lastErr = clip(err.Error())
		return err
	}
	f.lastSuccess, f.lastErr = f.cfg.Now(), ""
	return nil
}

func (f *Feed) poll(ctx context.Context) error {
	switch {
	case f.cfg.Client == nil:
		return errors.New("USSP_AUTHORITY_BASE_URL is not set")
	case f.cfg.Store == nil:
		return errors.New("no database: nothing is cached to invalidate")
	}
	cur, err := f.cfg.Store.Cursor(ctx)
	if err != nil {
		return fmt.Errorf("feed cursor: %w", err)
	}
	since, etag := cur.Since, cur.ETag
	rf := &refetchPoll{budget: f.cfg.MaxRefetchPerPoll}
	for range f.cfg.MaxPages {
		page, err := f.cfg.Client.Changes(ctx, since, f.cfg.PageLimit, etag)
		if err != nil {
			return err
		}
		if page.NotModified {
			f.cfg.Counters.Inc(CounterFeedNotModified)
			_, err := f.apply(ctx, nil, nil, since, etag)
			return err
		}
		inv := make([]Invalidation, 0, len(page.Changes))
		for _, ch := range page.Changes {
			e := EntityType(ch.EntityType)
			var fold string
			switch e {
			case EntityOperator:
				fold, _ = operatorKey(ch.PublicKey)
			case EntityUAS:
				_, fold = serialKey(ch.PublicKey)
			case EntityPilot:
				fold = pilotKey(ch.PublicKey)
			}
			if fold == "" {
				continue
			}
			inv = append(inv, Invalidation{Entity: e, KeyFold: fold, Seq: ch.Seq})
		}
		put, failed, err := f.refetch(ctx, rf, inv)
		if err != nil {
			return err
		}
		applied, err := f.apply(ctx, inv, put, page.NextSince, page.ETag)
		if err != nil {
			return err
		}
		f.settle(failed, applied)
		f.cfg.Counters.Inc(CounterFeedPolled)
		f.cfg.Counters.Add(CounterFeedInvalidated, uint64(len(applied.Deleted)+len(applied.Replaced)))
		f.cfg.Counters.Add(CounterFeedRetained, uint64(len(applied.Retained)))
		f.cfg.Counters.Add(CounterFeedRefetched, uint64(len(applied.Put)))
		f.cfg.Counters.Add(CounterRefetchFailed, uint64(len(failed)))
		if len(applied.Deleted) > 0 || len(applied.Retained) > 0 || len(applied.Put) > 0 {
			attrs := []any{slog.Int("entries", len(applied.Deleted)+len(applied.Replaced)), slog.Int("deleted", len(applied.Deleted)),
				slog.Int("retained", len(applied.Retained)), slog.Int("refetched", len(applied.Put)), slog.Int("refetch_failed", len(failed)),
				slog.Int64("since", page.NextSince)}
			if n := len(page.Changes); n > 0 {
				// The authority's clock, for the reader only: it decides nothing.
				attrs = append(attrs, slog.Time("last_change_at", page.Changes[n-1].At))
			}
			f.cfg.Logger.Info("registry answers invalidated by the change feed", attrs...)
		}
		if len(page.Changes) < f.cfg.PageLimit || page.NextSince == since {
			return nil
		}
		since, etag = page.NextSince, page.ETag
	}
	return nil
}

// refetchPoll is one poll's re-fetch budget and its bound keys, read
// once per poll.
type refetchPoll struct {
	budget int
	bound  []Key
	read   bool
}

// refetch asks the authority, purpose identification, for the bound
// keys inv names whose cached answer the change does not leave standing,
// within the poll's budget: the rest are counted deferred and deleted.
// It returns the answers to write with the page and the keys the
// authority did not answer (deleted: unavailable). A Bound that cannot
// be read fails the poll, which then moves nothing. Each batch runs on
// a context the caller cannot cancel (E-14), bounded by the client's
// deadline (DefaultTimeout); a caller whose context is done starts no
// batch.
func (f *Feed) refetch(ctx context.Context, rf *refetchPoll, inv []Invalidation) ([]Entry, map[Key]string, error) {
	if f.cfg.Bound == nil || f.cfg.Cache == nil || len(inv) == 0 {
		return nil, nil, nil
	}
	if !rf.read {
		bound, err := f.cfg.Bound.Bound(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("registry bound keys: %w", err)
		}
		rf.bound, rf.read = bound, true
	}
	seqOf := map[Key]int64{}
	var named []Key
	for _, k := range rf.bound {
		for _, in := range inv {
			if in.Entity == k.Entity && in.KeyFold == entryKeyFold(k.Entity, k.Key) {
				if _, ok := seqOf[k]; !ok {
					named = append(named, k)
				}
				seqOf[k] = max(seqOf[k], in.Seq)
			}
		}
	}
	if len(named) == 0 {
		return nil, nil, nil
	}
	// An answer fetched under a cursor at or beyond its change stands
	// (Store.Invalidate retains it): it needs no re-fetch.
	rows, _, err := f.cfg.Store.Entries(ctx, named)
	if err != nil {
		return nil, nil, fmt.Errorf("registry_validity: %w", err)
	}
	for i := range rows {
		if rows[i].FetchedSince >= seqOf[rows[i].Key] {
			named = slices.DeleteFunc(named, func(k Key) bool { return k == rows[i].Key })
		}
	}
	if n := len(named) - rf.budget; n > 0 {
		f.cfg.Counters.Add(CounterRefetchDeferred, uint64(n))
		named = named[:rf.budget]
	}
	rf.budget -= len(named)
	var put []Entry
	failed := map[Key]string{}
	for start := 0; start < len(named); start += MaxQueries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		batch := named[start:min(start+MaxQueries, len(named))]
		got, reasons := f.cfg.Cache.refetch(context.WithoutCancel(ctx), batch)
		put = append(put, got...)
		for _, k := range batch {
			if !slices.ContainsFunc(got, func(e Entry) bool { return e.Key == k }) {
				failed[k] = reasons[k]
			}
		}
	}
	return put, failed, nil
}

// settle records the re-fetch's outcome for the readiness entry: a key
// the authority did not answer is failed until a later re-fetch answers
// it.
func (f *Feed) settle(failed map[Key]string, a Applied) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range failed {
		f.failed[k] = true
	}
	for i := range a.Put {
		delete(f.failed, a.Put[i].Key)
	}
}

func (f *Feed) apply(ctx context.Context, inv []Invalidation, put []Entry, next int64, etag string) (Applied, error) {
	return f.cfg.Store.Invalidate(ctx, inv, put, next, etag, InvalidationKeep, func(ctx context.Context, a Applied) error {
		if f.cfg.Projector == nil {
			return nil
		}
		// Every projected answer the invalidations name goes, not only
		// those the table had a row for (audit S1), except the answers
		// the table retained (they reflect the change) and the ones
		// re-fetched, whose put overwrites the key: it is never deleted
		// first, so the hot path never reads a hole.
		if fd, ok := f.cfg.Projector.(FoldDeleter); ok && len(inv) > 0 {
			except := slices.Clone(a.Retained)
			for i := range a.Put {
				except = append(except, a.Put[i].Key)
			}
			projected, err := fd.DeleteFolds(ctx, inv, except)
			if err != nil {
				return &policy.ProjectionError{Bucket: BucketRegistryValidity, Err: err}
			}
			if n := countOrphans(projected, a.Deleted); n > 0 {
				f.cfg.Counters.Add(CounterFeedOrphansDeleted, uint64(n))
			}
		}
		if len(a.Deleted) == 0 && len(a.Put) == 0 {
			return nil
		}
		if err := f.cfg.Projector.ProjectRegistry(ctx, a.Put, a.Deleted); err != nil {
			return &policy.ProjectionError{Bucket: BucketRegistryValidity, Err: err}
		}
		return nil
	})
}

// countOrphans is how many of projected have no row in rows.
func countOrphans(projected, rows []Key) int {
	n := 0
	for _, k := range projected {
		if !slices.Contains(rows, k) {
			n++
		}
	}
	return n
}

// status is the feed's state for the readiness entry: up while it has
// answered within three periods, degraded otherwise; the detail names
// the cursor and its age on the database clock.
func (f *Feed) status(ctx context.Context) (obs.State, string, time.Time) {
	f.mu.Lock()
	last, lastErr := f.lastSuccess, f.lastErr
	f.mu.Unlock()
	detail := "feed_cursor=unread"
	var ageS *float64
	if f.cfg.Store != nil {
		if cur, err := f.cfg.Store.Cursor(ctx); err == nil {
			detail, ageS = fmt.Sprintf("feed_cursor=%d", cur.Since), cur.AgeS
		}
	}
	if ageS != nil {
		detail += fmt.Sprintf(", feed_age_s=%.0f", *ageS)
	} else {
		detail += ", feed_age_s=none"
	}
	f.mu.Lock()
	failed := len(f.failed)
	f.mu.Unlock()
	stale := 3 * f.cfg.Interval
	switch {
	case last.IsZero() && lastErr == "":
		return obs.StateUnknown, detail + ", not polled yet", last
	case ageS == nil || time.Duration(*ageS*float64(time.Second)) > stale:
		if lastErr != "" {
			detail += ", last poll failed: " + lastErr
		}
		return obs.StateDegraded, detail + fmt.Sprintf(", no answer within %.0f s", stale.Seconds()), last
	case lastErr != "":
		return obs.StateDegraded, detail + ", last poll failed: " + lastErr, last
	case failed > 0:
		return obs.StateDegraded, detail + fmt.Sprintf(", refetch_failed=%d: bound keys without an answer since a change (registry_unavailable)", failed), last
	}
	return obs.StateUp, detail, last
}
