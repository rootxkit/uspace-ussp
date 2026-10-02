package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

func tokenErr(t *testing.T, err error) *coreauth.TokenError {
	t.Helper()
	var te *coreauth.TokenError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not a *TokenError", err)
	}
	return te
}

// The unverified iss only routes: a token of this issuer to the
// own-issuer verifier, any other to the ecosystem's; each refuses what
// it does not allow-list.
func TestVerifierRoutesByIssuer(t *testing.T) {
	c := newClock(t0())
	eco := newEcosystem(t)
	own := newOwnIssuer(t, false)
	v := newVerifier(t, own, eco, c)
	ctx := context.Background()

	// Before the ecosystem verifier is built, its tokens are
	// jwks_unavailable, not "unknown issuer".
	_, err := v.Verify(ctx, eco.token(t, "cisp-01", ownHost, []string{"cis.read"}, c.Now()))
	if te := tokenErr(t, err); te.Counter != CounterJWKSUnavailable || v.Counters().Get(CounterJWKSUnavailable) != 1 {
		t.Fatalf("before build: %+v", te)
	}
	if v.EcosystemReady() {
		t.Fatal("ready before build")
	}
	mustNoErr(t, v.BuildEcosystem(ctx))
	mustNoErr(t, v.BuildEcosystem(ctx)) // a second build is a no-op
	claims, err := v.Verify(ctx, eco.token(t, "cisp-01", ownHost, []string{"cis.read"}, c.Now()))
	if err != nil || claims.Issuer != eco.URL || claims.Subject != "cisp-01" {
		t.Fatalf("ecosystem token: %v %+v", err, claims)
	}
	op, err := own.IssueOperator("op-1", []string{ScopeGeo}, time.Hour, c.Now())
	mustNoErr(t, err)
	if claims, err := v.Verify(ctx, op.Token); err != nil || claims.Issuer != ownIssuer {
		t.Fatalf("own token: %v", err)
	}
	// A third issuer goes to the ecosystem verifier and is refused there.
	stranger, err := coreauth.NewIssuer("https://stranger.test", keyEco, "k")
	mustNoErr(t, err)
	tok, err := stranger.Issue("x", ownHost, nil, time.Minute, c.Now())
	mustNoErr(t, err)
	if te := tokenErr(t, func() error { _, err := v.Verify(ctx, tok); return err }()); te.Counter != coreauth.CounterRejectedIssuer {
		t.Fatalf("stranger: %+v", te)
	}
	sets := v.CounterSets()
	if sets[CounterSetEcosystem].Get(coreauth.CounterRejectedIssuer) != 1 || sets[CounterSetOwn].Get(coreauth.CounterAccepted) != 1 {
		t.Fatalf("counters: eco %v own %v", sets[CounterSetEcosystem].Snapshot(), sets[CounterSetOwn].Snapshot())
	}
}

// Without ecosystem issuers or without an own issuer, the other kind of
// token is an unknown issuer.
func TestVerifierWithoutOneSide(t *testing.T) {
	c := newClock(t0())
	eco := newEcosystem(t)
	onlyOwn := newVerifier(t, newOwnIssuer(t, false), nil, c)
	_, err := onlyOwn.Verify(context.Background(), eco.token(t, "x", ownHost, nil, c.Now()))
	if te := tokenErr(t, err); te.Counter != coreauth.CounterRejectedIssuer || onlyOwn.Counters().Get(coreauth.CounterRejectedIssuer) != 1 {
		t.Fatalf("got %+v", te)
	}
	mustNoErr(t, onlyOwn.BuildEcosystem(context.Background())) // nothing to build
	onlyEco := newVerifier(t, nil, eco, c)
	if onlyEco.OwnIssuer() != "" {
		t.Fatal("an own issuer without a key")
	}
	op, err := newOwnIssuer(t, false).IssueOperator("op-1", []string{ScopeGeo}, time.Hour, c.Now())
	mustNoErr(t, err)
	if _, err := onlyEco.Verify(context.Background(), op.Token); err == nil {
		t.Fatal("a token of an issuer without a key verified")
	}
}

