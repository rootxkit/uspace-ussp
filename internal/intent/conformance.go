package intent

import (
	"context"
	"errors"
	"net/http"
)

// EventConformance is the audit event of a state change conformance
// monitoring made (WP-10).
const EventConformance = "intent_conformance"

// The conformance states SetConformance reads (internal/conformance's
// base states: the judgement without the link, a lost link already
// being nonconforming there).
const (
	ConformanceConforming    = "conforming"
	ConformanceNonconforming = "nonconforming"
	ConformanceContingent    = "contingent"
)

// conformanceTarget is the local state a conformance state moves an
// intent in cur to, and false when it moves nothing. The F3548 order
// holds: Activated -> Nonconforming -> Contingent, Nonconforming ->
// Activated when the flight is back; Contingent never returns.
func conformanceTarget(cur, state string) (string, bool) {
	switch {
	case state == ConformanceNonconforming && cur == StateActivated:
		return StateNonconforming, true
	case state == ConformanceContingent && (cur == StateActivated || cur == StateNonconforming):
		return StateContingent, true
	case state == ConformanceConforming && cur == StateNonconforming:
		return StateActivated, true
	}
	return "", false
}

// dssStateOf is the F3548 state of a local state the DSS knows.
var dssStateOf = map[string]string{
	StateActivated: "Activated", StateNonconforming: "Nonconforming", StateContingent: "Contingent",
}

// SetConformance moves the intent as conformance monitoring found its
// flight (monitor -> conf.v1 -> api; WP-7's state machine): activated ->
// nonconforming -> contingent, and nonconforming -> activated. Any other
// pair changes nothing and reports false (a repeat, an ended intent, an
// unknown state). The new version is audited with the reason and
// projected after the commit, so intent.v1 carries it to dss-sync
// (WP-13) and the console. Judged on the database clock.
func (s *Service) SetConformance(ctx context.Context, id, state, reason string) (bool, error) {
	if !validUUID(id) {
		return false, refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	changed := false
	err := s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		r, err := tx.Lock(ctx, id)
		if err != nil {
			return err
		}
		if r == nil {
			return ErrNotFound
		}
		to, ok := conformanceTarget(r.LocalState, state)
		if !ok {
			return nil
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		r.LocalState = to
		if r.Decision.DSSState != nil {
			d := dssStateOf[to]
			r.Decision.DSSState = &d
		}
		r.ChangeReason, r.Actor = "conformance monitoring: "+state, "system"
		if reason != "" {
			r.ChangeReason += " (" + reason + ")"
		}
		s.advance(r, now)
		if err := tx.Update(ctx, r, EventConformance); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		s.count("conformance_intent_unknown")
		return false, nil
	}
	if err != nil {
		return false, s.txError(err)
	}
	if changed {
		s.count("conformance_" + state)
		s.projectCommitted(ctx, id)
	}
	return changed, nil
}
