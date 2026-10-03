package dss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Counter names of the writer (E-09).
const (
	CounterOIRCreated          = "dss_oir_created"
	CounterOIRUpdated          = "dss_oir_updated"
	CounterOIRDeleted          = "dss_oir_deleted"
	CounterOIRFailed           = "dss_oir_write_failed"
	CounterOIRAdopted          = "dss_oir_adopted"
	CounterKeyConflict         = "dss_key_conflict"
	CounterKeyConflictResolved = "dss_key_conflict_resolved"
	CounterPeerDetails         = "dss_peer_details_fetched"
	CounterPeerDetailsFailed   = "dss_peer_details_failed"
	CounterPeerUnavailable     = "dss_peer_unavailable"
	CounterPeerStoredUsed      = "dss_peer_stored_copy_used"
	CounterConstraintDetails   = "dss_constraint_details_fetched"
	CounterConstraintFailed    = "dss_constraint_details_failed"
	CounterNotified            = "dss_peer_notified"
	CounterNotifyRetried       = "dss_peer_notify_retried"
	CounterNotifyDropped       = "dss_peer_notify_dropped"
	CounterNotifyLate          = "peer_notify_late"
	CounterDisplaced           = "dss_peer_displaced"
	CounterDisplacedInline     = "dss_peer_displaced_notified_inline"
	CounterHeldAvailability    = "dss_writes_held_uss_availability_down"
	CounterHeldDSSDown         = "dss_writes_held_dss_down"
	CounterOutboxUnreadable    = "dss_outbox_unreadable"
	CounterSubscribersRefused  = "dss_subscribers_refused"
	CounterNotConverged        = "dss_mirror_not_converged"
)

// Kinds of the writer's outbox items.
var (
	// OIRKinds are the items of the intents' DSS work.
	OIRKinds = []string{store.OutboxOIRPut, store.OutboxOIRDelete}
	// NotifyKinds are the subscribers' notifications.
	NotifyKinds = []string{store.OutboxPeerNotify}
)

// Timing and bounds of the writer.
const (
	// DefaultMaxBackoff caps the wait before an item is retried.
	DefaultMaxBackoff = 30 * time.Second
	// DefaultNotifyBudget bounds one pass of the notification loop.
	DefaultNotifyBudget = 5 * time.Second
	// MaxNotifyAttempts bounds the attempts of one notification; after the
	// last it is dropped, counted and logged.
	MaxNotifyAttempts = 5
	// notifyRetry is the wait before a notification is tried again.
	notifyRetry = time.Second
	// displacedDeadline is the inline attempt at a displaced peer's
	// notification (PLAN §15 Q16: 900 ms of
	// ConflictingOIMaxUSSNotificationTimeSeconds).
	displacedDeadline = 900 * time.Millisecond
	// maxMirrorSteps bounds the writes one mirror makes before the intent
	// and the DSS agree (a create, an update, a delete and a re-read).
	maxMirrorSteps = 4
)

// errWaiting is a write that cannot be made now; the item is retried.
var errWaiting = errors.New("the write waits")

// Writer mirrors this USSP's intents in the DSS (brief WP-13, the intent
// write protocol): every version of an intent the DSS must hold is an
// outbox item queued in the transaction that wrote it; the writer takes
// the item, reads the intent as it is then, holding its lock
// (store.LockClassOIR, so two writes of one intent never interleave in
// any process), and makes the DSS agree with it: the first write of a
// pending_dss intent (the peers' intents and the constraints the DSS
// names fetched and stored, judged by intent.PeerCheck, then PUT with the
// key of every ovn seen, and the intent authorised once the DSS took it),
// an update to Activated, Nonconforming or Contingent, or a delete. The
// subscribers the DSS lists are told through the outbox (peer_notify);
// a peer this intent displaced is told inline within 900 ms first.
type Writer struct {
	Client  *Client
	Store   Store
	Intents Intents
	// USSBaseURL is ours (uss_base_url of every reference); Manager is
	// our client id at the DSS (the token's sub), by which our own
	// references are told from the peers'.
	USSBaseURL string
	Manager    string
	ForAll     bool
	// Availability stops new writes while the authority holds this USSP
	// Down (*Availability); nil never stops them.
	Availability interface{ Down() bool }
	Counters     *core.Counters
	Logger       *slog.Logger
	Now          func() time.Time
	// Batch (16) items per claim, a claim every Every (1 s); retries no
	// later than MaxBackoff; a notification pass spends at most
	// NotifyBudget.
	Batch        int
	Every        time.Duration
	MaxBackoff   time.Duration
	NotifyBudget time.Duration

	once sync.Once
	kick chan struct{}
}

func (w *Writer) init() {
	w.once.Do(func() { w.kick = make(chan struct{}, 1) })
}

func (w *Writer) count(name string) {
	if w.Counters != nil {
		w.Counters.Inc(name)
	}
}

func (w *Writer) logger() *slog.Logger {
	if w.Logger == nil {
		return obs.Discard()
	}
	return w.Logger
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Writer) every() time.Duration {
	if w.Every <= 0 {
		return time.Second
	}
	return w.Every
}

