package geo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// KindRestrictionActivated is the alert/v1 kind of a re-check that
// affected an intent (spec 04 §3.3).
const KindRestrictionActivated = "restriction_activated"

// Counters of the re-check (E-09).
const (
	CounterRecheckRuns     = "geo_recheck_runs"
	CounterRecheckFailed   = "geo_recheck_failed"
	CounterRecheckQueued   = "geo_recheck_queue_full"
	CounterNoticePublished = "geo_notices_published"
)

// Defaults of the re-check.
const (
	// DefaultSweepEvery is the periodic re-check of every active intent:
	// a change no notification announced, a re-check that could not be
	// judged (a stale CIS) or one a full queue skipped is run again
	// within one period.
	DefaultSweepEvery = 60 * time.Second
	// ChangeQueue bounds the changes waiting for the re-check (E-10);
	// past it a full sweep is owed instead, counted.
	ChangeQueue = 64
	// featurePadM widens a changed feature's box for the prefilter of
	// the active intents (the exact check is intent.Recheck's).
	featurePadM = 100
)

// Intents is the intent service's re-check (internal/intent.Service).
type Intents interface {
	RecheckAll(ctx context.Context, boxes []geodesy.BBox, from, to *time.Time, cause intent.Cause) ([]intent.RecheckResult, error)
}

// Snapshotter is the CIS cache's current snapshot (cis.Evaluator).
type Snapshotter interface {
	Snapshot() *cis.Snapshot
}

// Rechecker runs the standing re-check of Art. 10(10) (brief WP-12,
// PLAN §15.1 Q20) on every installed version of zones, uspace_airspace
// and restrictions, on every constraint notification (WP-13 calls
// Constraint), and every SweepEvery over every active intent.
type Rechecker struct {
	Intents    Intents
	CIS        Snapshotter
	SweepEvery time.Duration
	Counters   *core.Counters
	Logger     *slog.Logger

	ch   chan cis.Change
	owed atomic.Bool
}

func (r *Rechecker) init() {
	if r.ch == nil {
		r.ch = make(chan cis.Change, ChangeQueue)
	}
	if r.Counters == nil {
		r.Counters = &core.Counters{}
	}
	if r.Logger == nil {
		r.Logger = obs.Discard()
	}
}

// NewRechecker is a re-check over intents and the CIS cache.
func NewRechecker(intents Intents, snap Snapshotter, counters *core.Counters, logger *slog.Logger) *Rechecker {
	r := &Rechecker{Intents: intents, CIS: snap, Counters: counters, Logger: logger}
	r.init()
	return r
}

// Changed queues an installed version (the cis.Cache's hook); it never
// blocks: a full queue owes a full sweep instead.
func (r *Rechecker) Changed(_ context.Context, c cis.Change) {
	if !c.Dataset.ED318() {
		return
	}
	select {
	case r.ch <- c:
	default:
		r.Counters.Inc(CounterRecheckQueued)
		r.owed.Store(true)
	}
}

// Run takes the changes and sweeps until ctx ends.
func (r *Rechecker) Run(ctx context.Context) {
	every := r.SweepEvery
	if every <= 0 {
		every = DefaultSweepEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-r.ch:
			r.Handle(ctx, c)
		case <-t.C:
			r.owed.Store(false)
			r.pass(ctx, nil, intent.Cause{Kind: intent.CauseSweep})
		}
		if r.owed.Swap(false) {
			r.pass(ctx, nil, intent.Cause{Kind: intent.CauseSweep})
		}
	}
}

// causeOfDataset is the cause a change of d gives.
func causeOfDataset(d cis.Dataset) string {
	switch d {
	case cis.Restrictions:
		return intent.CauseRestriction
	case cis.USpaceAirspace:
		return intent.CauseUSpaceAirspace
	case cis.Zones, cis.USSPList:
	}
	return intent.CauseZone
}

// Handle re-checks the active intents one change can affect: those
// whose envelope meets a changed feature's parts (every active intent
// when the list is truncated or after a warm start). A feature that is
// gone can lift a conflict, never add one: it re-checks nothing.
func (r *Rechecker) Handle(ctx context.Context, c cis.Change) []intent.RecheckResult {
	cause := intent.Cause{Kind: causeOfDataset(c.Dataset), CISVersion: c.CISVersion}
	if c.Truncated || c.Reason == cis.ChangeWarm {
		return r.pass(ctx, nil, cause)
	}
	boxes := r.boxesOf(c)
	if len(boxes) == 0 {
		return nil
	}
	return r.pass(ctx, boxes, cause)
}

// boxesOf are the padded boxes of the changed features' parts in the
// version installed.
func (r *Rechecker) boxesOf(c cis.Change) []geodesy.BBox {
	if r.CIS == nil {
		return nil
	}
	s := r.CIS.Snapshot()
	var out []geodesy.BBox
	for _, e := range s.Entries(c.Dataset) {
		if !slices.Contains(c.FeatureIDs, e.Identifier) {
			continue
		}
		for _, p := range e.Parts {
			out = append(out, p.BBox.PadM(featurePadM))
		}
	}
	return out
}

// Constraint re-checks the active intents a constraint notification
// meets (WP-13's intake calls it with the constraint's extent and
// window): the same re-check as a restriction's.
func (r *Rechecker) Constraint(ctx context.Context, ref string, boxes []geodesy.BBox, from, to *time.Time) []intent.RecheckResult {
	r.init()
	if len(boxes) == 0 {
		return nil
	}
	return r.passWindow(ctx, boxes, from, to, intent.Cause{Kind: intent.CauseRestriction, Ref: ref})
}

