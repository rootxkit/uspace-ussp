package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry/authclient"
)

// CacheConfig configures a Cache.
type CacheConfig struct {
	// Client is nil when no authority is configured
	// (USSP_AUTHORITY_BASE_URL): every uncached key is then unknown with
	// registry_unavailable, and /readyz says why.
	Client *Client
	// Store is nil when no database is configured: nothing is cached.
	Store Store
	// Projector receives every write (nil: none; WP-6's KV projector).
	Projector Projector
	// Audit records the lookups of ValidateAudited; without it they are
	// refused.
	Audit Auditor
	// TTL is the policy's lifetime of an answer.
	TTL func() TTL
	// Counters are shared with the feed; nil makes its own.
	Counters *core.Counters
	Logger   *slog.Logger
	// Now is the process clock, used only for the age of the last
	// success shown on /readyz (time.Now).
	Now func() time.Time
}

// Cache answers F8 lookups from the cache within the policy's TTL and
// asks the authority for the rest. Safe for concurrent use.
type Cache struct {
	cfg CacheConfig

	mu          sync.Mutex
	lastSuccess time.Time
	// lastErr is the last failed lookup's error, cleared by a success.
	lastErr   string
	lastErrAt time.Time
}

// TTLFromPolicy reads the cache's TTLs from a policy version.
func TTLFromPolicy(v policy.Values) TTL {
	return TTL{Positive: time.Duration(v.RegistryPositiveTTLS * float64(time.Second)), Negative: time.Duration(v.RegistryNegativeTTLS * float64(time.Second))}
}

