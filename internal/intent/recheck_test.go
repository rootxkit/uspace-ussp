package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/cis"
)

// ActiveIn is the store's prefilter in memory: every active intent (the
// exact check is Recheck's).
func (m *memStore) ActiveIn(_ context.Context, _ []geodesy.BBox, _, _ *time.Time, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, r := range m.byID {
		if slices.Contains(ActiveStates, r.LocalState) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	if len(out) > limit+1 {
		out = out[:limit+1]
	}
	return out, nil
}

// restriction is an active ANSP restriction over the base request's
// volume (41.70-41.71, 44.80-44.81), 0-1000 m AMSL, from t0 to t1.
func restriction(t *testing.T, id, state string) feature {
	t.Helper()
	from, to := t0, t1
	return feature{Dataset: "restrictions", ID: id, Type: "PROHIBITED", RestrictionState: state,
		Polygon:  [][2]float64{{41.699, 44.799}, {41.699, 44.812}, {41.712, 44.812}, {41.712, 44.799}},
		LowerRaw: &limit{ValueM: 0, Ref: "AMSL"}, UpperRaw: &limit{ValueM: 1000, Ref: "AMSL"}, From: &from, To: &to}
}

// The standing re-check (brief WP-12 done-when): an accepted intent a
// new restriction overlaps is withdrawn with the change_reason, a new
// version, the conflict and the notice the projection turns into
// restriction_activated; an intent 5 km away is untouched (E-01 twin);
// a second re-check tells nothing twice; the restriction ending later
// leaves the withdrawn intent withdrawn.
func TestRecheckWithdrawsAnAcceptedIntentAndLeavesAFarOne(t *testing.T) {
	g := newRig()
	s, st, pr := newService(g)
	near, _, err := submit(t, s, baseRequest())
	if err != nil || near.State != StateAccepted {
		t.Fatalf("%v %+v", err, near)
	}
	far5km := wireVolumeJSON(squareWire(41.75, 44.80, 0.01), 500, 550, t0, t1)
	far, _, err := submit(t, s, with(baseRequest(), "client_ref", "far", "volumes", []any{far5km}))
	if err != nil || far.State != StateAccepted {
		t.Fatalf("%v %+v", err, far)
	}
	g.cis.features = append(g.cis.features, restriction(t, "TRS001", "active").candidate(t))
	rs, err := s.RecheckAll(t.Context(), nil, nil, nil, Cause{Kind: CauseRestriction, CISVersion: "restrictions:2"})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]RecheckResult{}
	for _, r := range rs {
		by[r.IntentID] = r
	}
	if by[far.IntentID].Outcome != RecheckUntouched {
		t.Fatalf("the intent 5 km away: %+v", by[far.IntentID])
	}
	if v := st.byID[far.IntentID]; v.Version != 1 || v.LocalState != StateAccepted {
		t.Fatalf("far intent written: v%d %s", v.Version, v.LocalState)
	}
	got := by[near.IntentID]
	if got.Outcome != RecheckWithdrawn || got.Notice == nil {
		t.Fatalf("near: %+v", got)
	}
	r := st.byID[near.IntentID]
	if r.LocalState != StateWithdrawn || r.Version != 2 || r.Decision.State != StateWithdrawn || r.Decision.ChangeReason == nil ||
		*r.Decision.ChangeReason != "restriction TRS001" || r.Decision.DSSState != nil || r.Actor != "system" {
		t.Fatalf("withdrawn record %+v", r.Decision)
	}
	if !slices.ContainsFunc(r.Decision.Conflicts, func(c Conflict) bool { return c.Reason == ReasonRestrictionActive && c.Ref == "TRS001" }) {
		t.Fatalf("conflicts %+v", r.Decision.Conflicts)
	}
	n := NoticeOf(r)
	if n == nil || n.Decision != RecheckWithdrawn || !n.Withdrawn || n.AuthorisationUpdated || n.RestrictionID != "TRS001" ||
		n.Version != 2 || n.PreviousState != StateAccepted || !validUUID(n.AlertID) || n.Window == nil ||
		n.Window.StartsAt == nil || !slices.Equal(n.AffectedIntents, []string{near.IntentID}) {
		t.Fatalf("notice %+v", n)
	}
	if _, active := pr.kv[near.IntentID]; active || pr.last() != "intent.v1.withdrawn."+near.IntentID {
		t.Fatalf("projection: %v %s", active, pr.last())
	}
	// Again: nothing twice (the intent is no longer active).
	if r, err := s.Recheck(t.Context(), near.IntentID, Cause{Kind: CauseRestriction}); err != nil || r.Outcome != RecheckNotActive {
		t.Fatalf("again: %+v %v", r, err)
	}
	// The restriction ends: nothing is re-authorised; the operator files
	// anew.
	g.cis.features = []cis.ZoneCandidate{restriction(t, "TRS001", "ended").candidate(t)}
	if _, err := s.RecheckAll(t.Context(), nil, nil, nil, Cause{Kind: CauseRestriction}); err != nil {
		t.Fatal(err)
	}
	if r := st.byID[near.IntentID]; r.LocalState != StateWithdrawn || r.Version != 2 {
		t.Fatalf("after the end: %s v%d", r.LocalState, r.Version)
	}
	if r := st.byID[far.IntentID]; r.Version != 1 {
		t.Fatalf("far intent written after the end: v%d", r.Version)
	}
}

