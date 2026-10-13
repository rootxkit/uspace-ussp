package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
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

// testCacheKey is the key of the processes of these tests;
// otherCacheKey one they do not hold.
var (
	testCacheKey  = bytes.Repeat([]byte{0x5a}, JWKSCacheKeyBytes)
	otherCacheKey = bytes.Repeat([]byte{0xa5}, JWKSCacheKeyBytes)
)

// cacheKeyFor is testCacheKey with a store, nil without.
func cacheKeyFor(store JWKSStore) []byte {
	if store == nil {
		return nil
	}
	return testCacheKey
}

// cachedVerifier is newVerifier with the store (nil: none).
func cachedVerifier(t *testing.T, eco *ecosystem, c *clock, store JWKSStore) *Verifier {
	t.Helper()
	cfg := coreauth.Config{Audiences: []string{ownHost, labAlias}, StrictSessionClaims: true, Now: c.fn(),
		Issuers: map[string]coreauth.IssuerConfig{eco.URL: {JWKSURL: eco.JWKS}}}
	v, err := NewVerifier(context.Background(), VerifierConfig{Ecosystem: cfg, RetryInterval: 10 * time.Millisecond,
		Cache: store, CacheKey: cacheKeyFor(store)})
	mustNoErr(t, err)
	return v
}

// sealed is entry as a process holding key writes it under storeKey.
func sealed(t *testing.T, key []byte, storeKey string, entry CachedJWKS) []byte {
	t.Helper()
	entry.MAC = jwksMAC(key, storeKey, entry)
	data, err := json.Marshal(entry)
	mustNoErr(t, err)
	return data
}

// forger is an issuer that says it is iss and signs with a key of its
// own (key A, not the ecosystem's): a leaked credential's planted keys.
func forger(t *testing.T, iss string) (*coreauth.Issuer, []byte) {
	t.Helper()
	k, _, _ := testKeys(t)
	f, err := coreauth.NewIssuer(iss, k, signingKey(t, k).KID)
	mustNoErr(t, err)
	doc, err := json.Marshal(f.JWKS())
	mustNoErr(t, err)
	return f, doc
}

// startDuringOutage is a process started while the token service is
// down: its build fails, which restores the stored keys.
func startDuringOutage(t *testing.T, eco *ecosystem, c *clock, store JWKSStore) *Verifier {
	t.Helper()
	v := cachedVerifier(t, eco, c, store)
	if err := v.BuildEcosystem(context.Background()); err == nil {
		t.Fatal("built while the token service is down")
	}
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
		// Written by this process (its MAC verifies): what it says is
		// then checked.
		mustNoErr(t, store.Put(ctx, JWKSCacheKey(eco.URL), sealed(t, testCacheKey, JWKSCacheKey(eco.URL), value)))
		eco.Down()
		v := startDuringOutage(t, eco, c, store)
		if v.cached.Load() != nil || v.Counters().Get(CounterJWKSCacheRefused) != 1 {
			t.Fatalf("%s: used (%v)", name, v.Counters().Snapshot())
		}
	}
}

// Any user of the NATS account can write jwks_cache, so a leaked
// process credential could plant a JWKS of its own keys under an
// issuer's key: a verifier restarted during that issuer's outage would
// then accept the tokens those keys sign. A stored JWKS is used only
// when its MAC verifies under the process's own key. Planted (no MAC,
// another key's MAC, a genuine entry moved from another key, a genuine
// entry with its keys replaced): refused, counted, and the forged token
// refused. The pair: the genuine entry restores and verifies the
// issuer's token, and still not the forged one.
func TestAPlantedJWKSIsRefusedAndAGenuineOneRestores(t *testing.T) {
	c := newClock(t0())
	ctx := context.Background()
	eco := newEcosystem(t)
	genuine := &memJWKS{}
	first := cachedVerifier(t, eco, c, genuine)
	mustNoErr(t, first.BuildEcosystem(ctx))
	first.saves.Wait()
	raw, found, err := genuine.Get(ctx, JWKSCacheKey(eco.URL))
	if err != nil || !found {
		t.Fatalf("nothing stored: %v", err)
	}
	var stored CachedJWKS
	mustNoErr(t, json.Unmarshal(raw, &stored))
	eco.Down()
	c.Add(time.Minute)

	forged, forgedKeys := forger(t, eco.URL)
	forgedToken := func() string {
		tok, err := forged.Issue("forged", ownHost, nil, 10*time.Minute, c.Now())
		mustNoErr(t, err)
		return tok
	}
	plant := CachedJWKS{Issuer: eco.URL, FetchedAt: c.Now(), JWKS: forgedKeys}
	unsealed, err := json.Marshal(plant)
	mustNoErr(t, err)
	swapped := stored
	swapped.JWKS = forgedKeys
	swappedData, err := json.Marshal(swapped)
	mustNoErr(t, err)
	for name, data := range map[string][]byte{
		"no MAC":                    unsealed,
		"another key's MAC":         sealed(t, otherCacheKey, JWKSCacheKey(eco.URL), plant),
		"moved from another key":    sealed(t, testCacheKey, JWKSCacheKey("https://other.test"), plant),
		"the keys of a genuine one": swappedData,
	} {
		store := &memJWKS{}
		mustNoErr(t, store.Put(ctx, JWKSCacheKey(eco.URL), data))
		v := startDuringOutage(t, eco, c, store)
		if v.cached.Load() != nil || v.Counters().Get(CounterJWKSCacheMACMismatch) != 1 || v.Counters().Get(CounterJWKSCacheRestored) != 0 {
			t.Fatalf("%s: used (%v)", name, v.Counters().Snapshot())
		}
		_, err := v.Verify(ctx, forgedToken())
		if te := tokenErr(t, err); te.Counter != CounterJWKSUnavailable {
			t.Fatalf("%s: a forged token: %+v", name, te)
		}
	}

	v := startDuringOutage(t, eco, c, genuine)
	if v.Counters().Get(CounterJWKSCacheRestored) != 1 || v.Counters().Get(CounterJWKSCacheMACMismatch) != 0 {
		t.Fatalf("the genuine entry was not restored: %v", v.Counters().Snapshot())
	}
	if cl, err := v.Verify(ctx, eco.token(t, "cisp-01", ownHost, nil, c.Now())); err != nil || cl.Subject != "cisp-01" {
		t.Fatalf("the issuer's token with the genuine stored keys: %v %+v", err, cl)
	}
	if _, err := v.Verify(ctx, forgedToken()); err == nil {
		t.Fatal("a forged token verified with the genuine stored keys")
	}
}

