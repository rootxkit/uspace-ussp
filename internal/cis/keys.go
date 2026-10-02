package cis

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// CounterNotifyKeysUnavailable counts notifications refused because the
// issuers' keys were never fetched since the process started.
const CounterNotifyKeysUnavailable = "cis_notify_keys_unavailable"

// DefaultKeysRetry is how often LazyVerifier tries to fetch the keys
// again while it has none.
const DefaultKeysRetry = 30 * time.Second

// LazyVerifier is the notification receiver's CompactVerifier, built
// when the issuers' JWKS can be fetched: a process starts while the
// CISP or the ANSP is down (B-08) and refuses every notification, 401
// and counted, until the keys are fetched (the CISP retries its
// deliveries for 24 h, and the reconciliation keeps the cache).
type LazyVerifier struct {
	cfg      coreauth.CompactConfig
	retry    time.Duration
	counters *core.Counters
	v        atomic.Pointer[coreauth.CompactVerifier]

	mu      sync.Mutex
	lastErr string
}

// NewLazyVerifier returns a verifier that is built by Run.
func NewLazyVerifier(cfg coreauth.CompactConfig, retry time.Duration, counters *core.Counters) *LazyVerifier {
	if retry <= 0 {
		retry = DefaultKeysRetry
	}
	if counters == nil {
		counters = &core.Counters{}
	}
	return &LazyVerifier{cfg: cfg, retry: retry, counters: counters}
}

// Build tries once to build the verifier (core fetches every issuer's
// JWKS).
func (l *LazyVerifier) Build(ctx context.Context) error {
	if l.v.Load() != nil {
		return nil
	}
	v, err := coreauth.NewCompactVerifier(ctx, l.cfg)
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
func (l *LazyVerifier) Run(ctx context.Context) {
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

// Verify verifies with the built verifier, or refuses.
func (l *LazyVerifier) Verify(ctx context.Context, token string) (coreauth.CompactClaims, json.RawMessage, error) {
	if v := l.v.Load(); v != nil {
		return v.Verify(ctx, token)
	}
	l.counters.Inc(CounterNotifyKeysUnavailable)
	return coreauth.CompactClaims{}, nil, &coreauth.TokenError{Counter: CounterNotifyKeysUnavailable, Claim: "kid",
		Reason: "the notification issuers' keys have not been fetched since this process started"}
}

// Counters are core's verifier counters once built, nil before.
func (l *LazyVerifier) Counters() *core.Counters {
	if v := l.v.Load(); v != nil {
		return v.Counters()
	}
	return nil
}

// Probe is the readiness of the notification keys: up once fetched
// (core then keeps them, refreshing on its own), down before with the
// last error.
func (l *LazyVerifier) Probe(context.Context) (obs.State, string) {
	if l.v.Load() != nil {
		return obs.StateUp, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastErr == "" {
		return obs.StateUnknown, "the notification issuers' keys are being fetched"
	}
	return obs.StateDown, "the notification issuers' keys are not fetched: " + l.lastErr
}