// An activated intent keeps its state (the aircraft may be flying;
// conformance keeps judging it) and is marked: withdrawn true with the
// restriction's window, a new version with the change_reason, and its
// activation would be refused; a second re-check of the same
// restriction tells nothing twice.
func TestRecheckMarksAnActivatedIntent(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	st.now = t0.Add(-5 * time.Minute)
	if a, err := patch(t, s, d.IntentID, map[string]any{"action": "activate"}); err != nil || a.State != StateActivated {
		t.Fatalf("activate %v %+v", err, a)
	}
	g.cis.features = append(g.cis.features, restriction(t, "TRS002", "active").candidate(t))
	r, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseRestriction})
	if err != nil || r.Outcome != RecheckMarked {
		t.Fatalf("%+v %v", r, err)
	}
	rec := st.byID[d.IntentID]
	n := NoticeOf(rec)
	if rec.LocalState != StateActivated || rec.Version != 3 || n == nil || !n.Withdrawn || n.Decision != RecheckMarked ||
		n.IntentState != StateActivated || n.Window == nil || n.Window.EndsAt == nil || !n.Window.EndsAt.Equal(t1) {
		t.Fatalf("marked %s v%d %+v", rec.LocalState, rec.Version, n)
	}
	again, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseRestriction})
	if err != nil || again.Outcome != RecheckAlreadyMarked || st.byID[d.IntentID].Version != 3 {
		t.Fatalf("again %+v %v v%d", again, err, st.byID[d.IntentID].Version)
	}
}

// A planned restriction is not in force (spec 02 F2): over an activated
// intent it marks nothing and tells nothing, even with its starts_at
// come; the same restriction activated (a new version, state active)
// marks the intent and tells it once (the lab's ussp-wp12-restriction,
// where the plan step raised restriction_activated before the activate
// step).
func TestRecheckIgnoresAPlannedRestrictionUntilItIsActivated(t *testing.T) {
	g := newRig()
	s, st, pr := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	st.now = t0.Add(-5 * time.Minute)
	if a, err := patch(t, s, d.IntentID, map[string]any{"action": "activate"}); err != nil || a.State != StateActivated {
		t.Fatalf("activate %v %+v", err, a)
	}
	published := len(pr.subjects)
	g.cis.features = []cis.ZoneCandidate{restriction(t, "TRSPLAN", "planned").candidate(t)}
	r, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseRestriction})
	if err != nil || r.Outcome != RecheckUntouched || r.Notice != nil {
		t.Fatalf("planned: %+v %v", r, err)
	}
	if rec := st.byID[d.IntentID]; rec.Version != 2 || NoticeOf(rec) != nil || len(pr.subjects) != published {
		t.Fatalf("planned wrote: v%d %+v %v", rec.Version, NoticeOf(rec), pr.subjects[published:])
	}
	g.cis.features = []cis.ZoneCandidate{restriction(t, "TRSPLAN", "active").candidate(t)}
	r, err = s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseRestriction})
	if err != nil || r.Outcome != RecheckMarked || r.Notice == nil || r.Notice.RestrictionID != "TRSPLAN" || !r.Notice.Withdrawn {
		t.Fatalf("activated: %+v %v", r, err)
	}
	if rec := st.byID[d.IntentID]; rec.Version != 3 || rec.LocalState != StateActivated || NoticeOf(rec) == nil {
		t.Fatalf("activated record v%d %s", rec.Version, rec.LocalState)
	}
}

