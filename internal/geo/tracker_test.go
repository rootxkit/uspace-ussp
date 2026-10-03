package geo

import (
	"slices"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

const (
	westOut = 44.79  // west of the zone
	inside  = 44.825 // inside it
	eastOut = 44.86  // east of it
)

// SC-03 in unit form: a track through a REQ_AUTHORIZATION zone raises a
// warning on entry, refreshes it while inside, and clears resolved
// after the exit plus the 3 s hysteresis; never before (E-01 pair).
func TestSC03ReqZoneRaisesOnEntryAndClearsAfterExitPlusHysteresis(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	g := newRig(t, setOf(t, "zones:1", at, map[string][]feat{"zones": {amslZone("TZR001", "REQ_AUTHORIZATION")}}))
	if evs := g.sample(westOut, 300); len(evs) != 0 {
		t.Fatalf("outside: %s", states(evs))
	}
	evs := g.sample(inside, 300)
	if len(evs) != 1 || evs[0].State != StateRaised || evs[0].Alert.Kind != KindZoneIncursion || evs[0].Alert.Severity != core.SeverityWarning {
		t.Fatalf("entry: %s", states(evs))
	}
	a := evs[0].Alert
	if a.Detail["zone_id"] != "TZR001" || a.Detail["zone_type"] != "REQ_AUTHORIZATION" || a.Detail["vertical_known"] != true ||
		a.Detail["limit_not_judged"] != false || a.FlightID != g.flight || a.IntentID != g.ref.IntentID || a.PolicyVersion != 7 || a.Cell5 != g.ref.Cell5 {
		t.Fatalf("alert %+v", a)
	}
	// Inside, core refreshes it silently; the republish of each tick is
	// the worker's (Active).
	for range 3 {
		if evs := g.sample(inside, 300); len(evs) != 0 {
			t.Fatalf("inside: %s", states(evs))
		}
		if act := g.tr.Active(); len(act) != 1 || act[0].ID != a.ID {
			t.Fatalf("active %+v", act)
		}
	}
	// Out: shown false at once, cleared only once more than 3 s passed
	// since it was last inside.
	exitS := g.wallS()
	var cleared *Event
	for range 6 {
		for _, e := range g.sample(eastOut, 300) {
			if e.State == StateCleared {
				cleared = &e
			}
		}
		if cleared != nil {
			break
		}
	}
	if cleared == nil || cleared.ClearReason != "resolved" || cleared.Alert.ID != a.ID {
		t.Fatalf("no resolved clear: %+v", cleared)
	}
	if d := g.wallS() - exitS; d <= 3 || d > 4 {
		t.Fatalf("cleared %.0f s after the exit, want 4 (hysteresis 3 s)", d)
	}
	if len(g.tr.Active()) != 0 {
		t.Fatalf("active after the clear: %v", g.tr.Active())
	}
}

// A PROHIBITED zone raises critical; a CONDITIONAL one the policy's
// severity, never above it (Z-10); a U-space airspace is information and
// never an incursion (it is not in the set).
func TestSeverityPerTypeAndUSpaceIsNotAnIncursion(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		typ, dataset string
		sev          core.Severity
		cond         string
	}{
		{"PROHIBITED", "zones", core.SeverityCritical, ""},
		{"PROHIBITED", "restrictions", core.SeverityCritical, ""},
		{"CONDITIONAL", "zones", core.SeverityWarning, "warning"},
		{"CONDITIONAL", "zones", core.SeverityInfo, "info"},
	} {
		g := newRig(t, setOf(t, "v", at, map[string][]feat{c.dataset: {amslZone("Z1", c.typ)}}))
		if c.cond != "" {
			g.pol.Values.ZoneConditionalSeverity = c.cond
			g.pol.Version++
			g.tr.Configure(g.tr.Set(), g.pol, g.clk)
		}
		evs := g.sample(inside, 300)
		if len(evs) != 1 || evs[0].Alert.Severity != c.sev || evs[0].Alert.Detail["dataset"] != c.dataset {
			t.Errorf("%s/%s: %s %+v", c.dataset, c.typ, states(evs), evs)
		}
	}
	s := BuildZoneSet(projection("v", at, map[string][]feat{"uspace_airspace": {amslZone("U1", "USPACE")}, "zones": {amslZone("N1", "NO_RESTRICTION")}}), nil, at)
	if len(s.Zones) != 0 {
		t.Fatalf("USPACE or NO_RESTRICTION held: %d zones", len(s.Zones))
	}
	g := newRig(t, s)
	for range 5 {
		if evs := g.sample(inside, 300); len(evs) != 0 {
			t.Fatalf("U-space airspace raised %s", states(evs))
		}
	}
}

