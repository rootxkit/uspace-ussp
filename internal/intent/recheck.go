package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// The causes of a standing re-check (Art. 10(10); brief WP-12, PLAN
// §15.1 Q20).
const (
	// CauseRestriction: a restriction of the ANSP was published or
	// changed (cis.v1.restrictions) or a constraint was notified (WP-13).
	CauseRestriction = "restriction"
	// CauseZone: a geographical zone was published or changed
	// (cis.v1.zones).
	CauseZone = "zone"
	// CauseUSpaceAirspace: a U-space airspace or its Art. 3(4)
	// requirements changed (cis.v1.uspace_airspace).
	CauseUSpaceAirspace = "uspace_airspace"
	// CausePriority: a later intent with precedence took the space (WP-7
	// step 5); no CIS judgement, the deconfliction already decided.
	CausePriority = "priority"
	// CauseSweep: the periodic re-check of every active intent, which
	// catches a change no notification announced.
	CauseSweep = "sweep"
)

// The outcomes of Recheck.
const (
	// RecheckUntouched: the exact check finds no conflict; nothing is
	// written.
	RecheckUntouched = "untouched"
	// RecheckWithdrawn: an accepted intent that was not activated now
	// conflicts; it is withdrawn.
	RecheckWithdrawn = "withdrawn"
	// RecheckMarked: an activated (or nonconforming, contingent) intent
	// now conflicts: it stays in its state (the aircraft may be flying:
	// conformance keeps judging it, WP-10) and its authorisation is
	// marked withdrawn for the operator with the conflict's window.
	RecheckMarked = "marked"
	// RecheckAlreadyMarked: the same conflict was already told; nothing
	// is written twice.
	RecheckAlreadyMarked = "already_marked"
	// RecheckNotActive: the intent is not in an active state.
	RecheckNotActive = "not_active"
	// RecheckNotJudged: the CIS cannot be judged now (stale, outdated,
	// unavailable): nothing changes and the re-check is owed (counted;
	// the next change or sweep runs it again). A stale CIS never
	// withdraws an authorisation.
	RecheckNotJudged = "not_judged"
)

// EventWithdrawn and EventRechecked are the audit events of a re-check.
const (
	EventWithdrawn = "intent_withdrawn"
	EventRechecked = "intent_authorisation_withdrawn_in_flight"
)

// Cause is why an intent is re-checked: the kind, the feature or intent
// that caused it, and the CIS version it was seen in.
type Cause struct {
	Kind       string `json:"cause"`
	Ref        string `json:"ref,omitempty"`
	CISVersion string `json:"cis_version,omitempty"`
}

func (c Cause) String() string {
	if c.Ref == "" {
		return c.Kind
	}
	return c.Kind + " " + c.Ref
}

// Window is when a conflict applies (a restriction's starts_at and
// ends_at, a zone's validity; nil ends are open).
type Window struct {
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
}

// Notice is what a re-check that affected an intent told its operator,
// kept in the intent's update_required (so its activation stays refused,
// and its projection after the commit publishes the alert, retried until
// it is published): the restriction_activated alert's detail.
type Notice struct {
	Cause string `json:"cause"`
	// Ref is the feature (restriction, zone, airspace) or intent that
	// caused it; RestrictionID the same for a restriction.
	Ref           string `json:"ref"`
	RestrictionID string `json:"restriction_id,omitempty"`
	// ByIntentID keeps WP-7's flag shape for a priority cause.
	ByIntentID string `json:"by_intent_id,omitempty"`
	Reason     string `json:"reason"`
	// Decision is withdrawn or marked; Withdrawn says the authorisation
	// no longer stands, AuthorisationUpdated that it was replaced (never
	// here: an update is the operator's new request).
	Decision             string     `json:"decision"`
	Withdrawn            bool       `json:"withdrawn"`
	AuthorisationUpdated bool       `json:"authorisation_updated"`
	PreviousState        string     `json:"previous_state"`
	IntentState          string     `json:"intent_state"`
	AffectedIntents      []string   `json:"affected_intents"`
	Window               *Window    `json:"window"`
	Conflicts            []Conflict `json:"conflicts"`
	ChangeReason         string     `json:"change_reason"`
	CISVersion           string     `json:"cis_version,omitempty"`
	// Version is the intent version the notice was written with: the
	// projection of that version publishes the alert.
	Version  int       `json:"version"`
	At       time.Time `json:"at"`
	AlertID  string    `json:"alert_id"`
	Severity string    `json:"severity"`
}