// Kick makes the notification loop run now.
func (w *Writer) Kick() {
	w.init()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run works through the intents' items and, beside it, the
// notifications, until ctx ends.
func (w *Writer) Run(ctx context.Context) {
	w.init()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { w.runNotify(ctx) })
	t := time.NewTicker(w.every())
	defer t.Stop()
	for {
		if _, err := w.Once(ctx); err != nil && ctx.Err() == nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "DSS outbox not read; retried", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Writer) runNotify(ctx context.Context) {
	t := time.NewTicker(w.every())
	defer t.Stop()
	for {
		if _, err := w.NotifyOnce(ctx); err != nil && ctx.Err() == nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "DSS notification outbox not read; retried", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
	}
}

// Once claims and handles one batch of the intents' items; it returns
// how many it took.
func (w *Writer) Once(ctx context.Context) (int, error) {
	n := w.Batch
	if n <= 0 {
		n = 16
	}
	items, err := w.Store.Claim(ctx, OIRKinds, n)
	if err != nil {
		return 0, err
	}
	for i := range items {
		it := &items[i]
		var p intent.DSSWorkload
		if err := json.Unmarshal(it.Payload, &p); err != nil || p.IntentID != it.EntityID {
			w.count(CounterOutboxUnreadable)
			w.logger().LogAttrs(ctx, slog.LevelError, "DSS outbox item is not readable; it is dropped",
				slog.Int64("outbox_id", it.ID), slog.String("kind", it.Kind), slog.String("intent_id", it.EntityID))
			_ = w.Store.Done(ctx, it.ID)
			continue
		}
		err := w.Mirror(ctx, it.EntityID)
		if err == nil {
			if derr := w.Store.Done(ctx, it.ID); derr != nil {
				w.logger().LogAttrs(ctx, slog.LevelWarn, "DSS item done but not marked; it will be taken again", obs.Err(derr))
			}
			continue
		}
		backoff := w.backoff(it.Attempts)
		if !errors.Is(err, errWaiting) {
			w.count(CounterOIRFailed)
		}
		w.logger().LogAttrs(ctx, slog.LevelWarn, "intent not mirrored in the DSS; retried from the outbox",
			slog.String("intent_id", it.EntityID), slog.Int("attempts", int(it.Attempts)), slog.Duration("retry_in", backoff), obs.Err(err))
		if ferr := w.Store.Fail(ctx, it.ID, err, backoff); ferr != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "DSS item failure not recorded; it is taken again after its lease", obs.Err(ferr))
		}
	}
	return len(items), nil
}

