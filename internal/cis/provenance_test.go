package cis

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

func (g *cacheRig) active(d Dataset) int64 {
	if v := g.eval.Snapshot().Version(d); v != nil {
		return v.Number
	}
	return 0
}

func (g *cacheRig) lastSaved(d Dataset) *Version {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	s := g.store.saved[d]
	if len(s) == 0 {
		return nil
	}
	return s[len(s)-1]
}

// pullHeld pulls d and expects version v held for a reason containing
// why: not used, stored untrusted, counted, and on /readyz.
func (g *cacheRig) pullHeld(t *testing.T, d Dataset, v int64, why string) {
	t.Helper()
	before := g.count(CounterUntrusted)
	err := g.cache.Pull(t.Context(), d, nil, false)
	var ue *UntrustedError
	if !errors.As(err, &ue) || ue.Version != v || !strings.Contains(ue.Reason, why) {
		t.Fatalf("pull: %v, want version %d held for %q", err, v, why)
	}
	if s := g.lastSaved(d); s == nil || s.Number != v || s.SignatureOK {
		t.Fatalf("stored: %+v", s)
	}
	if g.count(CounterUntrusted) != before+1 {
		t.Fatal("cis_publisher_untrusted did not move")
	}
	st, detail := g.cache.Probe(t.Context())
	if st == obs.StateUp || !strings.Contains(detail, ue.Error()) {
		t.Fatalf("probe: %s %q", st, detail)
	}
}

// The branch that says the provenance is fine: every dataset's version
// carries its publisher's signature (the authority's on zones,
// uspace_airspace and ussp_list, the ANSP's on restrictions), is
// promoted, stored trusted, and /readyz is up.
func TestProvenanceValidSignaturePromotes(t *testing.T) {
	g := newCacheRig(t, "https://ussp.test/v1/cis/notifications")
	ctx := t.Context()
	for _, d := range ED318Datasets {
		g.fake.Publish(string(d), featureOf(d, "TZP001").json())
	}
	g.cache.subscribeLoop(ctx)
	for _, d := range ED318Datasets {
		if err := g.cache.Pull(ctx, d, nil, false); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		v := g.eval.Snapshot().Version(d)
		if v == nil || v.Number != 1 || !v.SignatureOK {
			t.Fatalf("%s: %+v", d, v)
		}
		if s := g.lastSaved(d); s == nil || !s.SignatureOK {
			t.Fatalf("%s stored: %+v", d, s)
		}
		if n := g.fake.Requests("GET /v1/" + string(d) + "/versions/1"); n != 1 {
			t.Fatalf("%s: %d reads of the version as published", d, n)
		}
	}
	if st, detail := g.cache.Probe(ctx); st != obs.StateUp || detail != "zones:1,uspace_airspace:1,restrictions:1, age 0 s" {
		t.Fatalf("probe: %s %q", st, detail)
	}
	if g.count(CounterUntrusted) != 0 {
		t.Fatal("counted as untrusted")
	}
}

// A version without X-Publisher-Signature is held: stored untrusted,
// never used, the previous one kept, /readyz degraded naming it. The
// next version with a valid signature is promoted and clears it.
func TestProvenanceMissingSignatureHeld(t *testing.T) {
	g := newCacheRig(t, "")
	g.fake.Publish("zones", prohibited("TZP001").json())
	if err := g.cache.Pull(t.Context(), Zones, nil, false); err != nil {
		t.Fatal(err)
	}
	g.fake.Publish("zones", prohibited("TZP002").json())
	g.fake.SetPublisherSignature("zones", 2, "", "")
	g.pullHeld(t, Zones, 2, "no X-Publisher-Signature")
	if g.active(Zones) != 1 {
		t.Fatalf("active %d, want 1 kept", g.active(Zones))
	}
	// The same held version found again by the reconciliation is
	// checked again and stays held.
	g.pullHeld(t, Zones, 2, "no X-Publisher-Signature")

	g.fake.Publish("zones", prohibited("TZP003").json())
	if err := g.cache.Pull(t.Context(), Zones, nil, true); err != nil {
		t.Fatal(err)
	}
	if g.active(Zones) != 3 {
		t.Fatalf("v3 not promoted: %d", g.active(Zones))
	}
	if _, detail := g.cache.Probe(t.Context()); strings.Contains(detail, "held") {
		t.Fatalf("still held after v3: %q", detail)
	}
}

