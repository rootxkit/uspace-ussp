package auth

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// memJWKS is the jwks_cache bucket in memory.
type memJWKS struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memJWKS) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *memJWKS) Put(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[key] = append([]byte(nil), value...)
	return nil
}

// cachedVerifier is newVerifier with the store (nil: none).
func cachedVerifier(t *testing.T, eco *ecosystem, c *clock, store JWKSStore) *Verifier {
	t.Helper()
	cfg := coreauth.Config{Audiences: []string{ownHost, labAlias}, StrictSessionClaims: true, Now: c.fn(),
		Issuers: map[string]coreauth.IssuerConfig{eco.URL: {JWKSURL: eco.JWKS}}}
	v, err := NewVerifier(context.Background(), VerifierConfig{Ecosystem: cfg, RetryInterval: 10 * time.Millisecond, Cache: store})
	mustNoErr(t, err)
	return v
}

// startDuringOutage is a process started while the token service is
// down: its build fails, and what Run does next (restore) is done.
func startDuringOutage(t *testing.T, eco *ecosystem, c *clock, store JWKSStore) *Verifier {
	t.Helper()
	v := cachedVerifier(t, eco, c, store)
	if err := v.BuildEcosystem(context.Background()); err == nil {
		t.Fatal("built while the token service is down")
	}
	v.restore(context.Background())
	return v
}

// Q35 b: a process that fetched an issuer's JWKS stores it; one started
// later while that token service is down verifies its tokens with the
// stored keys, says so on /readyz (degraded, not down), and goes back to
// the fetched keys once the service answers. Its twin without a store
// refuses them jwks_unavailable, as before.
func TestAStartDuringATokenOutageVerifiesWithTheStoredJWKS(t *testing.T) {
	c := newClock(t0())
	eco := newEcosystem(t)
	store := &memJWKS{}
	ctx := context.Background()
	first := cachedVerifier(t, eco, c, store)
	mustNoErr(t, first.BuildEcosystem(ctx))
	first.saves.Wait()
	if first.Counters().Get(CounterJWKSCacheSaved) != 1 {
		t.Fatalf("the fetched JWKS was not stored: %v", first.Counters().Snapshot())
	}

	eco.Down()
	c.Add(time.Hour)
	v := startDuringOutage(t, eco, c, store)
	claims, err := v.Verify(ctx, eco.token(t, "cisp-01", ownHost, []string{"cis.read"}, c.Now()))
	if err != nil || claims.Subject != "cisp-01" || v.Counters().Get(CounterJWKSCacheRestored) != 1 {
		t.Fatalf("a token of the issuer down at start: %v %+v %v", err, claims, v.Counters().Snapshot())
	}
	if st, detail := v.Probe(ctx); st != obs.StateDegraded || !strings.Contains(detail, "stored keys") {
		t.Fatalf("readiness with the stored keys: %s %q", st, detail)
	}

	none := startDuringOutage(t, eco, c, nil)
	_, err = none.Verify(ctx, eco.token(t, "cisp-01", ownHost, []string{"cis.read"}, c.Now()))
	if te := tokenErr(t, err); te.Counter != CounterJWKSUnavailable {
		t.Fatalf("without a store: %+v", te)
	}

	eco.Up()
	mustNoErr(t, v.BuildEcosystem(ctx))
	if v.cached.Load() != nil || !v.EcosystemReady() {
		t.Fatal("the fetched keys did not replace the stored ones")
	}
	if _, err := v.Verify(ctx, eco.token(t, "cisp-01", ownHost, nil, c.Now())); err != nil {
		t.Fatalf("after the service is back: %v", err)
	}
}

// The stored keys verify for at most CacheMaxAge from their fetch (05
// §6, 24 h): a start later than that does not use them, and a process
// that restored them refuses its issuer's tokens once they reach it.
func TestTheStoredJWKSIsBoundedByItsAge(t *testing.T) {
	c := newClock(t0())
	eco := newEcosystem(t)
	store := &memJWKS{}
	ctx := context.Background()
	first := cachedVerifier(t, eco, c, store)
	mustNoErr(t, first.BuildEcosystem(ctx))
	first.saves.Wait()
	eco.Down()

	c.Add(DefaultJWKSCacheMaxAge - time.Minute)
	v := startDuringOutage(t, eco, c, store)
	if _, err := v.Verify(ctx, eco.token(t, "x", ownHost, nil, c.Now())); err != nil {
		t.Fatalf("within the age: %v", err)
	}
	c.Add(time.Minute)
	_, err := v.Verify(ctx, eco.token(t, "x", ownHost, nil, c.Now()))
	if te := tokenErr(t, err); te.Counter != CounterJWKSUnavailable || v.Counters().Get(CounterJWKSCacheExpired) != 1 {
		t.Fatalf("past the age: %+v", te)
	}
	if st, _ := v.Probe(ctx); st != obs.StateDown {
		t.Fatalf("readiness past the age: %s", st)
	}

	late := startDuringOutage(t, eco, c, store)
	if late.cached.Load() != nil || late.Counters().Get(CounterJWKSCacheRefused) != 1 {
		t.Fatalf("a start past the age used the stored keys: %v", late.Counters().Snapshot())
	}
}