// A planned restriction refuses no new intent: it is told as the
// condition restriction_planned and the intent is authorised; the same
// restriction active refuses it (restriction_active).
func TestDecisionTellsAPlannedRestrictionAndRefusesAnActiveOne(t *testing.T) {
	g := newRig()
	s, _, _ := newService(g)
	g.cis.features = []cis.ZoneCandidate{restriction(t, "TRSPLAN", "planned").candidate(t)}
	d, _, err := submit(t, s, baseRequest())
	if err != nil || d.State != StateAccepted || len(d.Conflicts) != 0 ||
		!slices.ContainsFunc(d.Conditions, func(c Condition) bool { return c.Code == CondRestrictionPlanned && c.Ref == "TRSPLAN" }) {
		t.Fatalf("planned: %v %+v", err, d)
	}
	g.cis.features = []cis.ZoneCandidate{restriction(t, "TRSPLAN", "active").candidate(t)}
	d, _, err = submit(t, s, with(baseRequest(), "client_ref", "second"))
	if err != nil || d.State != StateRejected ||
		!slices.ContainsFunc(d.Conflicts, func(c Conflict) bool { return c.Reason == ReasonRestrictionActive && c.Ref == "TRSPLAN" }) ||
		slices.ContainsFunc(d.Conditions, func(c Condition) bool { return c.Code == CondRestrictionPlanned }) {
		t.Fatalf("active: %v %+v", err, d)
	}
}

// A stale CIS never withdraws: the re-check is not judged and owed, and
// nothing is written; a current one withdraws (E-01 pair).
func TestRecheckOnAStaleCISChangesNothing(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	g.cis.features = append(g.cis.features, restriction(t, "TRS003", "active").candidate(t))
	g.cis.basis.Stale = true
	r, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseRestriction})
	if err != nil || r.Outcome != RecheckNotJudged || st.byID[d.IntentID].Version != 1 || g.counters.Get("intent_recheck_not_judged") != 1 {
		t.Fatalf("stale: %+v %v v%d", r, err, st.byID[d.IntentID].Version)
	}
	g.cis.basis.Stale = false
	if r, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseRestriction}); err != nil || r.Outcome != RecheckWithdrawn {
		t.Fatalf("current: %+v %v", r, err)
	}
}

// A new PROHIBITED zone withdraws as a restriction does (PLAN §15.1
// Q20), and a U-space airspace published since the authorisation over
// an intent authorised outside one withdraws it too: the DSS
// deconfliction and the Art. 3(4) requirements it now needs were never
// applied.
func TestRecheckOnZonesAndUSpaceAirspace(t *testing.T) {
	g := newRig()
	s, _, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	zone := restriction(t, "TZP009", "")
	zone.Dataset = "zones"
	g.cis.features = []cis.ZoneCandidate{zone.candidate(t)}
	r, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseZone})
	if err != nil || r.Outcome != RecheckWithdrawn || r.Notice.Cause != CauseZone || r.Notice.ChangeReason != "zone TZP009" {
		t.Fatalf("zone: %+v %v", r, err)
	}
	g.cis.features = nil
	d2, _, err := submit(t, s, with(baseRequest(), "client_ref", "second"))
	if err != nil || d2.InUSpaceAirspace {
		t.Fatalf("%v %+v", err, d2)
	}
	in := feature{Dataset: "uspace_airspace", ID: "TSA", Type: "USPACE", Polygon: [][2]float64{{41, 44}, {41, 46}, {42, 46}, {42, 44}}}
	g.cis.features = []cis.ZoneCandidate{in.candidate(t)}
	r, err = s.Recheck(t.Context(), d2.IntentID, Cause{Kind: CauseUSpaceAirspace})
	if err != nil || r.Outcome != RecheckWithdrawn || r.Notice.Reason != ReasonAirspaceEntered || r.Notice.Ref != "TSA" {
		t.Fatalf("airspace: %+v %v", r, err)
	}
}