// SC-12 run 2: a zone whose window excludes today raises nothing for 20
// samples inside it; the same zone on a day its window holds raises at
// once (E-01 pair).
func TestSC12ZoneWindowExcludingTodayRaisesNothing(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	nextWeek := amslZone("TZW001", "PROHIBITED")
	nextWeek.applicability = []any{map[string]any{"startDateTime": "2026-10-10T00:00:00Z", "endDateTime": "2026-10-11T00:00:00Z"}}
	g := newRig(t, setOf(t, "v", at, map[string][]feat{"zones": {nextWeek}}))
	for i := range 20 {
		if evs := g.sample(inside, 300); len(evs) != 0 {
			t.Fatalf("sample %d raised %s", i, states(evs))
		}
	}
	today := amslZone("TZW002", "PROHIBITED")
	today.applicability = []any{map[string]any{"startDateTime": "2026-10-03T00:00:00Z", "endDateTime": "2026-10-04T00:00:00Z"}}
	g = newRig(t, setOf(t, "v", at, map[string][]feat{"zones": {today}}))
	if evs := g.sample(inside, 300); len(evs) != 1 || evs[0].State != StateRaised {
		t.Fatalf("today's window: %s", states(evs))
	}
}

// SC-13: an AGL zone with no terrain warns with limit_not_judged true
// and not_judged ["AGL"] (a PROHIBITED zone is never silent where it
// cannot be judged), while a CONDITIONAL one is not evaluated: nothing
// raised, counted by core.
func TestSC13AGLZoneWithoutTerrain(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	g := newRig(t, setOf(t, "v", at, map[string][]feat{"zones": {aglZone("TZA001", "PROHIBITED")}}))
	evs := g.sample(inside, 300)
	if len(evs) != 1 || evs[0].Alert.Severity != core.SeverityWarning {
		t.Fatalf("PROHIBITED AGL without terrain: %s", states(evs))
	}
	d := evs[0].Alert.Detail
	nj, _ := d["not_judged"].([]string)
	if d["limit_not_judged"] != true || d["vertical_known"] != false || !slices.Equal(nj, []string{"AGL"}) {
		t.Fatalf("detail %+v", d)
	}
	g = newRig(t, setOf(t, "v", at, map[string][]feat{"zones": {aglZone("TZA002", "CONDITIONAL")}}))
	for range 5 {
		if evs := g.sample(inside, 300); len(evs) != 0 {
			t.Fatalf("CONDITIONAL AGL without terrain raised %s", states(evs))
		}
	}
	if n := g.tr.mon.Counters().Get("zone_checks_not_evaluated"); n != 5 {
		t.Fatalf("not counted: %d", n)
	}
}

// A new zone set rebuilds core's monitor: an active alert is carried
// under its id and continues, without a second raise, when core judges
// it true again; a zone the new set no longer holds ends its alert at
// once, not_reconfirmed (E-01 pair).
func TestRebuildCarriesAndWithdrawn(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	z := amslZone("TZP001", "PROHIBITED")
	g := newRig(t, setOf(t, "zones:1", at, map[string][]feat{"zones": {z}}))
	evs := g.sample(inside, 300)
	id := evs[0].Alert.ID
	if evs := g.tr.Configure(setOf(t, "zones:2", at.Add(time.Minute), map[string][]feat{"zones": {z, amslZone("TZP002", "CONDITIONAL")}}), g.pol, g.clk); len(evs) != 0 {
		t.Fatalf("rebuild with the zone kept: %s", states(evs))
	}
	if a := g.tr.Active(); len(a) != 1 || a[0].CarriedSince == nil || a[0].ID != id {
		t.Fatalf("not carried: %+v", a)
	}
	evs = g.sample(inside, 300)
	var ours []Event
	for _, e := range evs {
		if e.Alert.Detail["zone_id"] == "TZP001" {
			ours = append(ours, e)
		}
	}
	if len(ours) != 1 || ours[0].State != StateUpdated || ours[0].Alert.ID != id || ours[0].Alert.CarriedSince != nil {
		t.Fatalf("continued: %s", states(ours))
	}
	// The zone is withdrawn from the CIS: the alert ends at once.
	evs = g.tr.Configure(setOf(t, "zones:3", at.Add(2*time.Minute), map[string][]feat{"zones": {amslZone("TZP002", "CONDITIONAL")}}), g.pol, g.clk)
	var ended *Event
	for i := range evs {
		if evs[i].Alert.ID == id {
			ended = &evs[i]
		}
	}
	if ended == nil || ended.ClearReason != ClearNotReconfirmed || ended.ClearingDetail["reason"] != "zone_no_longer_published" {
		t.Fatalf("withdrawn zone: %s", states(evs))
	}
	// A set that is not loaded withdraws nothing (SC-22).
	g2 := newRig(t, setOf(t, "zones:1", at, map[string][]feat{"zones": {z}}))
	g2.sample(inside, 300)
	if evs := g2.tr.Configure(&ZoneSet{}, g2.pol, g2.clk); len(evs) != 0 || len(g2.tr.Active()) != 1 {
		t.Fatalf("an unloaded set cleared %s", states(evs))
	}
}