// With nothing held before, a first version without a signature leaves
// the dataset unloaded: nothing unverified is ever used.
func TestProvenanceFirstVersionHeld(t *testing.T) {
	g := newCacheRig(t, "")
	g.fake.Publish("restrictions", prohibited("DAR0001").json())
	g.fake.SetPublisherSignature("restrictions", 1, "", "")
	g.pullHeld(t, Restrictions, 1, "no X-Publisher-Signature")
	if g.active(Restrictions) != 0 {
		t.Fatal("an unsigned first version is used")
	}
	if _, _, stale := g.eval.Age(); !stale {
		t.Fatal("not stale with nothing loaded")
	}
}

// A signature that does not verify is held like a missing one: made by
// another key under the publisher's kid, by the other publisher, over
// other bytes, or naming another kid in X-Publisher-Kid.
func TestProvenanceBadSignatureHeld(t *testing.T) {
	g := newCacheRig(t, "")
	g.fake.Publish("zones", prohibited("TZP001").json())
	if err := g.cache.Pull(t.Context(), Zones, nil, false); err != nil {
		t.Fatal(err)
	}
	impostor, err := signer.New(cisp.PublisherAuthority, "fake-authority-1")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, why string
		sign      func(body []byte) (sig, kid string)
	}{
		{"another key", "does not verify with the authority's keys", func(body []byte) (string, string) {
			s, _ := impostor.SignDetached(body, time.Now())
			return s, "fake-authority-1"
		}},
		{"the ANSP's key on zones", "does not verify with the authority's keys", func(body []byte) (string, string) {
			s, _ := g.fake.Publishers[cisp.PublisherANSP].SignDetached(body, time.Now())
			return s, "fake-ansp-1"
		}},
		{"other bytes", "does not verify with the authority's keys", func(body []byte) (string, string) {
			s, _ := g.fake.Publishers[cisp.PublisherAuthority].SignDetached(append([]byte(" "), body...), time.Now())
			return s, "fake-authority-1"
		}},
		{"a kid header that is not the signature's", `X-Publisher-Kid names "fake-authority-2"`, func(body []byte) (string, string) {
			s, _ := g.fake.Publishers[cisp.PublisherAuthority].SignDetached(body, time.Now())
			return s, "fake-authority-2"
		}},
		{"not a JWS", "does not verify", func([]byte) (string, string) { return "not-a-jws", "" }},
	}
	for i, tc := range cases {
		v := int64(i + 2)
		g.fake.Publish("zones", prohibited("TZP001").json(), prohibited("TZP00"+string(rune('2'+i))).json())
		sig, kid := tc.sign(g.fake.VersionBody("zones", v))
		g.fake.SetPublisherSignature("zones", v, sig, kid)
		g.pullHeld(t, Zones, v, tc.why)
		if g.active(Zones) != 1 {
			t.Fatalf("%s: active %d", tc.name, g.active(Zones))
		}
	}
}

