package cis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// CounterUntrusted counts dataset versions held because their
// publisher's signature is missing or does not verify.
const CounterUntrusted = "cis_publisher_untrusted"

// Counters of the CISP's expiry versions (spec 02 F3 erratum
// 2026-10-16): a restrictions version without a publisher signature
// installed by the expiry rule, and one held by it (also counted
// cis_publisher_untrusted).
const (
	CounterExpiryInstalled = "cis_expiry_installed"
	CounterExpiryHeld      = "cis_expiry_held"
)

// The publishers of the datasets (the CISP's auth.PublisherOf; the
// names of USSP_CIS_PUBLISHER_KEYS), and the CISP itself, whose key
// verifies the X-CIS-Signature of the restrictions versions it makes
// when a restriction expires.
const (
	PublisherAuthority = "authority"
	PublisherANSP      = "ansp"
	PublisherCISP      = "cisp"
)

// OpExpire is the op of the record the CISP stores and serves as the
// bytes of a restrictions version it made when a restriction reached
// its ends_at; no publisher can send it (the ANSP's ops are create,
// activate, extend, end, cancel).
const OpExpire = "expire"

// PublisherOf is the publisher whose key must have signed a version of
// d: the ANSP for restrictions (F2), the authority for the others (F1).
func PublisherOf(d Dataset) string {
	if d == Restrictions {
		return PublisherANSP
	}
	return PublisherAuthority
}

// PublisherVerifier verifies a publisher's detached JWS over a version's
// bytes; core's auth.DetachedVerifier and LazyPublisherVerifier
// implement it.
type PublisherVerifier interface {
	Verify(ctx context.Context, publisher, header string, payload []byte) (coreauth.Signature, error)
}

// UntrustedError is a version whose provenance could not be shown: its
// publisher's signature is missing, does not verify, or there are no
// keys to verify it with. The version is stored but never used; the one
// held before stays active.
type UntrustedError struct {
	Dataset Dataset
	Version int64
	Reason  string
}

func (e *UntrustedError) Error() string {
	return fmt.Sprintf("%s version %d held, not used: %s", e.Dataset, e.Version, e.Reason)
}

// errNoPublisherKeys is the reason of every hold while no publisher keys
// are configured.
const errNoPublisherKeys = "no publisher keys are configured (USSP_CIS_PUBLISHER_KEYS)"

// provenance reads version v as published (GET
// /v1/{dataset}/versions/{v}), verifies X-Publisher-Signature over
// its bytes with the key of the dataset's publisher, and checks that v
// (as served, or built from a delta) carries what those signed bytes
// say (signedMatches). A restrictions version without a publisher
// signature is judged by expiryProvenance instead. It returns the kind
// shown, an *UntrustedError when the signature is missing or does not
// verify, no keys are configured, or v is not what was signed, and
// another error when the version could not be read (a pull failure:
// nothing is held, the next pull tries again).
func (c *Cache) provenance(ctx context.Context, v *Version) (Provenance, error) {
	untrusted := func(format string, a ...any) error {
		return &UntrustedError{Dataset: v.Dataset, Version: v.Number, Reason: fmt.Sprintf(format, a...)}
	}
	if c.cfg.Publishers == nil {
		return "", untrusted(errNoPublisherKeys)
	}
	f, err := c.cfg.Client.GetVersion(ctx, v.Dataset, v.Number)
	if err != nil {
		return "", fmt.Errorf("reading %s version %d as published: %w", v.Dataset, v.Number, err)
	}
	if f.Status != http.StatusOK {
		return "", fmt.Errorf("reading %s version %d as published: the CISP answered %d", v.Dataset, v.Number, f.Status)
	}
	if f.Version != 0 && f.Version != v.Number {
		return "", untrusted("the CISP served version %d for version %d", f.Version, v.Number)
	}
	if f.PublisherSignature == "" {
		if v.Dataset == Restrictions {
			if err := c.expiryProvenance(ctx, v, f); err != nil {
				return "", err
			}
			return ProvenanceCISPExpiry, nil
		}
		return "", untrusted("no %s", HeaderPublisherSignature)
	}
	pub := PublisherOf(v.Dataset)
	sig, err := c.cfg.Publishers.Verify(ctx, pub, f.PublisherSignature, f.Body)
	if err != nil {
		return "", untrusted("%s does not verify with the %s's keys: %s", HeaderPublisherSignature, pub, short(err.Error()))
	}
	if f.PublisherKID != "" && f.PublisherKID != sig.KID {
		return "", untrusted("%s names %q, the signature's kid is %q", HeaderPublisherKID, short(f.PublisherKID), sig.KID)
	}
	// The signature covers the bytes just read; what is installed is v,
	// read from another path (or built from a delta). Without this the
	// check would show only that the publisher signed something.
	if reason := signedMatches(v, f.Body); reason != "" {
		return "", untrusted("%s", reason)
	}
	return ProvenancePublisher, nil
}

