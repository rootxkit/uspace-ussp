package cis

import (
	"testing"
	"time"
)

// restrictionIn is a restriction feature id in state (no cis_restriction
// when state is empty).
func restrictionIn(id, state string) feat {
	r := prohibited(id)
	r.reason = []string{"DAR"}
	if state != "" {
		r.extended = map[string]any{RestrictionMember: map[string]any{"id": "r-" + id, "ansp_ref": "A-" + id, "ansp_version": 1, "state": state,
			"starts_at": "2026-10-02T08:00:00Z", "ends_at": "2026-10-02T12:00:00Z", "ended_by": nil, "uspace_airspace_id": "TSA001"}}
	}
	return r
}

// Only an active restriction is in force; planned, ended and cancelled
// are not; an unknown state and an absent cis_restriction are enforced
// (fail-safe), and so is every zone.
func TestRestrictionInForce(t *testing.T) {
	for state, want := range map[string]bool{"active": true, "planned": false, "ended": false, "cancelled": false, "suspended": true, "": true} {
		if got := RestrictionStateInForce(state); got != want {
			t.Errorf("%q: %v, want %v", state, got, want)
		}
	}
	clk := newClock()
	e := loaded(t, clk, mustVersion(t, Zones, 1, prohibited("TZP001").json()), mustVersion(t, USpaceAirspace, 1),
		mustVersion(t, Restrictions, 3, restrictionIn("DARPLN1", "planned").json(), restrictionIn("DARACT1", "active").json(),
			restrictionIn("DARNONE", "").json()))
	want := map[string][2]bool{"TZP001": {true, false}, "DARPLN1": {false, true}, "DARACT1": {true, false}, "DARNONE": {true, false}}
	for _, en := range e.Snapshot().all {
		w, ok := want[en.Identifier]
		if !ok {
			continue
		}
		if en.InForce() != w[0] || en.Planned() != w[1] {
			t.Errorf("%s: in force %v planned %v, want %v", en.Identifier, en.InForce(), en.Planned(), w)
		}
		delete(want, en.Identifier)
	}
	if len(want) != 0 {
		t.Fatalf("not built: %v", want)
	}
}

// An ended, cancelled or planned restriction is ended; an active one is
// in force (E-01 pair); one the dataset no longer holds is absent, not
// ended (the Cache asks the CISP why); a stale cache judges nothing.
func TestRestrictionLift(t *testing.T) {
	clk := newClock()
	e := loaded(t, clk, mustVersion(t, Zones, 1), mustVersion(t, USpaceAirspace, 1),
		mustVersion(t, Restrictions, 4, restrictionIn("DARACT1", "active").json(), restrictionIn("DAREND1", "ended").json(),
			restrictionIn("DARCAN1", "cancelled").json(), restrictionIn("DARPLN1", "planned").json()))
	for id, want := range map[string]Lift{"DARACT1": LiftInForce, "DAREND1": LiftEnded, "DARCAN1": LiftEnded, "DARPLN1": LiftEnded, "DARGONE": LiftAbsent} {
		if got := e.RestrictionLift(id); got != want {
			t.Errorf("%s: %v, want %v", id, got, want)
		}
	}
	clk.advance(301 * time.Second)
	if got := e.RestrictionLift("DARGONE"); got != LiftUnjudged {
		t.Fatalf("stale: %v", got)
	}
	if got := NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: clk.Now}).RestrictionLift("DARGONE"); got != LiftUnjudged {
		t.Fatalf("nothing loaded: %v", got)
	}
	for l, want := range map[Lift]string{LiftUnjudged: "unjudged", LiftInForce: "in_force", LiftEnded: "ended", LiftAbsent: "absent", LiftGone: "gone"} {
		if l.String() != want {
			t.Errorf("%d: %s", l, l)
		}
	}
}
