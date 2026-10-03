package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/regnum"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// The F3548 strategic coordination side of an intent (brief WP-13): which
// intents the DSS must hold, the hold that keeps an intent pending_dss
// until the DSS has taken it, and the transitions internal/dss's writer
// asks for after a DSS answer. Nothing here calls the DSS; the writer does,
// after the commit that queued the work (LESSONS: write to the DSS only
// after commit, serialised per entity).

// Event types of the DSS transitions.
const (
	// EventDSSPending: the reason an intent waits for its DSS write
	// changed.
	EventDSSPending = "intent_dss_pending"
	// EventDSSAuthorised: the DSS took the intent; it is authorised.
	EventDSSAuthorised = "intent_dss_authorised"
	// EventDSSRejected: the deconfliction against the intents and the
	// constraints the DSS named refused it.
	EventDSSRejected = "intent_dss_rejected"
)

// OutboxOIR is the dss_outbox kind of an intent's DSS work: one item per
// version of an intent the DSS must hold or holds (internal/store
// OutboxOIRPut); the writer mirrors the intent as it is when it takes the
// item, so an item of an older version is a no-op.
const OutboxOIR = "oir_put"

// DSSWorkload is the payload of an OutboxOIR item.
type DSSWorkload struct {
	IntentID string `json:"intent_id"`
	Version  int    `json:"version"`
}

// DSSHeld is what the DSS holds of an intent, from its last answer: the
// state, the OperationalIntentReference verbatim (ovn, version, manager,
// subscription), the extents written and the implicit subscription.
type DSSHeld struct {
	State          f3548.OperationalIntentState     `json:"state"`
	OVN            string                           `json:"ovn"`
	Version        int64                            `json:"version"`
	SubscriptionID string                           `json:"subscription_id"`
	Reference      f3548.OperationalIntentReference `json:"reference"`
	Extents        []f3548.Volume4D                 `json:"extents"`
	WrittenAt      *time.Time                       `json:"written_at,omitempty"`
	LastError      string                           `json:"last_error,omitempty"`
}

// OutboxSpec is one outbox item a DSS transition queues in its
// transaction (the subscribers' notifications of a write).
type OutboxSpec struct {
	Kind     string
	EntityID string
	Version  int64
	Payload  any
}

// DSSWriter writes an intent to the DSS now (internal/dss Writer): the
// request path asks for it right after the commit, so an intent the DSS
// takes at once is answered authorised; whatever it does not finish the
// outbox does.
type DSSWriter interface {
	WriteNow(ctx context.Context, id string) error
}

// DSSWriteTimeout bounds the write made in the request path (spec 05
// §9: the DSS write adds its round trips to POST /v1/intents); the
// outbox finishes what it does not.
const DSSWriteTimeout = 5 * time.Second

// DSSManaged reports whether the DSS must hold r: an intent that needs an
// authorisation inside U-space airspace (02 F5), or anywhere when forAll
// (USSP_DSS_FOR_ALL). An exempt intent never goes to the DSS.
func DSSManaged(r *Record, forAll bool) bool {
	return !r.Exempt && (r.Decision.InUSpaceAirspace || forAll)
}

// DSSDesired is the F3548 state the DSS must hold for r and true, or ""
// and false when the DSS must hold nothing of it. pending_dss is
// Accepted: the first write is the one that authorises it. The state is
// only ever one of f3548.DSSStates (spec 04 §4: local states never
// leave); a decision naming anything else writes nothing.
func DSSDesired(r *Record, forAll bool) (f3548.OperationalIntentState, bool) {
	if !DSSManaged(r, forAll) {
		return "", false
	}
	if r.LocalState == StatePendingDSS {
		return f3548.Accepted, true
	}
	if !slices.Contains(ActiveStates, r.LocalState) || r.Decision.DSSState == nil {
		return "", false
	}
	s := f3548.OperationalIntentState(*r.Decision.DSSState)
	if !slices.Contains(f3548.DSSStates, s) {
		return "", false
	}
	return s, true
}

// queuingTx queues the DSS work of every version written: an item for a
// managed intent, and for one the DSS still holds (a modification that
// left U-space airspace, an exempt one), whatever its state, so the
// writer mirrors it or deletes it.
type queuingTx struct {
	Tx
	s *Service
}

