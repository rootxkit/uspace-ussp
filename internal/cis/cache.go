package cis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Counter names of the cache and the receiver (E-09; brief WP-4).
const (
	CounterPulls              = "cis_pulls"
	CounterPullFailed         = "cis_pull_failed"
	CounterNotModified        = "cis_not_modified"
	CounterDeltaPulls         = "cis_delta_pulls"
	CounterDeltaUnusable      = "cis_delta_unusable"
	CounterRefused            = "cis_refused_publications"
	CounterVersionReplays     = "cis_version_replays"
	CounterReconcileCatchups  = "cis_reconcile_catchups"
	CounterStoreFailed        = "cis_store_failed"
	CounterProjectionFailed   = "cis_projection_failed"
	CounterProjectionRetried  = "cis_projection_retried"
	CounterSubscribeFailed    = "cis_subscribe_failed"
	CounterWebhooks           = "cis_webhooks"
	CounterBadSignature       = "cis_webhook_bad_signature"
	CounterWebhookMalformed   = "cis_webhook_malformed"
	CounterWebhookReplayed    = "cis_webhook_replayed"
	CounterWebhookAckOnly     = "cis_webhook_ack_only"
	CounterWebhookUnknown     = "cis_webhook_unknown_reason"
	CounterWebhookJTIFull     = "cis_webhook_jti_full"
	CounterWebhookStoreFailed = "cis_webhook_store_failed"
	CounterPullURLMismatch    = "cis_pull_url_mismatch"
	CounterANSPDirect         = "cis_ansp_direct_notifications"
)

// DefaultReconcileInterval is the conditional pull of every dataset that
// bounds what a missed notification costs (spec 02 F3: 60 s).
const DefaultReconcileInterval = 60 * time.Second

// sweepInterval is how often expired delivery ids and old notification
// log rows are deleted.
const sweepInterval = 10 * time.Minute

// Store is the database side of the cache (pgstore.go implements it
// on internal/store).
type Store interface {
	// SaveVersion stores v and its features once (a second save of the
	// same version is a no-op) and keeps only the current and the
	// previous version of the dataset, and the newest trusted one
	// (SignatureOK) when it is older.
	SaveVersion(ctx context.Context, v *Version, es []*Entry) error
	// TouchVersion records that the CISP confirmed the version.
	TouchVersion(ctx context.Context, d Dataset, version int64) error
	// LoadCurrent returns the newest stored trusted version
	// (SignatureOK) of every dataset, with its age on the database
	// clock; a held version is never loaded.
	LoadCurrent(ctx context.Context) ([]StoredVersion, error)
	// MarkPulled marks d's notifications up to version as pulled.
	MarkPulled(ctx context.Context, d Dataset, version int64) error
	ReceiverStore
	// Sweep deletes expired delivery ids and notification log rows
	// older than keepDays.
	Sweep(ctx context.Context, keepDays int) error
}

// StoredVersion is one version read back from the database.
type StoredVersion struct {
	Version *Version
	AgeS    float64
}

// Hint is what a notification says about a dataset.
type Hint struct {
	Version int64
	ETag    string
	// PullURL is set only when its host is the CISP's configured host
	// (the SSRF guard ran in the receiver).
	PullURL string
	Issuer  string
	At      time.Time
}

// CacheConfig configures a Cache.
type CacheConfig struct {
	// Client is nil when no CISP is configured: the cache serves what
	// the database holds and says so.
	Client *Client
	// Publishers verifies the publisher's signature of every version
	// before it is used (USSP_CIS_PUBLISHER_KEYS); nil holds every new
	// version, untrusted.
	Publishers PublisherVerifier
	Store      Store
	Evaluator  *Evaluator
	Projector  Projector
	Counters   *core.Counters
	Logger     *slog.Logger
	// CallbackURL is USSP_USS_BASE_URL + /v1/cis/notifications; empty
	// means no push subscription (the reconciliation alone).
	CallbackURL string
	// BBox is the subscription's box [min lng, min lat, max lng, max
	// lat]; nil is everywhere.
	BBox              []float64
	ReconcileInterval time.Duration
	// SubscribeRetry is the wait between two subscription attempts.
	SubscribeRetry time.Duration
	// RetentionDays is how long the notification log is kept.
	RetentionDays func() int
	Now           func() time.Time
}