func TestNewVerifierRefusesItsOwnIssuerOnTheEcosystemList(t *testing.T) {
	own := newOwnIssuer(t, false)
	cfg := coreauth.Config{Audiences: []string{ownHost}, Issuers: map[string]coreauth.IssuerConfig{ownIssuer: {JWKSURL: "https://x.test/jwks"}}}
	if _, err := NewVerifier(context.Background(), VerifierConfig{Ecosystem: cfg, Own: own}); err == nil {
		t.Fatal("own issuer accepted as an ecosystem issuer")
	}
	if _, err := NewVerifier(context.Background(), VerifierConfig{Ecosystem: coreauth.Config{}, Own: own}); err == nil {
		t.Fatal("an own verifier without an audience built")
	}
}

// E-02: the jwks dependency is up after a successful fetch; when the
// fake authority's JWKS goes away it is degraded with the cache's age,
// and tokens still verify from the cache; back up, it is up again.
func TestJWKSReadinessUpDegradedUp(t *testing.T) {
	c := newClock(t0())
	eco := newEcosystem(t)
	v := newVerifier(t, nil, eco, c)
	ctx := context.Background()

	// Never fetched (the authority is down at start): down, and Run keeps
	// trying until it comes back.
	eco.Down()
	if st, detail := v.Probe(ctx); st != obs.StateDown || !strings.Contains(detail, "never fetched") {
		t.Fatalf("before any fetch: %s %q", st, detail)
	}
	if err := v.BuildEcosystem(ctx); err == nil || v.Counters().Get(CounterEcosystemBuildFailed) != 1 {
		t.Fatal("a build against a down JWKS succeeded")
	}
	eco.Up()
	// The JWKS answers but the verifier is not built yet: still down.
	if st, _ := v.Probe(ctx); st != obs.StateDown {
		t.Fatalf("answering but unbuilt: %s", st)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { v.Run(runCtx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not build the verifier")
	}
	cancel()

	if st, detail := v.Probe(ctx); st != obs.StateUp || detail != "" {
		t.Fatalf("after a fetch: %s %q", st, detail)
	}
	tok := eco.token(t, "cisp-01", ownHost, []string{"cis.read"}, c.Now())
	eco.Down()
	c.Add(90 * time.Second)
	st, detail := v.Probe(ctx)
	if st != obs.StateDegraded || !strings.Contains(detail, "cached, age 90 s") {
		t.Fatalf("authority down: %s %q", st, detail)
	}
	if _, err := v.Verify(ctx, tok); err != nil {
		t.Fatalf("verification from the cache failed during the outage: %v", err)
	}
	eco.Up()
	if st, _ := v.Probe(ctx); st != obs.StateUp {
		t.Fatalf("authority back: %s", st)
	}
}

func TestProbeWithoutEcosystemIssuers(t *testing.T) {
	v := newVerifier(t, newOwnIssuer(t, false), nil, nil)
	if st, detail := v.Probe(context.Background()); st != obs.StateDown || !strings.Contains(detail, "USSP_TOKEN_ISSUERS is empty") {
		t.Fatalf("%s %q", st, detail)
	}
}

// Run returns when its context ends even while the JWKS stays down.
func TestRunStopsWithItsContext(t *testing.T) {
	eco := newEcosystem(t)
	eco.Down()
	v := newVerifier(t, nil, eco, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { v.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if v.EcosystemReady() {
		t.Fatal("built against a down JWKS")
	}
}

func TestFetchJWKSRefusesWhatIsNotAJWKS(t *testing.T) {
	v := newVerifier(t, nil, nil, nil)
	ctx := context.Background()
	if err := v.fetchJWKS(ctx, "http://127.0.0.1:1/jwks"); err == nil {
		t.Error("an unreachable URL fetched")
	}
	if err := v.fetchJWKS(ctx, "::bad"); err == nil {
		t.Error("a malformed URL fetched")
	}
	eco := newEcosystem(t)
	if err := v.fetchJWKS(ctx, eco.srv.URL+"/x"); err != nil {
		t.Errorf("the fake JWKS: %v", err)
	}
	eco.Down()
	if err := v.fetchJWKS(ctx, eco.srv.URL+"/x"); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("a 502: %v", err)
	}
}