// NoticeOf is the notice an intent carries, nil when none (or a WP-7
// flag without a notice).
func NoticeOf(r *Record) *Notice {
	if !r.Flagged() {
		return nil
	}
	var n Notice
	if json.Unmarshal(r.UpdateRequired, &n) != nil || n.Decision == "" || n.AlertID == "" {
		return nil
	}
	return &n
}

// RecheckResult is what one re-check did.
type RecheckResult struct {
	IntentID string
	Outcome  string
	Notice   *Notice
	// Detail says why a re-check was not judged.
	Detail string
}

// assessCIS runs WP-7's steps 3 and 4 alone (the U-space airspaces and
// their constraints; the zones and the restrictions) on n: the
// decision's own CIS judgement, re-applied on the stored volumes.
func (d *Decider) assessCIS(n *Normalised, pol policy.Record, now time.Time) *Assessment {
	a := &Assessment{n: n, pol: pol, now: now}
	a.d = Decision{USpaceAirspaceIDs: []string{}, Conflicts: []Conflict{}, Conditions: []Condition{}}
	d.cis(a)
	return a
}

// affecting are the conflicts of a re-check that withdraw an
// authorisation: what refuses or holds a request of the zones, the
// restrictions and the U-space airspaces. A CIS conflict (stale,
// outdated, unavailable) is never one: the CIS was not judged.
func affecting(cs []Conflict) []Conflict {
	var out []Conflict
	for _, c := range cs {
		if c.Effect != EffectRejects && c.Effect != EffectHolds {
			continue
		}
		switch c.Kind {
		case KindZone, KindRestriction, KindAirspace:
			out = append(out, c)
		}
	}
	return out
}

func notJudged(cs []Conflict) (string, bool) {
	for _, c := range cs {
		if c.Kind == KindCIS {
			return c.Reason + ": " + c.Detail, true
		}
	}
	return "", false
}

// causeOf names the cause from the first conflict not told yet: its kind
// and its feature.
func causeOf(c Conflict, given Cause) Cause {
	out := Cause{Kind: given.Kind, Ref: c.Ref, CISVersion: given.CISVersion}
	switch c.Kind {
	case KindRestriction:
		out.Kind = CauseRestriction
	case KindZone:
		out.Kind = CauseZone
	case KindAirspace:
		out.Kind = CauseUSpaceAirspace
	}
	return out
}

// windowOf is when the feature ref applies, from the CIS cache: a
// restriction's starts_at and ends_at, else the zone's validity.
func (d *Decider) windowOf(n *Normalised, ref string) *Window {
	if d.CIS == nil {
		return nil
	}
	for i := range n.Volumes {
		v := &n.Volumes[i]
		for _, c := range d.CIS.ZonesFor(v.BBox, v.Start, v.End).Zones {
			e := c.Entry
			if e == nil || e.Identifier != ref {
				continue
			}
			if r := e.Restriction; r != nil && !r.StartsAt.IsZero() && !r.EndsAt.IsZero() {
				s, en := r.StartsAt.UTC(), r.EndsAt.UTC()
				return &Window{StartsAt: &s, EndsAt: &en}
			}
			if e.ValidFrom != nil || e.ValidTo != nil {
				return &Window{StartsAt: e.ValidFrom, EndsAt: e.ValidTo}
			}
			// How the zone applies over the volume's window, as the cache
			// says (nil ends are open).
			return &Window{StartsAt: c.From, EndsAt: c.To}
		}
	}
	return nil
}