// backoff is 1 s doubled per earlier attempt, at most MaxBackoff.
func (w *Writer) backoff(attempts int32) time.Duration {
	limit := w.MaxBackoff
	if limit <= 0 {
		limit = DefaultMaxBackoff
	}
	d := time.Second
	for i := int32(1); i < attempts && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// WriteNow mirrors the intent now (intent.DSSWriter: the request path
// right after its commit).
func (w *Writer) WriteNow(ctx context.Context, id string) error { return w.Mirror(ctx, id) }

// Mirror makes the DSS hold what the intent asks for, holding the
// intent's lock: at most maxMirrorSteps writes, each re-reading the
// intent and what the DSS holds of it.
func (w *Writer) Mirror(ctx context.Context, id string) error {
	return w.Store.Lock(ctx, store.LockClassOIR, id, func() error {
		for range maxMirrorSteps {
			done, err := w.step(ctx, id)
			if err != nil || done {
				return err
			}
		}
		w.count(CounterNotConverged)
		return fmt.Errorf("intent %s: the DSS and the intent do not agree after %d writes", id, maxMirrorSteps)
	})
}

// step makes one write towards agreement; done when none is needed.
func (w *Writer) step(ctx context.Context, id string) (bool, error) {
	r, err := w.Intents.Record(ctx, id)
	if err != nil || r == nil {
		return true, err
	}
	held, err := w.Intents.Held(ctx, id)
	if err != nil {
		return true, err
	}
	want, write := intent.DSSDesired(r, w.ForAll)
	switch {
	case !write && held == nil:
		return true, nil
	case !write:
		return false, w.delete(ctx, r, held)
	case r.LocalState == intent.StatePendingDSS:
		return false, w.plan(ctx, r, held)
	case held != nil && held.State == want:
		return true, nil
	case held == nil:
		// An authorisation the DSS lost (a 404 to an update): it is
		// written again, Accepted first (the only state a reference is
		// created in), then moved on.
		return false, w.recreate(ctx, r)
	}
	return false, w.update(ctx, r, held, want)
}

// extentsOf are the volumes written for r: its volumes as authorised.
func extentsOf(r *intent.Record) []f3548.Volume4D { return r.Request.Volumes }

// areaOfInterest is one Volume4D holding every extent: their box as a
// polygon, the lowest and highest altitude and the window. The DSS
// query is a prefilter; the deconfliction judges the exact volumes.
func areaOfInterest(ext []f3548.Volume4D) (f3548.Volume4D, error) {
	var out f3548.Volume4D
	if len(ext) == 0 {
		return out, core.Fieldf("extents", "none")
	}
	var box geodesy.BBox
	var from, to time.Time
	lo, hi := math.Inf(1), math.Inf(-1)
	for i, v := range ext {
		b, s, e, err := f3548.Volume4DToZonesEnvelope(v)
		if err != nil {
			return out, err
		}
		if s.IsZero() || e.IsZero() || v.Volume.AltitudeLower == nil || v.Volume.AltitudeUpper == nil {
			return out, core.Fieldf(fmt.Sprintf("extents[%d]", i), "has no window or no altitude band")
		}
		l, err := v.Volume.AltitudeLower.HAEM()
		if err != nil {
			return out, err
		}
		h, err := v.Volume.AltitudeUpper.HAEM()
		if err != nil {
			return out, err
		}
		if i == 0 {
			box, from, to = b, s, e
		} else {
			box = geodesy.BBox{MinLat: math.Min(box.MinLat, b.MinLat), MinLon: math.Min(box.MinLon, b.MinLon),
				MaxLat: math.Max(box.MaxLat, b.MaxLat), MaxLon: math.Max(box.MaxLon, b.MaxLon)}
			if s.Before(from) {
				from = s
			}
			if e.After(to) {
				to = e
			}
		}
		lo, hi = math.Min(lo, l), math.Max(hi, h)
	}
	out.Volume = boxVolume(box, &lo, &hi)
	out.TimeStart = &f3548.Time{Format: f3548.RFC3339, Value: from.UTC()}
	out.TimeEnd = &f3548.Time{Format: f3548.RFC3339, Value: to.UTC()}
	return out, nil
}

// boxVolume is a box as a Volume3D polygon with an optional W84 band.
func boxVolume(b geodesy.BBox, lo, hi *float64) f3548.Volume3D {
	v := f3548.Volume3D{OutlinePolygon: &f3548.Polygon{Vertices: []f3548.LatLngPoint{
		{Lat: b.MinLat, Lng: b.MinLon}, {Lat: b.MinLat, Lng: b.MaxLon}, {Lat: b.MaxLat, Lng: b.MaxLon}, {Lat: b.MaxLat, Lng: b.MinLon},
	}}}
	if lo != nil {
		v.AltitudeLower = &f3548.Altitude{Reference: f3548.W84, Units: f3548.AltitudeUnitsM, Value: *lo}
	}
	if hi != nil {
		v.AltitudeUpper = &f3548.Altitude{Reference: f3548.W84, Units: f3548.AltitudeUnitsM, Value: *hi}
	}
	return v
}

// keyOf is the key of a write of the intent self: the ovn of every
// reference and constraint the DSS names in its extents except self's,
// our own from the DSS's answer, the peers' from their managers (stored as
// peer_intents and constraints on the way), or from the stored copy when
// a manager does not answer; missing names the ones that could not be
// had.
type keyOf struct {
	ovns    []string
	missing []string
	seen    map[string]bool
}

func (k *keyOf) add(ovn string) {
	if k.seen == nil {
		k.seen = map[string]bool{}
	}
	if ovn == "" || k.seen[ovn] {
		return
	}
	k.seen[ovn] = true
	k.ovns = append(k.ovns, ovn)
}

// noOVN is what the DSS writes in place of an ovn it will not tell
// (scdmodels.NoOvnPhrase at the pinned DSS commit).
const noOVN = "Available from USS"

func (w *Writer) gather(ctx context.Context, self string, refs []f3548.OperationalIntentReference, cons []f3548.ConstraintReference, k *keyOf) {
	for i := range refs {
		ref := &refs[i]
		if ref.Id == self {
			continue
		}
		if ref.Manager == w.Manager && ref.Ovn != nil && *ref.Ovn != "" && *ref.Ovn != noOVN {
			k.add(*ref.Ovn)
			continue
		}
		if ovn, ok := w.peerOVN(ctx, *ref); ok {
			k.add(ovn)
		} else {
			k.missing = append(k.missing, intent.PeerPrefix+ref.Id)
		}
	}
	for i := range cons {
		if ovn, ok := w.constraintOVN(ctx, cons[i]); ok {
			k.add(ovn)
		} else {
			k.missing = append(k.missing, intent.ConstraintPrefix+cons[i].Id)
		}
	}
}

// peerOVN fetches a peer's intent from its manager, stores it, and
// returns its ovn; a manager that does not answer marks its stored
// intents peer_unavailable and the stored copy of this one is used when
// it is of the version and the manager the DSS names.
func (w *Writer) peerOVN(ctx context.Context, ref f3548.OperationalIntentReference) (string, bool) {
	oi, err := w.Client.PeerDetails(ctx, ref.UssBaseUrl, ref.Id)
	if err == nil && oi.Reference.Manager != ref.Manager {
		err = &RefusedError{Msg: "the manager's answer names another manager than the DSS"}
	}
	if err == nil {
		rec, rerr := peerRecordOf(oi, ref.UssBaseUrl)
		if rerr == nil {
			_, rerr = w.Store.UpsertPeerIntent(ctx, rec)
		}
		if rerr != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "peer intent not stored; it is not used", slog.String("peer_intent_id", ref.Id), obs.Err(rerr))
			return "", false
		}
		_, _ = w.Store.MarkPeerUnavailable(ctx, ref.UssBaseUrl, false)
		w.count(CounterPeerDetails)
		return *oi.Reference.Ovn, true
	}
	w.count(CounterPeerDetailsFailed)
	if errors.Is(err, ErrDSSDown) {
		if n, merr := w.Store.MarkPeerUnavailable(ctx, ref.UssBaseUrl, true); merr == nil && n > 0 {
			w.count(CounterPeerUnavailable)
		}
	}
	w.logger().LogAttrs(ctx, slog.LevelWarn, "peer intent details not read", slog.String("peer_intent_id", ref.Id),
		slog.String("uss_base_url", ref.UssBaseUrl), obs.Err(err))
	stored, serr := w.Store.PeerIntent(ctx, ref.Id)
	if serr != nil || stored == nil || stored.OVN == "" || stored.Version != int64(ref.Version) || stored.Manager != ref.Manager {
		return "", false
	}
	w.count(CounterPeerStoredUsed)
	return stored.OVN, true
}

