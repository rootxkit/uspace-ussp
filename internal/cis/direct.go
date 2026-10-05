package cis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/rootxkit/uspace-core/ed318"

	"github.com/rootxkit/uspace-ussp/internal/cis/cispclient"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// The ANSP's degraded direct delivery (spec 02 F2 failure rule,
// cross-plan M4, M5): while the CISP does not hold a restriction's
// version, the ANSP posts the cis/change/v1 record, signed with its own
// key, to /v1/cis/notifications. The record's version member is the
// restriction's ansp_version, not a CIS dataset version, and its
// pull_url is the ANSP's GET /v1/restrictions/{id}/direct, which serves
// the version as restriction/direct/v1 signed with the ANSP's key in
// X-JWS-Signature. The cache pulls it, verifies it with the ANSP's
// publisher keys (USSP_CIS_PUBLISHER_KEYS ansp=), and lays it over the
// CISP's restrictions version until the CISP holds that version or a
// newer one. The overlay is stored (cis_direct_restrictions), so a
// restart keeps it.

// DirectSchema names the body of the ANSP's GET
// /v1/restrictions/{id}/direct.
const DirectSchema = "restriction/direct/v1"

// HeaderDirectSignature carries the ANSP's detached JWS over that body.
const HeaderDirectSignature = "X-JWS-Signature"

// Bounds and defaults of the direct path (E-10).
const (
	// MaxDirectBytes bounds the pulled body: one restriction and its
	// feature.
	MaxDirectBytes = 256 << 10
	// DefaultDirectMax bounds the restrictions held from the direct
	// path, and the ones waiting to be pulled
	// (USSP_CIS_DIRECT_MAX).
	DefaultDirectMax = 500
	// DefaultDirectKeep is how long a direct restriction that is over
	// (ended, cancelled, or past its ends_at) is kept after it arrived
	// or ended, and how long a pull is retried (USSP_CIS_DIRECT_KEEP_S;
	// pending GCAA: the spec names no figure, 24 h is spec 02 F3's
	// notification retry window).
	DefaultDirectKeep = 24 * time.Hour
	// DefaultDirectTimeout bounds one pull.
	DefaultDirectTimeout = 10 * time.Second
	// directRetryEvery is how often pulls that failed are tried again,
	// and the overlay is pruned.
	directRetryEvery = 10 * time.Second
)

// Counters of the direct path.
const (
	CounterDirectQueued     = "cis_direct_queued"
	CounterDirectFull       = "cis_direct_full"
	CounterDirectPulls      = "cis_direct_pulls"
	CounterDirectPullFailed = "cis_direct_pull_failed"
	CounterDirectRefused    = "cis_direct_refused"
	CounterDirectApplied    = "cis_direct_applied"
	CounterDirectReplays    = "cis_direct_replays"
	CounterDirectSuperseded = "cis_direct_superseded_by_cisp"
	CounterDirectExpired    = "cis_direct_expired"
	CounterDirectStoreFail  = "cis_direct_store_failed"
)

// DirectHint is what a verified ANSP notification says: the restriction
// (the JWS sub), its ansp_version (the record's version member), the
// identifiers it names, and the pull_url on the ANSP's configured host.
type DirectHint struct {
	RestrictionID string
	AnspVersion   int64
	FeatureIDs    []string
	PullURL       string
	Issuer        string
	Reason        string
	At            time.Time

	attempts int
	lastErr  string
}

// DirectRestriction is restriction/direct/v1 (uspace-ansp
// api/openapi.yaml DirectRestriction): the version member is
// ansp_version (M4).
type DirectRestriction struct {
	Schema           string          `json:"schema"`
	ID               string          `json:"id"`
	AnspRef          string          `json:"ansp_ref"`
	AnspVersion      int64           `json:"ansp_version"`
	Identifier       string          `json:"identifier"`
	UspaceAirspaceID string          `json:"uspace_airspace_id"`
	State            string          `json:"state"`
	StartsAt         time.Time       `json:"starts_at"`
	EndsAt           time.Time       `json:"ends_at"`
	ChangedAt        time.Time       `json:"changed_at"`
	Feature          json.RawMessage `json:"feature"`
}