// Cache is the writer of the CIS cache: pulls, notifications,
// reconciliation, the store and the projection.
type Cache struct {
	cfg CacheConfig

	locks map[Dataset]*sync.Mutex
	kick  map[Dataset]chan struct{}

	mu          sync.Mutex
	hints       map[Dataset]*Hint
	pending     map[Dataset]*Hint
	refused     map[Dataset]*RefusalError
	held        map[Dataset]*UntrustedError
	pullErr     map[Dataset]string
	projErr     string
	subscribed  string
	subErr      string
	warmErr     string
	unpersisted map[Dataset]int64
}

// NewCache builds a Cache.
func NewCache(cfg CacheConfig) *Cache {
	if cfg.Counters == nil {
		cfg.Counters = &core.Counters{}
	}
	if cfg.Logger == nil {
		cfg.Logger = obs.Discard()
	}
	if cfg.Projector == nil {
		cfg.Projector = &MemoryProjector{}
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = DefaultReconcileInterval
	}
	if cfg.SubscribeRetry <= 0 {
		cfg.SubscribeRetry = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RetentionDays == nil {
		cfg.RetentionDays = func() int { return 30 }
	}
	c := &Cache{
		cfg: cfg, locks: map[Dataset]*sync.Mutex{}, kick: map[Dataset]chan struct{}{},
		hints: map[Dataset]*Hint{}, pending: map[Dataset]*Hint{}, refused: map[Dataset]*RefusalError{}, held: map[Dataset]*UntrustedError{},
		pullErr: map[Dataset]string{}, unpersisted: map[Dataset]int64{},
	}
	for _, d := range AllDatasets {
		c.locks[d] = &sync.Mutex{}
		c.kick[d] = make(chan struct{}, 1)
	}
	return c
}

// Counters are the cache's and the receiver's counters.
func (c *Cache) Counters() *core.Counters { return c.cfg.Counters }

// Evaluator is the evaluator the cache installs versions into.
func (c *Cache) Evaluator() *Evaluator { return c.cfg.Evaluator }

// Run warms the cache from the database, subscribes, and pulls every
// dataset on a notification and every ReconcileInterval until ctx ends.
func (c *Cache) Run(ctx context.Context) {
	c.Warm(ctx)
	var wg sync.WaitGroup
	for _, d := range AllDatasets {
		wg.Go(func() { c.worker(ctx, d) })
	}
	wg.Go(func() { c.subscribeLoop(ctx) })
	wg.Go(func() { c.sweepLoop(ctx) })
	wg.Wait()
}

// Warm installs the newest stored version of every dataset, with the
// age it has on the database clock, so a restart serves the last known
// zones (stale if they are old) instead of none.
func (c *Cache) Warm(ctx context.Context) {
	if c.cfg.Store == nil {
		return
	}
	stored, err := c.cfg.Store.LoadCurrent(ctx)
	if err != nil {
		c.mu.Lock()
		c.warmErr = err.Error()
		c.mu.Unlock()
		c.cfg.Logger.Warn("the CIS cache could not be read from the database", obs.Err(err))
		return
	}
	now := c.cfg.Now()
	for _, sv := range stored {
		v := sv.Version
		var es []*Entry
		if v.Dataset.ED318() {
			var rf *RefusalError
			if es, rf = buildEntries(v); rf != nil {
				c.cfg.Logger.Error("a stored CIS version no longer builds", slog.String("dataset", string(v.Dataset)),
					slog.Int64("version", v.Number), slog.String("problem", rf.First))
				continue
			}
		}
		age := time.Duration(math.Max(0, sv.AgeS) * float64(time.Second))
		c.cfg.Evaluator.install(v, es, now.Add(-age))
		c.cfg.Logger.Info("CIS version loaded from the database", slog.String("dataset", string(v.Dataset)),
			slog.Int64("version", v.Number), slog.Float64("age_s", sv.AgeS))
	}
	c.project(ctx)
}

// Trigger asks for a pull of d after a notification; it never blocks.
// The newest hint wins.
func (c *Cache) Trigger(d Dataset, h Hint) {
	c.mu.Lock()
	if old := c.hints[d]; old == nil || h.Version >= old.Version {
		c.hints[d] = &h
	}
	if p := c.pending[d]; p == nil || h.Version > p.Version {
		hc := h
		c.pending[d] = &hc
	}
	c.mu.Unlock()
	select {
	case c.kick[d] <- struct{}{}:
	default:
	}
}

func (c *Cache) worker(ctx context.Context, d Dataset) {
	t := time.NewTicker(c.cfg.ReconcileInterval)
	defer t.Stop()
	_ = c.Pull(ctx, d, nil, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick[d]:
			c.mu.Lock()
			h := c.hints[d]
			delete(c.hints, d)
			c.mu.Unlock()
			_ = c.Pull(ctx, d, h, false)
		case <-t.C:
			_ = c.Pull(ctx, d, nil, true)
			c.retryProjection(ctx)
		}
	}
}