// peerRecordOf is a peer's operational intent as peer_intents stores it.
func peerRecordOf(oi *f3548.OperationalIntent, base string) (PeerRecord, error) {
	details, err := json.Marshal(oi.Details)
	if err != nil {
		return PeerRecord{}, err
	}
	rec := PeerRecord{EntityID: oi.Reference.Id, Manager: oi.Reference.Manager, USSBaseURL: base, State: string(oi.Reference.State),
		Version: int64(oi.Reference.Version), TimeStart: oi.Reference.TimeStart.Value.UTC(), TimeEnd: oi.Reference.TimeEnd.Value.UTC(), Details: details}
	if oi.Reference.Ovn != nil {
		rec.OVN = *oi.Reference.Ovn
	}
	if oi.Details.Priority != nil {
		rec.Priority = *oi.Details.Priority
	}
	return rec, nil
}

// constraintOVN fetches a constraint from its manager, stores it, and
// returns its ovn, or the stored copy's of the version the DSS names.
func (w *Writer) constraintOVN(ctx context.Context, ref f3548.ConstraintReference) (string, bool) {
	c, err := w.Client.PeerConstraint(ctx, ref.UssBaseUrl, ref.Id)
	if err == nil && c.Reference.Manager != ref.Manager {
		err = &RefusedError{Msg: "the manager's answer names another manager than the DSS"}
	}
	if err == nil {
		rec, rerr := constraintRecordOf(c, ref.UssBaseUrl)
		if rerr == nil {
			_, rerr = w.Store.UpsertConstraint(ctx, rec)
		}
		if rerr == nil {
			w.count(CounterConstraintDetails)
			return rec.OVN, true
		}
		err = rerr
	}
	w.count(CounterConstraintFailed)
	w.logger().LogAttrs(ctx, slog.LevelWarn, "constraint details not read", slog.String("constraint_id", ref.Id), obs.Err(err))
	stored, serr := w.Store.Constraint(ctx, ref.Id)
	if serr != nil || stored == nil || stored.OVN == "" || stored.Version != int64(ref.Version) || stored.Manager != ref.Manager {
		return "", false
	}
	return stored.OVN, true
}

func constraintRecordOf(c *f3548.Constraint, base string) (ConstraintRecord, error) {
	details, err := json.Marshal(c.Details)
	if err != nil {
		return ConstraintRecord{}, err
	}
	rec := ConstraintRecord{EntityID: c.Reference.Id, Manager: c.Reference.Manager, USSBaseURL: base, Version: int64(c.Reference.Version),
		TimeStart: c.Reference.TimeStart.Value.UTC(), TimeEnd: c.Reference.TimeEnd.Value.UTC(), Details: details}
	if c.Reference.Ovn != nil {
		rec.OVN = *c.Reference.Ovn
	}
	if c.Details.Geozone != nil {
		rec.CISRestrictionID = c.Details.Geozone.Identifier
	}
	return rec, nil
}

// survey queries the DSS for what the extents meet and gathers the key.
func (w *Writer) survey(ctx context.Context, self string, ext []f3548.Volume4D) (*keyOf, error) {
	aoi, err := areaOfInterest(ext)
	if err != nil {
		return nil, err
	}
	refs, err := w.Client.QueryOperationalIntents(ctx, aoi)
	if err != nil {
		return nil, err
	}
	cons, err := w.Client.QueryConstraints(ctx, aoi)
	if err != nil {
		return nil, err
	}
	k := &keyOf{}
	w.gather(ctx, self, refs, cons, k)
	return k, nil
}

// hold records why a pending_dss intent waits and returns errWaiting.
func (w *Writer) hold(ctx context.Context, r *intent.Record, reason, detail string) error {
	if err := w.Intents.DSSHold(ctx, r.ID, r.Version, reason, detail); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s: %s", errWaiting, reason, detail)
}

