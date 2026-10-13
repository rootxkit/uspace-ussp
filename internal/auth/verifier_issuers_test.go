package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// twoIssuers is a Verifier with two ecosystem issuers, the authority and
// the lab's token service, each its own fake JWKS, and the OnBuilt calls
// it saw.
type twoIssuers struct {
	v              *Verifier
	authority, lab *ecosystem
	mu             sync.Mutex
	built          []string
}

func newTwoIssuers(t *testing.T) *twoIssuers {
	t.Helper()
	f := &twoIssuers{authority: newEcosystem(t), lab: newEcosystem(t)}
	// The same key, another iss: the lab's token service.
	_, _, k := testKeys(t)
	var err error
	f.lab.URL = "https://lab.test"
	f.lab.issuer, err = coreauth.NewIssuer(f.lab.URL, k, signingKey(t, k).KID)
	mustNoErr(t, err)
	cfg := coreauth.Config{Audiences: []string{ownHost}, StrictSessionClaims: true, Issuers: map[string]coreauth.IssuerConfig{
		f.authority.URL: {JWKSURL: f.authority.JWKS},
		f.lab.URL:       {JWKSURL: f.lab.JWKS},
	}}
	f.v, err = NewVerifier(context.Background(), VerifierConfig{Ecosystem: cfg, RetryInterval: 10 * time.Millisecond,
		OnBuilt: func(set string, _ *core.Counters) { f.mu.Lock(); f.built = append(f.built, set); f.mu.Unlock() }})
	mustNoErr(t, err)
	return f
}

func (f *twoIssuers) builtSets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.built...)
}

// The case the conformance run found: the authority's JWKS does not
// answer when the process starts. The lab's tokens verify anyway; only
// the authority's are jwks_unavailable, and /readyz names it. When the
// authority comes back, its verifier is built too.
func TestOneIssuerDownAtStartHoldsBackOnlyItsOwnTokens(t *testing.T) {
	f := newTwoIssuers(t)
	ctx := context.Background()
	f.authority.Down()

	err := f.v.BuildEcosystem(ctx)
	if err == nil || !strings.Contains(err.Error(), f.authority.URL) || strings.Contains(err.Error(), f.lab.URL) {
		t.Fatalf("build with the authority down: %v", err)
	}
	if f.v.EcosystemReady() || !f.v.IssuerReady(f.lab.URL) || f.v.IssuerReady(f.authority.URL) {
		t.Fatalf("ready: all %v lab %v authority %v", f.v.EcosystemReady(), f.v.IssuerReady(f.lab.URL), f.v.IssuerReady(f.authority.URL))
	}
	now := time.Now()
	if c, err := f.v.Verify(ctx, f.lab.token(t, "lab-01", ownHost, []string{"police.query"}, now)); err != nil || c.Issuer != f.lab.URL {
		t.Fatalf("a lab token with the authority down: %v %+v", err, c)
	}
	_, err = f.v.Verify(ctx, f.authority.token(t, "authority-01", ownHost, []string{"ussp.records"}, now))
	if te := tokenErr(t, err); te.Counter != CounterJWKSUnavailable || f.v.Counters().Get(CounterJWKSUnavailable) != 1 {
		t.Fatalf("an authority token before its keys: %+v", te)
	}
	// The lab issuer sorts after the authority: its set is the second.
	if got := f.builtSets(); len(got) != 1 || got[0] != EcosystemSet(1) {
		t.Fatalf("published sets: %v", got)
	}
	if sets := f.v.CounterSets(); sets[EcosystemSet(1)] == nil || sets[EcosystemSet(0)] != nil {
		t.Fatalf("counter sets: %v", sets)
	}
	st, detail := f.v.Probe(ctx)
	if st != obs.StateDown || !strings.Contains(detail, f.authority.URL+": never fetched") || strings.Contains(detail, f.lab.URL) {
		t.Fatalf("probe with the authority down: %s %q", st, detail)
	}

	f.authority.Up()
	mustNoErr(t, f.v.BuildEcosystem(ctx))
	if !f.v.EcosystemReady() {
		t.Fatal("not ready after the authority came back")
	}
	if _, err := f.v.Verify(ctx, f.authority.token(t, "authority-01", ownHost, []string{"ussp.records"}, now)); err != nil {
		t.Fatalf("an authority token after its keys: %v", err)
	}
	if got := f.builtSets(); len(got) != 2 || got[1] != CounterSetEcosystem {
		t.Fatalf("published sets: %v", got)
	}
	if st, detail := f.v.Probe(ctx); st != obs.StateUp || detail != "" {
		t.Fatalf("probe with both up: %s %q", st, detail)
	}
}

// The twin with nothing wrong: both answer, one build verifies both, and
// a token of a third issuer is refused by core as rejected_issuer.
func TestTwoIssuersUpVerifyBoth(t *testing.T) {
	f := newTwoIssuers(t)
	ctx := context.Background()
	mustNoErr(t, f.v.BuildEcosystem(ctx))
	now := time.Now()
	for _, e := range []*ecosystem{f.authority, f.lab} {
		if c, err := f.v.Verify(ctx, e.token(t, "x", ownHost, nil, now)); err != nil || c.Issuer != e.URL {
			t.Fatalf("%s: %v %+v", e.URL, err, c)
		}
	}
	if got := f.builtSets(); len(got) != 2 {
		t.Fatalf("published sets: %v", got)
	}
	_, _, k := testKeys(t)
	stranger, err := coreauth.NewIssuer("https://stranger.test", k, "k")
	mustNoErr(t, err)
	tok, err := stranger.Issue("x", ownHost, nil, time.Minute, now)
	mustNoErr(t, err)
	_, err = f.v.Verify(ctx, tok)
	if te := tokenErr(t, err); te.Counter != coreauth.CounterRejectedIssuer || f.v.CounterSets()[CounterSetEcosystem].Get(coreauth.CounterRejectedIssuer) != 1 {
		t.Fatalf("a stranger: %+v", te)
	}
}

// A JWKS that does not answer at all (a host that resolves slowly, a
// proxy holding the request) is reported by name before the check's own
// bound, beside the other issuer's state: the check answers, it does not
// overrun into "did not answer". Its twin: the same verifier answers up
// when both answer.
func TestProbeAnswersWithinItsBoundWhenAJWKSHangs(t *testing.T) {
	f := newTwoIssuers(t)
	ctx := context.Background()
	mustNoErr(t, f.v.BuildEcosystem(ctx))
	if st, detail := f.v.Probe(ctx); st != obs.StateUp || detail != "" {
		t.Fatalf("both answering: %s %q", st, detail)
	}

	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); hang.Close() })
	f.v.cfg.Ecosystem.Issuers[f.authority.URL] = coreauth.IssuerConfig{JWKSURL: hang.URL}

	bound := 400 * time.Millisecond
	pctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	st, detail := f.v.Probe(pctx)
	if pctx.Err() != nil {
		t.Fatalf("the probe returned only after its bound (%s): %s %q", bound, st, detail)
	}
	if st != obs.StateDegraded || !strings.Contains(detail, f.authority.URL+": cached, age") || !strings.Contains(detail, "unreachable") ||
		strings.Contains(detail, f.lab.URL) {
		t.Fatalf("a hanging JWKS: %s %q", st, detail)
	}
}