// DirectStored is one row of cis_direct_restrictions.
type DirectStored struct {
	Identifier    string
	RestrictionID string
	AnspRef       string
	AnspVersion   int64
	State         string
	Body          []byte
	Signature     string
	Issuer        string
	// AgeS is the time since it was stored, on the database clock (read
	// back only).
	AgeS float64
}

// DirectStore keeps the direct restrictions across a restart.
type DirectStore interface {
	// SaveDirect stores d unless a version at or above it is stored for
	// its identifier; stored says whether it was written.
	SaveDirect(ctx context.Context, d DirectStored) (stored bool, err error)
	// LoadDirect returns at most maxRows stored rows.
	LoadDirect(ctx context.Context, maxRows int) ([]DirectStored, error)
	// DeleteDirect deletes identifier's row when its version is at or
	// below anspVersion.
	DeleteDirect(ctx context.Context, identifier string, anspVersion int64) error
}

// DirectConfig configures the direct path of a Cache.
type DirectConfig struct {
	// Client pulls the ANSP's pull_url: no credential (the route is
	// public; the signature is what is trusted). Nil is a client
	// bounded by DefaultDirectTimeout.
	Client *http.Client
	Store  DirectStore
	// Max is USSP_CIS_DIRECT_MAX; Keep USSP_CIS_DIRECT_KEEP_S.
	Max  int
	Keep time.Duration
}

// directHeld is a direct restriction in force over the CISP's version.
type directHeld struct {
	d      DirectRestriction
	body   []byte
	sig    string
	issuer string
	// since is when it arrived, on this process's clock.
	since time.Time
	entry *Entry
}

func (c *Cache) directMax() int {
	if c.cfg.Direct.Max > 0 {
		return c.cfg.Direct.Max
	}
	return DefaultDirectMax
}

func (c *Cache) directKeep() time.Duration {
	if c.cfg.Direct.Keep > 0 {
		return c.cfg.Direct.Keep
	}
	return DefaultDirectKeep
}

// TriggerDirect queues the pull of a direct notification; it never
// blocks. The newest version of a restriction wins. It returns false,
// counted, when DirectMax pulls are already waiting: the receiver then
// answers 503 and the ANSP retries.
func (c *Cache) TriggerDirect(h DirectHint) bool {
	c.mu.Lock()
	p := c.directPending[h.RestrictionID]
	switch {
	case p != nil && h.AnspVersion <= p.AnspVersion:
	case p == nil && len(c.directPending) >= c.directMax():
		c.mu.Unlock()
		c.cfg.Counters.Inc(CounterDirectFull)
		return false
	default:
		hc := h
		c.directPending[h.RestrictionID] = &hc
	}
	c.mu.Unlock()
	c.cfg.Counters.Inc(CounterDirectQueued)
	select {
	case c.directKick <- struct{}{}:
	default:
	}
	return true
}

// directWorker pulls the queued direct notifications, retries the ones
// that failed and prunes the overlay, until ctx ends.
func (c *Cache) directWorker(ctx context.Context) {
	t := time.NewTicker(directRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.directKick:
		case <-t.C:
			c.pruneDirect(ctx)
		}
		c.drainDirect(ctx)
	}
}

// drainDirect tries every pending pull once.
func (c *Cache) drainDirect(ctx context.Context) {
	c.mu.Lock()
	ids := make([]string, 0, len(c.directPending))
	for id := range c.directPending {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		p := c.directPending[id]
		var h DirectHint
		if p != nil {
			h = *p
		}
		c.mu.Unlock()
		if p == nil {
			continue
		}
		err := c.ApplyDirect(ctx, h)
		c.mu.Lock()
		cur := c.directPending[id]
		switch {
		case cur == nil || cur.AnspVersion > h.AnspVersion:
			// A newer notification came meanwhile: it is pulled next.
		case err == nil || errors.Is(err, errDirectPermanent):
			delete(c.directPending, id)
		case c.cfg.Now().Sub(h.At) > c.directKeep():
			delete(c.directPending, id)
			c.cfg.Counters.Inc(CounterDirectExpired)
			c.cfg.Logger.Error("a direct restriction could not be pulled within the keep period; given up",
				slog.String("restriction_id", id), slog.Int64("ansp_version", h.AnspVersion), obs.Err(err))
		default:
			cur.attempts++
			cur.lastErr = short(err.Error())
		}
		c.mu.Unlock()
	}
}

