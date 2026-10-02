//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/cis/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/ansp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

// withCIS adds a fake CISP to an api process's variables: its base URL,
// its notification issuer and JWKS, this USSP's callback base and the
// client secret the token service (the fake authority) is asked with.
func withCIS(t *testing.T, vars map[string]string) *cisp.Fake {
	t.Helper()
	fake, err := cisp.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	secret := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secret, []byte("integration-client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vars["USSP_TOKEN_CLIENT_SECRET_FILE"] = secret
	vars["USSP_CISP_BASE_URL"] = fake.URL()
	vars["USSP_USS_BASE_URL"] = "https://" + testHost
	vars["USSP_CIS_NOTIFY_ISSUERS"] = fake.Signer.Issuer + "=" + fake.URL() + "/.well-known/jwks.json"
	return fake
}

// cleanCIS empties the CIS tables, so a test's versions start at 1.
func cleanCIS(t *testing.T) {
	t.Helper()
	ensureSchemas(t)
	if _, err := relOwner(t).Exec(context.Background(),
		"DELETE FROM cis_notification_jtis; DELETE FROM cis_notifications; DELETE FROM cis_datasets"); err != nil {
		t.Fatal(err)
	}
}

// zone is an ED-318 PROHIBITED feature over Tbilisi up to upperM AGL.
func zone(id string, upperM float64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{"type": "Polygon",
			"coordinates": []any{[]any{[]any{44.80, 41.70}, []any{44.85, 41.70}, []any{44.85, 41.73}, []any{44.80, 41.73}, []any{44.80, 41.70}}},
			"layer":       map[string]any{"lower": 0, "lowerReference": "AGL", "upper": upperM, "upperReference": "AGL", "uom": "m"}},
		"properties": map[string]any{"identifier": id, "country": "GEO", "type": "PROHIBITED", "variant": "COMMON",
			"name": []any{map[string]any{"text": "Integration zone " + id, "lang": "en-GB"}}, "reason": []string{"SENSITIVE"},
			"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test authority", "lang": "en-GB"}}, "purpose": "AUTHORIZATION"}}},
	})
	return b
}

type cisRig struct {
	t        *testing.T
	fake     *cisp.Fake
	ansp     *ansp.Fake
	store    pgstore.Store
	eval     *cis.Evaluator
	cache    *cis.Cache
	proj     *cis.MemoryProjector
	counters *core.Counters
	verifier *coreauth.CompactVerifier
	senders  map[string]cis.Sender
	srv      *httptest.Server
	dead     atomic.Bool
	callback string
}

// newCISRig runs the CIS cache against the fake CISP and the real
// relational database, with its receiver on an HTTP server whose URL is
// the callback the cache subscribes with.
func newCISRig(t *testing.T, reconcile time.Duration) *cisRig {
	t.Helper()
	cleanCIS(t)
	g := &cisRig{t: t, counters: &core.Counters{}, proj: &cis.MemoryProjector{}, store: pgstore.Store{S: appStore(t)}}
	var err error
	if g.fake, err = cisp.New(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.fake.Close)
	if g.ansp, err = ansp.New(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.ansp.Close)
	g.verifier, err = coreauth.NewCompactVerifier(context.Background(), coreauth.CompactConfig{
		Issuers:   map[string]coreauth.IssuerConfig{g.fake.Signer.Issuer: g.fake.IssuerConfig(), g.ansp.Signer.Issuer: g.ansp.IssuerConfig()},
		Audiences: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	g.senders = map[string]cis.Sender{
		g.fake.Signer.Issuer: {BaseHost: g.fake.Host()},
		g.ansp.Signer.Issuer: {ANSP: true, BaseHost: g.ansp.Host()},
	}
	g.eval = cis.NewEvaluator(cis.EvaluatorConfig{StaleS: func() float64 { return 300 }, Counters: g.counters})
	client, err := cis.NewClient(cis.ClientConfig{BaseURL: g.fake.URL(), Tokens: cisp.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	logger := obs.NewLogger(logs, "debug", "api")
	g.srv = httptest.NewServer(g.receiverHandler())
	t.Cleanup(g.srv.Close)
	g.callback = g.srv.URL + cis.NotificationsPath
	g.cache = cis.NewCache(cis.CacheConfig{Client: client, Store: g.store, Evaluator: g.eval, Projector: g.proj, Counters: g.counters,
		Logger: logger, CallbackURL: g.callback, ReconcileInterval: reconcile})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); g.cache.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		if t.Failed() {
			t.Logf("cache log:\n%s", logs.String())
		}
	})
	// Subscribed, and every dataset read once (the CISP has none yet).
	g.waitFor("subscription and first reads", 10*time.Second, func() bool {
		if len(g.fake.Subscriptions()) != 1 {
			return false
		}
		_, detail := g.cache.Probe(context.Background())
		return strings.HasPrefix(detail, "zones:0,uspace_airspace:0,restrictions:0")
	})
	return g
}

// receiverHandler is a receiver on the shared store; while dead it
// answers 503, as a stopped receiver behind its proxy would.
func (g *cisRig) receiverHandler() http.Handler {
	rc := g.newReceiver()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.dead.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rc.ServeHTTP(w, r)
	})
}