// A store is used only with the key that binds it; the key file holds
// the base64 of exactly 32 bytes, and an error never repeats what the
// file holds.
func TestTheJWKSCacheKeyIsRequiredAndRead(t *testing.T) {
	cfg := coreauth.Config{Audiences: []string{ownHost}}
	if _, err := NewVerifier(context.Background(), VerifierConfig{Ecosystem: cfg, Cache: &memJWKS{}}); err == nil ||
		!strings.Contains(err.Error(), "USSP_JWKS_CACHE_KEY_FILE") {
		t.Fatalf("a store without a key: %v", err)
	}
	if _, err := NewVerifier(context.Background(), VerifierConfig{Ecosystem: cfg, Cache: &memJWKS{}, CacheKey: testCacheKey}); err != nil {
		t.Fatalf("a store with a key: %v", err)
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		mustNoErr(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	key, err := LoadJWKSCacheKey(write("good", base64.StdEncoding.EncodeToString(testCacheKey)+"\n"))
	if err != nil || !bytes.Equal(key, testCacheKey) {
		t.Fatalf("a good key file: %v", err)
	}
	for name, body := range map[string]string{
		"short":    base64.StdEncoding.EncodeToString(testCacheKey[:16]),
		"not b64":  "not base64 at all",
		"too long": strings.Repeat("A", 2048),
	} {
		if _, err := LoadJWKSCacheKey(write(name, body)); err == nil || strings.Contains(err.Error(), body) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := LoadJWKSCacheKey(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("an absent key file was read")
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

// Q35 b with #38's per-issuer verifiers: each issuer's JWKS is stored
// on its own, and a start with one issuer down takes only that issuer's
// keys from the store. The lab's tokens verify with its own fetched
// keys, the authority's with its stored ones, and /readyz names the
// authority alone as verifying from the store. Once the authority
// answers, its own verifier replaces the stored keys.
func TestTheStoredJWKSOfOneIssuerDownAtStart(t *testing.T) {
	ctx := context.Background()
	store := &memJWKS{}
	first := newTwoIssuersWith(t, store)
	mustNoErr(t, first.v.BuildEcosystem(ctx))
	first.v.saves.Wait()
	if n := first.v.Counters().Get(CounterJWKSCacheSaved); n != 2 {
		t.Fatalf("stored %d issuers' JWKS, want 2", n)
	}

	f := newTwoIssuersWith(t, store)
	// The second process's issuers sign with the first's keys: the same
	// token services, restarted.
	f.authority, f.lab = first.authority, first.lab
	f.v.cfg.Ecosystem.Issuers[f.authority.URL] = coreauth.IssuerConfig{JWKSURL: f.authority.JWKS}
	f.v.cfg.Ecosystem.Issuers[f.lab.URL] = coreauth.IssuerConfig{JWKSURL: f.lab.JWKS}
	f.authority.Down()
	if err := f.v.BuildEcosystem(ctx); err == nil || !strings.Contains(err.Error(), f.authority.URL) {
		t.Fatalf("build with the authority down: %v", err)
	}
	if !f.v.IssuerReady(f.lab.URL) || f.v.IssuerReady(f.authority.URL) || f.v.Counters().Get(CounterJWKSCacheRestored) != 1 {
		t.Fatalf("lab built %v, authority built %v, restored %v", f.v.IssuerReady(f.lab.URL), f.v.IssuerReady(f.authority.URL), f.v.Counters().Snapshot())
	}
	if _, ok := f.v.cachedAt(f.lab.URL); ok {
		t.Fatal("the lab's stored keys were restored although its own were fetched")
	}
	now := time.Now()
	if c, err := f.v.Verify(ctx, f.authority.token(t, "authority-01", ownHost, []string{"ussp.records"}, now)); err != nil || c.Issuer != f.authority.URL {
		t.Fatalf("an authority token from the stored keys: %v %+v", err, c)
	}
	if c, err := f.v.Verify(ctx, f.lab.token(t, "lab-01", ownHost, nil, now)); err != nil || c.Issuer != f.lab.URL {
		t.Fatalf("a lab token: %v %+v", err, c)
	}
	st, detail := f.v.Probe(ctx)
	if st != obs.StateDegraded || !strings.Contains(detail, f.authority.URL+": not fetched since this process started; the stored keys") ||
		strings.Contains(detail, f.lab.URL) {
		t.Fatalf("probe: %s %q", st, detail)
	}

	f.authority.Up()
	mustNoErr(t, f.v.BuildEcosystem(ctx))
	if f.v.cached.Load() != nil || !f.v.EcosystemReady() {
		t.Fatal("the authority's fetched keys did not replace its stored ones")
	}
	if _, err := f.v.Verify(ctx, f.authority.token(t, "authority-01", ownHost, nil, now)); err != nil {
		t.Fatalf("an authority token after it answers: %v", err)
	}
}