// errDirectPermanent marks a pull that no retry changes (a 404, a body
// that is not the version named): counted, logged, dropped.
var errDirectPermanent = errors.New("permanent")

// ApplyDirect pulls h's pull_url, verifies the ANSP's signature over
// the body, checks it is the restriction and version h names, stores it
// and lays it over the CISP's restrictions version. A version at or
// below the one held for the restriction, from either path, is a replay
// (counted, nothing pulled).
//
// The pull runs without the restrictions lock (a CISP pull is not held
// behind the ANSP's answer); the checks against what is held run again
// under it before anything is stored.
func (c *Cache) ApplyDirect(ctx context.Context, h DirectHint) error {
	log := c.cfg.Logger.With(slog.String("restriction_id", h.RestrictionID), slog.Int64("ansp_version", h.AnspVersion),
		slog.String("issuer", h.Issuer))
	if held := c.heldAnspVersion(h.FeatureIDs); held >= h.AnspVersion {
		c.cfg.Counters.Inc(CounterDirectReplays)
		log.Info("direct restriction at or below the version held; nothing pulled", slog.Int64("held", held))
		return nil
	}
	c.cfg.Counters.Inc(CounterDirectPulls)
	body, sig, err := c.pullDirect(ctx, h.PullURL)
	if err != nil {
		c.cfg.Counters.Inc(CounterDirectPullFailed)
		log.Warn("direct restriction not pulled; retried", obs.Err(err))
		return err
	}
	if c.cfg.Publishers == nil {
		c.cfg.Counters.Inc(CounterDirectRefused)
		log.Error("direct restriction not applied: " + errNoPublisherKeys)
		return errors.New(errNoPublisherKeys)
	}
	if _, err := c.cfg.Publishers.Verify(ctx, PublisherANSP, sig, body); err != nil {
		// Keys not fetched yet and a signature that does not verify
		// read alike: retried until the keep period ends.
		c.cfg.Counters.Inc(CounterDirectRefused)
		log.Error("direct restriction not applied: the ANSP's signature does not verify", obs.Err(err))
		return err
	}
	held, err := c.buildDirect(h, body, sig)
	if err != nil {
		c.cfg.Counters.Inc(CounterDirectRefused)
		log.Error("direct restriction refused", obs.Err(err))
		return fmt.Errorf("%w: %w", errDirectPermanent, err)
	}
	d := held.d
	lock := c.locks[Restrictions]
	lock.Lock()
	defer lock.Unlock()
	if prev := c.heldAnspVersion([]string{d.Identifier}); prev >= d.AnspVersion && c.baseAnspVersion(d.Identifier) < prev {
		c.cfg.Counters.Inc(CounterDirectReplays)
		log.Info("a direct restriction at or above this version was applied meanwhile", slog.Int64("held", prev))
		return nil
	}
	if base := c.baseAnspVersion(d.Identifier); base >= d.AnspVersion {
		c.cfg.Counters.Inc(CounterDirectSuperseded)
		log.Info("the CISP holds this restriction version or a newer one; the direct one is not applied")
		return nil
	}
	if ds := c.cfg.Direct.Store; ds != nil {
		if _, err := ds.SaveDirect(ctx, DirectStored{Identifier: d.Identifier, RestrictionID: d.ID, AnspRef: d.AnspRef,
			AnspVersion: d.AnspVersion, State: d.State, Body: body, Signature: sig, Issuer: h.Issuer}); err != nil {
			// Applied anyway: the restriction is in force now; a restart
			// before the CISP holds it loses it, which is said.
			c.cfg.Counters.Inc(CounterDirectStoreFail)
			log.Error("direct restriction not stored; applied from memory, lost at a restart until the CISP holds it", obs.Err(err))
		}
	}
	c.mu.Lock()
	c.direct[d.Identifier] = held
	c.mu.Unlock()
	c.cfg.Counters.Inc(CounterDirectApplied)
	log.Info("direct restriction applied over the CISP's version", slog.String("identifier", d.Identifier),
		slog.String("state", d.State), slog.String("reason", h.Reason))
	c.reinstall(ctx)
	return nil
}