// Recheck re-checks one intent (Art. 10(10); brief WP-12): WP-7's CIS
// steps re-applied on its stored volumes, or for a priority cause the
// precedence the deconfliction already found. No conflict after the
// exact check: nothing. A conflict: an accepted intent is withdrawn
// (change_reason "<cause> <ref>"), an activated, nonconforming or
// contingent one keeps its state and is marked (its authorisation
// withdrawn for the operator, with the conflict's window); either way a
// new version with the change_reason and a notice the projection turns
// into restriction_activated after the commit. The decision is the
// database's: the intent row is locked and its version checked, on the
// database clock. A CIS that cannot be judged changes nothing.
func (s *Service) Recheck(ctx context.Context, intentID string, cause Cause) (RecheckResult, error) {
	out := RecheckResult{IntentID: intentID}
	if !validUUID(intentID) {
		return out, refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	cur, err := s.Store.Get(ctx, intentID)
	if err != nil {
		return out, &UnavailableError{Dependency: "database", Detail: "the intent could not be read"}
	}
	if cur == nil || !slices.Contains(ActiveStates, cur.LocalState) {
		out.Outcome = RecheckNotActive
		return out, nil
	}
	pol := s.policy()
	now, err := s.Store.Now(ctx)
	if err != nil {
		return out, &UnavailableError{Dependency: "database", Detail: "the database clock could not be read"}
	}
	var found []Conflict
	var norm *Normalised
	var cisVersion string
	if cause.Kind == CausePriority {
		found = []Conflict{{Kind: KindIntent, Reason: ReasonIntentPriority, Effect: EffectRejects, Ref: cause.Ref,
			Detail: "a later intent with precedence overlaps this authorisation (Art. 10(10))"}}
	} else {
		n, err := normalisedOf(cur)
		if err != nil {
			s.count("recheck_unreadable")
			out.Outcome, out.Detail = RecheckNotJudged, reasonOf(err)
			return out, nil
		}
		a := s.Decider.assessCIS(n, pol, now)
		if a.d.CISVersionChecked != nil {
			cisVersion = *a.d.CISVersionChecked
		}
		if why, bad := notJudged(a.d.Conflicts); bad {
			s.count("recheck_not_judged")
			out.Outcome, out.Detail = RecheckNotJudged, why
			return out, nil
		}
		found = affecting(a.d.Conflicts)
		if a.d.InUSpaceAirspace && !cur.Decision.InUSpaceAirspace {
			// A U-space airspace published after the authorisation now
			// holds the volumes: its Art. 3(4) requirements and the
			// deconfliction through the DSS apply, which the
			// authorisation did not rest on.
			ref := ""
			if len(a.d.USpaceAirspaceIDs) > 0 {
				ref = a.d.USpaceAirspaceIDs[0]
			}
			found = append(found, Conflict{Kind: KindAirspace, Reason: ReasonAirspaceEntered, Effect: EffectHolds, Ref: ref, Item: ptr(5),
				Detail: "the volumes are now inside a U-space airspace published after the authorisation: its Art. 3(4) requirements and the deconfliction through the DSS apply, which the authorisation did not rest on"})
		}
		if len(found) == 0 {
			s.count("recheck_untouched")
			out.Outcome = RecheckUntouched
			return out, nil
		}
		norm = n
	}
	if cisVersion == "" {
		cisVersion = cause.CISVersion
	}
	err = s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		r, err := tx.Lock(ctx, intentID)
		if err != nil {
			return err
		}
		if r == nil || !slices.Contains(ActiveStates, r.LocalState) {
			out.Outcome = RecheckNotActive
			return nil
		}
		if r.Version != cur.Version {
			// Changed while judged (a modification, an activation): the
			// next change or sweep judges the new version.
			s.count("recheck_raced")
			out.Outcome, out.Detail = RecheckNotJudged, "the intent changed while it was re-checked"
			return nil
		}
		// The dedupe is on the whole conflict set: a conflict already told
		// is not told twice, but a further one (restriction B while A
		// still applies) is a new notice, named after the first conflict
		// the operator was not told of.
		prev := NoticeOf(r)
		fresh := untold(found, prev)
		if len(fresh) == 0 {
			out.Outcome, out.Notice = RecheckAlreadyMarked, prev
			return nil
		}
		cause := cause
		var window *Window
		if norm != nil {
			cause = causeOf(fresh[0], cause)
			window = s.Decider.windowOf(norm, cause.Ref)
		}
		at, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		prevState := r.LocalState
		reason := cause.String()
		notice := &Notice{
			Cause: cause.Kind, Ref: cause.Ref, Reason: fresh[0].Reason, Withdrawn: true, PreviousState: prevState,
			AffectedIntents: []string{r.ID}, Window: window, Conflicts: found, ChangeReason: reason, CISVersion: cisVersion,
			Severity: string(core.SeverityCritical),
		}
		if cause.Kind == CauseRestriction {
			notice.RestrictionID = cause.Ref
		}
		if cause.Kind == CausePriority {
			notice.ByIntentID = cause.Ref
		}
		event := EventWithdrawn
		if prevState == StateAccepted {
			notice.Decision = RecheckWithdrawn
			r.LocalState = StateWithdrawn
			r.Decision.DSSState = nil
		} else {
			notice.Decision = RecheckMarked
			event = EventRechecked
		}
		r.Decision.Conflicts = append(slices.Clone(r.Decision.Conflicts), fresh...)
		r.ChangeReason, r.Actor = reason, "system"
		s.advance(r, at)
		notice.IntentState, notice.Version, notice.At = r.LocalState, r.Version, at
		notice.AlertID = noticeAlertID(r.ID, r.Version, reason)
		raw, err := json.Marshal(notice)
		if err != nil {
			return err
		}
		r.UpdateRequired = raw
		if err := tx.Update(ctx, r, event); err != nil {
			return err
		}
		if err := tx.SetNotice(ctx, r.ID, raw); err != nil {
			return err
		}
		out.Outcome, out.Notice = notice.Decision, notice
		return nil
	})
	if err != nil {
		return RecheckResult{IntentID: intentID}, s.txError(err)
	}
	switch out.Outcome {
	case RecheckWithdrawn, RecheckMarked:
		s.count("recheck_" + out.Outcome)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "authorisation withdrawn by the standing re-check (Art. 10(10))",
			slog.String("intent_id", intentID), slog.String("outcome", out.Outcome), slog.String("cause", out.Notice.ChangeReason))
		s.projectCommitted(ctx, intentID)
	case RecheckAlreadyMarked:
		s.count("recheck_already_marked")
	}
	return out, nil
}

