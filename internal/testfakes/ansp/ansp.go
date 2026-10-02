// Package ansp is a fake ANSP for tests. WP-4 needs only its degraded
// direct delivery of a restriction (spec 02 F2, M5): a cis/change/v1
// record as a compact JWS signed with the ANSP's own key, POSTed to a
// subscriber's /v1/cis/notifications. Later work packages add the F4
// stream and the coordination inbox here. Down makes its server answer
// 503.
package ansp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

// Fake is a running fake ANSP.
type Fake struct {
	Signer     *signer.Signer
	HTTPClient *http.Client
	srv        *httptest.Server

	mu   sync.Mutex
	down bool
}

// New starts a fake ANSP whose issuer is its own URL.
func New() (*Fake, error) {
	f := &Fake{HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		f.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/.well-known/jwks.json" {
			f.Signer.JWKSHandler(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	s, err := signer.New(f.srv.URL, "fake-ansp-1")
	if err != nil {
		f.srv.Close()
		return nil, err
	}
	f.Signer = s
	return f, nil
}

// Close stops the server.
func (f *Fake) Close() { f.srv.Close() }

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.srv.URL }

// Host is its host (no port).
func (f *Fake) Host() string { u, _ := url.Parse(f.srv.URL); return u.Hostname() }

// IssuerConfig is the static key set of its signing key.
func (f *Fake) IssuerConfig() coreauth.IssuerConfig { return f.Signer.IssuerConfig() }

// Down makes its server answer 503 until Up.
func (f *Fake) Down() { f.mu.Lock(); f.down = true; f.mu.Unlock() }

// Up ends Down.
func (f *Fake) Up() { f.mu.Lock(); f.down = false; f.mu.Unlock() }

// Notify POSTs change, signed by the ANSP (sub = the restriction id),
// to callback and returns the status.
func (f *Fake) Notify(ctx context.Context, callback, restrictionID string, change any) (int, error) {
	body, err := json.Marshal(change)
	if err != nil {
		return 0, err
	}
	tok, err := f.Signer.Sign(callback, restrictionID, signer.NewID(), body, time.Now())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, strings.NewReader(tok))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/jose")
	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
