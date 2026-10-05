package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
)

// Counters of the persisted JWKS (PLAN §15.2 Q35 b).
const (
	// CounterJWKSCacheSaved counts JWKS written to the store.
	CounterJWKSCacheSaved = "jwks_cache_saved"
	// CounterJWKSCacheSaveFailed counts writes the store refused.
	CounterJWKSCacheSaveFailed = "jwks_cache_save_failed"
	// CounterJWKSCacheRestored counts issuers whose keys a start took
	// from the store because their JWKS could not be fetched.
	CounterJWKSCacheRestored = "jwks_cache_restored"
	// CounterJWKSCacheRefused counts stored JWKS not used: unreadable,
	// for another issuer, or older than CacheMaxAge.
	CounterJWKSCacheRefused = "jwks_cache_refused"
	// CounterJWKSCacheExpired counts tokens refused because the only keys
	// of their issuer are a stored JWKS past CacheMaxAge.
	CounterJWKSCacheExpired = "jwks_cache_expired"
)

// DefaultJWKSCacheMaxAge is how long a stored JWKS may verify tokens
// while its issuer cannot be fetched: spec 05 §6 "JWKS cached 24 h"
// (pending GCAA; USSP_JWKS_CACHE_MAX_AGE_S).
const DefaultJWKSCacheMaxAge = 24 * time.Hour

// jwksSaveEvery is how often an unchanged JWKS is written again, so its
// fetched_at stays recent while the issuer answers.
const jwksSaveEvery = time.Hour

// maxCachedJWKSBytes bounds a stored JWKS read back.
const maxCachedJWKSBytes = 64 << 10

// JWKSStore keeps each ecosystem issuer's last fetched JWKS outside the
// process (the jwks_cache bucket, bus.KVStore), so a process started
// while a token service is down still verifies that service's tokens.
type JWKSStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Put(ctx context.Context, key string, value []byte) error
}

// CachedJWKS is one issuer's JWKS as the store holds it.
type CachedJWKS struct {
	Issuer    string          `json:"issuer"`
	FetchedAt time.Time       `json:"fetched_at"`
	JWKS      json.RawMessage `json:"jwks"`
}

// JWKSCacheKey is an issuer's key in the store: a hash, since an issuer
// URL holds characters a KV key may not.
func JWKSCacheKey(issuer string) string {
	h := sha256.Sum256([]byte(issuer))
	return hex.EncodeToString(h[:16])
}

// cachedEco is, per issuer, the verifier built from its stored JWKS and
// when that JWKS was fetched.
type cachedEco struct {
	v         map[string]*coreauth.Verifier
	fetchedAt map[string]time.Time
}

// storedJWKS answers core's JWKS fetch with a stored document: core
// parses and indexes it exactly as one it fetched (JWT and JWS go
// through uspace-core/auth), and nothing leaves the process.
type storedJWKS struct {
	url string
	doc []byte
}

