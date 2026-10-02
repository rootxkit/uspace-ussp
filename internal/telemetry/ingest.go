package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/rid"
	"github.com/rootxkit/uspace-core/serial"
	coresources "github.com/rootxkit/uspace-core/sources"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

// The outcome of one sample: a stable snake_case name, counted under it
// (E-09) and reported to the client in its status frame or batch answer.
const (
	OutcomeAccepted            = "accepted"
	OutcomeDuplicate           = "duplicate"         // taken before: acknowledged, nothing published twice (B-05)
	OutcomeDuplicatePending    = "duplicate_pending" // taken before and still on its way: acknowledged when it lands
	RefusedInvalid             = "refused_invalid"
	RefusedUnbound             = "refused_unbound"
	RefusedBindingsUnavailable = "refused_bindings_unavailable"
	RefusedReplaced            = "refused_replaced"
	RefusedSourceDisabled      = "refused_source_disabled"
	RefusedCapacity            = "refused_capacity"
	RejectedOutOfOrder         = "rejected_out_of_order"
	RejectedTooOld             = "rejected_too_old"
	RefusedIntentState         = "refused_intent_state"
	RefusedNoAuthorisation     = "refused_no_authorisation"
	RefusedBatchSpan           = "refused_batch_span"
	DroppedRate                = "dropped_rate"
	DroppedQueueFull           = CounterDroppedQueueFull
)

// settled reports whether an outcome is final for the client: anything
// but a sample still on its way, or one no queue could take (the client
// sends that one again).
func settled(reason string) bool {
	return reason != OutcomeAccepted && reason != OutcomeDuplicatePending && reason != DroppedQueueFull
}

// Counters of the ingest beside the outcomes.
const (
	CounterTSAheadClamped    = "ts_ahead_clamped"
	CounterPlacedAtReceipt   = "placed_at_receipt_" // + the network rule's note: clock_ahead, too_old, ahead_of_response
	CounterSpacingClamped    = "placement_spacing_clamped"
	CounterPlacedByAnchor    = "placed_by_anchor"
	CounterAnchorKept        = "anchor_kept"
	CounterAnchorRelearnt    = "anchor_relearnt"
	CounterSentAtDisagrees   = "sent_at_disagrees_with_receipt"
	CounterBacklogByAge      = "backlog_by_age"
	CounterBacklog           = "backlog_accepted"
	CounterAltNoGeoid        = "alt_amsl_none_no_geoid"
	CounterAltPressure       = "alt_source_pressure"
	CounterAltNone           = "alt_source_none"
	CounterGeoidFailed       = "geoid_lookup_failed"
	CounterIntentRefused     = "intent_refused"
	CounterAirspaceNotJudged = "uspace_airspace_not_judged"
	CounterSessionReplaced   = "session_replaced"
	CounterDedupeEvicted     = "dedupe_evicted"
	CounterSeqReused         = "seq_reused"
	CounterIdentPublished    = "ident_changes"
	CounterIdentUnavailable  = "ident_registry_unavailable"
	CounterTeleport          = "anomaly_teleport"
)

// FlightBinder is the flight lifecycle (internal/flights.Binder): Bind
// returns the flight a sample of the aircraft key belongs to, starting
// one when there is none or when the intent differs; End ends the
// aircraft's flight. Times are the samples' captured_at.
type FlightBinder interface {
	Bind(key, clientID, uasSerial string, intentID, authorisationNumber, operatorReg *string, capturedAt time.Time, live bool) string
	End(key, reason string, at time.Time)
}

// EventPublisher queues a durable message (Events).
type EventPublisher interface {
	Publish(subject string, m bus.Enveloped)
}

// Config is what an Ingestor reads and writes through.
type Config struct {
	Bindings BindingsSource
	// Intents, Registry and Airspace may be nil: every intent is then
	// unknown, identification registry_unavailable, and the airspace not
	// judged (each counted and on /readyz).
	Intents  IntentSource
	Registry RegistrySource
	Airspace AirspaceJudge
	// Geoid is the undulation; nil is no geoid: no AMSL (R-07, SC-22).
	Geoid geoid.Undulator
	// Sources is the source-control follower; nil enables everything.
	Sources SourceGate
	Policy  func() policy.Record
	Flights FlightBinder
	Outbox  *Outbox
	Events  EventPublisher
	// MaxAircraft bounds the aircraft followed (MaxAircraft).
	MaxAircraft int
	Counters    *core.Counters
	Logger      *slog.Logger
	Now         func() time.Time
}