// A displaced authorisation (WP-7 step 5) is re-checked with the cause
// "priority <id>" and no CIS judgement.
func TestRecheckPriorityCause(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	by := "00000000-0000-4000-8000-0000000000ff"
	r, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CausePriority, Ref: by})
	if err != nil || r.Outcome != RecheckWithdrawn || r.Notice.ByIntentID != by || *st.byID[d.IntentID].Decision.ChangeReason != "priority "+by {
		t.Fatalf("%+v %v", r, err)
	}
}

// The dedupe is on the whole conflict set: a second restriction over a
// marked intent while the first still applies is a new notice, named
// after the new restriction and carrying both conflicts; the same set
// again tells nothing twice, and the first restriction ending tells
// nothing either (E-01 pair).
func TestRecheckTellsASecondConflictWhileTheFirstStillApplies(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	st.now = t0.Add(-5 * time.Minute)
	if _, err := patch(t, s, d.IntentID, map[string]any{"action": "activate"}); err != nil {
		t.Fatal(err)
	}
	a := restriction(t, "TRSA", "active").candidate(t)
	g.cis.features = append(g.cis.features, a)
	if r, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseRestriction}); err != nil || r.Outcome != RecheckMarked {
		t.Fatalf("A: %+v %v", r, err)
	}
	g.cis.features = append(g.cis.features, restriction(t, "TRSB", "active").candidate(t))
	r, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseRestriction})
	if err != nil || r.Outcome != RecheckMarked || r.Notice == nil || r.Notice.RestrictionID != "TRSB" || r.Notice.ChangeReason != "restriction TRSB" {
		t.Fatalf("B while A applies: %+v %v", r, err)
	}
	if len(r.Notice.Conflicts) != 2 {
		t.Fatalf("the notice carries the whole set: %+v", r.Notice.Conflicts)
	}
	rec := st.byID[d.IntentID]
	if rec.Version != 4 || NoticeOf(rec).AlertID == "" {
		t.Fatalf("v%d", rec.Version)
	}
	if again, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseSweep}); err != nil || again.Outcome != RecheckAlreadyMarked {
		t.Fatalf("again: %+v %v", again, err)
	}
	g.cis.features = []cis.ZoneCandidate{g.cis.features[len(g.cis.features)-1]}
	if after, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseSweep}); err != nil || after.Outcome != RecheckAlreadyMarked || st.byID[d.IntentID].Version != 4 {
		t.Fatalf("A ended: %+v %v", after, err)
	}
}

// installRestrictionOnce installs a CIS version carrying restriction id
// before the next transaction only: between an assessment and its
// commit.
func installRestrictionOnce(t *testing.T, g *rig, st *memStore, id string) {
	t.Helper()
	c := restriction(t, id, "active").candidate(t)
	st.beforeTx = func() {
		st.beforeTx = nil
		g.cis.mu.Lock()
		defer g.cis.mu.Unlock()
		g.cis.features = append(g.cis.features, c)
		g.cis.basis.CISVersion = "zones:1,uspace_airspace:1,restrictions:2"
	}
}