// retryProjection writes the projection again when the last write
// failed (the KV was unreachable): the hot path gets the zones back
// within a reconciliation period of the KV's return, not at the next
// CIS version.
func (c *Cache) retryProjection(ctx context.Context) {
	c.mu.Lock()
	failed := c.projErr != ""
	c.mu.Unlock()
	if failed {
		c.cfg.Counters.Inc(CounterProjectionRetried)
		c.project(ctx)
	}
}

// Pull reads d from the CISP and installs a newer version. h is the
// notification that asked for it (nil for a reconciliation); reconcile
// counts a newer version found as a reconciliation catch-up. A version
// at or below the one held, named by h, is not pulled (a replay; it is
// counted). With h.PullURL on the CISP's host naming the version held
// as since_version, the delta is read and applied; any trouble with it
// reads the dataset whole.
func (c *Cache) Pull(ctx context.Context, d Dataset, h *Hint, reconcile bool) error {
	if c.cfg.Client == nil {
		return c.failPull(d, errors.New("no CISP configured (USSP_CISP_BASE_URL)"))
	}
	lock := c.locks[d]
	lock.Lock()
	defer lock.Unlock()
	cur := c.cfg.Evaluator.Snapshot().Version(d)
	if h != nil && h.Version > 0 && cur != nil && h.Version <= cur.Number {
		c.cfg.Counters.Inc(CounterVersionReplays)
		c.clearPending(d, cur.Number)
		return nil
	}
	c.cfg.Counters.Inc(CounterPulls)
	if h != nil && h.PullURL != "" && cur != nil && d.ED318() {
		if v, ok := c.pullDelta(ctx, d, cur, h); ok {
			return c.accept(ctx, v, cur, reconcile)
		}
	}
	etag := ""
	if cur != nil {
		etag = cur.ETag
	}
	f, err := c.cfg.Client.GetDataset(ctx, d, etag)
	if err != nil {
		return c.failPull(d, err)
	}
	now := c.cfg.Now()
	switch f.Status {
	case http.StatusNotModified:
		c.cfg.Counters.Inc(CounterNotModified)
		c.cfg.Evaluator.confirm(d, now, false)
		c.clearPullErr(d)
		if cur != nil {
			c.persistTouch(ctx, d, cur.Number)
			c.clearPending(d, cur.Number)
		}
		return nil
	case http.StatusNotFound:
		c.cfg.Evaluator.confirm(d, now, true)
		c.clearPullErr(d)
		return nil
	}
	v, rf := ParseVersion(d, f.Body, f.ETag, f.Version)
	if rf != nil {
		return c.refuse(d, rf)
	}
	return c.accept(ctx, v, cur, reconcile)
}