// pullDirect reads raw (no credential), bounded.
func (c *Cache) pullDirect(ctx context.Context, raw string) ([]byte, string, error) {
	client := c.cfg.Direct.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultDirectTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultDirectTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: the pull_url is not a request: %w", errDirectPermanent, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDirectBytes+1))
	if err != nil {
		return nil, "", err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", fmt.Errorf("%w: the ANSP answered 404", errDirectPermanent)
	case resp.StatusCode != http.StatusOK:
		return nil, "", fmt.Errorf("the ANSP answered %d", resp.StatusCode)
	case len(body) > MaxDirectBytes:
		return nil, "", fmt.Errorf("%w: the body is longer than %d bytes", errDirectPermanent, MaxDirectBytes)
	}
	sig := resp.Header.Get(HeaderDirectSignature)
	if sig == "" {
		return nil, "", fmt.Errorf("no %s", HeaderDirectSignature)
	}
	return body, sig, nil
}

// buildDirect reads a verified body and checks it is what h names: the
// restriction (sub), an identifier the record listed, a version at
// least the record's, a known state, a feature that builds as every
// restrictions feature does, under the same identifier.
func (c *Cache) buildDirect(h DirectHint, body []byte, sig string) (*directHeld, error) {
	d, err := parseDirect(body)
	if err != nil {
		return nil, err
	}
	switch {
	case d.ID != h.RestrictionID:
		return nil, fmt.Errorf("the body is restriction %q, the notification names %q", short(d.ID), short(h.RestrictionID))
	case !slices.Contains(h.FeatureIDs, d.Identifier):
		return nil, fmt.Errorf("identifier %q is not among the notification's feature_ids", short(d.Identifier))
	case d.AnspVersion < h.AnspVersion:
		return nil, fmt.Errorf("ansp_version %d is below the notification's %d", d.AnspVersion, h.AnspVersion)
	}
	e, err := directEntry(d, c.baseNumber())
	if err != nil {
		return nil, err
	}
	return &directHeld{d: d, body: body, sig: sig, issuer: h.Issuer, since: c.cfg.Now(), entry: e}, nil
}

// parseDirect reads restriction/direct/v1. Members it does not know are
// ignored (additive within v1); the ones it needs are checked.
func parseDirect(body []byte) (DirectRestriction, error) {
	var d DirectRestriction
	if err := json.Unmarshal(body, &d); err != nil {
		return d, fmt.Errorf("not a %s body", DirectSchema)
	}
	switch {
	case d.Schema != DirectSchema:
		return d, fmt.Errorf("schema %q, want %s", short(d.Schema), DirectSchema)
	case d.ID == "" || d.AnspRef == "" || d.Identifier == "" || d.AnspVersion < 1 || len(d.Feature) == 0:
		return d, errors.New("id, ansp_ref, identifier, ansp_version or feature is missing")
	case !cispclient.CisRestrictionState(d.State).Valid():
		return d, fmt.Errorf("state %q is not a restriction state", short(d.State))
	}
	return d, nil
}

// directEntry builds the restrictions entry of d, as the CISP would
// serve it: its feature, and its state in Restriction.
func directEntry(d DirectRestriction, baseNumber int64) (*Entry, error) {
	f, err := parseFeature(d.Feature)
	if err != nil {
		return nil, err
	}
	if f.Properties.Identifier != d.Identifier {
		return nil, fmt.Errorf("the feature is %q, the body names %q", short(f.Properties.Identifier), short(d.Identifier))
	}
	fc := &ed318.FeatureCollection{Type: "FeatureCollection", Features: []ed318.Feature{*f}}
	raw, err := exportFeatures(fc)
	if err != nil {
		return nil, err
	}
	e, err := buildEntry(&Version{Dataset: Restrictions, Number: baseNumber}, &fc.Features[0])
	if err != nil {
		return nil, err
	}
	e.Raw = raw[0]
	r := &cispclient.CisRestriction{AnspRef: d.AnspRef, AnspVersion: d.AnspVersion, State: cispclient.CisRestrictionState(d.State),
		StartsAt: d.StartsAt, EndsAt: d.EndsAt, Id: d.ID, UspaceAirspaceId: d.UspaceAirspaceID}
	if d.State == string(cispclient.CisRestrictionStateEnded) || d.State == string(cispclient.CisRestrictionStateCancelled) {
		by := cispclient.CisRestrictionEndedByAnsp
		r.EndedBy = &by
	}
	e.Restriction, e.Direct = r, true
	return e, nil
}

