package cis

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

type cacheRig struct {
	fake  *cisp.Fake
	clk   *clock
	store *memStore
	proj  *MemoryProjector
	eval  *Evaluator
	cache *Cache
}

func newCacheRig(t *testing.T, callback string) *cacheRig {
	t.Helper()
	fake, err := cisp.NewTLS()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	g := &cacheRig{fake: fake, clk: newClock(), store: newMemStore(), proj: &MemoryProjector{}}
	g.eval = NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: g.clk.Now})
	client, err := NewClient(ClientConfig{BaseURL: fake.URL(), Tokens: cisp.Tokens{}, HTTPClient: fake.Client()})
	if err != nil {
		t.Fatal(err)
	}
	pubs, err := coreauth.NewDetachedVerifier(t.Context(), coreauth.DetachedConfig{Publishers: fake.PublisherKeys(), MaxAge: 366 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	g.cache = NewCache(CacheConfig{Client: client, Publishers: pubs, Store: g.store, Evaluator: g.eval, Projector: g.proj,
		CallbackURL: callback, Now: g.clk.Now, SubscribeRetry: 10 * time.Millisecond})
	return g
}

func (g *cacheRig) count(name string) uint64 { return g.cache.Counters().Get(name) }

func hintOf(ch cisp.Change) *Hint {
	return &Hint{Version: ch.Version, ETag: ch.ETag, PullURL: ch.PullURL, Issuer: "https://cisp.test", At: time.Now()}
}

func TestCachePullDeltaAndReplay(t *testing.T) {
	g := newCacheRig(t, "")
	ctx := t.Context()
	g.fake.Publish("zones", prohibited("TZP001").json(), prohibited("TZP002").json())
	if err := g.cache.Pull(ctx, Zones, nil, true); err != nil {
		t.Fatal(err)
	}
	v := g.eval.Snapshot().Version(Zones)
	if v == nil || v.Number != 1 || v.ETag != `"zones:1"` || len(g.eval.Snapshot().Entries(Zones)) != 2 {
		t.Fatalf("v1: %+v", v)
	}
	if g.count(CounterReconcileCatchups) != 1 || g.count(CounterPulls) != 1 || len(g.store.saved[Zones]) != 1 || g.store.pulled[Zones] != 1 {
		t.Fatalf("counters %v, saved %d", g.cache.Counters().Snapshot(), len(g.store.saved[Zones]))
	}
	if p, n := g.proj.Last(); p == nil || n != 1 {
		t.Fatal("no projection after v1")
	}

	// v2 changes one zone: the notification's pull_url names v1, the
	// delta is read and applied.
	changed := prohibited("TZP002")
	changed.upper = f64(60)
	ch := g.fake.Publish("zones", prohibited("TZP001").json(), changed.json(), prohibited("TZP003").json())
	before := g.fake.Requests("GET /v1/zones")
	if err := g.cache.Pull(ctx, Zones, hintOf(ch), false); err != nil {
		t.Fatal(err)
	}
	v = g.eval.Snapshot().Version(Zones)
	if v.Number != 2 || !v.Meta.Delta || g.count(CounterDeltaPulls) != 1 || g.fake.Requests("GET /v1/zones") != before+1 {
		t.Fatalf("v2: %+v, counters %v", v.Meta, g.cache.Counters().Snapshot())
	}
	for _, e := range g.eval.Snapshot().Entries(Zones) {
		if e.Identifier == "TZP002" && (e.Parts[0].Upper == nil || e.Parts[0].Upper.ValueM != 60) {
			t.Fatalf("TZP002 not changed: %+v", e.Parts[0].Upper)
		}
	}
	if n := len(g.eval.Snapshot().Entries(Zones)); n != 3 {
		t.Fatalf("v2 has %d zones", n)
	}
	// The same notification again (or one naming an older version) does
	// not ask the CISP: counted as a replay.
	before = g.fake.TotalRequests()
	if err := g.cache.Pull(ctx, Zones, hintOf(ch), false); err != nil {
		t.Fatal(err)
	}
	if g.fake.TotalRequests() != before || g.count(CounterVersionReplays) != 1 {
		t.Fatalf("a replayed version asked the CISP: %d requests", g.fake.TotalRequests()-before)
	}
	// Reconciliation with nothing new: 304, confirmed, nothing installed.
	if err := g.cache.Pull(ctx, Zones, nil, true); err != nil {
		t.Fatal(err)
	}
	if g.count(CounterNotModified) != 1 || g.count(CounterReconcileCatchups) != 1 || g.store.touched != 1 {
		t.Fatalf("304: %v", g.cache.Counters().Snapshot())
	}
}

// A delta that does not apply (the version held is not its from) is
// never guessed at: the dataset is read whole.
func TestCacheDeltaFallsBackToWhole(t *testing.T) {
	g := newCacheRig(t, "")
	ctx := t.Context()
	g.fake.Publish("zones", prohibited("TZP001").json())
	if err := g.cache.Pull(ctx, Zones, nil, false); err != nil {
		t.Fatal(err)
	}
	g.fake.Publish("zones", prohibited("TZP002").json())
	ch := g.fake.Publish("zones", prohibited("TZP003").json())
	// The notification of v3 names since_version=2; we hold 1.
	if err := g.cache.Pull(ctx, Zones, hintOf(ch), false); err != nil {
		t.Fatal(err)
	}
	if v := g.eval.Snapshot().Version(Zones); v.Number != 3 || v.Meta.Delta {
		t.Fatalf("v3: %+v", v.Meta)
	}
	// A pull_url whose delta answer is not usable is counted and the
	// dataset read whole.
	ch = g.fake.Publish("zones", prohibited("TZP004").json())
	h := hintOf(ch)
	h.PullURL = g.fake.URL() + "/v1/zones?since_version=3&x=1"
	g.fake.PublishRaw("zones", collection(Zones, 5, prohibited("TZP005").json())) // v5 raw: the delta is 410
	h.Version = 5
	if err := g.cache.Pull(ctx, Zones, h, false); err != nil {
		t.Fatal(err)
	}
	if v := g.eval.Snapshot().Version(Zones); v.Number != 5 || g.count(CounterDeltaUnusable) != 1 {
		t.Fatalf("v5: %d, %v", v.Number, g.cache.Counters().Snapshot())
	}
}

// A malformed publication is refused whole, the previous version kept,
// /readyz degraded with the first problem and the counter moved; a valid
// next version clears it (E-01 twin).
func TestCacheRefusedPublication(t *testing.T) {
	g := newCacheRig(t, "https://ussp.test/v1/cis/notifications")
	ctx := t.Context()
	for _, d := range ED318Datasets {
		g.fake.Publish(string(d), featureOf(d, "TZP001").json())
		if d == USpaceAirspace {
			g.fake.Publish(string(d), airspace("TSA001").json())
		}
	}
	for _, d := range ED318Datasets {
		if err := g.cache.Pull(ctx, d, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	bad := strings.Replace(string(collection(Zones, 2, prohibited("TZP001").json())), `"PROHIBITED"`, `"FORBIDDEN"`, 1)
	g.fake.PublishRaw("zones", []byte(bad))
	err := g.cache.Pull(ctx, Zones, nil, false)
	var rf *RefusalError
	if !errors.As(err, &rf) || !strings.Contains(rf.First, "features[0].properties.type") {
		t.Fatalf("refusal: %v", err)
	}
	if v := g.eval.Snapshot().Version(Zones); v.Number != 1 || g.count(CounterRefused) != 1 {
		t.Fatalf("kept %d, refused %d", v.Number, g.count(CounterRefused))
	}
	st, detail := g.cache.Probe(ctx)
	if st != obs.StateDegraded || !strings.Contains(detail, "last publication refused: zones features[0].properties.type") {
		t.Fatalf("probe: %s %q", st, detail)
	}
	g.fake.Publish("zones", prohibited("TZP009").json())
	if err := g.cache.Pull(ctx, Zones, nil, false); err != nil {
		t.Fatal(err)
	}
	if st, detail := g.cache.Probe(ctx); st != obs.StateUp || strings.Contains(detail, "refused") {
		t.Fatalf("after a valid version: %s %q", st, detail)
	}
	if v := g.eval.Snapshot().Version(Zones); v.Number != 3 {
		t.Fatalf("v3 not installed: %d", v.Number)
	}
}

// A version whose collection parses but whose shape core cannot build is
// refused too, as is a body whose cis_* members do not fit.
func TestCacheRefusesUnbuildableAndMislabelled(t *testing.T) {
	g := newCacheRig(t, "")
	ctx := t.Context()
	g.fake.PublishRaw("zones", collection(Restrictions, 1, prohibited("TZP001").json()))
	if err := g.cache.Pull(ctx, Zones, nil, false); err == nil || !strings.Contains(err.Error(), "cis_dataset") {
		t.Fatalf("mislabelled: %v", err)
	}
	if g.eval.Snapshot().Version(Zones) != nil {
		t.Fatal("a mislabelled body was installed")
	}
}

// The uspace_airspace dataset holds U-space airspaces only: a feature of
// any other type there would be read neither as an airspace nor as a
// zone, so the version is refused whole rather than installed with a
// zone nobody judges. Twin: the same dataset with airspaces only is
// installed.
func TestCacheRefusesAZoneInTheAirspaceDataset(t *testing.T) {
	g := newCacheRig(t, "")
	ctx := t.Context()
	g.fake.Publish(string(USpaceAirspace), airspace("TSA001").json(), prohibited("TZP001").json())
	err := g.cache.Pull(ctx, USpaceAirspace, nil, false)
	var rf *RefusalError
	if !errors.As(err, &rf) || !strings.Contains(rf.First, "features[1]") || !strings.Contains(rf.First, "USPACE") {
		t.Fatalf("refusal: %v", err)
	}
	if g.eval.Snapshot().Version(USpaceAirspace) != nil {
		t.Fatal("a zone in the airspace dataset was installed")
	}
	g.fake.Publish(string(USpaceAirspace), airspace("TSA001").json(), airspace("TSA002").json())
	if err := g.cache.Pull(ctx, USpaceAirspace, nil, false); err != nil {
		t.Fatal(err)
	}
	if es := g.eval.Snapshot().Entries(USpaceAirspace); len(es) != 2 {
		t.Fatalf("%d airspaces installed", len(es))
	}
}

// With the CISP down the age crosses the bound (stale: true), and a
// confirmation once it is back clears it (both directions).
func TestCacheAgeCrossesWithCISPDown(t *testing.T) {
	g := newCacheRig(t, "")
	ctx := t.Context()
	for _, d := range ED318Datasets {
		g.fake.Publish(string(d), featureOf(d, "TZP001").json())
		if err := g.cache.Pull(ctx, d, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, age, stale := g.eval.Age(); stale || age != 0 {
		t.Fatalf("fresh: %v %v", age, stale)
	}
	g.fake.Down()
	for range 7 {
		g.clk.advance(60 * time.Second)
		for _, d := range ED318Datasets {
			_ = g.cache.Pull(ctx, d, nil, true)
		}
	}
	_, age, stale := g.eval.Age()
	if !stale || age != 420 {
		t.Fatalf("down 420 s: %v %v", age, stale)
	}
	st, detail := g.cache.Probe(ctx)
	if st != obs.StateDegraded || !strings.Contains(detail, "age 420 s > 300 s") {
		t.Fatalf("probe: %s %q", st, detail)
	}
	if g.count(CounterPullFailed) != 21 {
		t.Fatalf("pull failures %d", g.count(CounterPullFailed))
	}
	g.fake.Up()
	for _, d := range ED318Datasets {
		if err := g.cache.Pull(ctx, d, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, age, stale := g.eval.Age(); stale || age != 0 {
		t.Fatalf("back: %v %v", age, stale)
	}
}

// E-02: started without a CISP, the cache says what it is.
func TestCacheProbeWithoutCISP(t *testing.T) {
	clk := newClock()
	eval := NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: clk.Now})
	c := NewCache(CacheConfig{Evaluator: eval, Now: clk.Now})
	if err := c.Pull(t.Context(), Zones, nil, true); err == nil {
		t.Fatal("pulled without a CISP")
	}
	st, detail := c.Probe(t.Context())
	if st != obs.StateUnknown || !strings.HasPrefix(detail, "no version loaded") || !strings.Contains(detail, "USSP_CISP_BASE_URL") {
		t.Fatalf("probe: %s %q", st, detail)
	}
	if v, _, stale := eval.Age(); v != "" || !stale {
		t.Fatalf("age: %q %v", v, stale)
	}
}

// A dataset never published (404 no_version) is known to be empty; one
// never loaded is said by name.
func TestCacheEmptyDatasetsAndMissing(t *testing.T) {
	g := newCacheRig(t, "")
	ctx := t.Context()
	g.fake.Publish("zones", prohibited("TZP001").json())
	for _, d := range []Dataset{Zones, USpaceAirspace} {
		if err := g.cache.Pull(ctx, d, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	st, detail := g.cache.Probe(ctx)
	if st != obs.StateDegraded || !strings.Contains(detail, "no version loaded of restrictions") ||
		!strings.HasPrefix(detail, "zones:1,uspace_airspace:0") {
		t.Fatalf("probe: %s %q", st, detail)
	}
	if err := g.cache.Pull(ctx, Restrictions, nil, false); err != nil {
		t.Fatal(err)
	}
	st, detail = g.cache.Probe(ctx)
	if st != obs.StateDegraded || detail != "zones:1,uspace_airspace:0,restrictions:0, age 0 s; no change notifications (USSP_USS_BASE_URL is not set): the reconciliation alone" {
		t.Fatalf("probe: %s %q", st, detail)
	}
}

// The branch that says nothing is wrong (E-02): subscribed, every
// dataset current, the projection written: up, with the versions.
func TestCacheProbeUp(t *testing.T) {
	g := newCacheRig(t, "https://ussp.test/v1/cis/notifications")
	ctx, cancel := context.WithCancel(t.Context())
	for _, d := range ED318Datasets {
		g.fake.Publish(string(d), featureOf(d, "TZP001").json())
	}
	g.cache.subscribeLoop(ctx)
	for _, d := range ED318Datasets {
		if err := g.cache.Pull(ctx, d, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	st, detail := g.cache.Probe(t.Context())
	if st != obs.StateUp || detail != "zones:1,uspace_airspace:1,restrictions:1, age 0 s" {
		t.Fatalf("probe: %s %q", st, detail)
	}
}

func TestCacheSubscribeIdempotent(t *testing.T) {
	g := newCacheRig(t, "https://ussp.test/v1/cis/notifications")
	g.cache.cfg.BBox = []float64{44, 41, 46, 43}
	ctx := t.Context()
	g.cache.subscribeLoop(ctx)
	g.cache.subscribeLoop(ctx)
	subs := g.fake.Subscriptions()
	if len(subs) != 1 || len(subs[0].Datasets) != 4 || len(subs[0].Bbox) != 4 || g.fake.Requests("POST /v1/subscriptions") != 1 {
		t.Fatalf("subscriptions: %+v", subs)
	}
	// Suspended, or with another box: patched (re-activated), never a
	// second subscription.
	g.fake.Suspend()
	g.cache.cfg.BBox = nil
	g.cache.subscribeLoop(ctx)
	subs = g.fake.Subscriptions()
	if len(subs) != 1 || subs[0].Status != "active" || subs[0].Bbox != nil || g.fake.Requests("PATCH /v1/subscriptions/sub-1") != 1 {
		t.Fatalf("after patch: %+v", subs)
	}
	// The CISP down: retried, counted, readyz says so.
	g.fake.Down()
	ctx2, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	g.cache.mu.Lock()
	g.cache.subscribed = ""
	g.cache.mu.Unlock()
	g.cache.subscribeLoop(ctx2)
	if g.count(CounterSubscribeFailed) < 2 {
		t.Fatalf("subscribe failures %d", g.count(CounterSubscribeFailed))
	}
	g.fake.Up()
	g.fake.Publish("zones")
	_ = g.cache.Pull(ctx, Zones, nil, false)
	if _, detail := g.cache.Probe(ctx); !strings.Contains(detail, "not subscribed: CISP answered 503") {
		t.Fatalf("probe: %q", detail)
	}
}

// The store failing never stops the cache: the version is served from
// memory, counted, and stored at the next confirmation.
func TestCacheStoreFailure(t *testing.T) {
	g := newCacheRig(t, "")
	ctx := t.Context()
	g.store.setDown(true)
	g.fake.Publish("zones", prohibited("TZP001").json())
	if err := g.cache.Pull(ctx, Zones, nil, false); err != nil {
		t.Fatal(err)
	}
	if g.eval.Snapshot().Version(Zones) == nil || g.count(CounterStoreFailed) != 1 || len(g.store.saved[Zones]) != 0 {
		t.Fatal("store failure handling")
	}
	g.store.setDown(false)
	if err := g.cache.Pull(ctx, Zones, nil, true); err != nil { // 304
		t.Fatal(err)
	}
	if len(g.store.saved[Zones]) != 1 {
		t.Fatal("not stored at the next confirmation")
	}
	// A failed touch is counted.
	g.store.setDown(true)
	_ = g.cache.Pull(ctx, Zones, nil, true)
	if g.count(CounterStoreFailed) != 2 {
		t.Fatalf("touch failure counted %d", g.count(CounterStoreFailed))
	}
}

type failingProjector struct{}

func (failingProjector) ProjectCIS(context.Context, *Projection) error {
	return errors.New("KV unreachable")
}

func TestCacheProjectionFailure(t *testing.T) {
	g := newCacheRig(t, "")
	g.cache.cfg.Projector = failingProjector{}
	g.fake.Publish("zones", prohibited("TZP001").json())
	if err := g.cache.Pull(t.Context(), Zones, nil, false); err != nil {
		t.Fatal(err)
	}
	if g.count(CounterProjectionFailed) != 1 {
		t.Fatal("projection failure not counted")
	}
	if _, detail := g.cache.Probe(t.Context()); !strings.Contains(detail, "projection: KV unreachable") {
		t.Fatalf("probe: %q", detail)
	}
	// The KV is back: the next reconciliation tick writes the projection
	// again and the probe no longer names it.
	g.cache.cfg.Projector = g.proj
	g.cache.retryProjection(t.Context())
	if p, n := g.proj.Last(); p == nil || n != 1 || g.count(CounterProjectionRetried) != 1 {
		t.Fatalf("not retried: %v %d", p, n)
	}
	if _, detail := g.cache.Probe(t.Context()); strings.Contains(detail, "projection") {
		t.Fatalf("probe after the retry: %q", detail)
	}
	// Nothing failed: no retry.
	g.cache.retryProjection(t.Context())
	if _, n := g.proj.Last(); n != 1 || g.count(CounterProjectionRetried) != 1 {
		t.Fatal("retried with nothing failed")
	}
}

// Warm installs the stored versions with their database age: an old one
// is stale at once.
func TestCacheWarm(t *testing.T) {
	g := newCacheRig(t, "")
	for _, d := range ED318Datasets {
		g.store.stored = append(g.store.stored, StoredVersion{Version: mustVersion(t, d, 4, featureOf(d, "TZP001").json()), AgeS: 420})
	}
	g.cache.Warm(t.Context())
	v, age, stale := g.eval.Age()
	if v != "zones:4,uspace_airspace:4,restrictions:4" || age != 420 || !stale {
		t.Fatalf("warm: %q %v %v", v, age, stale)
	}
	if p, _ := g.proj.Last(); p == nil {
		t.Fatal("no projection after warm")
	}
	g2 := newCacheRig(t, "")
	g2.store.loadErr = errors.New("relation missing")
	g2.cache.Warm(t.Context())
	if st, detail := g2.cache.Probe(t.Context()); st != obs.StateUnknown || !strings.Contains(detail, "relation missing") {
		t.Fatalf("warm error: %s %q", st, detail)
	}
}

// Run: a trigger pulls at once, a notified version that cannot be
// pulled is shown on /readyz until it is, and the reconciliation catches
// a version no notification announced.
func TestCacheRun(t *testing.T) {
	g := newCacheRig(t, "")
	g.cache.cfg.ReconcileInterval = 50 * time.Millisecond
	g.clk = nil
	g.cache.cfg.Now = time.Now
	g.eval.cfg.Now = time.Now
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); g.cache.Run(ctx) }()
	defer func() { cancel(); <-done }()

	ch := g.fake.Publish("zones", prohibited("TZP001").json())
	g.cache.Trigger(Zones, *hintOf(ch))
	waitFor(t, func() bool { v := g.eval.Snapshot().Version(Zones); return v != nil && v.Number == 1 })

	g.fake.Down()
	ch = g.fake.Publish("restrictions", prohibited("DAR0001").json())
	g.cache.Trigger(Restrictions, *hintOf(ch))
	waitFor(t, func() bool {
		_, d := g.cache.Probe(ctx)
		return strings.Contains(d, "restrictions version 1 notified by https://cisp.test") && strings.Contains(d, "not pulled yet (CISP answered 503")
	})
	g.fake.Up()
	waitFor(t, func() bool {
		_, d := g.cache.Probe(ctx)
		v := g.eval.Snapshot().Version(Restrictions)
		return v != nil && !strings.Contains(d, "not pulled yet")
	})
	if g.count(CounterReconcileCatchups) == 0 {
		t.Fatal("the reconciliation caught nothing")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCacheUSSPList(t *testing.T) {
	g := newCacheRig(t, "")
	list := map[string]any{"schema": "cis/ussp_list/v1", "issued": "2026-10-01T00:00:00Z", "ussps": []any{},
		"cis_dataset": "ussp_list", "cis_version": 1, "cis_updated_at": "2026-10-01T00:00:00Z"}
	b, _ := json.Marshal(list)
	g.fake.PublishRaw("ussp_list", b)
	if err := g.cache.Pull(t.Context(), USSPList, nil, false); err != nil {
		t.Fatal(err)
	}
	v := g.eval.Snapshot().Version(USSPList)
	if v == nil || v.Number != 1 || v.USSPList == nil || len(v.Meta.USSPList) == 0 {
		t.Fatalf("ussp_list: %+v", v)
	}
	// The ussp_list is not part of the geo-awareness age.
	if ver, _, _ := g.eval.Age(); strings.Contains(ver, "ussp_list") {
		t.Fatalf("age names the ussp_list: %q", ver)
	}
}

// An unreachable CISP host (nothing listening) is a pull failure with
// the transport's error, not a crash.
func TestCacheUnreachableCISP(t *testing.T) {
	clk := newClock()
	eval := NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: clk.Now})
	client, err := NewClient(ClientConfig{BaseURL: "http://127.0.0.1:1", Tokens: cisp.Tokens{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c := NewCache(CacheConfig{Client: client, Evaluator: eval, Now: clk.Now})
	if err := c.Pull(t.Context(), Zones, nil, true); err == nil {
		t.Fatal("pulled from nowhere")
	}
	if c.Counters().Get(CounterPullFailed) != 1 {
		t.Fatal("not counted")
	}
}