// expiryRecord is the record the CISP stores as the bytes of a
// restrictions version it made when a restriction reached its ends_at
// (uspace-cisp internal/httpapi/restriction_jobs.go, expire):
// restriction_id is the CISP's head id, feature_id the ED-318
// identifier of the feature removed, ends_at the head's end and at the
// instant of the expiry, both RFC 3339. A CISP from before the record
// named its feature sends no feature_id and no ends_at: such a version
// is held, as every version without a publisher signature was before.
type expiryRecord struct {
	Op            string  `json:"op"`
	RestrictionID string  `json:"restriction_id"`
	FeatureID     *string `json:"feature_id"`
	EndsAt        *string `json:"ends_at"`
	At            *string `json:"at"`
}

// errNotExpiry starts the reason of every hold of a restrictions
// version without a publisher signature.
const errNotExpiry = "no " + HeaderPublisherSignature + ", and not an expiry the CISP may make: "

// expiryProvenance is the one case in which a restrictions version
// without a publisher signature is installed (spec 02 F3 erratum
// 2026-10-16): the bytes at /v1/restrictions/versions/{v} are the
// CISP's expiry record and X-CIS-Signature over them verifies with the
// CISP's key (publisher cisp of USSP_CIS_PUBLISHER_KEYS, under the same
// iat bound as a publisher's); its at is not later than this process's
// clock plus USSP_CIS_EXPIRY_SKEW_S; and compared feature by feature
// with the CISP's restrictions version in use (Cache.restrBase, without
// the ANSP's direct overlay; cis_* members left out), v removes one or
// more features and adds or changes none, every removed feature has a
// single limitedApplicability period whose endDateTime is at or before
// at, and feature_id is one of them. The removed features are the ones
// bound to the ANSP's signature when they were installed, so a
// compromised CISP can end a restriction at most the skew early, and
// the Evaluator applies limitedApplicability itself regardless. The
// record's ends_at must read as a time but decides nothing: the
// feature's signed end does. Any other case is an *UntrustedError (the
// version is held, counted cis_expiry_held), each with its own reason.
func (c *Cache) expiryProvenance(ctx context.Context, v *Version, f Fetched) error {
	held := func(format string, a ...any) error {
		c.cfg.Counters.Inc(CounterExpiryHeld)
		return &UntrustedError{Dataset: v.Dataset, Version: v.Number, Reason: errNotExpiry + fmt.Sprintf(format, a...)}
	}
	if f.CISSignature == "" {
		return held("no %s", HeaderCISSignature)
	}
	if _, err := c.cfg.Publishers.Verify(ctx, PublisherCISP, f.CISSignature, f.Body); err != nil {
		return held("%s does not verify with the CISP's keys: %s", HeaderCISSignature, short(err.Error()))
	}
	var rec expiryRecord
	if err := json.Unmarshal(f.Body, &rec); err != nil {
		return held("its bytes are not a CISP expiry record")
	}
	if rec.Op != OpExpire {
		return held("op %q is not %q", short(rec.Op), OpExpire)
	}
	if rec.FeatureID == nil || *rec.FeatureID == "" {
		return held("the expiry record names no feature_id (a CISP from before the record carried it)")
	}
	if rec.EndsAt == nil {
		return held("the expiry record carries no ends_at")
	}
	endsAt, err := time.Parse(time.RFC3339Nano, *rec.EndsAt)
	if err != nil {
		return held("ends_at %q is not an RFC 3339 time", short(*rec.EndsAt))
	}
	if rec.At == nil {
		return held("the expiry record carries no at")
	}
	at, err := time.Parse(time.RFC3339Nano, *rec.At)
	if err != nil {
		return held("at %q is not an RFC 3339 time", short(*rec.At))
	}
	if limit := c.cfg.Now().Add(c.cfg.ExpirySkew); at.After(limit) {
		return held("at %s is later than this process's clock plus %s (USSP_CIS_EXPIRY_SKEW_S)",
			at.UTC().Format(time.RFC3339Nano), c.cfg.ExpirySkew)
	}
	c.mu.Lock()
	base := c.restrBase
	c.mu.Unlock()
	if base == nil || base.Collection == nil {
		return held("no restrictions version of the CISP is in use to compare it with")
	}
	if v.Collection == nil {
		return held("the version holds no collection to compare")
	}
	prev, err := canonicalFeatures(base.Collection.Features)
	if err != nil {
		return held("the features in use do not export: %s", short(err.Error()))
	}
	next, err := canonicalFeatures(v.Collection.Features)
	if err != nil {
		return held("the version's features do not export: %s", short(err.Error()))
	}
	for _, id := range slices.Sorted(maps.Keys(next)) {
		p, ok := prev[id]
		if !ok {
			return held("it adds %q", short(id))
		}
		if !bytes.Equal(p, next[id]) {
			return held("it changes %q", short(id))
		}
	}
	var removed []string
	for i := range base.Collection.Features {
		ft := &base.Collection.Features[i]
		id := ft.Properties.Identifier
		if _, kept := next[id]; kept {
			continue
		}
		removed = append(removed, id)
		lims := ft.Properties.LimitedApplicability
		if len(lims) != 1 || lims[0].EndDateTime == nil {
			return held("it removes %q, which has no single limitedApplicability period with an endDateTime", short(id))
		}
		if end := lims[0].EndDateTime.Time; end.After(at) {
			return held("it removes %q before its signed end %s (at %s)", short(id),
				end.UTC().Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano))
		}
	}
	if len(removed) == 0 {
		return held("it removes nothing")
	}
	if !slices.Contains(removed, *rec.FeatureID) {
		return held("feature_id %q is not among the features it removes", short(*rec.FeatureID))
	}
	c.cfg.Logger.Info("CIS restrictions version made by the CISP at an expiry: its content is the expiry rule's",
		slog.Int64("version", v.Number), slog.String("restriction_id", short(rec.RestrictionID)),
		slog.String("feature_id", short(*rec.FeatureID)), slog.String("ends_at", endsAt.UTC().Format(time.RFC3339Nano)),
		slog.String("at", at.UTC().Format(time.RFC3339Nano)), slog.Any("removed", removed))
	return nil
}