// Insert writes the intent and queues its DSS work.
func (q queuingTx) Insert(ctx context.Context, r *Record) error {
	if err := q.Tx.Insert(ctx, r); err != nil {
		return err
	}
	return q.QueueDSS(ctx, r.ID, r.Version, DSSManaged(r, q.s.DSSForAll))
}

// Update writes the version and queues its DSS work.
func (q queuingTx) Update(ctx context.Context, r *Record, event string) error {
	if err := q.Tx.Update(ctx, r, event); err != nil {
		return err
	}
	return q.QueueDSS(ctx, r.ID, r.Version, DSSManaged(r, q.s.DSSForAll))
}

// inTx is Store.InTx with every version written queued for the DSS.
func (s *Service) inTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	return s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return fn(ctx, queuingTx{Tx: tx, s: s})
	})
}

// holdForDSS keeps an authorisation the DSS must hold pending until the
// DSS has taken it: the decision the local checks reached (authorised,
// with its thresholds) becomes pending_dss with dss_write_pending, or
// with the reason the DSS cannot be written now, and no authorisation
// number. The writer authorises it after the DSS answered (DSSAuthorise).
func (s *Service) holdForDSS(ctx context.Context, d *Decision, exempt bool) {
	if d.Decision != DecisionAuthorised || exempt || (!d.InUSpaceAirspace && !s.DSSForAll) {
		return
	}
	reason, detail := ReasonDSSWritePending, "the local checks passed; the intent is authorised once the DSS has taken it (strategic coordination, 02 F5)"
	switch {
	case s.Decider == nil || s.Decider.DSS == nil:
		reason, detail = ReasonDSSUnavailable, "the intent is deconflicted through the DSS, which cannot be written now: no DSS client is configured"
	default:
		if ok, why := s.Decider.DSS.Available(ctx); !ok {
			reason, detail = ReasonDSSUnavailable, "the intent is deconflicted through the DSS, which cannot be written now: "+why
			if strings.HasPrefix(why, ReasonUSSAvailabilityDown) {
				reason = ReasonUSSAvailabilityDown
			}
		}
	}
	s.count(reason)
	d.Conflicts = append(d.Conflicts, Conflict{Kind: KindDSS, Reason: reason, Effect: EffectHolds, Detail: detail})
	d.Decision, d.State = DecisionPendingDSS, StatePendingDSS
	d.AuthorisationNumber, d.DSSState = nil, nil
	d.Conditions = slices.DeleteFunc(d.Conditions, func(c Condition) bool { return c.Code == CondLocalDeconfliction })
}

// writeNow asks the DSS writer to write a committed pending_dss intent
// within DSSWriteTimeout and answers the decision as it then stands; a
// write that does not finish leaves the decision pending_dss, and the
// outbox goes on with it.
func (s *Service) writeNow(ctx context.Context, d Decision) Decision {
	if s.Writer == nil || d.State != StatePendingDSS || !holdsFor(d, ReasonDSSWritePending) {
		return d
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DSSWriteTimeout)
	defer cancel()
	if err := s.Writer.WriteNow(wctx, d.IntentID); err != nil {
		s.count("dss_write_deferred")
		s.logger().LogAttrs(ctx, slog.LevelInfo, "intent not written to the DSS in the request; the outbox goes on with it",
			slog.String("intent_id", d.IntentID), obs.Err(err))
	}
	r, err := s.Store.Get(ctx, d.IntentID)
	if err != nil || r == nil {
		return d
	}
	return r.Decision
}

func holdsFor(d Decision, reason string) bool {
	for _, c := range d.Conflicts {
		if c.Kind == KindDSS && c.Reason == reason {
			return true
		}
	}
	return false
}

// localFlagged splits the intents a decision displaced into this USSP's
// own, which are flagged in the transaction (Art. 10(10)), and the peers',
// which the DSS writer notifies (WP-13).
func localFlagged(flagged []string) []string {
	return slices.DeleteFunc(slices.Clone(flagged), func(id string) bool { return strings.HasPrefix(id, PeerPrefix) })
}

// Record is the intent as stored; nil when none (the DSS writer and the
// F3548 USS endpoints read it; no caller check: they act for this USSP).
func (s *Service) Record(ctx context.Context, id string) (*Record, error) {
	if !validUUID(id) {
		return nil, nil
	}
	return s.Store.Get(ctx, id)
}