func (c *Cache) pullDelta(ctx context.Context, d Dataset, cur *Version, h *Hint) (*Version, bool) {
	if sv, ok := sinceVersion(h.PullURL); !ok || sv != cur.Number {
		return nil, false
	}
	f, err := c.cfg.Client.GetURL(ctx, h.PullURL)
	if err == nil && f.Status != http.StatusOK {
		err = fmt.Errorf("the delta answered %d", f.Status)
	}
	if err != nil {
		c.cfg.Counters.Inc(CounterDeltaUnusable)
		c.cfg.Logger.Warn("CIS delta not read; reading the dataset whole", slog.String("dataset", string(d)), obs.Err(err))
		return nil, false
	}
	body, to, err := mergeDelta(cur, c.cfg.Evaluator.Snapshot().Entries(d), f.Body)
	if err != nil {
		c.cfg.Counters.Inc(CounterDeltaUnusable)
		c.cfg.Logger.Warn("CIS delta not applied; reading the dataset whole", slog.String("dataset", string(d)), obs.Err(err))
		return nil, false
	}
	etag := h.ETag
	if h.Version != to {
		etag = ""
	}
	v, rf := ParseVersion(d, body, etag, to)
	if rf != nil {
		// The merged collection is refused: read it whole, so that a
		// refusal names the CISP's bytes, not ours.
		c.cfg.Counters.Inc(CounterDeltaUnusable)
		return nil, false
	}
	v.Meta.Delta = true
	c.cfg.Counters.Inc(CounterDeltaPulls)
	return v, true
}

// accept installs v when it is newer than cur and its publisher's
// signature verifies; a version whose signature is missing or does not
// verify is held (stored, never used) and cur stays active.
func (c *Cache) accept(ctx context.Context, v, cur *Version, reconcile bool) error {
	now := c.cfg.Now()
	if cur != nil && v.Number <= cur.Number {
		// The CISP served the version held, or an older one (a replay
		// of a cached answer): nothing to install.
		if v.Number < cur.Number {
			c.cfg.Counters.Inc(CounterVersionReplays)
		} else {
			c.cfg.Evaluator.confirm(v.Dataset, now, false)
			c.persistTouch(ctx, v.Dataset, cur.Number)
		}
		c.clearPullErr(v.Dataset)
		return nil
	}
	var es []*Entry
	if v.Dataset.ED318() {
		var rf *RefusalError
		if es, rf = buildEntries(v); rf != nil {
			return c.refuse(v.Dataset, rf)
		}
	}
	if err := c.provenance(ctx, v); err != nil {
		var ue *UntrustedError
		if errors.As(err, &ue) {
			return c.hold(ctx, v, es, ue)
		}
		return c.failPull(v.Dataset, err)
	}
	v.SignatureOK = true
	c.cfg.Evaluator.install(v, es, now)
	c.mu.Lock()
	delete(c.refused, v.Dataset)
	delete(c.pullErr, v.Dataset)
	if h := c.held[v.Dataset]; h != nil && h.Version <= v.Number {
		delete(c.held, v.Dataset)
	}
	c.mu.Unlock()
	if reconcile {
		c.cfg.Counters.Inc(CounterReconcileCatchups)
	}
	c.cfg.Logger.Info("CIS version installed", slog.String("dataset", string(v.Dataset)), slog.Int64("version", v.Number),
		slog.Int("features", len(es)), slog.Bool("delta", v.Meta.Delta), slog.Bool("reconcile", reconcile))
	c.project(ctx)
	c.clearPending(v.Dataset, v.Number)
	c.persist(ctx, v, es)
	return nil
}

func (c *Cache) persist(ctx context.Context, v *Version, es []*Entry) {
	if c.cfg.Store == nil {
		return
	}
	err := c.cfg.Store.SaveVersion(ctx, v, es)
	if err == nil {
		err = c.cfg.Store.MarkPulled(ctx, v.Dataset, v.Number)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.cfg.Counters.Inc(CounterStoreFailed)
		c.unpersisted[v.Dataset] = v.Number
		c.cfg.Logger.Error("CIS version not stored; it is served from memory and stored at the next pull",
			slog.String("dataset", string(v.Dataset)), slog.Int64("version", v.Number), obs.Err(err))
		return
	}
	delete(c.unpersisted, v.Dataset)
}

