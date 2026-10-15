package intent

import (
	"slices"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/cis"
)

// aglRestriction is restriction() with the limits the ANSP now sends in
// an AGL U-space airspace (uspace-ansp docs/PLAN.md D3): 0-120 m AGL.
func aglRestriction(t *testing.T, id string) cis.ZoneCandidate {
	t.Helper()
	f := restriction(t, id, "active")
	f.LowerRaw, f.UpperRaw = &limit{ValueM: 0, Ref: "AGL"}, &limit{ValueM: 120, Ref: "AGL"}
	return f.candidate(t)
}

// An active AGL restriction is judged against the ground under the
// volume: over 400..450 m ground its ceiling is at most 570 m AMSL, so an
// intent at 480..530 m AMSL is refused (presence) and one at 680..730 m
// AMSL, above the ceiling over the highest ground, is accepted (the
// absence twin). Without terrain the limit is not judged and the high
// intent is refused too, saying AGL was not judged: fail closed.
func TestDecisionJudgesAnAGLRestrictionWithTheGround(t *testing.T) {
	refused := func(d Decision) bool {
		return d.State == StateRejected && slices.ContainsFunc(d.Conflicts, func(c Conflict) bool { return c.Reason == ReasonRestrictionActive && c.Ref == "TRSAGL" })
	}
	g := newRig() // ground 400..450 m AMSL; the geoid is 20 m
	s, _, _ := newService(g)
	g.cis.features = []cis.ZoneCandidate{aglRestriction(t, "TRSAGL")}
	low, _, err := submit(t, s, baseRequest()) // 500..550 m W84: 480..530 m AMSL
	if err != nil || !refused(low) {
		t.Fatalf("under the ceiling: %v %+v", err, low)
	}
	if slices.ContainsFunc(low.Conflicts, func(c Conflict) bool { return strings.Contains(c.Detail, "not judged") }) {
		t.Fatalf("judged with the ground, yet said not judged: %+v", low.Conflicts)
	}
	highVolume := wireVolumeJSON(squareWire(41.70, 44.80, 0.01), 700, 750, t0, t1)
	high, _, err := submit(t, s, with(baseRequest(), "client_ref", "high", "volumes", []any{highVolume}))
	if err != nil || refused(high) {
		t.Fatalf("above the ceiling over the highest ground: %v %+v", err, high)
	}

	g = newRig()
	g.decider.Terrain = nil
	s, _, _ = newService(g)
	g.cis.features = []cis.ZoneCandidate{aglRestriction(t, "TRSAGL")}
	blind, _, err := submit(t, s, with(baseRequest(), "client_ref", "blind", "volumes", []any{highVolume}))
	if err != nil || !refused(blind) ||
		!slices.ContainsFunc(blind.Conflicts, func(c Conflict) bool { return strings.Contains(c.Detail, "not judged: AGL") }) {
		t.Fatalf("without terrain: %v %+v", err, blind)
	}
}
