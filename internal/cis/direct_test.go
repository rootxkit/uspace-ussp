package cis

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
)

// anspDirectDir is uspace-ansp's testdata/contract/direct, vendored at
// the commit its SOURCE names: what the ANSP's code signs and serves on
// the degraded direct path (the cross-repo contract of H-2).
const anspDirectDir = "../../testdata/contracts/ansp-direct"

// The fixture's names.
const (
	fixtureIssuer      = "https://ansp.test"
	fixtureAudience    = "receiver.test"
	fixtureRestriction = "01K6P0A1B2C3D4E5F6G7H8J9KM"
	fixtureIdentifier  = "DAR7K2Q"
)

func fixtureFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(anspDirectDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

// fixtureCompact joins the three parts of a fixture's compact JWS.
func fixtureCompact(t *testing.T, name string) string {
	t.Helper()
	var p struct{ Protected, Payload, Signature string }
	if err := json.Unmarshal(fixtureFile(t, name), &p); err != nil || p.Protected == "" || p.Payload == "" || p.Signature == "" {
		t.Fatalf("%s: not the three parts of a compact JWS: %v", name, err)
	}
	return p.Protected + "." + p.Payload + "." + p.Signature
}

// The vendored files are the ones SOURCE pins, byte for byte.
func TestANSPDirectFixtureIsPinned(t *testing.T) {
	f, err := os.Open(filepath.Join(anspDirectDir, "SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	pinned, origin := 0, false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if fields[0] == "ansp" {
			origin = len(fields) == 4 && fields[1] == "rootxkit/uspace-ansp" && len(fields[2]) == 40
			continue
		}
		raw, err := os.ReadFile(filepath.Join(anspDirectDir, fields[1]))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != fields[0] {
			t.Errorf("%s does not hash to the SHA-256 SOURCE pins", fields[1])
		}
		pinned++
	}
	if !origin || pinned != 7 {
		t.Fatalf("SOURCE names no commit of uspace-ansp, or pins %d files", pinned)
	}
}

// fakeANSP serves the fixture's pull_url (GET /v1/restrictions/{id}/
// direct on ansp.test) through the cache's client: the body and the
// signature of the case it is set to, and records every request.
type fakeANSP struct {
	mu       sync.Mutex
	body     []byte
	sig      string
	status   int
	requests []*http.Request
}

func (f *fakeANSP) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	rec := httptest.NewRecorder()
	switch {
	case r.URL.Host != "ansp.test" || r.URL.Path != "/v1/restrictions/"+fixtureRestriction+"/direct":
		rec.WriteHeader(http.StatusNotFound)
	case f.status != 0:
		rec.WriteHeader(f.status)
	default:
		rec.Header().Set("Content-Type", "application/json")
		rec.Header().Set(HeaderDirectSignature, f.sig)
		_, _ = rec.Write(f.body)
	}
	return rec.Result(), nil
}

func (f *fakeANSP) serve(t *testing.T, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body, f.sig, f.status = fixtureFile(t, name+".direct.json"), string(fixtureFile(t, name+".direct.jws")), 0
}

func (f *fakeANSP) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// memDirect is an in-memory DirectStore.
type memDirect struct {
	mu   sync.Mutex
	rows map[string]DirectStored
	down bool
}

func (m *memDirect) SaveDirect(_ context.Context, d DirectStored) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, errDown
	}
	if r, ok := m.rows[d.Identifier]; ok && r.AnspVersion >= d.AnspVersion {
		return false, nil
	}
	m.rows[d.Identifier] = d
	return true, nil
}

func (m *memDirect) LoadDirect(context.Context, int) ([]DirectStored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DirectStored, 0, len(m.rows))
	for id := range m.rows {
		out = append(out, m.rows[id])
	}
	return out, nil
}

func (m *memDirect) DeleteDirect(_ context.Context, id string, v int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok && r.AnspVersion <= v {
		delete(m.rows, id)
	}
	return nil
}

type directRig struct {
	t      *testing.T
	clk    *clock
	ansp   *fakeANSP
	store  *memStore
	direct *memDirect
	proj   *MemoryProjector
	eval   *Evaluator
	cache  *Cache
	rc     *Receiver
	cispMu sync.Mutex
	cisp   []Hint
	pubs   PublisherVerifier
	keys   coreauth.IssuerConfig
}