// holdFor holds r for the failure of a DSS call: unreachable, refused,
// or a key the DSS kept refusing.
func (w *Writer) holdFor(ctx context.Context, r *intent.Record, err error) error {
	var ce *ConflictError
	var re *RefusedError
	switch {
	case errors.Is(err, ErrDSSDown):
		w.count(CounterHeldDSSDown)
		return w.hold(ctx, r, intent.ReasonDSSUnavailable, "the DSS does not answer: "+clipErr(err))
	case errors.As(err, &ce):
		return w.hold(ctx, r, intent.ReasonDSSKeyConflict, clipErr(ce))
	case errors.As(err, &re):
		return w.hold(ctx, r, intent.ReasonDSSRefused, clipErr(re))
	}
	return err
}

// plan is the first write of a pending_dss intent (or its write after a
// modification, held the reference the DSS already has): the survey,
// intent.PeerCheck, the PUT with the key (once more after a 409 with the
// missing ovns fetched and judged), and the authorisation.
func (w *Writer) plan(ctx context.Context, r *intent.Record, held *intent.DSSHeld) error {
	if w.Availability != nil && w.Availability.Down() {
		w.count(CounterHeldAvailability)
		return w.hold(ctx, r, intent.ReasonUSSAvailabilityDown, intent.ReasonUSSAvailabilityDown+": the authority set this USSP's availability Down in the DSS; no new DSS write is made")
	}
	ext := extentsOf(r)
	k, err := w.survey(ctx, r.ID, ext)
	if err != nil {
		return w.holdFor(ctx, r, err)
	}
	if len(k.missing) > 0 {
		return w.hold(ctx, r, intent.ReasonPeerUnavailable, "the details of "+strings.Join(k.missing, ", ")+" could not be read from their manager and no copy of them is held")
	}
	res, displaced, err := w.checkAndPut(ctx, r, held, ext, k)
	var ce *ConflictError
	if errors.As(err, &ce) {
		// 409: someone wrote in the extents after the survey. The ovns it
		// names are fetched, judged and the write is made once more.
		w.count(CounterKeyConflict)
		w.gather(ctx, r.ID, ce.MissingOperationalIntents, ce.MissingConstraints, k)
		if len(k.missing) > 0 {
			return w.hold(ctx, r, intent.ReasonPeerUnavailable, "the details of "+strings.Join(k.missing, ", ")+" could not be read from their manager and no copy of them is held")
		}
		res, displaced, err = w.checkAndPut(ctx, r, held, ext, k)
		if err == nil {
			w.count(CounterKeyConflictResolved)
		}
	}
	if errors.Is(err, errStale) {
		return nil
	}
	if errors.Is(err, errWaiting) {
		return err
	}
	if err != nil {
		if held == nil {
			if rerr := w.recover(ctx, r, ext, err); rerr != nil {
				return rerr
			}
		}
		return w.holdFor(ctx, r, err)
	}
	h := heldOf(res.OperationalIntentReference, ext)
	notes := w.notes(ctx, r, &h, res.Subscribers, displaced)
	authorised, err := w.Intents.DSSAuthorise(ctx, r.ID, r.Version, h, notes)
	if err != nil {
		return err
	}
	if held == nil {
		w.count(CounterOIRCreated)
	} else {
		w.count(CounterOIRUpdated)
	}
	w.logger().LogAttrs(ctx, slog.LevelInfo, "intent written to the DSS", slog.String("intent_id", r.ID),
		slog.Bool("authorised", authorised), slog.Int64("dss_version", h.Version), slog.Int("subscribers", len(notes)),
		slog.Int("displaced", len(displaced)))
	w.displacedNow(ctx, r, &h, notes, displaced)
	w.Kick()
	return nil
}

// errStale: the intent changed while it was judged; the mirror re-reads it.
var errStale = errors.New("the intent changed while it was judged")

// checkAndPut judges the intent (intent.PeerCheck) against what is stored
// now and makes the write; the displaced are the peers' intents it takes
// precedence over.
func (w *Writer) checkAndPut(ctx context.Context, r *intent.Record, held *intent.DSSHeld, ext []f3548.Volume4D, k *keyOf) (*f3548.ChangeOperationalIntentReferenceResponse, []string, error) {
	chk, err := w.Intents.PeerCheck(ctx, r.ID, r.Version)
	if err != nil {
		return nil, nil, err
	}
	switch chk.Outcome {
	case intent.PeerCheckStale, intent.PeerCheckRejected:
		return nil, nil, errStale
	case intent.PeerCheckNotJudged:
		return nil, nil, w.hold(ctx, r, chk.Reason, chk.Detail)
	}
	res, err := w.put(ctx, r.ID, held, f3548.Accepted, ext, k.ovns)
	return res, chk.Displaced, err
}

