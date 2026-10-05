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
	// process started while that token service was down (B-08).
	CounterJWKSUnavailable = "jwks_unavailable"
	// CounterEcosystemBuildFailed counts a failed attempt to fetch an
	// ecosystem issuer's JWKS at start; the attempt is repeated.
	CounterEcosystemBuildFailed = "ecosystem_verifier_build_failed"
)

// Names of the counter sets CounterSets returns.
const (
	CounterSetAuth = "auth"
	CounterSetOwn  = "auth_own"
	// CounterSetEcosystem is the set of the first ecosystem issuer in
	// the order of their iss; EcosystemSet names the others.
	CounterSetEcosystem = "auth_ecosystem"
)

// EcosystemSet is the counter set name of the i-th ecosystem issuer
// (0-based, in the order of their iss): auth_ecosystem, auth_ecosystem_2,
// auth_ecosystem_3, ... With one issuer the set keeps the name it always
// had.
func EcosystemSet(i int) string {
	if i == 0 {
		return CounterSetEcosystem
	}
	return fmt.Sprintf("%s_%d", CounterSetEcosystem, i+1)
}

// DepJWKS is the readiness dependency of the ecosystem issuers' keys.
const DepJWKS = "jwks"

// DefaultRetryInterval is the wait between two attempts to build the
// ecosystem verifiers while an issuer's JWKS cannot be fetched.
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
	// verifiers; zero is DefaultRetryInterval.
	RetryInterval time.Duration
	// ProbeClient fetches the JWKS for the readiness probe; nil is a
	// client that follows no redirect.
	ProbeClient *http.Client
	// OnBuilt, when set, is called once per ecosystem issuer when its
	// verifier is built, with the counter set's name (EcosystemSet) and
	// the set: core creates the set with the verifier, so the process
	// publishes it then.
	OnBuilt func(set string, c *core.Counters)
}

// Verifier verifies every token this system accepts with core's
// Verifier (RS256, kid, allow-listed iss, aud in USSP_AUDIENCES, 30 s
// skew, jti, StrictSessionClaims): tokens of this USSP's own issuer
// against its keys held in memory, every other token against the JWKS of
// the ecosystem issuer its iss names. The unverified iss only chooses
// which verifier judges the token: each allow-lists exactly one issuer
// and refuses a token of another, which is how a token whose iss is on
// no list is refused (rejected_issuer).
//
// Each ecosystem issuer has its own core verifier, built from its own
// JWKS (core fetches the keys when it is built and refuses to build
// without them). One issuer whose JWKS cannot be fetched at start holds
// back only its own tokens: until its first fetch succeeds they are
// refused as jwks_unavailable (503) and /readyz says jwks is down for
// that issuer, while every other issuer's tokens verify. Before, one
// verifier held every issuer, and an authority down at start refused
// the lab issuer's tokens too (uspace-lab conformance run of 2026-10-05,
// WP-19). A process does not wait for a token service (B-08). After the
// first fetch core serves the cached keys through any outage (T5) and
// the probe reports jwks degraded with the cache's age.
type Verifier struct {
	cfg    VerifierConfig
	own    *coreauth.Verifier
	ownIss string
	ecoISS []string
	// eco holds one verifier per ecosystem issuer; its keys are ecoISS,
	// fixed at construction, so the map is only read afterwards.
	eco      map[string]*atomic.Pointer[coreauth.Verifier]
	counters core.Counters
	now      func() time.Time

	mu      sync.Mutex
	fetched map[string]time.Time // issuer -> last successful JWKS fetch
	lastErr map[string]string
}

// NewVerifier builds the own-issuer verifier at once; the ecosystem ones
// are built by Run (or BuildEcosystem).
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
		eco: map[string]*atomic.Pointer[coreauth.Verifier]{}}
	if v.now == nil {
		v.now = time.Now
	}
	v.ecoISS = slices.Sorted(maps.Keys(cfg.Ecosystem.Issuers))
	for _, iss := range v.ecoISS {
		v.eco[iss] = &atomic.Pointer[coreauth.Verifier]{}
	}
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
// own-issuer verifier's and each ecosystem issuer's verifier once built
// (EcosystemSet).
func (v *Verifier) CounterSets() map[string]*core.Counters {
	out := map[string]*core.Counters{CounterSetAuth: &v.counters}
	if v.own != nil {
		out[CounterSetOwn] = v.own.Counters()
	}
	for i, iss := range v.ecoISS {
		if eco := v.eco[iss].Load(); eco != nil {
			out[EcosystemSet(i)] = eco.Counters()
		}
	}
	return out
}

// Verify verifies token. A refusal is a *coreauth.TokenError naming the
// claim; its Counter is core's, or CounterJWKSUnavailable.
func (v *Verifier) Verify(ctx context.Context, token string) (coreauth.Claims, error) {
	iss := peek(token).Iss
	if v.own != nil && iss == v.ownIss {
		return v.own.Verify(ctx, token)
	}
	if p, ok := v.eco[iss]; ok {
		if eco := p.Load(); eco != nil {
			return eco.Verify(ctx, token)
		}
		v.counters.Inc(CounterJWKSUnavailable)
		return coreauth.Claims{}, &coreauth.TokenError{Counter: CounterJWKSUnavailable, Claim: "kid",
			Reason: "the issuer's keys have not been fetched since this process started"}
	}
	// An iss on no list is core's judgement, through the first ecosystem
	// verifier that is built (rejected_issuer before any JWKS fetch,
	// counted in that verifier's set). Only while none is built is it
	// refused here, as it always was.
	for _, e := range v.ecoISS {
		if eco := v.eco[e].Load(); eco != nil {
			return eco.Verify(ctx, token)
		}
	}
	v.counters.Inc(coreauth.CounterRejectedIssuer)
	return coreauth.Claims{}, &coreauth.TokenError{Counter: coreauth.CounterRejectedIssuer, Claim: "iss", Reason: "the issuer is not allow-listed"}
}