// NewCache builds a Cache; without TTL the policy defaults apply.
func NewCache(cfg CacheConfig) *Cache {
	if cfg.Counters == nil {
		cfg.Counters = &core.Counters{}
	}
	if cfg.Logger == nil {
		cfg.Logger = obs.Discard()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TTL == nil {
		def := TTLFromPolicy(policy.Defaults())
		cfg.TTL = func() TTL { return def }
	}
	return &Cache{cfg: cfg}
}

// Counters are the cache's counters.
func (c *Cache) Counters() *core.Counters { return c.cfg.Counters }

// Validate answers each query, in order, one part per key asked. An
// entity is answered from the cache while its age is below its TTL
// (with cache_age_s), else by the authority (cache_age_s 0), whose
// answer is cached. When the authority cannot be asked or its answer is
// refused, an entity without a fresh cached answer is unknown with the
// reason registry_unavailable or registry_answer_refused, counted: the
// error is only for a query F8 does not define (a *core.FieldError
// join), never for a registry that cannot answer.
func (c *Cache) Validate(ctx context.Context, qs []Query, p Purpose) ([]Result, error) {
	ns, err := checkQueries(qs, p)
	if err != nil {
		return nil, err
	}
	ttl := c.cfg.TTL()
	var keys []Key
	seen := map[Key]bool{}
	for _, n := range ns {
		for _, k := range n.keys() {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}

	fresh := map[Key]Cached{}
	since, canWrite := int64(0), c.cfg.Store != nil
	if c.cfg.Store != nil {
		rows, s, err := c.cfg.Store.Entries(ctx, keys)
		if err != nil {
			c.cfg.Counters.Inc(CounterCacheReadFailed)
			obs.Error(ctx, c.cfg.Logger, "registry cache not read; every key is asked of the authority", err)
			canWrite = false
		}
		since = s
		for i := range rows {
			r := &rows[i]
			if r.AgeS >= 0 && time.Duration(r.AgeS*float64(time.Second)) < ttl.For(r.Entry) {
				fresh[r.Key] = *r
			}
		}
	}

	// The keys to ask, per query, without the ones answered fresh.
	var ask []normalised
	asked := map[Key]bool{}
	for _, n := range ns {
		var m normalised
		for _, k := range n.keys() {
			if _, ok := fresh[k]; ok || asked[k] {
				continue
			}
			asked[k] = true
			switch k.Entity {
			case EntityOperator:
				m.operatorKey, m.operatorPublic = n.operatorKey, n.operatorPublic
			case EntityUAS:
				m.serial, m.serialFold = n.serial, n.serialFold
			case EntityPilot:
				m.pilot = n.pilot
			}
		}
		if len(m.keys()) > 0 {
			ask = append(ask, m)
		}
	}
	c.cfg.Counters.Add(CounterCacheHit, uint64(len(fresh)))
	c.cfg.Counters.Add(CounterCacheMiss, uint64(len(asked)))

	got := map[Key]Entry{}
	failed := map[Key]string{}
	if len(ask) > 0 {
		entries, reasons := c.fetch(ctx, p, ask)
		for i := range entries {
			got[entries[i].Key] = entries[i]
		}
		for k := range asked {
			if _, ok := got[k]; ok {
				continue
			}
			r := reasons[k]
			if r == "" {
				r = ReasonRegistryUnavailable
			}
			failed[k] = r
			if r == ReasonRegistryUnavailable {
				c.cfg.Counters.Inc(CounterUnavailable)
			}
		}
		c.cfg.Counters.Add(CounterFetched, uint64(len(entries)))
		if len(entries) > 0 && canWrite {
			c.save(ctx, entries, since)
		}
	}

	out := make([]Result, len(ns))
	for i, n := range ns {
		answer := func(k Key, public string) *Answer {
			if r, ok := fresh[k]; ok {
				return answerOf(r.Entry, public, r.AgeS)
			}
			if e, ok := got[k]; ok {
				return answerOf(e, public, 0)
			}
			return &Answer{Key: public, Status: StatusUnknown, Reason: failed[k]}
		}
		if n.operatorKey != "" {
			out[i].Operator = answer(Key{EntityOperator, n.operatorKey}, n.operatorPublic)
		}
		if n.serial != "" {
			out[i].UAS = answer(Key{EntityUAS, n.serial}, n.serial)
		}
		if n.pilot != "" {
			out[i].Pilot = answer(Key{EntityPilot, n.pilot}, n.pilot)
		}
	}
	return out, nil
}

func answerOf(e Entry, public string, ageS float64) *Answer {
	age := max(ageS, 0)
	return &Answer{Key: public, Status: e.Status, ValidUntil: e.ValidUntil, ClassLabel: e.ClassLabel, MTOMBand: e.MTOMBand,
		Competencies: e.Competencies, CacheAgeS: &age}
}

// fetch asks the authority, in batches of MaxQueries, and returns the
// entries it answered and, for each key of a batch it did not answer,
// why (ReasonRegistryUnavailable or ReasonAnswerRefused).
func (c *Cache) fetch(ctx context.Context, p Purpose, ask []normalised) ([]Entry, map[Key]string) {
	reasons := map[Key]string{}
	if c.cfg.Client == nil {
		c.failure(errors.New("USSP_AUTHORITY_BASE_URL is not set"))
		return nil, reasons
	}
	var out []Entry
	for start := 0; start < len(ask); start += MaxQueries {
		batch := ask[start:min(start+MaxQueries, len(ask))]
		vs, err := c.cfg.Client.Validate(ctx, p, batch)
		if err != nil {
			reason := ReasonRegistryUnavailable
			var re *RefusedError
			if errors.As(err, &re) {
				c.cfg.Counters.Inc(re.Counter)
				reason = ReasonAnswerRefused
			}
			for _, n := range batch {
				for _, k := range n.keys() {
					reasons[k] = reason
				}
			}
			c.failure(err)
			obs.Error(ctx, c.cfg.Logger, "registry lookup failed; the keys asked are unknown", err, slog.String("purpose", string(p)))
			continue
		}
		c.success()
		for i := range vs {
			out = append(out, entriesOf(batch[i], vs[i])...)
		}
	}
	return out, reasons
}

// entriesOf maps a checked answer onto cache entries.
func entriesOf(n normalised, v authclient.RegistryValidity) []Entry {
	var out []Entry
	if o := v.Operator; o != nil {
		e := Entry{Key: Key{EntityOperator, n.operatorKey}, KeyFold: n.operatorKey, Status: Status(o.Status)}
		if o.ValidUntil != nil {
			t := o.ValidUntil.UTC()
			e.ValidUntil = &t
		}
		out = append(out, e)
	}
	if u := v.Uas; u != nil {
		e := Entry{Key: Key{EntityUAS, n.serial}, KeyFold: n.serialFold, Status: Status(u.Status)}
		if u.ClassLabel != nil {
			e.ClassLabel = string(*u.ClassLabel)
		}
		if u.MtomBand != nil {
			e.MTOMBand = *u.MtomBand
		}
		out = append(out, e)
	}
	if p := v.Pilot; p != nil {
		e := Entry{Key: Key{EntityPilot, n.pilot}, KeyFold: n.pilot, Status: Status(p.Status), Competencies: []Competency{}}
		for _, c := range p.Competencies {
			e.Competencies = append(e.Competencies, Competency{Competency: c.Competency, ValidUntil: c.ValidUntil.UTC()})
		}
		out = append(out, e)
	}
	return out
}

// save writes fetched entries and their projection in one step; a
// failure is counted and logged, and the answers stand (they are the
// authority's), uncached.
func (c *Cache) save(ctx context.Context, es []Entry, since int64) {
	written, err := c.cfg.Store.Save(ctx, es, since, func(ctx context.Context, put []Entry) error {
		if c.cfg.Projector == nil || len(put) == 0 {
			return nil
		}
		if err := c.cfg.Projector.ProjectRegistry(ctx, put, nil); err != nil {
			return &policy.ProjectionError{Bucket: BucketRegistryValidity, Err: err}
		}
		return nil
	})
	if err != nil {
		c.cfg.Counters.Inc(CounterCacheWriteFailed)
		obs.Error(ctx, c.cfg.Logger, "registry answers not cached", err)
		return
	}
	c.cfg.Counters.Add(CounterWriteSkipped, uint64(len(es)-len(written)))
}

func (c *Cache) success() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSuccess, c.lastErr = c.cfg.Now(), ""
}

func (c *Cache) failure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr, c.lastErrAt = clip(err.Error()), c.cfg.Now()
}