// put writes the reference in state: a create (held nil, with an
// implicit subscription telling of operational intents and constraints)
// or an update at the ovn held, on the subscription held.
func (w *Writer) put(ctx context.Context, id string, held *intent.DSSHeld, state f3548.OperationalIntentState, ext []f3548.Volume4D, key []string) (*f3548.ChangeOperationalIntentReferenceResponse, error) {
	p := f3548.PutOperationalIntentReferenceParameters{Extents: ext, State: state, UssBaseUrl: w.USSBaseURL}
	if key == nil {
		key = []string{}
	}
	p.Key = &key
	ovn := ""
	if held != nil && held.SubscriptionID != "" {
		ovn = held.OVN
		sub := held.SubscriptionID
		p.SubscriptionId = &sub
	} else {
		if held != nil {
			ovn = held.OVN
		}
		yes := true
		p.NewSubscription = &f3548.ImplicitSubscriptionParameters{UssBaseUrl: w.USSBaseURL, NotifyForConstraints: &yes}
	}
	return w.Client.PutOperationalIntent(ctx, id, ovn, p)
}

// recover reads the reference after a create that failed without an
// AirspaceConflictResponse naming ovns: a create whose answer was lost
// may have been made, and the DSS refuses a second one. A reference the
// DSS holds for us is adopted (its ovn recorded), and the next step
// updates it.
func (w *Writer) recover(ctx context.Context, r *intent.Record, ext []f3548.Volume4D, cause error) error {
	var ce *ConflictError
	if errors.As(cause, &ce) && (len(ce.MissingOperationalIntents) > 0 || len(ce.MissingConstraints) > 0) {
		return nil
	}
	if errors.Is(cause, ErrNotFound) {
		return nil
	}
	ref, err := w.Client.GetOperationalIntent(ctx, r.ID)
	if err != nil || ref.Ovn == nil || *ref.Ovn == "" || *ref.Ovn == noOVN || ref.Manager != w.Manager ||
		strings.TrimRight(ref.UssBaseUrl, "/") != strings.TrimRight(w.USSBaseURL, "/") {
		// Nothing of ours to adopt: the caller holds the intent for the
		// write's own failure.
		return nil //nolint:nilerr // the read is a recovery attempt; its failure is not the write's
	}
	h := heldOf(*ref, ext)
	if err := w.Intents.DSSRecord(ctx, r.ID, &h, nil); err != nil {
		return err
	}
	w.count(CounterOIRAdopted)
	return fmt.Errorf("%w: the DSS holds the reference already (version %d); it is updated next", errWaiting, h.Version)
}

// heldOf is what the DSS holds after an answer naming ref.
func heldOf(ref f3548.OperationalIntentReference, ext []f3548.Volume4D) intent.DSSHeld {
	h := intent.DSSHeld{State: ref.State, Version: int64(ref.Version), SubscriptionID: ref.SubscriptionId, Reference: ref, Extents: ext}
	if ref.Ovn != nil {
		h.OVN = *ref.Ovn
	}
	return h
}

// recreate writes again an authorised intent the DSS no longer holds.
func (w *Writer) recreate(ctx context.Context, r *intent.Record) error {
	ext := extentsOf(r)
	k, err := w.survey(ctx, r.ID, ext)
	if err != nil {
		return err
	}
	res, err := w.put(ctx, r.ID, nil, f3548.Accepted, ext, k.ovns)
	if err != nil {
		if rerr := w.recover(ctx, r, ext, err); rerr != nil {
			return rerr
		}
		return err
	}
	h := heldOf(res.OperationalIntentReference, ext)
	if err := w.Intents.DSSRecord(ctx, r.ID, &h, w.notes(ctx, r, &h, res.Subscribers, nil)); err != nil {
		return err
	}
	w.count(CounterOIRCreated)
	w.Kick()
	return nil
}

// update moves the reference to state (activation, nonconformance,
// contingency, back to Activated), with the key the state needs.
func (w *Writer) update(ctx context.Context, r *intent.Record, held *intent.DSSHeld, state f3548.OperationalIntentState) error {
	ext := held.Extents
	if len(ext) == 0 {
		ext = extentsOf(r)
	}
	var key []string
	k := &keyOf{}
	if state == f3548.Accepted || state == f3548.Activated {
		var err error
		if k, err = w.survey(ctx, r.ID, ext); err != nil {
			return err
		}
		key = k.ovns
	}
	res, err := w.put(ctx, r.ID, held, state, ext, key)
	var ce *ConflictError
	if errors.As(err, &ce) {
		w.count(CounterKeyConflict)
		w.gather(ctx, r.ID, ce.MissingOperationalIntents, ce.MissingConstraints, k)
		if res, err = w.put(ctx, r.ID, held, state, ext, k.ovns); err == nil {
			w.count(CounterKeyConflictResolved)
		}
	}
	if errors.Is(err, ErrNotFound) {
		// The DSS no longer holds it (or not at the ovn held): recorded,
		// and the next step writes it again.
		return w.Intents.DSSRecord(ctx, r.ID, nil, nil)
	}
	if err != nil {
		return err
	}
	h := heldOf(res.OperationalIntentReference, ext)
	if err := w.Intents.DSSRecord(ctx, r.ID, &h, w.notes(ctx, r, &h, res.Subscribers, nil)); err != nil {
		return err
	}
	w.count(CounterOIRUpdated)
	w.logger().LogAttrs(ctx, slog.LevelInfo, "intent state written to the DSS", slog.String("intent_id", r.ID),
		slog.String("dss_state", string(state)), slog.Int("subscribers", len(res.Subscribers)))
	w.Kick()
	return nil
}

