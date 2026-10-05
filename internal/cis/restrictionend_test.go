package cis

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// pullAll pulls the three ED-318 datasets.
func (g *cacheRig) pullAll(t *testing.T) {
	t.Helper()
	for _, d := range ED318Datasets {
		if err := g.cache.Pull(t.Context(), d, nil, false); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
	}
}

// A restriction that leaves the current set with its head ended or
// cancelled is ended; one that leaves it while the CISP holds its head
// neither ended nor cancelled is gone, not ended: the cis_inconsistency
// alarm is counted once per version and named on /readyz until the next
// version (E-01 pair: an active one is in force, and the probe is up
// while nothing is gone). The heads are read once per version.
func TestCacheRestrictionLiftTellsGoneFromEnded(t *testing.T) {
	g := newCacheRig(t, "https://ussp.test/v1/cis/notifications")
	ctx := t.Context()
	g.fake.Publish("zones")
	g.fake.Publish("uspace_airspace")
	g.fake.Publish("restrictions", restrictionIn("DARACT1", "active").json(), restrictionIn("DAREND1", "active").json(),
		restrictionIn("DARCAN1", "planned").json(), restrictionIn("DARGONE", "active").json())
	g.pullAll(t)
	for _, id := range []string{"DARACT1", "DAREND1", "DARGONE"} {
		if l := g.cache.RestrictionLift(ctx, id); l != LiftInForce {
			t.Fatalf("%s at v1: %v", id, l)
		}
	}
	if g.fake.Requests("GET /v1/restrictions/heads") != 0 {
		t.Fatal("the heads were read for restrictions the set holds")
	}

	// v2: the ANSP ended one and cancelled one (their heads say so);
	// DARGONE left the set while its head is still active.
	g.fake.SetRestrictionHead("DARACT1", "active")
	g.fake.SetRestrictionHead("DAREND1", "ended")
	g.fake.SetRestrictionHead("DARCAN1", "cancelled")
	g.fake.SetRestrictionHead("DARGONE", "active")
	g.fake.Publish("restrictions", restrictionIn("DARACT1", "active").json())
	if err := g.cache.Pull(ctx, Restrictions, nil, false); err != nil {
		t.Fatal(err)
	}
	if st, detail := g.cache.Probe(ctx); st != obs.StateUp || strings.Contains(detail, "cis_inconsistency") {
		t.Fatalf("nothing judged gone yet: %s %s", st, detail)
	}
	want := map[string]Lift{"DARACT1": LiftInForce, "DAREND1": LiftEnded, "DARCAN1": LiftEnded, "DARGONE": LiftGone}
	for range 2 {
		for id, w := range want {
			if l := g.cache.RestrictionLift(ctx, id); l != w {
				t.Fatalf("%s at v2: %v, want %v", id, l, w)
			}
		}
	}
	if n := g.fake.Requests("GET /v1/restrictions/heads"); n != 2 {
		t.Fatalf("heads read %d times for one version, want 2 (ended, cancelled)", n)
	}
	if g.count(CounterInconsistency) != 1 {
		t.Fatalf("cis_inconsistency counted %d", g.count(CounterInconsistency))
	}
	st, detail := g.cache.Probe(ctx)
	if st != obs.StateDegraded || !strings.Contains(detail, "cis_inconsistency: restrictions version 2 no longer holds DARGONE") ||
		strings.Contains(detail, "DAREND1") {
		t.Fatalf("probe %s %s", st, detail)
	}

	// v3 brings it back: in force, and the alarm of v2 is gone.
	g.fake.Publish("restrictions", restrictionIn("DARACT1", "active").json(), restrictionIn("DARGONE", "active").json())
	if err := g.cache.Pull(ctx, Restrictions, nil, false); err != nil {
		t.Fatal(err)
	}
	if l := g.cache.RestrictionLift(ctx, "DARGONE"); l != LiftInForce {
		t.Fatalf("back at v3: %v", l)
	}
	if st, detail := g.cache.Probe(ctx); st != obs.StateUp {
		t.Fatalf("v3 probe %s %s", st, detail)
	}
}

// A CISP that does not answer for the heads, or whose list is full
// without the restriction, judges nothing (counted); no CISP judges
// nothing; MaxInconsistenciesListed bounds what /readyz names (E-10).
func TestCacheRestrictionLiftUnjudged(t *testing.T) {
	g := newCacheRig(t, "https://ussp.test/v1/cis/notifications")
	ctx := t.Context()
	g.fake.Publish("zones")
	g.fake.Publish("uspace_airspace")
	g.fake.Publish("restrictions")
	g.pullAll(t)
	g.fake.Down()
	if l := g.cache.RestrictionLift(ctx, "DARGONE"); l != LiftUnjudged || g.count(CounterHeadsFailed) != 1 || g.count(CounterInconsistency) != 0 {
		t.Fatalf("CISP down: %v, %v", l, g.cache.Counters().Snapshot())
	}
	g.fake.Up()
	for i := range MaxInconsistenciesListed + 1 {
		if l := g.cache.RestrictionLift(ctx, fmt.Sprintf("DARG%03d", i)); l != LiftGone {
			t.Fatalf("%d: %v", i, l)
		}
	}
	_, detail := g.cache.Probe(ctx)
	if g.count(CounterInconsistency) != uint64(MaxInconsistenciesListed+1) || !strings.Contains(detail, "DARG019 and 1 more while") ||
		strings.Contains(detail, "DARG020") {
		t.Fatalf("over the bound: %d %s", g.count(CounterInconsistency), detail)
	}

	for i := range MaxRestrictionHeads {
		g.fake.SetRestrictionHead(fmt.Sprintf("DARE%03d", i), "ended")
	}
	g.fake.Publish("restrictions")
	if err := g.cache.Pull(ctx, Restrictions, nil, false); err != nil {
		t.Fatal(err)
	}
	if l := g.cache.RestrictionLift(ctx, "DARE001"); l != LiftEnded {
		t.Fatalf("listed: %v", l)
	}
	if l := g.cache.RestrictionLift(ctx, "DARGONE"); l != LiftUnjudged || g.count(CounterHeadsTruncated) != 1 {
		t.Fatalf("a full list without it: %v %d", l, g.count(CounterHeadsTruncated))
	}

	clk := newClock()
	bare := NewCache(CacheConfig{Evaluator: loaded(t, clk, mustVersion(t, Zones, 1), mustVersion(t, USpaceAirspace, 1), mustVersion(t, Restrictions, 1))})
	if l := bare.RestrictionLift(ctx, "DARGONE"); l != LiftUnjudged {
		t.Fatalf("no CISP: %v", l)
	}
}