// OperatorStatus is the F8 status of an operator registration number
// for the accounts service (internal/accounts.RegistryChecker): valid,
// suspended, revoked or unknown, never an error for a registry that
// cannot answer (unknown keeps the operator pending).
func (c *Cache) OperatorStatus(ctx context.Context, number string) (string, error) {
	rs, err := c.Validate(ctx, []Query{{Operator: number}}, PurposeAuthorisation)
	if err != nil {
		return "", err
	}
	return string(rs[0].Operator.Status), nil
}

// ReadinessProbe is the readiness entry registry of the cache and its
// feed (feed may be nil): down without an authority or a token client;
// degraded after a failed lookup more recent than the last success, or
// while the feed is not answering; unknown before either has reached
// the authority. The detail carries last_success_age_s (the last answer
// of a lookup or of the feed) and the feed's cursor and age.
func ReadinessProbe(c *Cache, feed *Feed) obs.Probe {
	return func(ctx context.Context) (obs.State, string) { return c.probe(ctx, feed) }
}

func (c *Cache) probe(ctx context.Context, feed *Feed) (obs.State, string) {
	if c.cfg.Client == nil {
		return obs.StateDown, "USSP_AUTHORITY_BASE_URL is not set: every uncached key is unknown (registry_unavailable)"
	}
	if c.cfg.Client.cfg.Tokens == nil {
		return obs.StateDown, ErrNoTokenSource.Error() + ": every uncached key is unknown (registry_unavailable)"
	}
	c.mu.Lock()
	last, lastErr, lastErrAt := c.lastSuccess, c.lastErr, c.lastErrAt
	c.mu.Unlock()
	feedState, feedDetail, feedLast := obs.StateUnknown, "no change feed", time.Time{}
	if feed != nil {
		feedState, feedDetail, feedLast = feed.status(ctx)
	}
	if feedLast.After(last) {
		last = feedLast
	}
	now := c.cfg.Now()
	parts := []string{"last_success_age_s=none"}
	if !last.IsZero() {
		parts[0] = fmt.Sprintf("last_success_age_s=%.0f", max(now.Sub(last).Seconds(), 0))
	}
	parts = append(parts, feedDetail)
	state := obs.StateUp
	if lastErr != "" {
		state = obs.StateDegraded
		parts = append(parts, fmt.Sprintf("last lookup failed %.0f s ago: %s", max(now.Sub(lastErrAt).Seconds(), 0), lastErr))
	}
	switch {
	case last.IsZero() && state == obs.StateUp:
		state = obs.StateUnknown
		parts = append(parts, "the authority has not answered yet")
	case feed != nil && feedState != obs.StateUp:
		state = obs.StateDegraded
	}
	return state, strings.Join(parts, "; ")
}

