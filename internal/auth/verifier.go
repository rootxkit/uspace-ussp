package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Counter names of the Verifier's own refusals (core's verifiers count
// theirs: rejected_*, jwks_refresh*).
const (
	// CounterJWKSUnavailable counts a token of an allow-listed ecosystem
	// issuer refused because that issuer's keys were never fetched: the
	// process started while the token service was down (B-08).
	CounterJWKSUnavailable = "jwks_unavailable"
	// CounterEcosystemBuildFailed counts a failed attempt to fetch the
	// ecosystem JWKS at start; the attempt is repeated.
	CounterEcosystemBuildFailed = "ecosystem_verifier_build_failed"
)

// Names of the counter sets CounterSets returns.
const (
	CounterSetAuth      = "auth"
	CounterSetOwn       = "auth_own"
	CounterSetEcosystem = "auth_ecosystem"
)

// DepJWKS is the readiness dependency of the ecosystem issuers' keys.
const DepJWKS = "jwks"

// DefaultRetryInterval is the wait between two attempts to build the
// ecosystem verifier while the issuers' JWKS cannot be fetched.
const DefaultRetryInterval = 15 * time.Second

// maxProbeBytes bounds a JWKS read by the readiness probe.
const maxProbeBytes = 1 << 20

// VerifierConfig configures a Verifier.
type VerifierConfig struct {
	// Ecosystem is the core configuration of the allow-listed ecosystem
	// issuers and the accepted audiences (config.VerifierConfig);
	// Ecosystem.Issuers may be empty (no ecosystem token is accepted).
	Ecosystem coreauth.Config
	// Own is this USSP's issuer; nil when no key is configured (no
	// operator or session token is accepted).
	Own *Issuer
	// RetryInterval is the wait between attempts to build the ecosystem
	// verifier; zero is DefaultRetryInterval.
	RetryInterval time.Duration
	// ProbeClient fetches the JWKS for the readiness probe; nil is a
	// client that follows no redirect.
	ProbeClient *http.Client
	// Cache, when set, keeps each ecosystem issuer's JWKS across
	// restarts (the jwks_cache bucket; PLAN §15.2 Q35 b): written when a
	// fetch answers, read when a start cannot fetch, and used for at
	// most CacheMaxAge (DefaultJWKSCacheMaxAge, 05 §6) from its fetch.
	Cache       JWKSStore
	CacheMaxAge time.Duration
}

// Verifier verifies every token this system accepts with core's
// Verifier (RS256, kid, allow-listed iss, aud in USSP_AUDIENCES, 30 s
// skew, jti, StrictSessionClaims): tokens of this USSP's own issuer
// against its keys held in memory, every other token against the
// ecosystem issuers' JWKS. The unverified iss only chooses which of the
// two verifies; each refuses an issuer it does not allow-list.
//
// The ecosystem verifier needs the issuers' JWKS: core fetches them when
// it is built and refuses to build without them. A process does not
// wait for the token service (B-08): until the first fetch succeeds an
// ecosystem token is refused as jwks_unavailable (503) and /readyz says
// jwks is down; after it, core serves the cached keys through any
// outage (T5) and the probe reports jwks degraded with the cache's age.
// With a Cache, a process started while an issuer cannot be fetched
// verifies that issuer's tokens with the JWKS a previous fetch stored,
// for at most CacheMaxAge from that fetch (Q35 b; before, the cache was
// in memory and a restart during a token-service outage refused every
// token of that service until it answered).
type Verifier struct {
	cfg      VerifierConfig
	own      *coreauth.Verifier
	ownIss   string
	eco      atomic.Pointer[coreauth.Verifier]
	ecoISS   []string
	counters core.Counters
	now      func() time.Time

	cached atomic.Pointer[cachedEco]

	mu       sync.Mutex
	fetched  map[string]time.Time // issuer -> last successful JWKS fetch
	lastErr  map[string]string
	savedAt  map[string]time.Time // issuer -> last write to Cache
	savedSum map[string][]byte
}