// Held is what the DSS holds of the intent; nil when nothing.
func (s *Service) Held(ctx context.Context, id string) (*DSSHeld, error) {
	return s.Store.Held(ctx, id)
}

// PeerCheck outcomes.
const (
	// PeerCheckOK: no conflict; Displaced names the peers' intents this
	// one takes precedence over.
	PeerCheckOK = "ok"
	// PeerCheckRejected: a conflict refused the intent (committed).
	PeerCheckRejected = "rejected"
	// PeerCheckNotJudged: the check did not run (the reason says why);
	// nothing changed.
	PeerCheckNotJudged = "not_judged"
	// PeerCheckStale: the intent is not the pending_dss version asked
	// about; nothing changed.
	PeerCheckStale = "stale"
)

// PeerCheckResult is the outcome of PeerCheck.
type PeerCheckResult struct {
	Outcome   string
	Reason    string
	Detail    string
	Conflicts []Conflict
	// Displaced are the entity ids of the peers' intents this intent
	// takes precedence over (Art. 10(8)): the writer notifies them
	// within ConflictingOIMaxUSSNotificationTimeSeconds.
	Displaced []string
}

// PeerCheck judges a pending_dss intent, before its write to the DSS,
// against everything the DSS named that was stored since its decision:
// this USSP's intents, the peers' intents (peer_intents, trust provider)
// and the constraints, with the deconfliction of WP-7, and against the
// CIS as it is now (the decision may be hours old after a DSS outage). A
// conflict that refuses commits the intent rejected, naming the peer's
// intent or the constraint; a check that cannot run changes nothing.
func (s *Service) PeerCheck(ctx context.Context, id string, version int) (PeerCheckResult, error) {
	var out PeerCheckResult
	cur, err := s.Store.Get(ctx, id)
	if err != nil {
		return out, err
	}
	if cur == nil || cur.LocalState != StatePendingDSS || cur.Version != version {
		out.Outcome = PeerCheckStale
		return out, nil
	}
	pol := s.policy()
	now, err := s.Store.Now(ctx)
	if err != nil {
		return out, err
	}
	f := s.cisFindings(cur, pol, now)
	switch {
	case f.notJudged != "":
		s.count("dss_check_cis_not_judged")
		out.Outcome, out.Reason, out.Detail = PeerCheckNotJudged, ReasonDeconflictNotJudged, "the CIS cannot be judged now: "+f.notJudged
		return out, nil
	case len(f.found) > 0:
		return s.dssReject(ctx, cur, f.found, "the CIS now conflicts with the intent")
	}
	var conflicts []Conflict
	var displaced []string
	notJudged := ""
	err = s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		conflicts, displaced, notJudged = nil, nil, ""
		r, err := tx.Lock(ctx, id)
		if err != nil {
			return err
		}
		if r == nil || r.LocalState != StatePendingDSS || r.Version != version {
			out.Outcome = PeerCheckStale
			return nil
		}
		others, err := s.othersOfRecord(ctx, tx, r)
		if err != nil {
			var ie *Error
			if errors.As(err, &ie) {
				notJudged = ie.Detail
				return nil
			}
			return err
		}
		mine, err := deconflictOfRecord(r)
		if err != nil {
			notJudged = reasonOf(err)
			return nil
		}
		mine.RankAt = r.FiledAt
		p := deconflict.Policy{HorizontalBufferM: pol.Values.DeconflictBufferM, VerticalBufferM: pol.Values.DeconflictVerticalBufferM}
		cs, err := deconflict.Check(mine, others, p)
		if err != nil {
			notJudged = "the strategic deconfliction did not run: " + reasonOf(err)
			return nil
		}
		found, flagged := s.Decider.conflictsOf(cs)
		for _, c := range found {
			if c.Effect == EffectRejects {
				conflicts = append(conflicts, c)
			}
		}
		for _, f := range flagged {
			if p, ok := strings.CutPrefix(f, PeerPrefix); ok {
				displaced = append(displaced, p)
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	switch {
	case out.Outcome == PeerCheckStale:
		return out, nil
	case notJudged != "":
		s.count("dss_check_not_judged")
		out.Outcome, out.Reason, out.Detail = PeerCheckNotJudged, ReasonDeconflictNotJudged, notJudged
		return out, nil
	case len(conflicts) > 0:
		return s.dssReject(ctx, cur, conflicts, "deconflicted against the intents and constraints the DSS named")
	}
	out.Outcome, out.Displaced = PeerCheckOK, displaced
	return out, nil
}

// dssReject commits a pending_dss intent rejected with the conflicts
// found when it was checked before its DSS write.
func (s *Service) dssReject(ctx context.Context, cur *Record, found []Conflict, why string) (PeerCheckResult, error) {
	out := PeerCheckResult{Outcome: PeerCheckRejected, Conflicts: found}
	err := s.inTx(ctx, func(ctx context.Context, tx Tx) error {
		r, err := tx.Lock(ctx, cur.ID)
		if err != nil {
			return err
		}
		if r == nil || r.LocalState != StatePendingDSS || r.Version != cur.Version {
			out.Outcome = PeerCheckStale
			return nil
		}
		at, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		r.Decision.Conflicts = append(withoutDSSHolds(r.Decision.Conflicts), found...)
		r.Decision.Decision, r.LocalState = DecisionRejected, StateRejected
		r.Decision.DSSState, r.Decision.AuthorisationNumber, r.Decision.DeviationThresholds = nil, nil, nil
		r.ChangeReason, r.Actor = why+": "+found[0].Kind+" "+found[0].Ref, "system"
		s.advance(r, at)
		return tx.Update(ctx, r, EventDSSRejected)
	})
	if err != nil {
		return PeerCheckResult{}, err
	}
	if out.Outcome == PeerCheckRejected {
		s.count("dss_rejected")
		s.projectCommitted(ctx, cur.ID)
	}
	return out, nil
}

func withoutDSSHolds(cs []Conflict) []Conflict {
	return slices.DeleteFunc(slices.Clone(cs), func(c Conflict) bool { return c.Kind == KindDSS && c.Effect == EffectHolds })
}

// othersOfRecord are the intents a stored intent is checked against at
// its DSS write: this USSP's active and pending ones near it, the peers'
// intents and the constraints fetched in the last 24 h.
func (s *Service) othersOfRecord(ctx context.Context, tx Tx, r *Record) ([]deconflict.Intent, error) {
	n := &Normalised{TimeStart: r.TimeStart, TimeEnd: r.TimeEnd}
	pol := s.policy().Values
	dist := pol.DeconflictBufferM
	if !core.IsFinite(dist) || dist < 0 {
		dist = 0
	}
	recs, err := tx.Overlapping(ctx, r.Envelope, dist+prefilterMarginM, n.TimeStart, n.TimeEnd, r.ID, MaxOverlapping)
	if err != nil {
		return nil, err
	}
	if len(recs) > MaxOverlapping {
		s.count("overlapping_bound_exceeded")
		return nil, refuse(http.StatusServiceUnavailable, "deconfliction_bound_exceeded", "more than %d authorised intents overlap the intent; it is not judged", MaxOverlapping)
	}
	out := make([]deconflict.Intent, 0, len(recs))
	for i := range recs {
		di, err := deconflictOfRecord(&recs[i])
		if err != nil {
			return nil, err
		}
		out = append(out, di)
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	peers, err := tx.PeerIntents(ctx, now.Add(-PeerMaxAge), n.TimeStart, n.TimeEnd, MaxPeerIntents)
	if err != nil {
		return nil, err
	}
	cons, err := tx.ConstraintsOverlapping(ctx, now.Add(-PeerMaxAge), n.TimeStart, n.TimeEnd, MaxPeerIntents)
	if err != nil {
		return nil, err
	}
	if len(peers)+len(cons) > MaxPeerIntents {
		s.count("peer_bound_exceeded")
		return nil, refuse(http.StatusServiceUnavailable, "deconfliction_bound_exceeded", "more than %d peer intents and constraints overlap the intent; it is not judged", MaxPeerIntents)
	}
	for _, p := range peers {
		di, err := s.deconflictOfPeer(p)
		if err != nil {
			s.count("peer_intent_unjudgeable")
			return nil, refuse(http.StatusServiceUnavailable, "deconfliction_not_judged", "the peer intent %s cannot be judged: %s", p.EntityID, reasonOf(err))
		}
		out = append(out, di)
	}
	for _, c := range cons {
		di, err := s.deconflictOfPeer(c)
		if err != nil {
			s.count("constraint_unjudgeable")
			return nil, refuse(http.StatusServiceUnavailable, "deconfliction_not_judged", "the constraint %s cannot be judged: %s", c.EntityID, reasonOf(err))
		}
		// A constraint is a restriction: it always has precedence, and
		// its conflict names it (conflictsOf).
		di.ID, di.Priority = ConstraintPrefix+c.EntityID, maxPriority
		out = append(out, di)
	}
	return out, nil
}

// maxPriority is the priority a constraint is judged at: above every
// intent's (an int32 column, F3548's integer).
const maxPriority = 1<<31 - 1

// DSSHold records why a pending_dss intent still waits for its DSS write
// (the DSS down, our availability Down, a key conflict twice, a peer's
// intent unreadable): a new version when the reason or its detail
// changed, nothing when the decision already says it.
func (s *Service) DSSHold(ctx context.Context, id string, version int, reason, detail string) error {
	changed := false
	err := s.inTx(ctx, func(ctx context.Context, tx Tx) error {
		changed = false
		r, err := tx.Lock(ctx, id)
		if err != nil {
			return err
		}
		if r == nil || r.LocalState != StatePendingDSS || r.Version != version {
			return nil
		}
		for _, c := range r.Decision.Conflicts {
			if c.Kind == KindDSS && c.Effect == EffectHolds && c.Reason == reason && c.Detail == detail {
				return nil
			}
		}
		at, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		r.Decision.Conflicts = append(withoutDSSHolds(r.Decision.Conflicts), Conflict{Kind: KindDSS, Reason: reason, Effect: EffectHolds, Detail: detail})
		r.ChangeReason, r.Actor = "waiting for the DSS: "+reason, "system"
		s.advance(r, at)
		changed = true
		return tx.Update(ctx, r, EventDSSPending)
	})
	if err != nil {
		return err
	}
	if changed {
		s.count("dss_held_" + reason)
		s.projectCommitted(ctx, id)
	}
	return nil
}

// DSSAuthorise records what the DSS holds after a write of the intent and
// queues the subscribers' notifications, in one transaction; when the
// write was the first of a pending_dss intent at version, it authorises
// it (the authorisation number, the previous one after a modification,
// Art. 6(6); dss_state Accepted). It reports whether it authorised.
func (s *Service) DSSAuthorise(ctx context.Context, id string, version int, held DSSHeld, notes []OutboxSpec) (bool, error) {
	authorised := false
	err := s.inTx(ctx, func(ctx context.Context, tx Tx) error {
		authorised = false
		r, err := tx.Lock(ctx, id)
		if err != nil {
			return err
		}
		if r == nil {
			return ErrNotFound
		}
		if err := tx.SetHeld(ctx, id, &held); err != nil {
			return err
		}
		for _, n := range notes {
			if err := tx.Enqueue(ctx, n.Kind, n.EntityID, n.Version, n.Payload); err != nil {
				return err
			}
		}
		if r.LocalState != StatePendingDSS || r.Version != version || held.State != f3548.Accepted {
			return nil
		}
		at, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		number, err := tx.PreviousNumber(ctx, id)
		if err != nil {
			return err
		}
		if number == "" {
			public, _ := regnum.Public(strings.TrimSpace(r.Request.OperatorReg))
			if s.Decider == nil || s.Decider.SystemID == "" || public == "" {
				return errors.New("no USSP code or operator registration to number the authorisation with")
			}
			number = s.Decider.SystemID + "-" + public + "-" + bus.NewULID(at)
		}
		r.Decision.Conflicts = withoutDSSHolds(r.Decision.Conflicts)
		r.Decision.Decision, r.LocalState = DecisionAuthorised, StateAccepted
		r.Decision.DSSState = ptr(string(f3548.Accepted))
		r.Decision.AuthorisationNumber = &number
		r.ChangeReason, r.Actor = fmt.Sprintf("deconflicted through the DSS (version %d)", held.Version), "system"
		s.advance(r, at)
		if err := tx.Update(ctx, r, EventDSSAuthorised); err != nil {
			return err
		}
		authorised = true
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if authorised {
		s.count("dss_authorised")
		s.projectCommitted(ctx, id)
	}
	return authorised, nil
}

// DSSRecord records what the DSS holds of the intent after an update or a
// delete (held nil: nothing) and queues the notifications, in one
// transaction; the intent's version does not change.
func (s *Service) DSSRecord(ctx context.Context, id string, held *DSSHeld, notes []OutboxSpec) error {
	return s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.SetHeld(ctx, id, held); err != nil {
			return err
		}
		for _, n := range notes {
			if err := tx.Enqueue(ctx, n.Kind, n.EntityID, n.Version, n.Payload); err != nil {
				return err
			}
		}
		return nil
	})
}

// PeerIntentDisplaced re-checks this USSP's authorisation that a peer's
// intent with precedence now overlaps (a peer's notification, WP-13):
// the standing re-check's priority cause, so it is withdrawn before its
// activation and marked after it, and the operator is told.
func (s *Service) PeerIntentDisplaced(ctx context.Context, id, peerEntityID string) (RecheckResult, error) {
	return s.Recheck(ctx, id, Cause{Kind: CausePriority, Ref: PeerPrefix + peerEntityID})
}

// PeerConflicts are this USSP's active intents a peer's intent meets:
// the deconfliction of WP-7 between the peer's volumes (judged as a
// PeerIntent, trust provider) and each of them, with whether the peer
// has precedence by priority. Nothing is written.
func (s *Service) PeerConflicts(ctx context.Context, p PeerIntent) ([]PeerConflict, error) {
	peer, err := s.deconflictOfPeer(p)
	if err != nil {
		return nil, err
	}
	var out []PeerConflict
	pol := s.policy().Values
	from, to := peerWindow(peer)
	err = s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		out = nil
		boxes := make([]geodesy.BBox, 0, len(peer.Volumes))
		for _, v := range peer.Volumes {
			boxes = append(boxes, PadForGeography(v.Shape.BBox()))
		}
		recs, err := tx.Overlapping(ctx, boxes, pol.DeconflictBufferM+prefilterMarginM, from, to, noIntentID, MaxOverlapping)
		if err != nil {
			return err
		}
		pp := deconflict.Policy{HorizontalBufferM: pol.DeconflictBufferM, VerticalBufferM: pol.DeconflictVerticalBufferM}
		for i := range recs {
			r := &recs[i]
			if !slices.Contains(ActiveStates, r.LocalState) {
				continue
			}
			mine, err := deconflictOfRecord(r)
			if err != nil {
				return err
			}
			cs, err := deconflict.Check(mine, []deconflict.Intent{peer}, pp)
			if err != nil {
				return err
			}
			for _, c := range cs {
				peerWins, rule := deconflict.Precedes(peer, mine)
				out = append(out, PeerConflict{IntentID: r.ID, PeerWinsByPriority: peerWins && rule == deconflict.RulePriority,
					MineVolume: c.MineVolume, Overlap: c.Overlap})
			}
		}
		return nil
	})
	return out, err
}

// noIntentID excludes no intent from Overlapping (the nil UUID, which no
// intent has: ids are version 4).
const noIntentID = "00000000-0000-0000-0000-000000000000"

// PeerConflict is one of this USSP's intents a peer's intent meets.
type PeerConflict struct {
	IntentID string
	// PeerWinsByPriority: the peer's priority is higher (Art. 10(8));
	// otherwise the DSS let a conflict through (spec 06 T9).
	PeerWinsByPriority bool
	MineVolume         int
	Overlap            deconflict.Overlap
}

func peerWindow(in deconflict.Intent) (time.Time, time.Time) {
	var from, to time.Time
	for i, v := range in.Volumes {
		if i == 0 || v.Start.Before(from) {
			from = v.Start
		}
		if i == 0 || v.End.After(to) {
			to = v.End
		}
	}
	return from, to
}

// MarshalHeld is the JSON of what the DSS holds (the store's columns).
func MarshalHeld(h *DSSHeld) (reference, extents []byte, err error) {
	if reference, err = json.Marshal(h.Reference); err != nil {
		return nil, nil, err
	}
	if extents, err = json.Marshal(h.Extents); err != nil {
		return nil, nil, err
	}
	return reference, extents, nil
}