// delete removes the reference of an intent the DSS must no longer hold.
func (w *Writer) delete(ctx context.Context, r *intent.Record, held *intent.DSSHeld) error {
	res, err := w.Client.DeleteOperationalIntent(ctx, r.ID, held.OVN)
	if errors.Is(err, ErrNotFound) {
		w.logger().LogAttrs(ctx, slog.LevelWarn, "the DSS no longer holds the intent; recorded as deleted", slog.String("intent_id", r.ID))
		return w.Intents.DSSRecord(ctx, r.ID, nil, nil)
	}
	if err != nil {
		var ce *ConflictError
		if errors.As(err, &ce) {
			// The ovn moved: read the one the DSS holds; the next step
			// deletes at it.
			if ref, gerr := w.Client.GetOperationalIntent(ctx, r.ID); gerr == nil && ref.Ovn != nil && *ref.Ovn != noOVN {
				h := heldOf(*ref, held.Extents)
				// The next step deletes at the ovn now held.
				return w.Intents.DSSRecord(ctx, r.ID, &h, nil)
			}
		}
		return err
	}
	if err := w.Intents.DSSRecord(ctx, r.ID, nil, w.notes(ctx, r, nil, res.Subscribers, nil)); err != nil {
		return err
	}
	w.count(CounterOIRDeleted)
	w.logger().LogAttrs(ctx, slog.LevelInfo, "intent deleted from the DSS", slog.String("intent_id", r.ID))
	w.Kick()
	return nil
}

// PeerNotify is the payload of a peer_notify item: one notification of
// one of our intents to one subscriber (or a peer it displaced).
type PeerNotify struct {
	IntentID  string                                      `json:"intent_id"`
	URL       string                                      `json:"url"`
	Body      f3548.PutOperationalIntentDetailsParameters `json:"body"`
	Displaced bool                                        `json:"displaced,omitempty"`
	QueuedAt  time.Time                                   `json:"queued_at"`
}

// Details is the operational intent our endpoint and our notifications
// give for r as the DSS holds it (held): the reference verbatim and the
// volumes as written, the off-nominal volumes in Nonconforming and
// Contingent (the volumes written: no smaller off-nominal volume is
// known; F3548 requires one), and the priority.
func Details(r *intent.Record, held *intent.DSSHeld) f3548.OperationalIntent {
	pri := r.Priority
	ext := held.Extents
	d := f3548.OperationalIntentDetails{Priority: &pri}
	switch held.State {
	case f3548.Accepted, f3548.Activated:
		empty := []f3548.Volume4D{}
		d.Volumes, d.OffNominalVolumes = &ext, &empty
	case f3548.Nonconforming:
		d.Volumes, d.OffNominalVolumes = &ext, &ext
	case f3548.Contingent:
		empty := []f3548.Volume4D{}
		d.Volumes, d.OffNominalVolumes = &empty, &ext
	default:
		empty := []f3548.Volume4D{}
		d.Volumes, d.OffNominalVolumes = &ext, &empty
	}
	return f3548.OperationalIntent{Reference: held.Reference, Details: d}
}

func sameBase(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

// notes are the notifications of a write (held) or a delete (held nil):
// one per subscriber the DSS listed but ourselves, and one to each
// displaced peer the DSS did not list.
func (w *Writer) notes(ctx context.Context, r *intent.Record, held *intent.DSSHeld, subs []f3548.SubscriberToNotify, displaced []string) []intent.OutboxSpec {
	var oi *f3548.OperationalIntent
	if held != nil {
		d := Details(r, held)
		oi = &d
	}
	displacedAt := map[string]bool{}
	for _, id := range displaced {
		if p, err := w.Store.PeerIntent(ctx, id); err == nil && p != nil {
			displacedAt[strings.TrimRight(p.USSBaseURL, "/")] = true
		}
	}
	now := w.now().UTC()
	var out []intent.OutboxSpec
	listed := map[string]bool{}
	for _, s := range subs {
		if sameBase(s.UssBaseUrl, w.USSBaseURL) || len(s.Subscriptions) == 0 {
			continue
		}
		base := strings.TrimRight(s.UssBaseUrl, "/")
		listed[base] = true
		first := s.Subscriptions[0]
		n := PeerNotify{IntentID: r.ID, URL: s.UssBaseUrl, Displaced: displacedAt[base], QueuedAt: now,
			Body: f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: r.ID, OperationalIntent: oi, Subscriptions: s.Subscriptions}}
		out = append(out, intent.OutboxSpec{Kind: store.OutboxPeerNotify, EntityID: r.ID + "/" + first.SubscriptionId,
			Version: int64(first.NotificationIndex), Payload: n})
	}
	var v int64
	if held != nil {
		v = held.Version
	}
	for base := range displacedAt {
		if listed[base] {
			continue
		}
		n := PeerNotify{IntentID: r.ID, URL: base, Displaced: true, QueuedAt: now,
			Body: f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: r.ID, OperationalIntent: oi, Subscriptions: []f3548.SubscriptionState{}}}
		out = append(out, intent.OutboxSpec{Kind: store.OutboxPeerNotify, EntityID: r.ID + "/displaced/" + base, Version: v, Payload: n})
	}
	return out
}

