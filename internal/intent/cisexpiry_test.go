package intent

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

// The decision on a real CIS cache fed by the fake CISP (spec 02 F3
// erratum 2026-10-16; brief WP-4, WP-7): what a natural expiry of a
// restriction does to the authorisations, before the CISP's expiry
// version could be installed and after.

// expiryStaleS is the cis_stale_s of these tests.
const expiryStaleS = 300

type cisClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *cisClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *cisClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type cisExpiryRig struct {
	fake  *cisp.Fake
	clk   *cisClock
	eval  *cis.Evaluator
	cache *cis.Cache
	*rig
}

// newCISExpiryRig is newRig deciding on a cache of the fake CISP, with
// zones and uspace_airspace published empty and restrictions v1 = {A
// ending at testNow, B ending two days later}, both far from the
// request, all installed at testNow.
func newCISExpiryRig(t *testing.T) *cisExpiryRig {
	t.Helper()
	fake, err := cisp.NewTLS()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	g := &cisExpiryRig{fake: fake, clk: &cisClock{t: testNow}, rig: newRig()}
	client, err := cis.NewClient(cis.ClientConfig{BaseURL: fake.URL(), Tokens: cisp.Tokens{}, HTTPClient: fake.Client()})
	if err != nil {
		t.Fatal(err)
	}
	pubs, err := coreauth.NewDetachedVerifier(t.Context(), coreauth.DetachedConfig{Publishers: fake.PublisherKeys(), MaxAge: 366 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	g.eval = cis.NewEvaluator(cis.EvaluatorConfig{StaleS: func() float64 { return expiryStaleS }, Now: g.clk.Now})
	g.cache = cis.NewCache(cis.CacheConfig{Client: client, Publishers: pubs, ExpirySkew: 30 * time.Second, Evaluator: g.eval, Now: g.clk.Now})
	g.decider.CIS, g.decider.Integrity = g.eval, g.cache
	fake.Publish("zones")
	fake.Publish("uspace_airspace")
	fake.Publish("restrictions", restrictionFeature("A", testNow), restrictionFeature("B", testNow.Add(48*time.Hour)))
	g.reconcile(t)
	for _, d := range cis.ED318Datasets {
		if v := g.eval.Snapshot().Version(d); v == nil || v.Number != 1 {
			t.Fatalf("%s: %+v", d, v)
		}
	}
	return g
}

// reconcile pulls every ED-318 dataset as the reconciliation does.
func (g *cisExpiryRig) reconcile(t *testing.T) {
	t.Helper()
	for _, d := range cis.ED318Datasets {
		_ = g.cache.Pull(t.Context(), d, nil, true)
	}
}

func (g *cisExpiryRig) decide(t *testing.T) Decision {
	t.Helper()
	d, _ := g.decider.Decide(t.Context(), normalise(t, baseRequest()), testPolicy(), testNow, newID(), testNow, nil)
	return d
}

// restrictionFeature is an active PROHIBITED restriction far east of
// the request, its single limitedApplicability period ending at end,
// with the CISP's cis_restriction.
func restrictionFeature(id string, end time.Time) json.RawMessage {
	ring := []any{[]any{45.50, 41.70}, []any{45.55, 41.70}, []any{45.55, 41.73}, []any{45.50, 41.73}, []any{45.50, 41.70}}
	b, err := json.Marshal(map[string]any{"type": "Feature",
		"geometry": map[string]any{"type": "Polygon", "coordinates": []any{ring},
			"layer": map[string]any{"uom": "m", "lower": 0, "lowerReference": "AGL", "upper": 120, "upperReference": "AGL"}},
		"properties": map[string]any{
			"identifier": id, "country": "GEO", "name": []any{map[string]any{"text": "Restriction " + id, "lang": "en-GB"}},
			"type": "PROHIBITED", "variant": "COMMON", "reason": []string{"SENSITIVE"},
			"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test ANSP", "lang": "en-GB"}}, "purpose": "AUTHORIZATION"}},
			"limitedApplicability": []any{map[string]any{"startDateTime": end.Add(-72 * time.Hour).UTC().Format(time.RFC3339),
				"endDateTime": end.UTC().Format(time.RFC3339)}},
			"extendedProperties": map[string]any{cis.RestrictionMember: map[string]any{"id": "r-" + id, "state": "active"}},
		}})
	if err != nil {
		panic(err)
	}
	return b
}