// RoundTrip answers the stored document for its URL, 404 for any other.
func (s storedJWKS) RoundTrip(r *http.Request) (*http.Response, error) {
	code, body := http.StatusOK, s.doc
	if r.URL.String() != s.url {
		code, body = http.StatusNotFound, nil
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

func (v *Verifier) maxAge() time.Duration {
	if v.cfg.CacheMaxAge > 0 {
		return v.cfg.CacheMaxAge
	}
	return DefaultJWKSCacheMaxAge
}

// jwksSaveTimeout bounds one write to the store.
const jwksSaveTimeout = 2 * time.Second

// persist hands iss's JWKS doc (fetched at now) to the store when it
// changed or was last written jwksSaveEvery ago. The write runs in the
// background, at most one per issuer at a time and bounded by
// jwksSaveTimeout, so neither the readiness probe nor the build ever
// waits on the bus (a bus outage made /readyz miss its 2 s bound).
func (v *Verifier) persist(ctx context.Context, iss string, doc []byte, now time.Time) {
	if v.cfg.Cache == nil {
		return
	}
	sum := sha256.Sum256(doc)
	v.mu.Lock()
	last, saved := v.savedAt[iss]
	same := bytes.Equal(v.savedSum[iss], sum[:])
	if (saved && same && now.Sub(last) < jwksSaveEvery) || v.saving[iss] {
		v.mu.Unlock()
		return
	}
	v.saving[iss] = true
	v.mu.Unlock()
	data, err := json.Marshal(CachedJWKS{Issuer: iss, FetchedAt: now.UTC(), JWKS: doc})
	if err != nil {
		v.counters.Inc(CounterJWKSCacheSaveFailed)
		v.mu.Lock()
		delete(v.saving, iss)
		v.mu.Unlock()
		return
	}
	wctx := context.WithoutCancel(ctx)
	v.saves.Go(func() {
		wctx, cancel := context.WithTimeout(wctx, jwksSaveTimeout)
		defer cancel()
		err := v.cfg.Cache.Put(wctx, JWKSCacheKey(iss), data)
		v.mu.Lock()
		defer v.mu.Unlock()
		delete(v.saving, iss)
		if err != nil {
			v.counters.Inc(CounterJWKSCacheSaveFailed)
			return
		}
		v.counters.Inc(CounterJWKSCacheSaved)
		v.savedAt[iss], v.savedSum[iss] = now, sum[:]
	})
}

// restore builds the verifier of the stored JWKS of every ecosystem
// issuer that has one younger than CacheMaxAge. It does nothing once one
// is built, or without a store.
func (v *Verifier) restore(ctx context.Context) {
	if v.cfg.Cache == nil || v.cached.Load() != nil {
		return
	}
	now := v.now()
	out := &cachedEco{v: map[string]*coreauth.Verifier{}, fetchedAt: map[string]time.Time{}}
	for _, iss := range v.ecoISS {
		data, found, err := v.cfg.Cache.Get(ctx, JWKSCacheKey(iss))
		if err != nil || !found {
			continue
		}
		c, err := decodeCached(data, iss)
		if err != nil || now.Sub(c.FetchedAt) >= v.maxAge() || c.FetchedAt.After(now) {
			v.counters.Inc(CounterJWKSCacheRefused)
			continue
		}
		url := v.cfg.Ecosystem.Issuers[iss].JWKSURL
		cfg := v.cfg.Ecosystem
		cfg.Issuers = map[string]coreauth.IssuerConfig{iss: {JWKSURL: url}}
		cfg.HTTPClient = &http.Client{Transport: storedJWKS{url: url, doc: c.JWKS}}
		cv, err := coreauth.NewVerifier(ctx, cfg)
		if err != nil {
			// Not a JWKS core can use.
			v.counters.Inc(CounterJWKSCacheRefused)
			continue
		}
		out.v[iss], out.fetchedAt[iss] = cv, c.FetchedAt
	}
	if len(out.v) == 0 {
		return
	}
	v.counters.Add(CounterJWKSCacheRestored, uint64(len(out.v)))
	v.cached.Store(out)
}

func decodeCached(data []byte, iss string) (CachedJWKS, error) {
	var c CachedJWKS
	if len(data) > maxCachedJWKSBytes {
		return c, errors.New("over the bound")
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.Issuer != iss || c.FetchedAt.IsZero() || len(c.JWKS) == 0 {
		return c, errors.New("not this issuer's")
	}
	return c, nil
}

// verifyCached verifies token with the stored keys of iss; false when
// there are none for it.
func (v *Verifier) verifyCached(ctx context.Context, iss, token string) (coreauth.Claims, bool, error) {
	c := v.cached.Load()
	if c == nil {
		return coreauth.Claims{}, false, nil
	}
	at, ok := c.fetchedAt[iss]
	if !ok {
		return coreauth.Claims{}, false, nil
	}
	if v.now().Sub(at) >= v.maxAge() {
		v.counters.Inc(CounterJWKSCacheExpired)
		return coreauth.Claims{}, true, &coreauth.TokenError{Counter: CounterJWKSUnavailable, Claim: "kid",
			Reason: "the issuer's keys could not be fetched and the stored ones are older than the cache's maximum age"}
	}
	cl, err := c.v[iss].Verify(ctx, token)
	return cl, true, err
}