// displacedNow tells each peer this intent displaced at once, within
// displacedDeadline (ConflictingOIMaxUSSNotificationTimeSeconds, PLAN §15
// Q16), audits the displacement with both references, and leaves a
// notification it could not deliver in time to the outbox, counted
// peer_notify_late.
func (w *Writer) displacedNow(ctx context.Context, r *intent.Record, held *intent.DSSHeld, notes []intent.OutboxSpec, displaced []string) {
	if len(displaced) == 0 {
		return
	}
	for _, id := range displaced {
		w.count(CounterDisplaced)
		p, _ := w.Store.PeerIntent(ctx, id)
		payload := map[string]any{"peer_intent_id": id, "ovn": held.OVN, "dss_version": held.Version}
		if p != nil {
			payload["peer_ovn"], payload["peer_manager"], payload["peer_uss_base_url"] = p.OVN, p.Manager, p.USSBaseURL
		}
		if err := w.Store.Audit(ctx, store.Event{ActorType: store.ActorSystem, ActorID: "dss", EntityType: "operational_intent",
			EntityID: r.ID, EventType: "dss_peer_displaced", Payload: payload}); err != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "displacement not audited", slog.String("intent_id", r.ID), obs.Err(err))
		}
	}
	for _, n := range notes {
		pn, ok := n.Payload.(PeerNotify)
		if !ok || !pn.Displaced {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, displacedDeadline)
		err := w.Client.Notify(dctx, pn.URL, pn.Body)
		cancel()
		if err != nil {
			w.count(CounterNotifyLate)
			w.logger().LogAttrs(ctx, slog.LevelWarn, "a displaced peer was not told within 900 ms; the outbox goes on",
				slog.String("intent_id", r.ID), slog.String("uss_base_url", pn.URL), obs.Err(err))
			continue
		}
		w.count(CounterDisplacedInline)
		w.count(CounterNotified)
		if _, err := w.Store.DoneByKey(ctx, n.Kind, n.EntityID, n.Version); err != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "displaced peer told but its item not marked; it may be told again", obs.Err(err))
		}
	}
}

func (w *Writer) notifyBudget() time.Duration {
	if w.NotifyBudget <= 0 {
		return DefaultNotifyBudget
	}
	return w.NotifyBudget
}

// NotifyOnce posts queued notifications, one claimed at a time, until
// none is due or NotifyBudget is spent; it returns how many it took. A
// notification a subscriber does not take is tried again after
// notifyRetry, and after MaxNotifyAttempts it is dropped, counted and
// logged. One delivered later than UssOiChangeNotificationMaxSeconds
// after it was queued (ConflictingOIMaxUSSNotificationTimeSeconds for a
// displaced peer) is counted peer_notify_late.
func (w *Writer) NotifyOnce(ctx context.Context) (int, error) {
	bctx, cancel := context.WithTimeout(ctx, w.notifyBudget())
	defer cancel()
	taken := 0
	for bctx.Err() == nil {
		items, err := w.Store.Claim(ctx, NotifyKinds, 1)
		if err != nil {
			return taken, err
		}
		if len(items) == 0 {
			return taken, nil
		}
		taken++
		w.notifyItem(ctx, bctx, items[0])
	}
	return taken, nil
}

func (w *Writer) notifyItem(ctx, bctx context.Context, it store.OutboxItem) {
	var n PeerNotify
	err := json.Unmarshal(it.Payload, &n)
	if err == nil {
		err = w.Client.Notify(bctx, n.URL, n.Body)
	} else {
		it.Attempts = MaxNotifyAttempts
	}
	if err == nil {
		w.count(CounterNotified)
		limit := time.Duration(f3548.UssOiChangeNotificationMaxSeconds) * time.Second
		if n.Displaced {
			limit = time.Duration(f3548.ConflictingOIMaxUSSNotificationTimeSeconds) * time.Second
		}
		if !n.QueuedAt.IsZero() && w.now().Sub(n.QueuedAt) > limit {
			w.count(CounterNotifyLate)
		}
		if derr := w.Store.Done(ctx, it.ID); derr != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "notification sent but not marked; it may be sent again", obs.Err(derr))
		}
		return
	}
	if it.Attempts < MaxNotifyAttempts {
		w.count(CounterNotifyRetried)
		if ferr := w.Store.Fail(ctx, it.ID, err, notifyRetry); ferr != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "notification failure not recorded; it is taken again after its lease", obs.Err(ferr))
		}
		return
	}
	w.count(CounterNotifyDropped)
	w.logger().LogAttrs(ctx, slog.LevelWarn, "notification not taken by a subscriber; dropped",
		slog.String("intent_id", n.IntentID), slog.String("subscriber", n.URL), slog.Int("attempts", int(it.Attempts)), obs.Err(err))
	if derr := w.Store.Done(ctx, it.ID); derr != nil {
		w.logger().LogAttrs(ctx, slog.LevelWarn, "notification dropped but not marked; it is taken again", obs.Err(derr))
	}
}