// persistTouch confirms the version in the database, or stores it when
// an earlier store failed.
func (c *Cache) persistTouch(ctx context.Context, d Dataset, version int64) {
	if c.cfg.Store == nil {
		return
	}
	c.mu.Lock()
	_, missing := c.unpersisted[d]
	c.mu.Unlock()
	if missing {
		if v := c.cfg.Evaluator.Snapshot().Version(d); v != nil {
			c.persist(ctx, v, c.cfg.Evaluator.Snapshot().Entries(d))
		}
		return
	}
	if err := c.cfg.Store.TouchVersion(ctx, d, version); err != nil {
		c.cfg.Counters.Inc(CounterStoreFailed)
		c.cfg.Logger.Warn("CIS confirmation not stored", slog.String("dataset", string(d)), obs.Err(err))
	}
}

// hold stores v as untrusted (signature_ok false: a warm start never
// loads it) without using it, and says so on /readyz until a version
// whose signature verifies replaces it. A pull that finds the same
// version again checks it again (the keys may have been fetched since),
// logging only a change.
func (c *Cache) hold(ctx context.Context, v *Version, es []*Entry, ue *UntrustedError) error {
	c.cfg.Counters.Inc(CounterUntrusted)
	v.SignatureOK = false
	c.mu.Lock()
	prev := c.held[v.Dataset]
	c.held[v.Dataset] = ue
	delete(c.pullErr, v.Dataset)
	c.mu.Unlock()
	if prev == nil || prev.Version != ue.Version || prev.Reason != ue.Reason {
		c.cfg.Logger.Error("CIS version held: its publisher's signature is not verified; the previous version is kept",
			slog.String("dataset", string(v.Dataset)), slog.Int64("version", v.Number), slog.String("reason", ue.Reason))
	}
	c.clearPending(v.Dataset, v.Number)
	c.persist(ctx, v, es)
	return ue
}

func (c *Cache) refuse(d Dataset, rf *RefusalError) error {
	c.cfg.Counters.Inc(CounterRefused)
	c.mu.Lock()
	c.refused[d] = rf
	c.mu.Unlock()
	c.cfg.Logger.Error("CIS publication refused; the previous version is kept", slog.String("dataset", string(d)),
		slog.Int64("version", rf.Version), slog.String("problem", rf.First), slog.Int("problems", rf.Problems))
	return rf
}

func (c *Cache) failPull(d Dataset, err error) error {
	c.cfg.Counters.Inc(CounterPullFailed)
	c.mu.Lock()
	c.pullErr[d] = err.Error()
	c.mu.Unlock()
	c.cfg.Logger.Warn("CIS pull failed", slog.String("dataset", string(d)), obs.Err(err))
	return err
}

func (c *Cache) clearPullErr(d Dataset) {
	c.mu.Lock()
	delete(c.pullErr, d)
	c.mu.Unlock()
}

func (c *Cache) clearPending(d Dataset, held int64) {
	c.mu.Lock()
	if p := c.pending[d]; p != nil && p.Version <= held {
		delete(c.pending, d)
	}
	c.mu.Unlock()
}

func (c *Cache) project(ctx context.Context) {
	p := c.cfg.Evaluator.Project(c.cfg.Now())
	err := c.cfg.Projector.ProjectCIS(ctx, p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.cfg.Counters.Inc(CounterProjectionFailed)
		c.projErr = err.Error()
		c.cfg.Logger.Error("CIS projection not written", obs.Err(err))
		return
	}
	c.projErr = ""
}