// What the store holds is checked before it is used: another issuer's
// entry under this issuer's key, or one that is not a JWKS, is refused.
func TestAStoredJWKSThatDoesNotReadIsRefused(t *testing.T) {
	c := newClock(t0())
	eco := newEcosystem(t)
	ctx := context.Background()
	for name, value := range map[string]CachedJWKS{
		"another issuer": {Issuer: "https://other.test", FetchedAt: c.Now(), JWKS: json.RawMessage(`{"keys":[]}`)},
		"not a JWKS":     {Issuer: eco.URL, FetchedAt: c.Now(), JWKS: json.RawMessage(`{"keys":"x"}`)},
		"no fetch time":  {Issuer: eco.URL, JWKS: json.RawMessage(`{"keys":[]}`)},
	} {
		store := &memJWKS{}
		data, err := json.Marshal(value)
		mustNoErr(t, err)
		mustNoErr(t, store.Put(ctx, JWKSCacheKey(eco.URL), data))
		eco.Down()
		v := startDuringOutage(t, eco, c, store)
		if v.cached.Load() != nil || v.Counters().Get(CounterJWKSCacheRefused) != 1 {
			t.Fatalf("%s: used (%v)", name, v.Counters().Snapshot())
		}
	}
}

// An unchanged JWKS is written again only every jwksSaveEvery; a
// changed one at once.
func TestTheStoredJWKSIsRewrittenWhenItChanges(t *testing.T) {
	c := newClock(t0())
	eco := newEcosystem(t)
	store := &memJWKS{}
	ctx := context.Background()
	v := cachedVerifier(t, eco, c, store)
	mustNoErr(t, v.BuildEcosystem(ctx))
	v.saves.Wait()
	v.Probe(ctx)
	v.saves.Wait()
	if n := v.Counters().Get(CounterJWKSCacheSaved); n != 1 {
		t.Fatalf("an unchanged JWKS written %d times", n)
	}
	c.Add(jwksSaveEvery)
	v.Probe(ctx)
	v.saves.Wait()
	if n := v.Counters().Get(CounterJWKSCacheSaved); n != 2 {
		t.Fatalf("not rewritten after jwksSaveEvery: %d", n)
	}
	v.persist(ctx, eco.URL, []byte(`{"keys":[]}`), c.Now())
	v.saves.Wait()
	if n := v.Counters().Get(CounterJWKSCacheSaved); n != 3 {
		t.Fatalf("a changed JWKS not written: %d", n)
	}
}

// stuckJWKS is a store whose writes wait until released: the bus away.
type stuckJWKS struct {
	memJWKS
	release chan struct{}
}

func (s *stuckJWKS) Put(ctx context.Context, key string, value []byte) error {
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.memJWKS.Put(ctx, key, value)
}

// A store that does not answer never holds up the readiness probe or
// the build: the write waits in the background, one per issuer. Its
// twin, the store answering, is the tests above.
func TestAStoreThatDoesNotAnswerHoldsUpNothing(t *testing.T) {
	c := newClock(t0())
	eco := newEcosystem(t)
	store := &stuckJWKS{release: make(chan struct{})}
	ctx := context.Background()
	v := cachedVerifier(t, eco, c, store)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mustNoErr(t, v.BuildEcosystem(ctx))
		for range 3 {
			c.Add(jwksSaveEvery)
			if st, _ := v.Probe(ctx); st != obs.StateUp {
				t.Errorf("probe %s", st)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(jwksSaveTimeout):
		t.Fatal("the build and the probe waited on the store")
	}
	v.mu.Lock()
	inFlight := len(v.saving)
	v.mu.Unlock()
	if inFlight != 1 {
		t.Fatalf("%d writes in flight for one issuer, want 1", inFlight)
	}
	close(store.release)
	v.saves.Wait()
	if v.Counters().Get(CounterJWKSCacheSaved) != 1 {
		t.Fatalf("saved %d", v.Counters().Get(CounterJWKSCacheSaved))
	}
}