// EventRegistryValidated is the events row of every operator lookup.
const EventRegistryValidated = "registry_validated"

// Validated is the audit record of one lookup: who asked, why, the keys
// as answered (public parts only) and how many were unknown.
type Validated struct {
	ActorType string
	ActorID   string
	Purpose   Purpose
	Operators []string
	Serials   []string
	Pilots    []string
	Unknown   int
}

// Auditor writes the events row of a lookup (pgstore implements it).
type Auditor interface {
	RecordValidated(ctx context.Context, v Validated) error
}

// AuditError is a lookup whose events row could not be written: the
// answer is not given (no answer leaves without its record).
type AuditError struct{ Err error }

func (e *AuditError) Error() string { return "registry lookup not audited: " + e.Err.Error() }

func (e *AuditError) Unwrap() error { return e.Err }

// HTTPStatus is 503: the record could not be written; retry.
func (e *AuditError) HTTPStatus() int { return 503 }

// ProblemSlug is the problem type of the refusal.
func (e *AuditError) ProblemSlug() string { return "audit_unavailable" }

// ProblemDetail says the answer is withheld; the cause is logged.
func (e *AuditError) ProblemDetail() string {
	return "the lookup could not be recorded, so its answer is withheld"
}

// ValidateAudited is Validate for a caller outside this USSP (an
// operator client on GET /v1/registry/validate): the answers and one
// events row with the actor and the purpose, or no answer at all.
func (c *Cache) ValidateAudited(ctx context.Context, actorType, actorID string, qs []Query, p Purpose) ([]Result, error) {
	rs, err := c.Validate(ctx, qs, p)
	if err != nil {
		return nil, err
	}
	if c.cfg.Audit == nil {
		return nil, &AuditError{Err: errors.New("no audit store")}
	}
	v := Validated{ActorType: actorType, ActorID: actorID, Purpose: p, Operators: []string{}, Serials: []string{}, Pilots: []string{}}
	for _, r := range rs {
		for _, part := range []struct {
			a    *Answer
			keys *[]string
		}{{r.Operator, &v.Operators}, {r.UAS, &v.Serials}, {r.Pilot, &v.Pilots}} {
			if part.a == nil {
				continue
			}
			*part.keys = append(*part.keys, part.a.Key)
			if part.a.Status == StatusUnknown {
				v.Unknown++
			}
		}
	}
	if err := c.cfg.Audit.RecordValidated(ctx, v); err != nil {
		obs.Error(ctx, c.cfg.Logger, "registry lookup not audited; the answer is withheld", err)
		return nil, &AuditError{Err: err}
	}
	return rs, nil
}