// LazyPublisherVerifier is the cache's PublisherVerifier, built when the
// publishers' JWKS can be fetched: a process that starts while they
// are unreachable holds every new version (its reason says why) until
// the keys are fetched, and keeps the versions it had.
type LazyPublisherVerifier struct {
	cfg   coreauth.DetachedConfig
	retry time.Duration
	v     atomic.Pointer[coreauth.DetachedVerifier]

	mu      sync.Mutex
	lastErr string
}

// NewLazyPublisherVerifier returns a verifier that is built by Run.
func NewLazyPublisherVerifier(cfg coreauth.DetachedConfig, retry time.Duration) *LazyPublisherVerifier {
	if retry <= 0 {
		retry = DefaultKeysRetry
	}
	return &LazyPublisherVerifier{cfg: cfg, retry: retry}
}

// Build tries once to build the verifier (core fetches every
// publisher's JWKS).
func (l *LazyPublisherVerifier) Build(ctx context.Context) error {
	if l.v.Load() != nil {
		return nil
	}
	v, err := coreauth.NewDetachedVerifier(ctx, l.cfg)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.lastErr = err.Error()
		return err
	}
	l.lastErr = ""
	l.v.Store(v)
	return nil
}

// Run builds the verifier, trying again every retry period until it
// succeeds or ctx ends.
func (l *LazyPublisherVerifier) Run(ctx context.Context) {
	t := time.NewTicker(l.retry)
	defer t.Stop()
	for l.Build(ctx) != nil {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// errPublisherKeysUnavailable refuses a verification before the keys
// were fetched.
var errPublisherKeysUnavailable = errors.New("the publishers' keys have not been fetched since this process started")

// Verify verifies with the built verifier, or refuses.
func (l *LazyPublisherVerifier) Verify(ctx context.Context, publisher, header string, payload []byte) (coreauth.Signature, error) {
	if v := l.v.Load(); v != nil {
		return v.Verify(ctx, publisher, header, payload)
	}
	return coreauth.Signature{}, errPublisherKeysUnavailable
}

// Counters are core's verifier counters once built, nil before.
func (l *LazyPublisherVerifier) Counters() *core.Counters {
	if v := l.v.Load(); v != nil {
		return v.Counters()
	}
	return nil
}

// Probe is the readiness of the publishers' keys: up once fetched, down
// before with the last error.
func (l *LazyPublisherVerifier) Probe(context.Context) (obs.State, string) {
	if l.v.Load() != nil {
		return obs.StateUp, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastErr == "" {
		return obs.StateUnknown, "the publishers' keys are being fetched"
	}
	return obs.StateDown, "the publishers' keys are not fetched: " + l.lastErr
}
