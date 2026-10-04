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

// RestrictionLifted reports whether the restriction id is no longer in
// force in the CIS as this cache holds it: ended, cancelled or planned,
// or no longer in the restrictions dataset (the CISP's current set holds
// planned and active restrictions only). judged is false while the cache
// is stale or a dataset was never loaded: a stale CIS lifts nothing.
func (e *Evaluator) RestrictionLifted(id string) (lifted, judged bool) {
	if _, _, stale := e.Age(); stale {
		return false, false
	}
	for _, en := range e.Snapshot().Entries(Restrictions) {
		if en.Identifier == id {
			return !en.InForce(), true
		}
	}
	return true, true
}