// The signature's iat is as old as the version; the bound is
// USSP_CIS_PUBLISHER_SIG_MAX_AGE_S, not core's 5 min default: a version
// published 30 min ago is promoted under a 1 h bound, one published 2 h
// ago is held.
func TestProvenanceMaxAge(t *testing.T) {
	g := newCacheRig(t, "")
	v, err := coreauth.NewDetachedVerifier(t.Context(), coreauth.DetachedConfig{Publishers: g.fake.PublisherKeys(), MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	g.cache.cfg.Publishers = v
	authority := g.fake.Publishers[cisp.PublisherAuthority]
	g.fake.Publish("zones", prohibited("TZP001").json())
	sig, _ := authority.SignDetached(g.fake.VersionBody("zones", 1), time.Now().Add(-30*time.Minute))
	g.fake.SetPublisherSignature("zones", 1, sig, "fake-authority-1")
	if err := g.cache.Pull(t.Context(), Zones, nil, false); err != nil || g.active(Zones) != 1 {
		t.Fatalf("30 min old: %v, active %d", err, g.active(Zones))
	}
	g.fake.Publish("zones", prohibited("TZP002").json())
	sig, _ = authority.SignDetached(g.fake.VersionBody("zones", 2), time.Now().Add(-2*time.Hour))
	g.fake.SetPublisherSignature("zones", 2, sig, "fake-authority-1")
	g.pullHeld(t, Zones, 2, "iat")
}

// Without publisher keys every new version is held and /readyz says
// which variable is missing; once keys are there, the same version
// found again is promoted.
func TestProvenanceWithoutKeys(t *testing.T) {
	g := newCacheRig(t, "")
	keys := g.cache.cfg.Publishers
	g.cache.cfg.Publishers = nil
	g.fake.Publish("zones", prohibited("TZP001").json())
	g.pullHeld(t, Zones, 1, "USSP_CIS_PUBLISHER_KEYS")
	if _, detail := g.cache.Probe(t.Context()); !strings.Contains(detail, "every new version is held") {
		t.Fatalf("probe: %q", detail)
	}
	g.cache.cfg.Publishers = keys
	if err := g.cache.Pull(t.Context(), Zones, nil, true); err != nil || g.active(Zones) != 1 {
		t.Fatalf("with keys: %v, active %d", err, g.active(Zones))
	}
}

// failVersions answers 503 to every version read and passes the rest.
type failVersions struct{ next http.RoundTripper }

func (f failVersions) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/versions/") {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	return f.next.RoundTrip(r)
}

// A version that cannot be read as published is a pull failure, not a
// hold: nothing is stored or held, and the next pull that can read it
// promotes it.
func TestProvenanceUnreadableVersionIsAPullFailure(t *testing.T) {
	g := newCacheRig(t, "")
	hc := g.fake.Client()
	hc.Transport = failVersions{next: hc.Transport}
	client, err := NewClient(ClientConfig{BaseURL: g.fake.URL(), Tokens: cisp.Tokens{}, HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	good := g.cache.cfg.Client
	g.cache.cfg.Client = client
	g.fake.Publish("zones", prohibited("TZP001").json())
	err = g.cache.Pull(t.Context(), Zones, nil, false)
	var ue *UntrustedError
	if err == nil || errors.As(err, &ue) || !strings.Contains(err.Error(), "as published") {
		t.Fatalf("pull: %v", err)
	}
	if g.active(Zones) != 0 || g.lastSaved(Zones) != nil || g.count(CounterPullFailed) != 1 || g.count(CounterUntrusted) != 0 {
		t.Fatalf("active %d, saved %+v, %v", g.active(Zones), g.lastSaved(Zones), g.cache.Counters().Snapshot())
	}
	g.cache.cfg.Client = good
	if err := g.cache.Pull(t.Context(), Zones, nil, false); err != nil || g.active(Zones) != 1 {
		t.Fatalf("readable: %v, active %d", err, g.active(Zones))
	}
}

// The lazy verifier the api process runs: before the publishers' JWKS
// is fetched it refuses (the version is held) and /readyz says down
// with the reason; once fetched it verifies and is up.
func TestLazyPublisherVerifier(t *testing.T) {
	fake, err := cisp.NewTLS()
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	body := []byte(`{"type":"FeatureCollection","features":[]}`)
	sig, err := fake.Publishers[cisp.PublisherAuthority].SignDetached(body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	urls := map[string]coreauth.IssuerConfig{}
	for _, e := range strings.Split(fake.PublisherKeysEnv(), ",") {
		p, u, _ := strings.Cut(e, "=")
		urls[p] = coreauth.IssuerConfig{JWKSURL: u}
	}
	down := NewLazyPublisherVerifier(coreauth.DetachedConfig{Publishers: map[string]coreauth.IssuerConfig{
		PublisherAuthority: {JWKSURL: "https://127.0.0.1:1/jwks.json"}}, JWKSFetchTimeout: time.Second}, time.Millisecond)
	if st, _ := down.Probe(t.Context()); st != obs.StateUnknown {
		t.Fatalf("before a try: %s", st)
	}
	if err := down.Build(t.Context()); err == nil {
		t.Fatal("built without keys")
	}
	if st, detail := down.Probe(t.Context()); st != obs.StateDown || !strings.Contains(detail, "not fetched") {
		t.Fatalf("down: %s %q", st, detail)
	}
	if _, err := down.Verify(t.Context(), PublisherAuthority, sig, body); err == nil || down.Counters() != nil {
		t.Fatal("verified without keys")
	}

	up := NewLazyPublisherVerifier(coreauth.DetachedConfig{Publishers: urls, HTTPClient: fake.Client(), MaxAge: time.Hour}, time.Millisecond)
	up.Run(t.Context())
	if st, _ := up.Probe(t.Context()); st != obs.StateUp {
		t.Fatalf("up: %s", st)
	}
	if s, err := up.Verify(t.Context(), PublisherAuthority, sig, body); err != nil || s.KID != "fake-authority-1" || up.Counters() == nil {
		t.Fatalf("verify: %+v %v", s, err)
	}
	if _, err := up.Verify(t.Context(), PublisherANSP, sig, body); err == nil {
		t.Fatal("the authority's signature verified as the ANSP's")
	}
	if fake.Requests("GET /publishers/authority/jwks.json") == 0 {
		t.Fatal("the JWKS was not fetched")
	}
}