// A carried alert core does not judge true again ends not_reconfirmed
// only after the hysteresis of admitted flying samples; a rejected
// sample is no evidence (only admitted samples count).
func TestCarriedEndsOnlyOnAdmittedEvidence(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	s := setOf(t, "zones:1", at, map[string][]feat{"zones": {amslZone("TZP001", "PROHIBITED")}})
	g := newRig(t, s)
	id := g.sample(inside, 300)[0].Alert.ID
	// A policy change rebuilds: carried.
	g.pol.Version++
	g.tr.Configure(s, g.pol, g.clk)
	// Late samples (received 60 s ago) are rejected: no evidence.
	for range 6 {
		g.clk = g.clk.Add(time.Second)
		tr := g.track(eastOut, 300, true)
		tr.RxAtS -= 60
		tr.CapturedAtS -= 60
		if evs := g.tr.Observe(tr, g.ref, g.wallS(), g.clk); len(evs) != 0 {
			t.Fatalf("a rejected sample ended it: %s", states(evs))
		}
	}
	var ended *Event
	for range 6 {
		for _, e := range g.sample(eastOut, 300) {
			if e.Alert.ID == id && e.State == StateCleared {
				ended = &e
			}
		}
	}
	if ended == nil || ended.ClearReason != ClearNotReconfirmed || ended.ClearingDetail["reason"] != "not_judged_true_again" {
		t.Fatalf("not ended on evidence: %+v", ended)
	}
}

// A restart or a handover: another tracker (another instance) restores
// the saved alerts and continues them under their ids; a landing ends
// one (landed), silence for the stale time another (stale); Forget
// ends nothing (E-01 pairs).
func TestRestoreOnAnotherInstanceContinuesLandsAndGoesStale(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	s := setOf(t, "zones:1", at, map[string][]feat{"zones": {amslZone("TZP001", "PROHIBITED")}})
	a := newRig(t, s)
	id := a.sample(inside, 300)[0].Alert.ID
	saved := a.tr.Saved(a.flight)
	if len(saved) != 1 || saved[0].ID != id {
		t.Fatalf("saved %+v", saved)
	}
	// The old owner lets it go without a clear.
	a.tr.Forget(a.flight, a.wallS())
	if len(a.tr.Active()) != 0 {
		t.Fatal("forget kept alerts")
	}
	b := newRig(t, s)
	b.clk = a.clk
	b.tr.Restore(b.flight, saved, b.clk)
	evs := b.sample(inside, 300)
	if len(evs) != 1 || evs[0].State != StateUpdated || evs[0].Alert.ID != id {
		t.Fatalf("continued on the new instance: %s", states(evs))
	}
	// Landed.
	b.clk = b.clk.Add(time.Second)
	evs = b.tr.Observe(b.track(inside, 300, false), b.ref, b.wallS(), b.clk)
	if len(evs) != 1 || evs[0].ClearReason != "landed" {
		t.Fatalf("landed: %s", states(evs))
	}
	// A restored alert whose flight is never heard again goes stale.
	c := newRig(t, s)
	c.clk = a.clk
	c.tr.Restore(c.flight, saved, c.clk)
	for i := range 20 {
		c.clk = c.clk.Add(time.Second)
		evs := c.tr.Tick(c.wallS(), c.clk)
		if len(evs) > 0 {
			if evs[0].ClearReason != "stale" || i < 14 {
				t.Fatalf("after %d s: %s", i+1, states(evs))
			}
			return
		}
	}
	t.Fatal("never stale")
}

// Drop ends every alert of the flight with the reason; Saved is bounded
// (E-10).
func TestDropAndSavedBound(t *testing.T) {
	at := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	var fs []feat
	for i := range MaxAlertsPerFlight + 2 {
		fs = append(fs, amslZone("TP"+string(rune('A'+i%26))+string(rune('A'+i/26)), "PROHIBITED"))
	}
	g := newRig(t, setOf(t, "v", at, map[string][]feat{"zones": fs}))
	if evs := g.sample(inside, 300); len(evs) != MaxAlertsPerFlight+2 {
		t.Fatalf("%d raised", len(evs))
	}
	if s := g.tr.Saved(g.flight); len(s) != MaxAlertsPerFlight || g.tr.Counters.Get(CounterSavedOverBound) != 2 {
		t.Fatalf("saved %d", len(s))
	}
	evs := g.tr.Drop(g.flight, "flight_ended", g.wallS(), g.clk)
	if len(evs) != MaxAlertsPerFlight+2 || evs[0].ClearReason != "flight_ended" || len(g.tr.Active()) != 0 {
		t.Fatalf("drop: %d %s", len(evs), evs[0].ClearReason)
	}
}

// Without a zone set nothing is judged, counted.
func TestObserveWithoutConfigure(t *testing.T) {
	tr := NewTracker(nil)
	g := &rig{t: t, tr: tr, clk: time.Now(), flight: "f"}
	if evs := tr.Observe(g.track(inside, 300, true), Ref{FlightID: "f"}, g.wallS(), g.clk); evs != nil || tr.Counters.Get(CounterNotJudgedNoZones) != 1 {
		t.Fatal("judged without zones")
	}
}