func (g *cisRig) newReceiver() *cis.Receiver {
	return cis.NewReceiver(cis.ReceiverConfig{Verifier: g.verifier, Senders: g.senders, Store: g.store,
		Trigger: func(d cis.Dataset, h cis.Hint) { g.cache.Trigger(d, h) }, Counters: g.counters})
}

func (g *cisRig) waitFor(what string, within time.Duration, cond func() bool) time.Duration {
	g.t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > within {
			g.t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return time.Since(start)
}

func (g *cisRig) version(d cis.Dataset) int64 {
	if v := g.eval.Snapshot().Version(d); v != nil {
		return v.Number
	}
	return 0
}

func (g *cisRig) projected(label string) bool {
	p, _ := g.proj.Last()
	return p != nil && strings.Contains(p.CISVersion, label)
}

func (g *cisRig) dbCount(sql string, args ...any) int64 {
	return count(g.t, relOwner(g.t), sql, args...)
}

// The brief's integration run (WP-4 done-when): publish v1, cache has
// it; v2 with one zone changed, the webhook arrives and the delta is
// pulled and projected within 1 s (measured, Z-12); the receiver
// killed, v3 published, the 60 s reconciliation picks it up (measured);
// a malformed publication refused, v3 kept, /readyz degraded, counter
// moved; a valid next version clears it (E-01); a restart serves the
// stored version.
func TestIntegrationCISWebhookDeltaReconcileRefusal(t *testing.T) {
	g := newCISRig(t, cis.DefaultReconcileInterval)
	ctx := context.Background()

	ch := g.fake.Publish("zones", zone("TZP001", 120), zone("TZP002", 120))
	g.fake.Deliver(ctx, ch)
	d1 := g.waitFor("v1", 5*time.Second, func() bool { return g.version(cis.Zones) == 1 && g.projected("zones:1") })
	t.Logf("v1 installed and projected %v after the delivery", d1.Round(time.Millisecond))
	g.waitFor("v1 stored", 5*time.Second, func() bool {
		return g.dbCount("SELECT count(*) FROM cis_features WHERE dataset = 'zones' AND version = 1") == 2
	})

	ch = g.fake.Publish("zones", zone("TZP001", 120), zone("TZP002", 60))
	start := time.Now()
	g.fake.Deliver(ctx, ch)
	d2 := g.waitFor("v2 projected", 5*time.Second, func() bool { return g.projected("zones:2") })
	d2 = time.Since(start)
	t.Logf("v2: webhook to projection %v (budget 1 s, Z-12: within one telemetry tick)", d2.Round(time.Millisecond))
	if d2 > time.Second {
		t.Fatalf("v2 projected after %v, budget 1 s", d2)
	}
	if g.counters.Get(cis.CounterDeltaPulls) != 1 || !g.eval.Snapshot().Version(cis.Zones).Meta.Delta {
		t.Fatalf("v2 not read as a delta: %v", g.counters.Snapshot())
	}
	for _, e := range g.eval.Snapshot().Entries(cis.Zones) {
		if e.Identifier == "TZP002" && e.Parts[0].Upper.ValueM != 60 {
			t.Fatalf("TZP002 upper %v", e.Parts[0].Upper.ValueM)
		}
	}
	if d := g.fake.Deliveries(); d[len(d)-1].Status != http.StatusNoContent {
		t.Fatalf("delivery answered %d", d[len(d)-1].Status)
	}

	// The receiver dies; v3's webhook fails; the reconciliation finds it.
	g.dead.Store(true)
	ch = g.fake.Publish("zones", zone("TZP001", 120), zone("TZP002", 60), zone("TZP003", 90))
	start = time.Now()
	g.fake.Deliver(ctx, ch)
	if d := g.fake.Deliveries(); d[len(d)-1].Status != http.StatusServiceUnavailable {
		t.Fatalf("the dead receiver answered %d", d[len(d)-1].Status)
	}
	g.waitFor("v3 by reconciliation", 70*time.Second, func() bool { return g.version(cis.Zones) == 3 })
	d3 := time.Since(start)
	t.Logf("v3 (webhook lost): reconciliation installed it %v after the publication (bound 60 s)", d3.Round(time.Millisecond))
	if d3 > 62*time.Second {
		t.Fatalf("reconciliation took %v", d3)
	}
	if g.counters.Get(cis.CounterReconcileCatchups) < 1 {
		t.Fatal("cis_reconcile_catchups did not move")
	}
	g.dead.Store(false)

	// A malformed publication: refused whole, v3 kept, /readyz degraded.
	bad, _ := json.Marshal(map[string]any{"type": "FeatureCollection", "features": []any{map[string]any{"type": "Feature"}},
		"cis_dataset": "zones", "cis_version": 4})
	ch = g.fake.PublishRaw("zones", bad)
	g.fake.Deliver(ctx, ch)
	g.waitFor("refusal", 5*time.Second, func() bool { return g.counters.Get(cis.CounterRefused) == 1 })
	if g.version(cis.Zones) != 3 {
		t.Fatalf("v3 not kept: %d", g.version(cis.Zones))
	}
	st, detail := g.cache.Probe(ctx)
	t.Logf("readyz cis after the refusal: %s (%s)", st, detail)
	if st != obs.StateDegraded || !strings.Contains(detail, "last publication refused: zones features[0]") {
		t.Fatalf("probe: %s %q", st, detail)
	}

	// E-01 twin: a valid v5 clears it.
	ch = g.fake.Publish("zones", zone("TZP001", 120))
	g.fake.Deliver(ctx, ch)
	g.waitFor("v5", 5*time.Second, func() bool { return g.version(cis.Zones) == 5 })
	if st, detail := g.cache.Probe(ctx); st != obs.StateUp || strings.Contains(detail, "refused") {
		t.Fatalf("after v5: %s %q", st, detail)
	}
	g.waitFor("v5 stored, older pruned", 5*time.Second, func() bool {
		return g.dbCount("SELECT count(*) FROM cis_datasets WHERE dataset = 'zones'") == 2 &&
			g.dbCount("SELECT max(version) FROM cis_datasets WHERE dataset = 'zones'") == 5
	})
	if n := g.dbCount("SELECT count(*) FROM cis_notifications WHERE dataset = 'zones' AND pulled_at IS NULL AND version <= 5 AND version <> 4"); n != 0 {
		t.Fatalf("%d pulled notifications not marked", n)
	}

	// A restart serves the stored version, judged as a pulled one.
	eval := cis.NewEvaluator(cis.EvaluatorConfig{StaleS: func() float64 { return 300 }})
	cis.NewCache(cis.CacheConfig{Store: g.store, Evaluator: eval}).Warm(ctx)
	v := eval.Snapshot().Version(cis.Zones)
	if v == nil || v.Number != 5 || len(eval.Snapshot().Entries(cis.Zones)) != 1 {
		t.Fatalf("warm: %+v", v)
	}
	if ver, age, _ := eval.Age(); !strings.HasPrefix(ver, "zones:5") || age > 60 {
		t.Fatalf("warm age: %q %v", ver, age)
	}
}

// The receiver's pairs (E-01) over HTTP against the fake CISP: the
// CISP's signature pulls, the ANSP's pulls (degraded direct path),
// neither's is 401 and counted; subscription_test and an unknown reason
// make no request at the CISP; a publication makes one; a pull_url on
// another host is never requested, counted, and the dataset is read
// from the configured base URL; a delivery replayed to another replica
// is caught through the database.
func TestIntegrationCISReceiverPairs(t *testing.T) {
	g := newCISRig(t, time.Hour)
	ctx := context.Background()
	quiet := func() { time.Sleep(300 * time.Millisecond) }
	post := func(url, token string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(token))
		req.Header.Set("Content-Type", "application/jose")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	change := func(dataset, reason string, version int64, pullURL string) json.RawMessage {
		b, _ := json.Marshal(cisp.Change{Schema: "cis/change/v1", MsgID: strconv.FormatInt(version, 10), Producer: "cisp/deliver-a",
			Dataset: dataset, Version: version, ETag: `"` + dataset + `:` + strconv.FormatInt(version, 10) + `"`, FeatureIDs: []string{},
			RemovedIDs: []string{}, Reason: reason, At: time.Now().UTC().Format(time.RFC3339), PullURL: pullURL})
		return b
	}
	sign := func(s *signer.Signer, body json.RawMessage) string {
		tok, err := s.Sign(g.callback, "sub-1", signer.NewID(), body, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	// subscription_test and an unknown reason: 204, no request at all.
	before := g.fake.TotalRequests()
	for _, reason := range []string{"subscription_test", "zones_changed"} {
		if code := post(g.callback, sign(g.fake.Signer, change("zones", reason, 0, g.fake.URL()+"/v1/zones?since_version=0"))); code != http.StatusNoContent {
			t.Fatalf("%s: %d", reason, code)
		}
	}
	quiet()
	if n := g.fake.TotalRequests() - before; n != 0 {
		t.Fatalf("ack-only reasons made %d requests at the CISP", n)
	}

	// A publication signed by the CISP: one pull.
	ch := g.fake.Publish("zones", zone("TZP001", 120))
	before = g.fake.Requests("GET /v1/zones")
	if code := post(g.callback, sign(g.fake.Signer, change("zones", "publication", ch.Version, ch.PullURL))); code != http.StatusNoContent {
		t.Fatalf("publication: %d", code)
	}
	g.waitFor("the CISP-signed pull", 5*time.Second, func() bool { return g.version(cis.Zones) == 1 })
	quiet()
	if n := g.fake.Requests("GET /v1/zones") - before; n != 1 {
		t.Fatalf("publication made %d reads, want 1", n)
	}

	// The same restriction signed by the ANSP (degraded direct path): a
	// pull of the restrictions dataset from the CISP.
	ch = g.fake.Publish("restrictions", zone("DAR0001", 120))
	before = g.fake.Requests("GET /v1/restrictions")
	if code, err := g.ansp.Notify(ctx, g.callback, "r-1", json.RawMessage(change("restrictions", "restriction_activated", ch.Version,
		g.ansp.URL()+"/v1/restrictions/r-1"))); err != nil || code != http.StatusNoContent {
		t.Fatalf("ANSP: %d %v", code, err)
	}
	g.waitFor("the ANSP-signed pull", 5*time.Second, func() bool { return g.version(cis.Restrictions) == 1 })
	if g.fake.Requests("GET /v1/restrictions")-before != 1 || g.counters.Get(cis.CounterANSPDirect) != 1 {
		t.Fatalf("ANSP pull: %v", g.counters.Snapshot())
	}
	if n := g.dbCount("SELECT count(*) FROM cis_notifications WHERE issuer = $1 AND subscription = 'r-1'", g.ansp.Signer.Issuer); n != 1 {
		t.Fatalf("ANSP notification logged %d times", n)
	}

	// A key of neither (it claims the CISP's issuer): 401, counted, no pull.
	other, err := signer.New(g.fake.Signer.Issuer, "fake-cisp-1")
	if err != nil {
		t.Fatal(err)
	}
	before = g.fake.TotalRequests()
	if code := post(g.callback, sign(other, change("zones", "publication", 9, g.fake.URL()+"/v1/zones?since_version=1"))); code != http.StatusUnauthorized {
		t.Fatalf("another key: %d", code)
	}
	quiet()
	if g.counters.Get(cis.CounterBadSignature) != 1 || g.fake.TotalRequests() != before {
		t.Fatalf("another key: counted %d, %d requests", g.counters.Get(cis.CounterBadSignature), g.fake.TotalRequests()-before)
	}

	// pull_url on another host: never requested; the configured CISP is read.
	var evilHits atomic.Int64
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { evilHits.Add(1) }))
	defer evil.Close()
	evilURL := strings.Replace(evil.URL, "127.0.0.1", "localhost", 1) + "/v1/zones?since_version=1"
	ch = g.fake.Publish("zones", zone("TZP001", 120), zone("TZP009", 50))
	before = g.fake.Requests("GET /v1/zones")
	if code := post(g.callback, sign(g.fake.Signer, change("zones", "publication", ch.Version, evilURL))); code != http.StatusNoContent {
		t.Fatalf("evil pull_url: %d", code)
	}
	g.waitFor("v2 from the base URL", 5*time.Second, func() bool { return g.version(cis.Zones) == 2 })
	if evilHits.Load() != 0 || g.counters.Get(cis.CounterPullURLMismatch) != 1 || g.fake.Requests("GET /v1/zones")-before != 1 {
		t.Fatalf("evil host hit %d, mismatch %d", evilHits.Load(), g.counters.Get(cis.CounterPullURLMismatch))
	}

	// A delivery replayed to another replica: the database catches it.
	replica := httptest.NewServer(g.newReceiver())
	defer replica.Close()
	tok := sign(g.fake.Signer, change("zones", "publication", 2, g.fake.URL()+"/v1/zones?since_version=1"))
	if code := post(g.callback, tok); code != http.StatusNoContent {
		t.Fatalf("first: %d", code)
	}
	replayed := g.counters.Get(cis.CounterWebhookReplayed)
	if code := post(replica.URL+cis.NotificationsPath, tok); code != http.StatusNoContent {
		t.Fatalf("replay: %d", code)
	}
	if g.counters.Get(cis.CounterWebhookReplayed) != replayed+1 {
		t.Fatal("the replay on another replica was not caught")
	}
	if n := g.dbCount("SELECT count(*) FROM cis_notification_jtis WHERE expires_at > now() + interval '9 minutes'"); n == 0 {
		t.Fatal("no delivery id remembered on the database clock")
	}
}

// Started without a CISP, api says so on /readyz (E-02): cis unknown,
// "no version loaded", naming the missing variable.
func TestIntegrationAPICISUnknownWithoutCISP(t *testing.T) {
	cleanCIS(t)
	c := run(t, api.Spec, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
	})
	var d struct {
		State  string `json:"state"`
		Detail string `json:"detail"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := c.GetReadyzWithResponse(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Dependencies map[string]json.RawMessage `json:"dependencies"`
		}
		_ = json.Unmarshal(resp.Body, &body)
		_ = json.Unmarshal(body.Dependencies["cis"], &d)
		if strings.Contains(d.Detail, "USSP_CISP_BASE_URL") || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("readyz cis: %s (%s)", d.State, d.Detail)
	if d.State != "unknown" || d.Detail != "no version loaded: no CISP configured (USSP_CISP_BASE_URL)" {
		t.Fatalf("cis: %+v", d)
	}
}
