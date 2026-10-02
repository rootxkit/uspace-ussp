package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
)

// FeedConfig configures a Feed.
type FeedConfig struct {
	Client    *Client
	Store     Store
	Projector Projector
	Interval  time.Duration
	PageLimit int
	MaxPages  int
	// Counters are the cache's (Cache.Counters).
	Counters *core.Counters
	Logger   *slog.Logger
	// Now is the process clock of the last success shown on /readyz.
	Now func() time.Time
}

// Feed polls the authority's change feed and invalidates the cached
// answers a change names (SC-17): from registry_validity and from the
// projection, in the transaction that moves the cursor.
type Feed struct {
	cfg FeedConfig

	mu          sync.Mutex
	lastSuccess time.Time
	lastErr     string
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
	if cfg.Counters == nil {
		cfg.Counters = &core.Counters{}
	}
	if cfg.Logger == nil {
		cfg.Logger = obs.Discard()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Feed{cfg: cfg}
}

// Run polls at once and then every Interval until ctx ends.
func (f *Feed) Run(ctx context.Context) {
	t := time.NewTicker(f.cfg.Interval)
	defer t.Stop()
	for {
		if err := f.Poll(ctx); err != nil && ctx.Err() == nil {
			obs.Error(ctx, f.cfg.Logger, "registry change feed not polled", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll applies the pages after the stored cursor, at most MaxPages.
// Each page is one transaction: the deletions, the invalidations remembered and the
// cursor move together, with the projection's deletions inside it.
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
	for range f.cfg.MaxPages {
		page, err := f.cfg.Client.Changes(ctx, since, f.cfg.PageLimit, etag)
		if err != nil {
			return err
		}
		if page.NotModified {
			f.cfg.Counters.Inc(CounterFeedNotModified)
			_, err := f.apply(ctx, nil, since, etag)
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
			default:
				fold = pilotKey(ch.PublicKey)
			}
			if fold == "" {
				continue
			}
			inv = append(inv, Invalidation{Entity: e, KeyFold: fold, Seq: ch.Seq})
		}
		deleted, err := f.apply(ctx, inv, page.NextSince, page.ETag)
		if err != nil {
			return err
		}
		f.cfg.Counters.Inc(CounterFeedPolled)
		f.cfg.Counters.Add(CounterFeedInvalidated, uint64(len(deleted)))
		if len(deleted) > 0 {
			f.cfg.Logger.Info("registry answers invalidated by the change feed", slog.Int("entries", len(deleted)),
				slog.Int64("since", page.NextSince))
		}
		if len(page.Changes) < f.cfg.PageLimit || page.NextSince == since {
			return nil
		}
		since, etag = page.NextSince, page.ETag
	}
	return nil
}

func (f *Feed) apply(ctx context.Context, inv []Invalidation, next int64, etag string) ([]Key, error) {
	return f.cfg.Store.Invalidate(ctx, inv, next, etag, InvalidationKeep, func(ctx context.Context, del []Key) error {
		if f.cfg.Projector == nil || len(del) == 0 {
			return nil
		}
		if err := f.cfg.Projector.ProjectRegistry(ctx, nil, del); err != nil {
			return &policy.ProjectionError{Bucket: BucketRegistryValidity, Err: err}
		}
		return nil
	})
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
	}
	return obs.StateUp, detail, last
}