// heldAnspVersion is the highest ansp_version held for any of ids, from
// the CISP's version or the direct path (0 when none).
func (c *Cache) heldAnspVersion(ids []string) int64 {
	var best int64
	for _, id := range ids {
		best = max(best, c.baseAnspVersion(id))
		c.mu.Lock()
		if h := c.direct[id]; h != nil {
			best = max(best, h.d.AnspVersion)
		}
		c.mu.Unlock()
	}
	return best
}

// baseAnspVersion is the ansp_version of id in the CISP's restrictions
// version held (0 when absent or unreadable).
func (c *Cache) baseAnspVersion(id string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.restrBaseEntries {
		if e.Identifier == id && e.Restriction != nil {
			return e.Restriction.AnspVersion
		}
	}
	return 0
}

func (c *Cache) baseNumber() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.restrBase != nil {
		return c.restrBase.Number
	}
	return 0
}

// mergedRestrictions is the CISP's restrictions version with the direct
// restrictions laid over it: a direct restriction replaces the CISP's
// feature of the same identifier while its ansp_version is higher, and
// is added when the CISP has none. The version is the CISP's (a copy;
// nil when the CISP's was never loaded). The caller holds c.mu.
func (c *Cache) mergedRestrictions() (*Version, []*Entry) {
	var mv *Version
	var number int64
	if c.restrBase != nil {
		cp := *c.restrBase
		mv, number = &cp, cp.Number
	}
	out := make([]*Entry, 0, len(c.restrBaseEntries)+len(c.direct))
	seen := map[string]bool{}
	for _, e := range c.restrBaseEntries {
		if h := c.direct[e.Identifier]; h != nil && (e.Restriction == nil || e.Restriction.AnspVersion < h.d.AnspVersion) {
			continue
		}
		seen[e.Identifier] = true
		out = append(out, e)
	}
	ids := make([]string, 0, len(c.direct))
	for id := range c.direct {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := *c.direct[id].entry
		e.Version = number
		out = append(out, &e)
	}
	return mv, out
}

// reinstall installs the merged restrictions after the direct overlay
// changed, keeping the CISP's confirmation time (a direct restriction
// says nothing of the CISP's age), projects them and tells the hooks.
// The caller holds the restrictions lock.
func (c *Cache) reinstall(ctx context.Context) {
	prev := c.cfg.Evaluator.Snapshot().Entries(Restrictions)
	c.mu.Lock()
	mv, es := c.mergedRestrictions()
	c.mu.Unlock()
	c.cfg.Evaluator.replace(Restrictions, mv, es)
	c.project(ctx)
	nv := mv
	if nv == nil {
		nv = &Version{Dataset: Restrictions}
	}
	c.notify(ctx, nv, nv.Number, prev, es, ChangeInstalled)
}

// pruneDirect drops the direct restrictions the CISP now holds at the
// same version or a newer one, and the ones over (ended, cancelled, or
// past ends_at) for longer than the keep period; dropped rows are
// deleted from the store.
func (c *Cache) pruneDirect(ctx context.Context) {
	lock := c.locks[Restrictions]
	lock.Lock()
	defer lock.Unlock()
	if c.dropDirect(ctx) {
		c.reinstall(ctx)
	}
}