// A CIS version installed between the assessment and the commit is not
// granted past: the version judged is compared with the cache's at the
// commit and the request is assessed again, here refused by the new
// restriction. Twin: without a new version the same request is granted.
func TestSubmitReassessesWhenTheCISChangesBeforeTheCommit(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	far := wireVolumeJSON(squareWire(41.75, 44.80, 0.01), 500, 550, t0, t1)
	if d, _, err := submit(t, s, with(baseRequest(), "client_ref", "twin", "volumes", []any{far})); err != nil || d.State != StateAccepted {
		t.Fatalf("twin: %v %+v", err, d)
	}
	installRestrictionOnce(t, g, st, "TRSRACE")
	d, _, err := submit(t, s, with(baseRequest(), "client_ref", "raced"))
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateRejected || d.CISVersionChecked == nil || *d.CISVersionChecked != "zones:1,uspace_airspace:1,restrictions:2" ||
		!slices.ContainsFunc(d.Conflicts, func(c Conflict) bool { return c.Ref == "TRSRACE" }) {
		t.Fatalf("granted past a newer CIS: %s %v %+v", d.State, d.CISVersionChecked, d.Conflicts)
	}
	if g.counters.Get("intent_cis_changed_during_decision") != 1 {
		t.Fatalf("counter %d", g.counters.Get("intent_cis_changed_during_decision"))
	}
}

// The same for a modification.
func TestModifyReassessesWhenTheCISChangesBeforeTheCommit(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil || d.State != StateAccepted {
		t.Fatalf("%v %+v", err, d)
	}
	installRestrictionOnce(t, g, st, "TRSRACE")
	vol := wireVolumeJSON(squareWire(41.70, 44.80, 0.01), 500, 560, t0, t1)
	m, err := patch(t, s, d.IntentID, map[string]any{"action": "modify", "volumes": []any{vol}})
	if err != nil {
		t.Fatal(err)
	}
	if m.State != StateRejected || !slices.ContainsFunc(m.Conflicts, func(c Conflict) bool { return c.Ref == "TRSRACE" }) {
		t.Fatalf("modified past a newer CIS: %s %+v", m.State, m.Conflicts)
	}
}

// An activation is judged against the CIS as it is now: a restriction
// the standing re-check has not reached yet refuses it, and the re-check
// it runs withdraws the authorisation with its notice. Twin: the same
// intent activates on the CIS it was granted on.
func TestActivationRecheckedAgainstTheCurrentCIS(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	ok, _, err := submit(t, s, with(baseRequest(), "client_ref", "ok"))
	if err != nil {
		t.Fatal(err)
	}
	st.now = t0.Add(-5 * time.Minute)
	if a, err := patch(t, s, ok.IntentID, map[string]any{"action": "activate"}); err != nil || a.State != StateActivated {
		t.Fatalf("twin: %v %+v", err, a)
	}
	st.now = testNow
	d, _, err := submit(t, s, with(baseRequest(), "client_ref", "late", "volumes", []any{wireVolumeJSON(squareWire(41.75, 44.80, 0.01), 500, 550, t0, t1)}))
	if err != nil || d.State != StateAccepted {
		t.Fatalf("%v %+v", err, d)
	}
	late := restriction(t, "TRSLATE", "active")
	late.Polygon = [][2]float64{{41.749, 44.799}, {41.749, 44.812}, {41.762, 44.812}, {41.762, 44.799}}
	g.cis.features = append(g.cis.features, late.candidate(t))
	st.now = t0.Add(-5 * time.Minute)
	_, err = patch(t, s, d.IntentID, map[string]any{"action": "activate"})
	var e *Error
	if !errors.As(err, &e) || e.Status != 409 || e.Slug != "authorisation_withdrawn" {
		t.Fatalf("activated over a restriction: %v", err)
	}
	r := st.byID[d.IntentID]
	if n := NoticeOf(r); r.LocalState != StateWithdrawn || n == nil || n.RestrictionID != "TRSLATE" {
		t.Fatalf("not withdrawn: %s %+v", r.LocalState, NoticeOf(r))
	}
}