// newDirectRig is a receiver and a cache as the api process wires them,
// with the fixture's keys, on a clock at the fixture's signing time, and
// a CIS that is fresh: zones, uspace_airspace and a restrictions version
// 7 the CISP holds without the fixture's restriction.
func newDirectRig(t *testing.T) *directRig {
	t.Helper()
	jwks := fixtureFile(t, "jwks.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	}))
	t.Cleanup(srv.Close)
	g := &directRig{t: t, clk: &clock{t: time.Date(2026, 10, 2, 12, 0, 10, 0, time.UTC)}, ansp: &fakeANSP{},
		store: newMemStore(), direct: &memDirect{rows: map[string]DirectStored{}}, proj: &MemoryProjector{},
		keys: coreauth.IssuerConfig{JWKSURL: srv.URL + "/.well-known/jwks.json"}}
	pubs, err := coreauth.NewDetachedVerifier(t.Context(), coreauth.DetachedConfig{
		Publishers: map[string]coreauth.IssuerConfig{PublisherANSP: g.keys}, MaxAge: 366 * 24 * time.Hour, Now: g.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	g.pubs = pubs
	g.start()
	v, err := coreauth.NewCompactVerifier(t.Context(), coreauth.CompactConfig{
		Issuers: map[string]coreauth.IssuerConfig{fixtureIssuer: g.keys}, Audiences: []string{fixtureAudience}, Now: g.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	g.rc = NewReceiver(ReceiverConfig{
		Verifier: v, Senders: map[string]Sender{fixtureIssuer: {ANSP: true, BaseHost: "ansp.test"}}, Store: g.store,
		Trigger: func(_ Dataset, h Hint) {
			g.cispMu.Lock()
			defer g.cispMu.Unlock()
			g.cisp = append(g.cisp, h)
		},
		TriggerDirect: func(h DirectHint) bool { return g.cache.TriggerDirect(h) }, Counters: g.cache.Counters(), Now: g.clk.Now,
	})
	return g
}

// start builds the cache (again, for a restart) on the rig's stores and
// installs the CISP's versions.
func (g *directRig) start() {
	g.eval = NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: g.clk.Now})
	g.cache = NewCache(CacheConfig{Publishers: g.pubs, Store: g.store, Evaluator: g.eval, Projector: g.proj, Now: g.clk.Now,
		Direct: DirectConfig{Client: &http.Client{Transport: g.ansp}, Store: g.direct}})
	g.cache.warmDirect(context.Background())
	g.installCISP(7)
}

// installCISP installs zones 1, uspace_airspace 1 and restrictions v
// holding the restrictions fs, as the CISP confirmed them now.
func (g *directRig) installCISP(v int64, fs ...json.RawMessage) {
	g.t.Helper()
	for _, ver := range []*Version{mustVersion(g.t, Zones, 1), mustVersion(g.t, USpaceAirspace, 1, airspace("GEOTU01").json()),
		mustVersion(g.t, Restrictions, v, fs...)} {
		es, rf := buildEntries(ver)
		if rf != nil {
			g.t.Fatal(rf)
		}
		g.cache.installVersion(ver, es, g.clk.Now())
	}
}

func (g *directRig) post(name string) int {
	g.t.Helper()
	r := httptest.NewRequest(http.MethodPost, NotificationsPath, strings.NewReader(fixtureCompact(g.t, name+".change.json")))
	r.Header.Set("Content-Type", ContentTypeJOSE)
	w := httptest.NewRecorder()
	g.rc.ServeHTTP(w, r)
	return w.Code
}

func (g *directRig) state() (Lift, string) {
	var state string
	for _, e := range g.eval.Snapshot().Entries(Restrictions) {
		if e.Identifier == fixtureIdentifier && e.Restriction != nil {
			state = string(e.Restriction.State)
		}
	}
	return g.eval.RestrictionLift(fixtureIdentifier), state
}

// projected is the restriction state the projection lists for the
// fixture's restriction ("" when it lists none).
func (g *directRig) projected() string {
	p, _ := g.proj.Last()
	if p == nil {
		return ""
	}
	for k := range p.Cells {
		zs := p.Cells[k].Zones
		for i := range zs {
			if zs[i].Identifier == fixtureIdentifier {
				return zs[i].RestrictionState
			}
		}
	}
	return ""
}

func (g *directRig) cispHints() []Hint {
	g.cispMu.Lock()
	defer g.cispMu.Unlock()
	return append([]Hint(nil), g.cisp...)
}

// H-2, the cross-repo contract: the ANSP's degraded direct delivery,
// exactly as uspace-ansp signs and serves it, lands here and is applied,
// not skipped. Its version (ansp_version 2) is far below the CISP's
// restrictions version held (7), which the receiver used to read it as,
// and skip as a replay; its pull_url is the ANSP's, which the receiver
// used never to follow. The activation puts the restriction in force,
// on the Evaluator and in the projection the monitor judges from; the
// end, delivered the same way, lifts it.
func TestDirectDeliveryOfTheANSPIsApplied(t *testing.T) {
	g := newDirectRig(t)
	if l, _ := g.state(); l != LiftAbsent {
		t.Fatalf("before: %v", l)
	}
	g.ansp.serve(t, "activated")
	if code := g.post("activated"); code != http.StatusNoContent {
		t.Fatalf("activated: %d", code)
	}
	if len(g.cispHints()) != 0 {
		t.Fatalf("a direct notification asked the CISP: %+v", g.cispHints())
	}
	g.cache.drainDirect(t.Context())
	if l, st := g.state(); l != LiftInForce || st != "active" || g.projected() != "active" {
		t.Fatalf("the activation is not applied: %v %q, projected %q; counters %v", l, st, g.projected(), g.cache.Counters().Snapshot())
	}
	if g.ansp.count() != 1 || g.ansp.requests[0].Header.Get("Authorization") != "" {
		t.Fatalf("pulls %d (no credential is sent to the ANSP)", g.ansp.count())
	}
	if v := g.eval.Snapshot().Version(Restrictions); v == nil || v.Number != 7 {
		t.Fatalf("the CISP's version moved: %+v", v)
	}
	if _, why := g.cache.Probe(t.Context()); !strings.Contains(why, "from the ANSP's direct delivery") {
		t.Fatalf("readiness does not say the restriction is the ANSP's alone: %s", why)
	}
	// The end, an hour later, the same way.
	g.clk.advance(time.Hour - 9*time.Second)
	// The CISP still answers this USSP's reads (its versions are
	// confirmed, the CIS is fresh) while it does not hold the ANSP's
	// version: a stale CIS lifts nothing (WP-12), from either path.
	for _, d := range ED318Datasets {
		g.eval.confirm(d, g.clk.Now(), false)
	}
	g.ansp.serve(t, "ended")
	if code := g.post("ended"); code != http.StatusNoContent {
		t.Fatalf("ended: %d", code)
	}
	g.cache.drainDirect(t.Context())
	if l, st := g.state(); l != LiftEnded || st != "ended" || g.projected() != "ended" {
		t.Fatalf("the end is not applied: %v %q, projected %q", l, st, g.projected())
	}
	if g.cache.Counters().Get(CounterDirectApplied) != 2 {
		t.Fatalf("counters %v", g.cache.Counters().Snapshot())
	}
	// An older version again (the activation's hint, a new delivery):
	// nothing pulled, the end stands.
	pulls := g.ansp.count()
	if err := g.cache.ApplyDirect(t.Context(), DirectHint{RestrictionID: fixtureRestriction, AnspVersion: 2,
		FeatureIDs: []string{fixtureIdentifier}, PullURL: "https://ansp.test/v1/restrictions/" + fixtureRestriction + "/direct"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := g.state(); l != LiftEnded || g.ansp.count() != pulls || g.cache.Counters().Get(CounterDirectReplays) != 1 {
		t.Fatalf("an older version was applied or pulled: %v, %d pulls", l, g.ansp.count()-pulls)
	}
	// A restart keeps it: the stored body is installed again.
	g.start()
	if l, st := g.state(); l != LiftEnded || st != "ended" {
		t.Fatalf("after a restart: %v %q", l, st)
	}
	// The CISP catches up: a version holding ansp_version 3 replaces
	// the direct one, which is dropped from memory and the store.
	cisp := prohibited(fixtureIdentifier)
	cisp.extended = map[string]any{RestrictionMember: map[string]any{"id": fixtureRestriction, "ansp_ref": "ansp-01:" + fixtureRestriction,
		"ansp_version": 3, "state": "active", "starts_at": "2026-10-02T12:00:00Z", "ends_at": "2026-10-02T16:00:00Z",
		"ended_by": nil, "uspace_airspace_id": "GEOTU01"}}
	g.installCISP(9, cisp.json())
	if g.cache.dropDirect(t.Context()) {
		g.cache.reinstall(t.Context())
	}
	if l, st := g.state(); l != LiftInForce || st != "active" || len(g.direct.rows) != 0 {
		t.Fatalf("the CISP's version 3 did not replace the direct one: %v %q, stored %d", l, st, len(g.direct.rows))
	}
}

// The absence pairs of the test above. A body whose signature is not
// the ANSP's, a body for another restriction, and a pull_url on another
// host are never applied; a full queue is a 503 that records no
// delivery id, so the ANSP's retry is taken.
func TestDirectDeliveryRefusals(t *testing.T) {
	g := newDirectRig(t)
	// The ended body under the activation's signature.
	g.ansp.serve(t, "activated")
	g.ansp.body = fixtureFile(t, "ended.direct.json")
	if code := g.post("activated"); code != http.StatusNoContent {
		t.Fatalf("activated: %d", code)
	}
	g.cache.drainDirect(t.Context())
	if l, _ := g.state(); l != LiftAbsent || g.cache.Counters().Get(CounterDirectRefused) != 1 {
		t.Fatalf("a body its signature does not cover was applied: %v", l)
	}
	if len(g.cache.directPending) != 1 {
		t.Fatal("a refused signature is not retried (the keys may be fetched later)")
	}
	// The right body: the retry applies it.
	g.ansp.serve(t, "activated")
	g.cache.drainDirect(t.Context())
	if l, _ := g.state(); l != LiftInForce || len(g.cache.directPending) != 0 {
		t.Fatalf("the retry did not apply: %v", l)
	}

	// Another restriction id under a valid signature: permanent, dropped.
	h := newDirectRig(t)
	h.ansp.serve(t, "activated")
	if err := h.cache.ApplyDirect(t.Context(), DirectHint{RestrictionID: "01K6P0A1B2C3D4E5F6G7H8J9ZZ", AnspVersion: 2,
		FeatureIDs: []string{fixtureIdentifier}, PullURL: "https://ansp.test/v1/restrictions/" + fixtureRestriction + "/direct"}); err == nil {
		t.Fatal("a body of another restriction was applied")
	}
	if l, _ := h.state(); l != LiftAbsent {
		t.Fatalf("applied: %v", l)
	}

	// Full: 503, nothing recorded; the retry once there is room is taken.
	f := newDirectRig(t)
	f.cache.cfg.Direct.Max = 1
	f.cache.directPending["other"] = &DirectHint{RestrictionID: "other", AnspVersion: 1}
	f.ansp.serve(t, "activated")
	if code := f.post("activated"); code != http.StatusServiceUnavailable || len(f.store.jtis) != 0 {
		t.Fatalf("full: %d, %d ids recorded", code, len(f.store.jtis))
	}
	delete(f.cache.directPending, "other")
	if code := f.post("activated"); code != http.StatusNoContent {
		t.Fatalf("the retry: %d", code)
	}
	f.cache.drainDirect(t.Context())
	if l, _ := f.state(); l != LiftInForce {
		t.Fatalf("the retry was not applied: %v", l)
	}
}

// The ANSP's pull failing (a 503) is retried; the restriction is applied
// once it answers, and a 404 is given up at once.
func TestDirectPullRetried(t *testing.T) {
	g := newDirectRig(t)
	g.ansp.serve(t, "activated")
	g.ansp.status = http.StatusServiceUnavailable
	if code := g.post("activated"); code != http.StatusNoContent {
		t.Fatal(code)
	}
	g.cache.drainDirect(t.Context())
	if l, _ := g.state(); l != LiftAbsent || len(g.cache.directPending) != 1 {
		t.Fatalf("%v pending %d", l, len(g.cache.directPending))
	}
	if out := strings.Join(g.cache.Outdated(), "; "); !strings.Contains(out, "not applied yet") {
		t.Fatalf("Outdated does not name the pending restriction: %q", out)
	}
	g.ansp.status = 0
	g.cache.drainDirect(t.Context())
	if l, _ := g.state(); l != LiftInForce || len(g.cache.directPending) != 0 {
		t.Fatalf("not applied after the ANSP answered: %v", l)
	}
	h := newDirectRig(t)
	h.ansp.status = http.StatusNotFound
	if code := h.post("activated"); code != http.StatusNoContent {
		t.Fatal(code)
	}
	h.cache.drainDirect(t.Context())
	if len(h.cache.directPending) != 0 {
		t.Fatal("a 404 is retried")
	}
}
