package cis

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// The publishers of the datasets (the CISP's auth.PublisherOf; the
// names of USSP_CIS_PUBLISHER_KEYS).
const (
	PublisherAuthority = "authority"
	PublisherANSP      = "ansp"
)

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
// say (signedMatches). It returns an *UntrustedError when the
// signature is missing or does not verify, no keys are configured, or v
// is not what was signed, and another error when the version could not
// be read (a pull failure: nothing is held, the next pull tries again).
func (c *Cache) provenance(ctx context.Context, v *Version) error {
	untrusted := func(format string, a ...any) error {
		return &UntrustedError{Dataset: v.Dataset, Version: v.Number, Reason: fmt.Sprintf(format, a...)}
	}
	if c.cfg.Publishers == nil {
		return untrusted(errNoPublisherKeys)
	}
	f, err := c.cfg.Client.GetVersion(ctx, v.Dataset, v.Number)
	if err != nil {
		return fmt.Errorf("reading %s version %d as published: %w", v.Dataset, v.Number, err)
	}
	if f.Status != http.StatusOK {
		return fmt.Errorf("reading %s version %d as published: the CISP answered %d", v.Dataset, v.Number, f.Status)
	}
	if f.Version != 0 && f.Version != v.Number {
		return untrusted("the CISP served version %d for version %d", f.Version, v.Number)
	}
	if f.PublisherSignature == "" {
		return untrusted("no %s", HeaderPublisherSignature)
	}
	pub := PublisherOf(v.Dataset)
	sig, err := c.cfg.Publishers.Verify(ctx, pub, f.PublisherSignature, f.Body)
	if err != nil {
		return untrusted("%s does not verify with the %s's keys: %s", HeaderPublisherSignature, pub, short(err.Error()))
	}
	if f.PublisherKID != "" && f.PublisherKID != sig.KID {
		return untrusted("%s names %q, the signature's kid is %q", HeaderPublisherKID, short(f.PublisherKID), sig.KID)
	}
	// The signature covers the bytes just read; what is installed is v,
	// read from another path (or built from a delta). Without this the
	// check would show only that the publisher signed something.
	if reason := signedMatches(v, f.Body); reason != "" {
		return untrusted("%s", reason)
	}
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