// expiryRecord is the CISP's expiry record of A at testNow; older is the
// record of a CISP from before the erratum (no feature_id, no ends_at).
func expiryRecord(older bool) []byte {
	m := map[string]any{"op": cis.OpExpire, "restriction_id": "r-A", "at": testNow.Format(time.RFC3339)}
	if !older {
		m["feature_id"], m["ends_at"] = "A", testNow.Format(time.RFC3339)
	}
	b, _ := json.Marshal(m)
	return b
}

// 7. The decision across an expiry. Before the erratum, the CISP's
// expiry version is held, and every authorisation is refused
// cis_outdated from the first pull after the hold (decide.go checks
// Outdated before staleness), and still cis_outdated past cis_stale_s,
// while Evaluator.Age goes stale. With the expiry installed the same
// request is authorised, at once and past the bound (the CISP's
// confirmations keep it current).
func TestDecideCISExpiry(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		g := newCISExpiryRig(t)
		if d := g.decide(t); d.Decision != DecisionAuthorised {
			t.Fatalf("before the expiry: %s %v", d.Decision, reasons(d))
		}
		g.fake.PublishExpiry("restrictions", expiryRecord(true), restrictionFeature("B", testNow.Add(48*time.Hour)))
		g.reconcile(t)
		if out := strings.Join(g.cache.Outdated(), "; "); !strings.Contains(out, "restrictions version 2 held, not used") ||
			!strings.Contains(out, "names no feature_id") {
			t.Fatalf("outdated: %q", out)
		}
		if d := g.decide(t); d.Decision != DecisionRejected || !slices.Equal(reasons(d), []string{ReasonCISOutdated}) {
			t.Fatalf("held: %s %v", d.Decision, reasons(d))
		}
		if _, _, stale := g.eval.Age(); stale {
			t.Fatal("stale at once")
		}
		g.clk.advance((expiryStaleS + 1) * time.Second)
		g.reconcile(t)
		if _, age, stale := g.eval.Age(); !stale {
			t.Fatalf("not stale past the bound: age %.0f s", age)
		}
		if d := g.decide(t); d.Decision != DecisionRejected || !slices.Equal(reasons(d), []string{ReasonCISOutdated}) {
			t.Fatalf("held past the bound: %s %v", d.Decision, reasons(d))
		}
	})
	t.Run("installed", func(t *testing.T) {
		g := newCISExpiryRig(t)
		g.fake.PublishExpiry("restrictions", expiryRecord(false), restrictionFeature("B", testNow.Add(48*time.Hour)))
		g.reconcile(t)
		if v := g.eval.Snapshot().Version(cis.Restrictions); v == nil || v.Number != 2 || v.Provenance != cis.ProvenanceCISPExpiry {
			t.Fatalf("not installed: %+v", v)
		}
		if d := g.decide(t); d.Decision != DecisionAuthorised {
			t.Fatalf("installed: %s %v", d.Decision, reasons(d))
		}
		g.clk.advance((expiryStaleS + 1) * time.Second)
		g.reconcile(t)
		if _, age, stale := g.eval.Age(); stale {
			t.Fatalf("stale past the bound with the CISP confirming: age %.0f s", age)
		}
		if d := g.decide(t); d.Decision != DecisionAuthorised {
			t.Fatalf("installed, past the bound: %s %v", d.Decision, reasons(d))
		}
	})
}

// The staleness branch on its own: nothing held, the CISP unreachable
// past cis_stale_s, refuses cis_stale; its presence pair is the same
// cache answering, authorised.
func TestDecideCISStaleWhenTheCISPIsUnreachable(t *testing.T) {
	g := newCISExpiryRig(t)
	g.clk.advance((expiryStaleS - 1) * time.Second)
	g.reconcile(t)
	if d := g.decide(t); d.Decision != DecisionAuthorised {
		t.Fatalf("the CISP answering: %s %v", d.Decision, reasons(d))
	}
	g.fake.Down()
	g.clk.advance((expiryStaleS + 1) * time.Second)
	g.reconcile(t)
	if out := g.cache.Outdated(); len(out) != 0 {
		t.Fatalf("outdated: %v", out)
	}
	if d := g.decide(t); d.Decision != DecisionRejected || !slices.Equal(reasons(d), []string{ReasonCISStale}) {
		t.Fatalf("the CISP unreachable past the bound: %s %v", d.Decision, reasons(d))
	}
}