func (c *Cache) subscribeLoop(ctx context.Context) {
	if c.cfg.Client == nil || c.cfg.CallbackURL == "" {
		return
	}
	for {
		s, err := c.cfg.Client.Subscribe(ctx, c.cfg.CallbackURL, AllDatasets, c.cfg.BBox)
		if err != nil && ctx.Err() != nil {
			return // stopping: the attempt was cut short, not answered
		}
		c.mu.Lock()
		if err == nil {
			c.subscribed, c.subErr = s.Id, ""
		} else {
			c.subErr = err.Error()
		}
		c.mu.Unlock()
		if err == nil {
			c.cfg.Logger.Info("subscribed to the CISP's change notifications", slog.String("subscription", s.Id),
				slog.String("status", string(s.Status)))
			return
		}
		c.cfg.Counters.Inc(CounterSubscribeFailed)
		c.cfg.Logger.Warn("CISP subscription failed; the reconciliation alone keeps the cache", obs.Err(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.SubscribeRetry):
		}
	}
}

func (c *Cache) sweepLoop(ctx context.Context) {
	if c.cfg.Store == nil {
		return
	}
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.cfg.Store.Sweep(ctx, c.cfg.RetentionDays()); err != nil {
				c.cfg.Logger.Warn("CIS sweep failed", obs.Err(err))
			}
		}
	}
}

// Probe is the readiness of the cache (the /readyz entry cis, also on
// the E-09 status line): unknown with no version loaded, degraded when
// a publication was refused, a version is held untrusted (or no
// publisher keys are configured), a dataset is stale, a notification is not pulled yet, the
// projection or the subscription failed; up otherwise, with the
// versions and the age.
func (c *Cache) Probe(context.Context) (obs.State, string) {
	b := c.cfg.Evaluator.basis()
	ages := c.cfg.Evaluator.Ages()
	c.mu.Lock()
	defer c.mu.Unlock()
	var problems []string
	if c.cfg.Client != nil && c.cfg.Publishers == nil {
		problems = append(problems, errNoPublisherKeys+": every new version is held")
	}
	for _, d := range AllDatasets {
		if rf := c.refused[d]; rf != nil {
			problems = append(problems, fmt.Sprintf("last publication refused: %s %s", d, rf.First))
		}
	}
	for _, d := range AllDatasets {
		if h := c.held[d]; h != nil {
			problems = append(problems, h.Error())
		}
	}
	for _, d := range AllDatasets {
		if p := c.pending[d]; p != nil {
			reason := ""
			if e := c.pullErr[d]; e != "" {
				reason = " (" + e + ")"
			}
			problems = append(problems, fmt.Sprintf("%s version %d notified by %s at %s not pulled yet%s",
				d, p.Version, p.Issuer, p.At.UTC().Format(time.RFC3339), reason))
		}
	}
	loaded := false
	for _, a := range ages {
		if a.Dataset.ED318() && (a.Loaded || a.Empty) {
			loaded = true
		}
	}
	if !loaded {
		why := ""
		for _, d := range ED318Datasets {
			if e := c.pullErr[d]; e != "" {
				why = ": " + e
				break
			}
		}
		if why == "" && c.warmErr != "" {
			why = ": " + c.warmErr
		}
		return obs.StateUnknown, strings.Join(append([]string{"no version loaded" + why}, problems...), "; ")
	}
	bound := c.cfg.Evaluator.cfg.StaleS()
	if b.Stale {
		missing := []string{}
		for _, a := range ages {
			if a.Dataset.ED318() && !a.Loaded && !a.Empty {
				missing = append(missing, string(a.Dataset))
			}
		}
		if len(missing) > 0 {
			problems = append(problems, "no version loaded of "+strings.Join(missing, ", "))
		}
		if !(b.CISAgeS <= bound) {
			problems = append(problems, fmt.Sprintf("age %.0f s > %.0f s", b.CISAgeS, bound))
		}
	}
	if c.projErr != "" {
		problems = append(problems, "projection: "+c.projErr)
	}
	switch {
	case c.cfg.CallbackURL == "":
		problems = append(problems, "no change notifications (USSP_USS_BASE_URL is not set): the reconciliation alone")
	case c.subscribed == "" && c.subErr != "":
		problems = append(problems, "not subscribed: "+c.subErr)
	}
	head := fmt.Sprintf("%s, age %.0f s", b.CISVersion, b.CISAgeS)
	if len(problems) > 0 {
		return obs.StateDegraded, head + "; " + strings.Join(problems, "; ")
	}
	return obs.StateUp, head
}