// E-10: a CIS that changes before every commit is assessed MaxAssess
// times and the request is refused with 503 cis_changed; nothing is
// written.
func TestSubmitRefusedWhileTheCISKeepsChanging(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	n := 1
	st.beforeTx = func() {
		g.cis.mu.Lock()
		defer g.cis.mu.Unlock()
		n++
		g.cis.basis.CISVersion = fmt.Sprintf("zones:1,uspace_airspace:1,restrictions:%d", n)
	}
	_, _, err := submit(t, s, baseRequest())
	var e *Error
	if !errors.As(err, &e) || e.Status != 503 || e.Slug != "cis_changed" || len(st.byID) != 0 ||
		g.counters.Get("intent_cis_changed_during_decision") != MaxAssess {
		t.Fatalf("%v, %d written, counter %d", err, len(st.byID), g.counters.Get("intent_cis_changed_during_decision"))
	}
}

// A displacement is durable: the flag written in the displacing
// intent's transaction is enough. When the re-check after the commit
// never ran (a crash), the sweep finds the flag without a notice and
// withdraws the authorisation with the priority cause; an unflagged
// intent the same sweep reads is untouched (E-01 pair).
func TestSweepWithdrawsADisplacementWhoseRecheckNeverRan(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := submit(t, s, with(baseRequest(), "client_ref", "other", "volumes", []any{wireVolumeJSON(squareWire(41.75, 44.80, 0.01), 500, 550, t0, t1)}))
	if err != nil {
		t.Fatal(err)
	}
	by := "00000000-0000-4000-8000-0000000000ee"
	if err := st.InTx(t.Context(), func(ctx context.Context, tx Tx) error { return tx.FlagUpdate(ctx, []string{d.IntentID}, by, testNow) }); err != nil {
		t.Fatal(err)
	}
	rs, err := s.RecheckAll(t.Context(), nil, nil, nil, Cause{Kind: CauseSweep})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RecheckResult{}
	for _, r := range rs {
		got[r.IntentID] = r
	}
	r := got[d.IntentID]
	if r.Outcome != RecheckWithdrawn || r.Notice == nil || r.Notice.Cause != CausePriority || r.Notice.ByIntentID != by ||
		st.byID[d.IntentID].LocalState != StateWithdrawn {
		t.Fatalf("displaced: %+v", r)
	}
	if got[other.IntentID].Outcome != RecheckUntouched {
		t.Fatalf("unflagged: %+v", got[other.IntentID])
	}
	if again, err := s.Recheck(t.Context(), d.IntentID, Cause{Kind: CauseSweep}); err != nil || again.Outcome != RecheckNotActive {
		t.Fatalf("again: %+v %v", again, err)
	}
}

// The update_required refusal names what the notice says: a restriction
// notice is not refused as a later intent with precedence. Twin: the
// WP-7 flag of a displacement keeps the precedence text.
func TestActivationRefusalMatchesTheNotice(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	st.now = t0.Add(-5 * time.Minute)
	refusal := func() *Error {
		t.Helper()
		_, err := patch(t, s, d.IntentID, map[string]any{"action": "activate"})
		var e *Error
		if !errors.As(err, &e) || e.Slug != "update_required" {
			t.Fatalf("not refused update_required: %v", err)
		}
		return e
	}
	n := Notice{Cause: CauseRestriction, Ref: "TRS042", RestrictionID: "TRS042", Reason: ReasonRestrictionActive, Decision: RecheckMarked,
		Withdrawn: true, AlertID: "4d6f0f7e-8d7c-4c1a-9e2b-3a4b5c6d7e82", Conflicts: []Conflict{}}
	raw, _ := json.Marshal(n)
	st.byID[d.IntentID].UpdateRequired = raw
	if e := refusal(); strings.Contains(e.Detail, "precedence") || !strings.Contains(e.Detail, "restriction TRS042") {
		t.Fatalf("restriction notice refused as %q", e.Detail)
	}
	st.byID[d.IntentID].UpdateRequired = json.RawMessage(`{"by_intent_id":"00000000-0000-4000-8000-0000000000ee"}`)
	if e := refusal(); !strings.Contains(e.Detail, "precedence") {
		t.Fatalf("displacement refused as %q", e.Detail)
	}
}