func (r *Rechecker) pass(ctx context.Context, boxes []geodesy.BBox, cause intent.Cause) []intent.RecheckResult {
	return r.passWindow(ctx, boxes, nil, nil, cause)
}

func (r *Rechecker) passWindow(ctx context.Context, boxes []geodesy.BBox, from, to *time.Time, cause intent.Cause) []intent.RecheckResult {
	r.Counters.Inc(CounterRecheckRuns)
	rs, err := r.Intents.RecheckAll(ctx, boxes, from, to, cause)
	if err != nil {
		r.Counters.Inc(CounterRecheckFailed)
		r.owed.Store(true)
		obs.Error(ctx, r.Logger, "standing re-check incomplete; a sweep runs it again", err, slog.String("cause", cause.Kind))
	}
	affected := 0
	for _, x := range rs {
		if x.Outcome == intent.RecheckWithdrawn || x.Outcome == intent.RecheckMarked {
			affected++
		}
		if x.Outcome == intent.RecheckNotJudged {
			// Owed: the next sweep judges it again.
			r.owed.Store(true)
		}
	}
	if affected > 0 || cause.Kind != intent.CauseSweep {
		r.Logger.LogAttrs(ctx, slog.LevelInfo, "standing re-check", slog.String("cause", cause.Kind),
			slog.String("cis_version", cause.CISVersion), slog.Int("intents", len(rs)), slog.String("outcomes", intent.Summary(rs)))
	}
	return rs
}

// NoticeBus publishes a re-check's notice as alert/v1
// restriction_activated (critical) on alrt.v1, where api's alerts
// record writes it and traffic-ws delivers it to the intent's operator
// (intent.NoticePublisher).
type NoticeBus struct {
	Pub interface {
		Publish(ctx context.Context, subject string, m bus.Enveloped) error
	}
	Now      func() time.Time
	Counters *core.Counters
}

// noticeBody is alert/v1's body as a notice writes it (schemas/alert/v1:
// flight_id null for an intent without a flight).
type noticeBody struct {
	AlertID             string          `json:"alert_id"`
	Kind                string          `json:"kind"`
	Severity            core.Severity   `json:"severity"`
	State               string          `json:"state"`
	ClearReason         *string         `json:"clear_reason"`
	FlightID            *string         `json:"flight_id"`
	IntentID            string          `json:"intent_id"`
	AuthorisationNumber *string         `json:"authorisation_number"`
	CapturedAt          time.Time       `json:"captured_at"`
	RaisedAt            time.Time       `json:"raised_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	PolicyVersion       int64           `json:"policy_version"`
	Detail              json.RawMessage `json:"detail"`
}

// NoticeMessage is one restriction_activated alert/v1 message.
type NoticeMessage struct {
	bus.Envelope
	Body noticeBody `json:"body"`
}

// noticeDetail is the alert's detail: the notice without its own
// bookkeeping (version, alert id, severity, time).
func noticeDetail(n *intent.Notice) (json.RawMessage, error) {
	d := map[string]any{
		"cause": n.Cause, "ref": n.Ref, "reason": n.Reason, "decision": n.Decision, "withdrawn": n.Withdrawn,
		"authorisation_updated": n.AuthorisationUpdated, "previous_state": n.PreviousState, "intent_state": n.IntentState,
		"affected_intents": n.AffectedIntents, "window": n.Window, "conflicts": n.Conflicts, "change_reason": n.ChangeReason,
		"cis_version": n.CISVersion,
	}
	if n.RestrictionID != "" {
		d["restriction_id"] = n.RestrictionID
	}
	if n.ByIntentID != "" {
		d["by_intent_id"] = n.ByIntentID
	}
	return json.Marshal(d)
}

// NoticeMessageOf is the alert of the notice n of intent r.
func NoticeMessageOf(r *intent.Record, n *intent.Notice, now time.Time) (*NoticeMessage, error) {
	detail, err := noticeDetail(n)
	if err != nil {
		return nil, err
	}
	at := n.At.UTC()
	return &NoticeMessage{
		Envelope: bus.SystemEnvelope("alert/v1", Producer, now),
		Body: noticeBody{
			AlertID: n.AlertID, Kind: KindRestrictionActivated, Severity: core.SeverityCritical, State: StateRaised,
			IntentID: r.ID, AuthorisationNumber: r.Decision.AuthorisationNumber, CapturedAt: at, RaisedAt: at, UpdatedAt: at,
			PolicyVersion: r.Decision.PolicyVersion, Detail: detail,
		},
	}, nil
}

// ErrNoCell is a notice of an intent with no cell to publish it under.
var ErrNoCell = errors.New("the intent has no cell to publish its notice under")

// PublishNotice implements intent.NoticePublisher.
func (b NoticeBus) PublishNotice(ctx context.Context, r *intent.Record, n *intent.Notice) error {
	if len(r.Cells) == 0 {
		return ErrNoCell
	}
	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	m, err := NoticeMessageOf(r, n, now)
	if err != nil {
		return err
	}
	subject, err := bus.Alrt(KindRestrictionActivated, r.Cells[0], n.AlertID)
	if err != nil {
		return err
	}
	if err := b.Pub.Publish(ctx, subject, m); err != nil {
		return err
	}
	if b.Counters != nil {
		b.Counters.Inc(CounterNoticePublished)
	}
	return nil
}