// conflictKey identifies a conflict for the dedupe of notices: its
// kind, reason and feature or intent.
func conflictKey(c Conflict) string { return c.Kind + "|" + c.Reason + "|" + c.Ref }

// untold are the conflicts of found that the notice prev did not carry
// (all of them when there is no notice).
func untold(found []Conflict, prev *Notice) []Conflict {
	told := map[string]bool{}
	if prev != nil {
		for _, c := range prev.Conflicts {
			told[conflictKey(c)] = true
		}
	}
	var out []Conflict
	for _, c := range found {
		if !told[conflictKey(c)] {
			out = append(out, c)
		}
	}
	return out
}

// noticeAlertID is the restriction_activated alert of one intent
// version and cause: the same notice always has the same id (a retried
// projection, a second api replica), a later one another.
func noticeAlertID(intentID string, version int, reason string) string {
	return alertUUID("restriction_activated|" + intentID + "|" + fmt.Sprint(version) + "|" + reason)
}

// ActiveLister lists the active intents a re-check runs on.
type ActiveLister interface {
	// ActiveIn are the ids of the active intents (exempt ones too: no
	// intent is flown over a PROHIBITED zone) whose envelope meets one
	// of boxes and whose window overlaps [from, to]; every active intent
	// when boxes is empty; at most limit+1.
	ActiveIn(ctx context.Context, boxes []geodesy.BBox, from, to *time.Time, limit int) ([]string, error)
}

// MaxRecheck bounds the intents one re-check pass reads (E-10); more is
// counted and the pass goes on with the next sweep (docs/PLAN.md §9: at
// most 1000 active intents are expected).
const MaxRecheck = 5000

// RecheckAll re-checks every active intent whose envelope meets boxes
// in [from, to] (every active intent when boxes is empty) and returns
// what each re-check did; the first error is returned after the rest
// ran.
func (s *Service) RecheckAll(ctx context.Context, boxes []geodesy.BBox, from, to *time.Time, cause Cause) ([]RecheckResult, error) {
	l, ok := s.Store.(ActiveLister)
	if !ok {
		return nil, errors.New("the intent store cannot list the active intents")
	}
	ids, err := l.ActiveIn(ctx, boxes, from, to, MaxRecheck)
	if err != nil {
		return nil, err
	}
	if len(ids) > MaxRecheck {
		s.count("recheck_bound_exceeded")
		obs.Error(ctx, s.logger(), "more active intents than one re-check pass reads; the rest wait for the next sweep",
			errors.New("bound exceeded"), slog.Int("bound", MaxRecheck))
		ids = ids[:MaxRecheck]
	}
	var out []RecheckResult
	var first error
	for _, id := range ids {
		r, err := s.Recheck(ctx, id, cause)
		if err != nil && first == nil {
			first = fmt.Errorf("intent %s: %w", id, err)
		}
		out = append(out, r)
	}
	return out, first
}

// summary is a short form of results for a log line.
func summary(rs []RecheckResult) string {
	n := map[string]int{}
	for _, r := range rs {
		n[r.Outcome]++
	}
	var parts []string
	for _, k := range []string{RecheckWithdrawn, RecheckMarked, RecheckAlreadyMarked, RecheckUntouched, RecheckNotJudged, RecheckNotActive} {
		if n[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n[k]))
		}
	}
	return strings.Join(parts, " ")
}

// Summary is RecheckAll's results counted by outcome, for a log line.
func Summary(rs []RecheckResult) string { return summary(rs) }

// alertUUID is a UUID-shaped (version 4 bits) id derived from key.
func alertUUID(key string) string {
	sum := sha256.Sum256([]byte(key))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
