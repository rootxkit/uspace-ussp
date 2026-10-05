package cis

// A restriction of the ANSP is in force only while it is active (spec
// 02 F2, 03 §4: states planned, active, ended, cancelled; ATS.TR.237(b)
// activation and deactivation). A planned restriction, even one whose
// starts_at has come, is not: the ANSP activates it (at once, or by its
// own ticker at starts_at, which publishes a new version in state
// active), and the CISP leaves a planned restriction past its starts_at
// planned (cross-plan cisp Q2). Ended and cancelled ones are over.

// RestrictionStateInForce reports whether a restriction in state is
// enforced: false for planned, ended and cancelled, true for active and
// for any state this USSP does not know (enforced, as a zone type it
// does not know is, fail-safe).
func RestrictionStateInForce(state string) bool {
	switch state {
	case "planned", "ended", "cancelled":
		return false
	}
	return true
}

// InForce reports whether the entry is enforced: every feature of the
// zones and uspace_airspace datasets, and a restriction whose
// cis_restriction state is in force (RestrictionStateInForce). A
// restriction without a readable cis_restriction is enforced.
func (e *Entry) InForce() bool {
	if e == nil || e.Restriction == nil {
		return true
	}
	return RestrictionStateInForce(string(e.Restriction.State))
}

// Planned reports whether the entry is a restriction the ANSP has
// planned and not yet activated.
func (e *Entry) Planned() bool {
	return e != nil && e.Restriction != nil && string(e.Restriction.State) == "planned"
}

// Lift is what the CIS says of a restriction an alert names (WP-12):
// whether the alert that it is in force may clear.
type Lift int

// The answers. Only LiftEnded clears an alert: an absent restriction is
// not an ended one. The CISP takes an ended or cancelled restriction
// out of the current set and records the end on its head; a restriction
// that left the set while its head says neither is a CIS inconsistency
// (a lost feature, a regression in the CISP), and an alert it raised
// stays raised.
const (
	// LiftUnjudged: the CIS cannot say (stale, never loaded, or the
	// CISP did not answer for a restriction gone from the set).
	LiftUnjudged Lift = iota
	// LiftInForce: in the current set and in force.
	LiftInForce
	// LiftEnded: no longer in force: in the current set ended,
	// cancelled or planned, or gone from it with the CISP holding its
	// head ended or cancelled.
	LiftEnded
	// LiftAbsent: not in the current set, why not known yet (the
	// Evaluator's answer; Cache.RestrictionLift asks the CISP).
	LiftAbsent
	// LiftGone: not in the current set while the CISP holds its head
	// neither ended nor cancelled, or holds none (cis_inconsistency).
	LiftGone
)

// String is the answer's name in logs and details.
func (l Lift) String() string {
	switch l {
	case LiftInForce:
		return "in_force"
	case LiftEnded:
		return "ended"
	case LiftAbsent:
		return "absent"
	case LiftGone:
		return "gone"
	case LiftUnjudged:
	}
	return "unjudged"
}

// RestrictionLift says whether the restriction id is in force in the CIS
// as this cache holds it: LiftEnded when it is ended, cancelled or
// planned, LiftInForce when it is in force, LiftAbsent when the
// restrictions dataset does not hold it (the CISP's current set holds
// planned and active restrictions only, so absent is either an end or an
// inconsistency: Cache.RestrictionLift tells them apart). LiftUnjudged
// while the cache is stale or a dataset was never loaded: a stale CIS
// lifts nothing.
func (e *Evaluator) RestrictionLift(id string) Lift {
	if _, _, stale := e.Age(); stale {
		return LiftUnjudged
	}
	for _, en := range e.Snapshot().Entries(Restrictions) {
		if en.Identifier == id {
			if en.InForce() {
				return LiftInForce
			}
			return LiftEnded
		}
	}
	return LiftAbsent
}