// NewVerifier builds the own-issuer verifier at once; the ecosystem one
// is built by Run (or BuildEcosystem).
func NewVerifier(ctx context.Context, cfg VerifierConfig) (*Verifier, error) {
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = DefaultRetryInterval
	}
	if cfg.ProbeClient == nil {
		cfg.ProbeClient = &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	v := &Verifier{cfg: cfg, now: cfg.Ecosystem.Now, fetched: map[string]time.Time{}, lastErr: map[string]string{},
		savedAt: map[string]time.Time{}, savedSum: map[string][]byte{}}
	if v.now == nil {
		v.now = time.Now
	}
	v.ecoISS = slices.Sorted(maps.Keys(cfg.Ecosystem.Issuers))
	if cfg.Own != nil {
		if _, clash := cfg.Ecosystem.Issuers[cfg.Own.URL]; clash {
			return nil, core.Fieldf("USSP_TOKEN_ISSUERS", "lists this USSP's own issuer %s", cfg.Own.URL)
		}
		oc := cfg.Ecosystem
		oc.Issuers = map[string]coreauth.IssuerConfig{cfg.Own.URL: cfg.Own.VerifierIssuer()}
		own, err := coreauth.NewVerifier(ctx, oc)
		if err != nil {
			return nil, fmt.Errorf("own-issuer verifier: %w", err)
		}
		v.own, v.ownIss = own, cfg.Own.URL
	}
	return v, nil
}

// OwnIssuer is the iss of this USSP's tokens, or "" without a key.
func (v *Verifier) OwnIssuer() string { return v.ownIss }

// Counters are the verifier's own refusals; CounterSets has every set.
func (v *Verifier) Counters() *core.Counters { return &v.counters }

// CounterSets are the counter sets to publish: this verifier's, the
// own-issuer verifier's and the ecosystem verifier's once built.
func (v *Verifier) CounterSets() map[string]*core.Counters {
	out := map[string]*core.Counters{CounterSetAuth: &v.counters}
	if v.own != nil {
		out[CounterSetOwn] = v.own.Counters()
	}
	if eco := v.eco.Load(); eco != nil {
		out[CounterSetEcosystem] = eco.Counters()
	}
	return out
}

// Verify verifies token. A refusal is a *coreauth.TokenError naming the
// claim; its Counter is core's, or CounterJWKSUnavailable.
func (v *Verifier) Verify(ctx context.Context, token string) (coreauth.Claims, error) {
	if v.own != nil && peek(token).Iss == v.ownIss {
		return v.own.Verify(ctx, token)
	}
	if eco := v.eco.Load(); eco != nil {
		return eco.Verify(ctx, token)
	}
	if cl, ok, err := v.verifyCached(ctx, peek(token).Iss, token); ok {
		return cl, err
	}
	if len(v.ecoISS) > 0 && slices.Contains(v.ecoISS, peek(token).Iss) {
		v.counters.Inc(CounterJWKSUnavailable)
		return coreauth.Claims{}, &coreauth.TokenError{Counter: CounterJWKSUnavailable, Claim: "kid",
			Reason: "the issuer's keys have not been fetched since this process started"}
	}
	v.counters.Inc(coreauth.CounterRejectedIssuer)
	return coreauth.Claims{}, &coreauth.TokenError{Counter: coreauth.CounterRejectedIssuer, Claim: "iss", Reason: "the issuer is not allow-listed"}
}

// EcosystemReady reports whether the ecosystem verifier is built.
func (v *Verifier) EcosystemReady() bool { return v.eco.Load() != nil }

// BuildEcosystem tries once to build the ecosystem verifier (core
// fetches every issuer's JWKS). Without ecosystem issuers it does
// nothing.
func (v *Verifier) BuildEcosystem(ctx context.Context) error {
	if len(v.ecoISS) == 0 || v.eco.Load() != nil {
		return nil
	}
	eco, err := coreauth.NewVerifier(ctx, v.cfg.Ecosystem)
	if err != nil {
		v.counters.Inc(CounterEcosystemBuildFailed)
		v.mu.Lock()
		for _, iss := range v.ecoISS {
			v.lastErr[iss] = err.Error()
		}
		v.mu.Unlock()
		return err
	}
	now := v.now()
	v.mu.Lock()
	for _, iss := range v.ecoISS {
		v.fetched[iss] = now
		delete(v.lastErr, iss)
	}
	v.mu.Unlock()
	v.eco.Store(eco)
	v.cached.Store(nil)
	// What core fetched is stored for a later start: it reads the same
	// URLs, so a fetch here stores the set it holds.
	for _, iss := range v.ecoISS {
		if doc, err := v.fetchJWKS(ctx, v.cfg.Ecosystem.Issuers[iss].JWKSURL); err == nil {
			v.persist(ctx, iss, doc, now)
		}
	}
	return nil
}

