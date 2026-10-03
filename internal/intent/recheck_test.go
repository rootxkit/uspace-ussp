package intent

import (
	"context"
	"slices"
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
