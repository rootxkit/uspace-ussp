package cis

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

// Started while the issuer's JWKS does not answer, the receiver refuses
// every notification (401-shaped, counted) and says so; once the keys
// answer it verifies (E-01 pair, B-08).
func TestLazyVerifier(t *testing.T) {
	s, err := signer.New("https://cisp.test", "k1")
	if err != nil {
		t.Fatal(err)
	}
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.JWKSHandler(w, r)
	}))
	defer srv.Close()
	l := NewLazyVerifier(coreauth.CompactConfig{
		Issuers:   map[string]coreauth.IssuerConfig{s.Issuer: {JWKSURL: srv.URL + "/jwks"}},
		Audiences: []string{ourHost},
	}, 10*time.Millisecond, nil)
	if st, _ := l.Probe(t.Context()); st != obs.StateUnknown {
		t.Fatalf("before any attempt: %s", st)
	}
	if err := l.Build(t.Context()); err == nil {
		t.Fatal("built without keys")
	}
	if st, d := l.Probe(t.Context()); st != obs.StateDown || d == "" {
		t.Fatalf("keys down: %s %q", st, d)
	}
	tok, err := s.SignFor(ourHost, "sub", "j1", changeBody("zones", "publication", 1, ""), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var te *coreauth.TokenError
	if _, _, err := l.Verify(t.Context(), tok); !errors.As(err, &te) || te.Counter != CounterNotifyKeysUnavailable {
		t.Fatalf("verify without keys: %v", err)
	}
	if l.Counters() != nil {
		t.Fatal("counters before the build")
	}
	up.Store(true)
	done := make(chan struct{})
	go func() { l.Run(t.Context()); close(done) }()
	<-done
	if st, _ := l.Probe(t.Context()); st != obs.StateUp {
		t.Fatalf("keys up: %s", st)
	}
	if c, _, err := l.Verify(t.Context(), tok); err != nil || c.JTI != "j1" {
		t.Fatalf("verify: %+v %v", c, err)
	}
	if l.Counters() == nil || l.Build(t.Context()) != nil {
		t.Fatal("built verifier")
	}
}