// dropDirect is pruneDirect's drop; true when it dropped any. The caller
// holds the restrictions lock.
func (c *Cache) dropDirect(ctx context.Context) bool {
	now := c.cfg.Now()
	keep := c.directKeep()
	type gone struct {
		id      string
		version int64
		why     string
	}
	var drop []gone
	c.mu.Lock()
	base := map[string]int64{}
	for _, e := range c.restrBaseEntries {
		if e.Restriction != nil {
			base[e.Identifier] = e.Restriction.AnspVersion
		}
	}
	for id, h := range c.direct {
		over := h.d.State == string(cispclient.CisRestrictionStateEnded) || h.d.State == string(cispclient.CisRestrictionStateCancelled)
		switch {
		case base[id] >= h.d.AnspVersion:
			drop = append(drop, gone{id, h.d.AnspVersion, "superseded"})
		case over && now.Sub(h.since) > keep:
			drop = append(drop, gone{id, h.d.AnspVersion, "kept long enough after its end"})
		case !over && !h.d.EndsAt.IsZero() && now.Sub(h.d.EndsAt) > keep:
			drop = append(drop, gone{id, h.d.AnspVersion, "kept long enough after its ends_at"})
		}
	}
	for _, g := range drop {
		delete(c.direct, g.id)
	}
	c.mu.Unlock()
	for _, g := range drop {
		if g.why == "superseded" {
			c.cfg.Counters.Inc(CounterDirectSuperseded)
		} else {
			c.cfg.Counters.Inc(CounterDirectExpired)
		}
		c.cfg.Logger.Info("direct restriction dropped: "+g.why, slog.String("identifier", g.id), slog.Int64("ansp_version", g.version))
		if ds := c.cfg.Direct.Store; ds != nil {
			if err := ds.DeleteDirect(ctx, g.id, g.version); err != nil {
				c.cfg.Counters.Inc(CounterDirectStoreFail)
				c.cfg.Logger.Warn("dropped direct restriction not deleted from the store; it is dropped again at the next start",
					slog.String("identifier", g.id), obs.Err(err))
			}
		}
	}
	return len(drop) > 0
}

// warmDirect loads the stored direct restrictions (Warm): each is built
// again from its stored body, which was verified when it arrived.
func (c *Cache) warmDirect(ctx context.Context) {
	ds := c.cfg.Direct.Store
	if ds == nil {
		return
	}
	rows, err := ds.LoadDirect(ctx, c.directMax())
	if err != nil {
		c.cfg.Counters.Inc(CounterDirectStoreFail)
		c.cfg.Logger.Error("the direct restrictions could not be read from the database", obs.Err(err))
		return
	}
	now := c.cfg.Now()
	base := c.baseNumber()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range rows {
		r := &rows[i]
		d, err := parseDirect(r.Body)
		var e *Entry
		if err == nil {
			e, err = directEntry(d, base)
		}
		if err != nil {
			c.cfg.Logger.Error("a stored direct restriction no longer builds", slog.String("identifier", r.Identifier), obs.Err(err))
			continue
		}
		age := time.Duration(max(0, r.AgeS) * float64(time.Second))
		c.direct[d.Identifier] = &directHeld{d: d, body: r.Body, sig: r.Signature, issuer: r.Issuer, since: now.Add(-age), entry: e}
	}
}

// directProblems are the /readyz and Outdated lines of the direct
// path: the pulls waiting, and the restrictions in force from it. The
// caller holds c.mu.
func (c *Cache) directProblems() (pending, applied []string) {
	ids := make([]string, 0, len(c.directPending))
	for id := range c.directPending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := c.directPending[id]
		why := ""
		if p.lastErr != "" {
			why = fmt.Sprintf(" (%d attempts: %s)", p.attempts, p.lastErr)
		}
		pending = append(pending, fmt.Sprintf("restriction %s ansp_version %d delivered directly by %s at %s not applied yet%s",
			id, p.AnspVersion, p.Issuer, p.At.UTC().Format(time.RFC3339), why))
	}
	held := make([]string, 0, len(c.direct))
	for id := range c.direct {
		held = append(held, id)
	}
	sort.Strings(held)
	for _, id := range held {
		h := c.direct[id]
		applied = append(applied, fmt.Sprintf("restriction %s (ansp_version %d, %s) is from the ANSP's direct delivery; the CISP does not hold it yet",
			id, h.d.AnspVersion, h.d.State))
	}
	return pending, applied
}