// EcosystemReady reports whether the verifier of every ecosystem issuer
// is built.
func (v *Verifier) EcosystemReady() bool {
	for _, iss := range v.ecoISS {
		if !v.IssuerReady(iss) {
			return false
		}
	}
	return true
}

// IssuerReady reports whether the verifier of ecosystem issuer iss is
// built; false for an issuer that is not on the list.
func (v *Verifier) IssuerReady(iss string) bool {
	p, ok := v.eco[iss]
	return ok && p.Load() != nil
}

// BuildEcosystem tries once to build the verifier of every ecosystem
// issuer that has none yet, each from its own JWKS, so one that fails
// leaves the others built. It returns the failures joined: nil when
// every issuer's verifier is built, or when there is no issuer.
func (v *Verifier) BuildEcosystem(ctx context.Context) error {
	var errs []error
	for i, iss := range v.ecoISS {
		if v.IssuerReady(iss) {
			continue
		}
		c := v.cfg.Ecosystem
		c.Issuers = map[string]coreauth.IssuerConfig{iss: v.cfg.Ecosystem.Issuers[iss]}
		eco, err := coreauth.NewVerifier(ctx, c)
		if err != nil {
			v.counters.Inc(CounterEcosystemBuildFailed)
			v.mu.Lock()
			v.lastErr[iss] = err.Error()
			v.mu.Unlock()
			errs = append(errs, fmt.Errorf("%s: %w", iss, err))
			continue
		}
		now := v.now()
		v.mu.Lock()
		v.fetched[iss] = now
		delete(v.lastErr, iss)
		v.mu.Unlock()
		v.eco[iss].Store(eco)
		if v.cfg.OnBuilt != nil {
			v.cfg.OnBuilt(EcosystemSet(i), eco.Counters())
		}
	}
	return errors.Join(errs...)
}

// Run builds the ecosystem verifiers, trying the ones not built yet
// again every RetryInterval until all are built or ctx ends. Started
// once per process, on the process's work context: never on a
// request's (E-14).
func (v *Verifier) Run(ctx context.Context) {
	t := time.NewTicker(v.cfg.RetryInterval)
	defer t.Stop()
	for {
		if v.BuildEcosystem(ctx) == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Probe is the readiness check of the ecosystem JWKS: it fetches every
// issuer's JWKS URL, all at once. Every fetch answers: up. One fails
// while that issuer's verifier holds its keys: degraded, "cached, age N
// s" (core keeps verifying from the cache, T5). One fails and was never
// fetched: down. No ecosystem issuer configured: down, saying no
// ecosystem token is accepted (the dependency is optional).
//
// The fetches end a fifth of the check's remaining time before the
// check's own bound, so an issuer whose JWKS does not answer (a host
// that resolves slowly, a proxy that holds the request) is reported by
// name, unreachable, beside the others' states, instead of the whole
// check overrunning and saying only that it did not answer.
func (v *Verifier) Probe(ctx context.Context) (obs.State, string) {
	if len(v.ecoISS) == 0 {
		return obs.StateDown, "USSP_TOKEN_ISSUERS is empty: no ecosystem token is accepted"
	}
	fctx := ctx
	if dl, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		fctx, cancel = context.WithDeadline(ctx, dl.Add(-time.Until(dl)/5))
		defer cancel()
	}
	errs := make([]error, len(v.ecoISS))
	var wg sync.WaitGroup
	for i, iss := range v.ecoISS {
		wg.Go(func() { errs[i] = v.fetchJWKS(fctx, v.cfg.Ecosystem.Issuers[iss].JWKSURL) })
	}
	wg.Wait()
	worst := obs.StateUp
	var details []string
	unbuilt := 0
	for i, iss := range v.ecoISS {
		built := v.IssuerReady(iss)
		if !built {
			unbuilt++
		}
		err := errs[i]
		now := v.now()
		v.mu.Lock()
		if err == nil {
			v.fetched[iss] = now
			delete(v.lastErr, iss)
			v.mu.Unlock()
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
		worst = obs.StateDown
		details = append(details, fmt.Sprintf("%s: never fetched (%v)", iss, err))
	}
	if worst == obs.StateUp && unbuilt > 0 {
		// Every JWKS answers now but a verifier has not been built yet:
		// that issuer's tokens are refused until Run's next attempt.
		return obs.StateDown, "the JWKS answer; the verifier is built at the next attempt"
	}
	return worst, strings.Join(details, "; ")
}

// fetchJWKS checks that url answers 200 with a JSON object holding a
// keys array.
func (v *Verifier) fetchJWKS(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := v.cfg.ProbeClient.Do(req)
	if err != nil {
		return errors.New("unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProbeBytes)).Decode(&set); err != nil || set.Keys == nil {
		return errors.New("not a JWKS")
	}
	return nil
}