// Ingestor takes operator samples (spec 02 F5, 05 §5, 06 T3): source
// control, the client's bindings, one session per aircraft (B-14),
// replays, order, rate, time placement, flights, altitude,
// identification, and hands each accepted sample to the Outbox. It is
// shared by every WebSocket session and batch request of the process
// and safe for concurrent use: one aircraft's samples are taken under
// its own lock, in order.
type Ingestor struct {
	cfg    Config
	fleet  *fleet
	stats  *clientStats
	orders sessionOrder
}

// New is an Ingestor of cfg.
func New(cfg Config) *Ingestor {
	if cfg.Counters == nil {
		cfg.Counters = &core.Counters{}
	}
	if cfg.Logger == nil {
		cfg.Logger = obs.Discard()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Policy == nil {
		cfg.Policy = func() policy.Record { return policy.Record{Values: policy.Defaults()} }
	}
	return &Ingestor{cfg: cfg, fleet: newFleet(cfg.MaxAircraft), stats: newClientStats()}
}

// Counters are the ingest's counters.
func (in *Ingestor) Counters() *core.Counters { return in.cfg.Counters }

// Aircraft is the number of aircraft followed.
func (in *Ingestor) Aircraft() int { return in.fleet.len() }

// Delivery is the samples of one client received together: one
// WebSocket message, or one batch request.
type Delivery struct {
	ClientID string
	// Session is the WebSocket session; nil for a batch.
	Session *Session
	RxTS    time.Time
	// SentAt is the client's clock when it sent a batch (its sent_at):
	// the samples are placed against it (PlaceOne). BatchRule places a
	// batch without one by the batch rule (PlaceBatch). A WebSocket
	// message has neither: PlaceOne against rx.
	SentAt    *time.Time
	BatchRule bool
	Frames    []Frame
	// Index is each frame's position in the request (nil: its own).
	Index []int
	// Handed is called once per accepted sample when it was published or
	// written to the work queue (true), or could not be (false). It may
	// run on another goroutine, after Take returned.
	Handed func(index int, f *Frame, handed bool)
}

// Result is the immediate outcome of one sample of a delivery; an
// accepted one is settled later through Delivery.Handed.
type Result struct {
	Index  int
	Serial string
	Seq    int64
	Reason string
	// Detail says more for a refusal (the intent's state, the airspace).
	Detail string
	// Backlog is true for an accepted sample placed as history.
	Backlog bool
}

func (in *Ingestor) count(name string) { in.cfg.Counters.Inc(name) }

// Take takes a delivery and returns one Result per frame, in order.
func (in *Ingestor) Take(ctx context.Context, d Delivery) []Result {
	results := make([]Result, len(d.Frames))
	for i := range d.Frames {
		idx := i
		if d.Index != nil {
			idx = d.Index[i]
		}
		results[i] = Result{Index: idx, Serial: d.Frames[i].Serial, Seq: d.Frames[i].Seq}
	}
	refuseAll := func(idxs []int, reason, detail string) {
		for _, i := range idxs {
			results[i].Reason, results[i].Detail = reason, detail
		}
	}
	all := make([]int, len(d.Frames))
	for i := range all {
		all[i] = i
	}
	if in.cfg.Sources != nil {
		inst := d.ClientID
		if dec := in.cfg.Sources.Query(SourceOperatorWS, &inst); !dec.Enabled {
			refuseAll(all, RefusedSourceDisabled, disabledBy(dec))
			in.record(d, results)
			return results
		}
	}
	folds, _, loaded := in.cfg.Bindings.Folds(d.ClientID)
	groups, order := groupBySerial(d.Frames)
	for _, fold := range order {
		idxs := groups[fold]
		sn := d.Frames[idxs[0]].Serial
		switch {
		case !loaded:
			refuseAll(idxs, RefusedBindingsUnavailable, "the client_bindings projection has not been read")
			continue
		case !contains(folds, fold):
			refuseAll(idxs, RefusedUnbound, "the serial is not bound to this client")
			continue
		}
		ac := in.fleet.get(keyOf(d.ClientID, sn), sn)
		if ac == nil {
			refuseAll(idxs, RefusedCapacity, "this instance follows its maximum of aircraft")
			continue
		}
		in.takeAircraft(ctx, ac, d, idxs, results)
	}
	in.record(d, results)
	return results
}

// disabledBy is how a source is disabled (type, instance or
// default_deny; B-11).
func disabledBy(d coresources.Decision) string {
	if d.WhyDisabled == nil {
		return "disabled"
	}
	return "disabled by " + string(*d.WhyDisabled)
}

// record counts the outcomes and keeps the client's statistics.
func (in *Ingestor) record(d Delivery, results []Result) {
	s := in.stats.get(d.ClientID)
	for _, r := range results {
		in.count(r.Reason)
		s.add(r.Reason, d.RxTS)
		if d.Session != nil {
			d.Session.outcome(r)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// groupBySerial groups the frames by fold key, keeping the order of
// first appearance and the order within each group.
func groupBySerial(frames []Frame) (map[string][]int, []string) {
	groups := map[string][]int{}
	var order []string
	for i := range frames {
		f := serial.FoldKey(frames[i].Serial)
		if _, ok := groups[f]; !ok {
			order = append(order, f)
		}
		groups[f] = append(groups[f], i)
	}
	return groups, order
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// placePolicy is the placement policy of the policy values.
func placePolicy(pol policy.Values) PlacePolicy {
	return PlacePolicy{AheadTolerance: seconds(pol.TelemetryAheadToleranceS), BacklogAfter: seconds(pol.BacklogAfterS),
		AnchorMaxAge: seconds(pol.TelemetryAnchorMaxAgeS), MaxAge: seconds(pol.IngestBacklogMaxS)}
}

// takeAircraft takes the samples idxs of one aircraft, under its lock.
func (in *Ingestor) takeAircraft(ctx context.Context, ac *aircraft, d Delivery, idxs []int, results []Result) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	pol := in.cfg.Policy().Values
	if s := d.Session; s != nil && ac.owner != s.ID {
		if ac.owner != 0 && s.Order < ac.ownerOrder {
			// B-14: a later session took the aircraft; this one is the
			// old one.
			for _, i := range idxs {
				results[i].Reason, results[i].Detail = RefusedReplaced, "a newer session streams this aircraft"
			}
			return
		}
		if ac.owner != 0 {
			in.count(CounterSessionReplaced)
		}
		ac.owner, ac.ownerOrder = s.ID, s.Order
		s.own(ac)
	}
	ac.serial = d.Frames[idxs[0]].Serial
	ac.lastRx = d.RxTS
	frames := make([]Frame, len(idxs))
	for k, i := range idxs {
		frames[k] = d.Frames[i]
	}
	places, next, ps := place(d.RxTS, frames, d.SentAt, d.BatchRule, ac.anchor, placePolicy(pol))
	ac.anchor = next
	in.cfg.Counters.Add(CounterSpacingClamped, uint64(ps.clamped))
	if ps.kept {
		in.count(CounterAnchorKept)
	}
	if ps.relearnt {
		in.count(CounterAnchorRelearnt)
	}
	window := seconds(pol.TelemetryDedupeS)
	for k, i := range idxs {
		f, p := &d.Frames[i], places[k]
		if p.aheadClamped {
			in.count(CounterTSAheadClamped)
		}
		if p.sentAtDisagrees {
			in.count(CounterSentAtDisagrees)
		}
		if p.note != "" {
			in.count(CounterPlacedAtReceipt + string(p.note))
		}
		if !p.shown {
			results[i].Reason, results[i].Detail = RejectedTooOld, "older than ingest_backlog_max_s"
			continue
		}
		dup, pending, reused := ac.duplicate(replayKeyOf(f), f.TS, d.RxTS, window)
		if dup {
			results[i].Reason = OutcomeDuplicate
			if pending {
				results[i].Reason = OutcomeDuplicatePending
			}
			continue
		}
		if reused {
			// The client reused a seq for another sample (a restart that
			// names no new epoch): taken, never acknowledged as a replay.
			in.count(CounterSeqReused)
		}
		st := ac.stream(p.backlog)
		if st.hasHeld && !p.tsEff.After(st.held) {
			results[i].Reason = RejectedOutOfOrder
			continue
		}
		rate := pol.TelemetryRateHz
		if p.backlog {
			rate = pol.TelemetryBacklogRateHz
		}
		if !st.rate.take(d.RxTS, rate, max(1, rate)) {
			results[i].Reason = DroppedRate
			continue
		}
		reason, detail := in.takeSample(ctx, ac, d, i, f, p, st, pol)
		results[i].Reason, results[i].Detail = reason, detail
		if reason == OutcomeAccepted {
			st.held, st.hasHeld = p.tsEff, true
			results[i].Backlog = p.backlog
		}
	}
}

// takeSample builds and hands one sample that passed order and rate.
func (in *Ingestor) takeSample(ctx context.Context, ac *aircraft, d Delivery, i int, f *Frame, p placed, st *stream, pol policy.Values) (string, string) {
	pos := f.Position.LatLon()
	alt, undulation := in.altitude(st, f, p, pol)

	// The intent the sample says it flies, when it may fly it.
	var intent *IntentFacts
	intentDetail := ""
	if f.IntentID != nil {
		intentDetail = in.checkIntent(*f.IntentID, ac.key.fold, &intent)
		if intent == nil {
			in.count(CounterIntentRefused)
		}
	}
	if intent == nil {
		v := AirspaceVerdict{Reason: ReasonCISUnavailable}
		if in.cfg.Airspace != nil {
			env := zones.Env{UndulationM: undulation}
			v = in.cfg.Airspace.At(pos, zones.Aircraft{AltAMSLM: alt.AltAMSLM, AltSource: alt.Source}, env)
		}
		switch {
		case v.Judged && v.Inside:
			reason := RefusedNoAuthorisation
			detail := "inside U-space airspace " + v.AirspaceID + " without an activated intent"
			if f.IntentID != nil {
				reason, detail = RefusedIntentState, intentDetail+"; inside U-space airspace "+v.AirspaceID
			}
			in.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "sample refused inside U-space airspace",
				slog.String("client_id", d.ClientID), slog.String("serial", f.Serial), slog.String("reason", reason),
				slog.String("airspace_id", v.AirspaceID), slog.String("intent", intentDetail))
			return reason, detail
		case !v.Judged:
			in.count(CounterAirspaceNotJudged)
		}
	}
	var intentID, authNo, opReg *string
	if intent != nil {
		id, reg := intent.IntentID, intent.OperatorReg
		intentID, authNo = &id, intent.AuthorisationNumber
		if reg != "" {
			opReg = &reg
		}
	}
	flightID := in.cfg.Flights.Bind(ac.key.String(), d.ClientID, f.Serial, intentID, authNo, opReg, p.capturedAt, !p.backlog)
	_, c3, err := cell.Key(pos)
	if err != nil {
		return RefusedInvalid, "the position has no cell"
	}
	ident := in.identify(f.Serial, opReg, d.RxTS, pol)
	body := TrackBody{
		TrackID: flightID, Trust: core.TrustAuthenticated, Source: SourceOperatorWS, SourceInstance: d.ClientID,
		Position: f.Position, AltWGS84M: f.AltWGS84M, AltAMSLM: alt.AltAMSLM, AltSource: alt.Source, AltPressureM: f.AltPressureM,
		HeightM: f.HeightM, HeightRef: f.HeightRef, SpeedMS: f.SpeedMS, TrackDeg: f.TrackDeg, VSpeedMS: f.VSpeedMS,
		AccuracyHM: horizontalM(f.AccuracyH), AccuracyVM: verticalM(f.AccuracyV), Emergency: f.Emergency,
		Identification: ident, FlightID: &flightID, IntentID: intentID, UndulationM: undulation,
		OperatorPosition: f.OperatorPosition, AccuracyH: f.AccuracyH, AccuracyV: f.AccuracyV,
		TimestampAccuracyS: f.TimestampAccuracyS, Seq: f.Seq,
	}
	status := string(f.Status)
	body.Status = &status
	if in.teleported(st, pos, p.capturedAt, pol.TeleportSpeedMS) {
		a := AnomalyTeleport
		body.Anomaly = &a
		in.count(CounterTeleport)
	}
	ts := f.TS
	times := core.Times{TS: &ts, RxTS: d.RxTS, CapturedAt: p.capturedAt, Source: p.source, Backlog: p.backlog}
	tr := &Track{Envelope: bus.NewEnvelope(SchemaTrack, Producer, times), Body: body}
	subject, err := trackSubject(&tr.Body)
	if err != nil {
		return RefusedInvalid, "the track has no subject"
	}
	in.publishIdent(ac, tr)
	if p.byAnchor {
		in.count(CounterPlacedByAnchor)
	}
	if p.backlog {
		in.count(CounterBacklog)
		if !f.Backlog {
			in.count(CounterBacklogByAge)
		}
	}
	rk := replayKeyOf(f)
	ac.remember(rk, f.TS, d.RxTS, in.cfg.Counters)
	seq, frame, idx := f.Seq, *f, i
	if d.Index != nil {
		idx = d.Index[i]
	}
	h := &handoff{subject: subject, cell3: c3, track: tr, done: func(handed bool) {
		ac.landed(rk, handed)
		if d.Handed != nil {
			d.Handed(idx, &frame, handed)
		}
		if d.Session != nil {
			d.Session.handed(frame.Serial, seq, handed)
		}
	}}
	if d.Session != nil {
		d.Session.pending(f.Serial, f.Seq)
	}
	in.stats.get(d.ClientID).lag(d.RxTS.Sub(p.capturedAt).Seconds())
	if !in.cfg.Outbox.Offer(h) {
		return DroppedQueueFull, "neither the publisher's memory nor the work queue can take it; send it again"
	}
	if f.End {
		in.cfg.Flights.End(ac.key.String(), "operator_ended", p.capturedAt)
	}
	return OutcomeAccepted, ""
}

// teleported reports whether the aircraft moved faster than limitMS
// between the stream's last sample and this one (uspace-core geodesy),
// and makes this one the last. Samples placed at one instant are not
// judged.
func (in *Ingestor) teleported(st *stream, pos core.LatLon, at time.Time, limitMS float64) bool {
	defer func() { st.lastPos, st.lastAt, st.hasLast = pos, at, true }()
	if !st.hasLast {
		return false
	}
	dt := at.Sub(st.lastAt).Seconds()
	if dt <= 0 {
		return false
	}
	d, err := geodesy.DistanceM(st.lastPos, pos)
	if err != nil {
		d = geodesy.HaversineM(st.lastPos, pos)
	}
	return d/dt > limitMS
}

// checkIntent sets *out when the sample may fly the intent id: in
// intent_active, in a flying state, for this aircraft (the intent's
// serial is the sample's, and the serial is bound to this client alone:
// 06 T3). Otherwise it says why.
func (in *Ingestor) checkIntent(id, fold string, out **IntentFacts) string {
	if in.cfg.Intents == nil {
		return "intents_unavailable"
	}
	facts, found, loaded := in.cfg.Intents.Intent(id)
	switch {
	case !loaded:
		return "intents_unavailable"
	case !found:
		return "intent_not_active"
	case !flying(facts.LocalState):
		return "intent_" + facts.LocalState
	case serial.FoldKey(facts.UASSerial) != fold:
		return "intent_not_this_aircraft"
	}
	*out = &facts
	return ""
}

// altitude is the AMSL altitude of a sample: the geoid's undulation at
// its position and uspace-core's selection with the pressure fallback
// and hold of the stream (R-07, R-08). A VerticalAccuracy worse than
// every code (VA150mPlus) is a geodetic altitude never used.
func (in *Ingestor) altitude(st *stream, f *Frame, p placed, pol policy.Values) (rid.AltResult, *float64) {
	var n *float64
	if in.cfg.Geoid != nil {
		v, err := in.cfg.Geoid.UndulationM(f.Position.LatLon())
		if err == nil {
			n = &v
		} else {
			in.count(CounterGeoidFailed)
		}
	}
	if st.altitude == nil {
		st.altitude = rid.NewAltitudeSelector(pol.AltPolicy())
	}
	code, known := verticalCode[f.AccuracyV]
	inp := rid.AltInput{AltHAEM: f.AltWGS84M, AltPressureM: f.AltPressureM, VertAccuracyCode: code, UndulationM: n}
	if !known {
		inp.AltHAEM = nil
	}
	nowS := float64(p.capturedAt.UnixMicro()) / 1e6
	r := st.altitude.Select(inp, nowS)
	switch r.Source {
	case core.AltPressure:
		in.count(CounterAltPressure)
	case core.AltNone:
		in.count(CounterAltNone)
		if in.cfg.Geoid == nil && f.AltWGS84M != nil {
			in.count(CounterAltNoGeoid)
		}
	case core.AltGeodetic, core.AltNetwork:
	}
	return r, n
}

// identify resolves the aircraft through uspace-core's ResolveBound
// against the registry projection (basis authenticated, reason
// session_binding or the registry's): the fleet is the aircraft itself
// and, when it flies an intent, the intent's operator. A projection that
// is missing, or that holds no fresh answer for the aircraft or its
// operator, is registry_unavailable (registry.Lookup), never registered.
func (in *Ingestor) identify(sn string, opReg *string, now time.Time, pol policy.Values) core.Identification {
	droneID := serial.Normalize(sn) //nolint:misspell // uspace-core's API name
	f := registry.Fleet{Aircraft: []registry.FleetAircraft{{DroneID: droneID, Serial: sn}}}
	if opReg != nil {
		f.Operators = []registry.FleetOperator{{ID: *opReg, RegistrationNumber: *opReg}}
		f.Aircraft[0].OperatorID = opReg
	}
	ttl := registry.TTL{Positive: seconds(pol.RegistryPositiveTTLS), Negative: seconds(pol.RegistryNegativeTTLS)}
	var src registry.ProjectionSource
	if in.cfg.Registry != nil {
		src = lookupSource{src: in.cfg.Registry, keys: registry.Keys(f)}
	}
	id := registry.FromProjection(f, src, ttl, now).ResolveBound(droneID)
	if id.Reason == core.ReasonRegistryUnavailable {
		in.count(CounterIdentUnavailable)
	}
	return id
}

// publishIdent announces the track's identification on ident.v1 when it
// changed for the track (a new flight is a new track).
func (in *Ingestor) publishIdent(ac *aircraft, tr *Track) {
	prev := ac.ident
	if ac.identTrack != tr.Body.TrackID {
		prev = nil
	}
	next := tr.Body.Identification
	if !identChanged(prev, next) {
		return
	}
	subject, err := bus.Ident(tr.Body.TrackID)
	if err != nil {
		return
	}
	var before *core.Identification
	if prev != nil {
		p := *prev
		before = &p
	}
	m := &IdentChange{Envelope: tr.Envelope, Body: IdentBody{
		TrackID: tr.Body.TrackID, FlightID: tr.Body.FlightID, Trust: tr.Body.Trust, Source: tr.Body.Source,
		SourceInstance: tr.Body.SourceInstance, Identification: next, Previous: before,
	}}
	m.Schema, m.MsgID = SchemaIdentChange, bus.NewULID(in.cfg.Now())
	if in.cfg.Events != nil {
		in.cfg.Events.Publish(subject, m)
	}
	in.count(CounterIdentPublished)
	ac.ident, ac.identTrack = &next, tr.Body.TrackID
}

// trackSubject is trk.v1.<cell3>.<cell5>.<track_id> of a body: the
// partition cell is the subject's, never a member of the message (D7).
func trackSubject(b *TrackBody) (string, error) {
	c5, _, err := cell.Key(b.Position.LatLon())
	if err != nil {
		return "", err
	}
	return bus.Trk(c5, b.TrackID)
}

// Release clears the aircraft s owns, only where s still owns them
// (B-14: the old session's teardown never disturbs the new one).
func (in *Ingestor) Release(s *Session) {
	for _, ac := range s.owned() {
		ac.mu.Lock()
		if ac.owner == s.ID {
			ac.owner, ac.ownerOrder = 0, 0
		}
		ac.mu.Unlock()
	}
}

// Sweep forgets the aircraft that sent nothing for idle and that no
// session streams; it returns how many it forgot.
func (in *Ingestor) Sweep(idle time.Duration) int {
	now := in.cfg.Now()
	n := 0
	for _, ac := range in.fleet.list() {
		ac.mu.Lock()
		gone := ac.owner == 0 && now.Sub(ac.lastRx) > idle && !ac.inflight()
		ac.mu.Unlock()
		if gone {
			in.fleet.forget(ac)
			n++
		}
	}
	return n
}

// sessionOrder hands out session ids and their opening order.
type sessionOrder struct {
	mu   sync.Mutex
	next uint64
}

func (o *sessionOrder) take() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.next++
	return o.next
}