// Run builds the ecosystem verifier, trying again every RetryInterval
// until it succeeds or ctx ends. Started once per process, on the
// process's work context: never on a request's (E-14).
func (v *Verifier) Run(ctx context.Context) {
	t := time.NewTicker(v.cfg.RetryInterval)
	defer t.Stop()
	for {
		if v.BuildEcosystem(ctx) == nil {
			return
		}
		// Not built: an issuer cannot be fetched. Its stored JWKS, when
		// young enough, verifies meanwhile.
		v.restore(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Probe is the readiness check of the ecosystem JWKS: it fetches each
// issuer's JWKS URL. Every fetch answers: up. One fails while the
// verifier holds that issuer's keys: degraded, "cached, age N s" (core
// keeps verifying from the cache, T5). One fails and was never fetched:
// down. No ecosystem issuer configured: down, saying no ecosystem token
// is accepted (the dependency is optional).
func (v *Verifier) Probe(ctx context.Context) (obs.State, string) {
	if len(v.ecoISS) == 0 {
		return obs.StateDown, "USSP_TOKEN_ISSUERS is empty: no ecosystem token is accepted"
	}
	built := v.eco.Load() != nil
	worst := obs.StateUp
	var details []string
	for _, iss := range v.ecoISS {
		doc, err := v.fetchJWKS(ctx, v.cfg.Ecosystem.Issuers[iss].JWKSURL)
		now := v.now()
		v.mu.Lock()
		if err == nil {
			v.fetched[iss] = now
			delete(v.lastErr, iss)
			v.mu.Unlock()
			if built {
				v.persist(ctx, iss, doc, now)
			}
			continue
		}
		v.lastErr[iss] = err.Error()
		last, ok := v.fetched[iss]
		v.mu.Unlock()
		if built && ok {
			if worst == obs.StateUp {
				worst = obs.StateDegraded
			}
			details = append(details, fmt.Sprintf("%s: cached, age %d s (%v)", iss, int64(now.Sub(last)/time.Second), err))
			continue
		}
		if c := v.cached.Load(); !built && c != nil {
			if at, ok := c.fetchedAt[iss]; ok && now.Sub(at) < v.maxAge() {
				if worst == obs.StateUp {
					worst = obs.StateDegraded
				}
				details = append(details, fmt.Sprintf("%s: not fetched since this process started; the stored keys fetched %s verify (age %d s, at most %d s) (%v)",
					iss, at.UTC().Format(time.RFC3339), int64(now.Sub(at)/time.Second), int64(v.maxAge()/time.Second), err))
				continue
			}
		}
		worst = obs.StateDown
		details = append(details, fmt.Sprintf("%s: never fetched (%v)", iss, err))
	}
	if worst == obs.StateUp && !built {
		// The JWKS answer now but the verifier has not been built yet:
		// tokens are still refused until Run's next attempt.
		return obs.StateDown, "the JWKS answer; the verifier is built at the next attempt"
	}
	return worst, strings.Join(details, "; ")
}

// fetchJWKS checks that url answers 200 with a JSON object holding a
// keys array, and returns the document.
func (v *Verifier) fetchJWKS(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := v.cfg.ProbeClient.Do(req)
	if err != nil {
		return nil, errors.New("unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	doc, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBytes))
	if err != nil {
		return nil, errors.New("unreachable")
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(doc, &set); err != nil || set.Keys == nil {
		return nil, errors.New("not a JWKS")
	}
	return doc, nil
}
