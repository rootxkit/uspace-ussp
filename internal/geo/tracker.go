package geo

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/alerting"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// The alert/v1 kinds of this path (spec 04 §3.3; the identification
// kinds carry core's names, brief WP-12).
const (
	KindZoneIncursion          = "zone_incursion"
	KindIdentification         = "identification"
	KindIdentificationMismatch = "identification_mismatch"
)

// The alert states and the clear reasons this path adds to core's.
const (
	StateRaised  = "raised"
	StateUpdated = "updated"
	StateCleared = "cleared"
	// ClearNotReconfirmed ends a carried alert core did not judge true
	// again: its aircraft was judged flying for longer than the
	// hysteresis without core raising it, or its zone is no longer
	// published (clearing_detail says which).
	ClearNotReconfirmed = "not_reconfirmed"
)

// Counters of the tracker (E-09).
const (
	CounterRaised           = "geo_zone_alerts_raised"
	CounterCleared          = "geo_zone_alerts_cleared"
	CounterCarried          = "geo_zone_alerts_carried"
	CounterContinued        = "geo_zone_alerts_continued"
	CounterNotReconfirmed   = "geo_zone_alerts_not_reconfirmed"
	CounterRebuilt          = "geo_zone_monitor_rebuilt"
	CounterOtherKind        = "geo_zone_alert_other_kind"
	CounterSavedOverBound   = "geo_zone_alerts_saved_over_bound"
	CounterRestoredAlerts   = "geo_zone_alerts_restored"
	CounterNotJudgedNoZones = "geo_zone_samples_without_zone_set"
	// CounterKeptZoneNotBuilt counts the carried alerts kept at a
	// rebuild although the new set lacks their zone, because the set
	// cannot tell it was withdrawn (the feature did not build, or the set
	// is over its bound).
	CounterKeptZoneNotBuilt = "geo_zone_alerts_kept_zone_not_built"
)

// MaxAlertsPerFlight bounds the zone alerts one flight saves (E-10);
// past it the most severe are kept and the rest counted.
const MaxAlertsPerFlight = 64

// Ref is what an alert names of the flight beside core's aircraft id.
type Ref struct {
	FlightID            string
	IntentID            string
	AuthorisationNumber string
	// Cell5 is where the aircraft was last (the alert's subject).
	Cell5 string
}

// Alert is one zone, identification or identification_mismatch alert
// as this path publishes it.
type Alert struct {
	ID                  string         `json:"alert_id"`
	Key                 string         `json:"key"`
	Kind                string         `json:"kind"`
	Severity            core.Severity  `json:"severity"`
	FlightID            string         `json:"flight_id"`
	IntentID            string         `json:"intent_id,omitempty"`
	AuthorisationNumber string         `json:"authorisation_number,omitempty"`
	Cell5               string         `json:"cell5"`
	Detail              map[string]any `json:"detail"`
	RaisedAt            time.Time      `json:"raised_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	CapturedAt          time.Time      `json:"captured_at"`
	PolicyVersion       int64          `json:"policy_version"`
	// CarriedSince is set while the alert is carried (across a rebuild,
	// a restart or a handover) and core has not judged it true again.
	CarriedSince *time.Time `json:"carried_since,omitempty"`
}

func (a *Alert) clone() Alert {
	c := *a
	c.Detail = maps.Clone(a.Detail)
	if a.CarriedSince != nil {
		t := *a.CarriedSince
		c.CarriedSince = &t
	}
	return c
}

// Event is a raise, an update or a clear of one alert.
type Event struct {
	State          string
	Alert          Alert
	ClearReason    string
	ClearingDetail map[string]any
}

// carry is a carried alert's evidence: since when it is carried, and
// since when its aircraft has been judged flying without core raising
// it (zero: not yet).
type carry struct {
	since         time.Time
	judgedSinceS  float64
	judgedFlights bool
}

// Tracker is the zone path of one monitor worker: one uspace-core
// alerting.Monitor (conflicts skipped: the CPA path is WP-11's) over the
// current zone set, and the alerts it raised, by core's key. It is not
// safe for concurrent use: the worker owns it.
type Tracker struct {
	Counters *core.Counters

	mon     *alerting.Monitor
	set     *ZoneSet
	pv      int64
	polKey  string
	alerts  map[string]*Alert // core key -> alert
	carried map[string]*carry // core key -> carry evidence
	refs    map[string]Ref    // core aircraft id -> its flight
	ids     map[string]string // flight id -> core aircraft id
	lastS   map[string]float64
}

// NewTracker is a tracker with no zones yet.
func NewTracker(counters *core.Counters) *Tracker {
	if counters == nil {
		counters = &core.Counters{}
	}
	return &Tracker{
		Counters: counters, alerts: map[string]*Alert{}, carried: map[string]*carry{},
		refs: map[string]Ref{}, ids: map[string]string{}, lastS: map[string]float64{},
	}
}

// ConfigOf is core's monitor configuration for the zone path under the
// policy v and the set s: conflicts skipped, the zones of s, the zone
// policy (pressure margin, CONDITIONAL severity, no height limit), the
// zone hysteresis and stale time, the ingest-to-monitor bound and the
// ahead tolerance of the other paths.
func ConfigOf(v policy.Values, s *ZoneSet) alerting.Config {
	c := alerting.DefaultConfig()
	c.SkipConflicts = true
	c.ZonePolicy = v.Zones()
	c.ClearAfterS = v.ZoneClearAfterS
	c.StaleAfterS = v.ZoneStaleAfterS
	c.LiveMaxAgeS = v.MonitorLiveMaxAgeS
	c.AheadToleranceS = v.TelemetryAheadToleranceS
	if s != nil {
		c.Zones = s.Zones
	}
	return c
}

func policyKey(r policy.Record) string {
	v := r.Values
	return strconv.FormatInt(r.Version, 10) + "|" + strconv.FormatFloat(v.ZoneClearAfterS, 'g', -1, 64) + "|" +
		strconv.FormatFloat(v.ZoneStaleAfterS, 'g', -1, 64) + "|" + v.ZoneConditionalSeverity + "|" +
		strconv.FormatFloat(v.PressureUncertaintyM, 'g', -1, 64) + "|" + strconv.FormatFloat(v.MonitorLiveMaxAgeS, 'g', -1, 64) + "|" +
		strconv.FormatFloat(v.TelemetryAheadToleranceS, 'g', -1, 64)
}

// Set is the zone set in force (nil before the first Configure).
func (t *Tracker) Set() *ZoneSet { return t.set }

// Configure makes core's monitor judge s under pol. A new set or a new
// policy builds a new monitor (core v1.3.0 has no way to change its
// zones in place): every active alert is then carried, under its id,
// until core judges it again or the evidence ends it; an alert whose
// zone the new set no longer holds ends at once (not_reconfirmed, the
// zone is no longer published). An unchanged set and policy change
// nothing.
func (t *Tracker) Configure(s *ZoneSet, pol policy.Record, now time.Time) []Event {
	pk := policyKey(pol)
	if t.mon != nil && t.set == s && t.polKey == pk {
		return nil
	}
	t.mon = alerting.NewMonitor(ConfigOf(pol.Values, s))
	t.pv = pol.Version
	first := t.set == nil
	t.set, t.polKey = s, pk
	if !first {
		t.Counters.Inc(CounterRebuilt)
	}
	for k, a := range t.alerts {
		if _, ok := t.carried[k]; !ok {
			t.carried[k] = &carry{since: now}
			a.CarriedSince = ptr(now)
			t.Counters.Inc(CounterCarried)
		}
	}
	return t.withdrawn(now)
}

// zoneUnknown reports whether the set lacks a's zone without telling
// that the CIS withdrew it: the set is over its bound (any zone may be
// left out) or a's feature is among those that did not build.
func (t *Tracker) zoneUnknown(a *Alert) bool {
	if t.set.OverBound {
		return true
	}
	id, _ := a.Detail["zone_id"].(string)
	ds, _ := a.Detail["dataset"].(string)
	for _, u := range t.set.Unbuildable {
		if (ds != "" && u == ds+"/"+id) || (ds == "" && strings.HasSuffix(u, "/"+id)) {
			return true
		}
	}
	return false
}

// withdrawn ends the carried zone alerts whose zone part the set no
// longer holds. A set that is not loaded withdraws nothing: an unknown
// CIS is not an empty sky (SC-22). Nor does a set that lacks the zone
// only because its feature did not build or the set is over its bound:
// the alert is kept (counted) and stays carried.
func (t *Tracker) withdrawn(now time.Time) []Event {
	if t.set == nil || !t.set.Loaded {
		return nil
	}
	var out []Event
	for _, k := range slices.Sorted(maps.Keys(t.carried)) {
		a := t.alerts[k]
		if a == nil || a.Kind == KindIdentificationMismatch {
			continue
		}
		part, _ := a.Detail["zone_part"].(string)
		if part == "" || t.set.Has(part) {
			continue
		}
		if t.zoneUnknown(a) {
			t.Counters.Inc(CounterKeptZoneNotBuilt)
			continue
		}
		t.Counters.Inc(CounterNotReconfirmed)
		out = append(out, t.end(k, ClearNotReconfirmed, map[string]any{
			"reason": "zone_no_longer_published", "cis_version": t.set.CISVersion, "carried_since": a.CarriedSince,
		}, now))
	}
	return out
}

// rejections are core's admission counters: a sample that moves one was
// not admitted, so it is no evidence for a carried alert.
var rejections = []string{
	alerting.CounterRejectedBacklog, alerting.CounterRejectedLate, alerting.CounterRejectedOutOfOrder,
	alerting.CounterRejectedPlacedAhead, alerting.CounterRejectedOlderPlacement, alerting.CounterRejectedSourceDisabled,
	alerting.CounterRejectedInvalid, alerting.CounterRejectedCapacity, alerting.CounterRejectedSourceShare,
	alerting.CounterInvalidPosition, alerting.CounterZoneNotEvaluated,
}

func (t *Tracker) rejectionsNow() uint64 {
	var n uint64
	c := t.mon.Counters()
	for _, name := range rejections {
		n += c.Get(name)
	}
	return n
}

// Observe judges one sample of one of this USSP's flights (tr is
// traffic.TrackOf's mapping, ref its flight) at wallS, the worker's
// clock in seconds on the ingest's time base. Without a zone set
// nothing is judged (counted): the caller configures first.
func (t *Tracker) Observe(tr alerting.Track, ref Ref, wallS float64, now time.Time) []Event {
	if t.mon == nil {
		t.Counters.Inc(CounterNotJudgedNoZones)
		return nil
	}
	t.refs[tr.ID] = ref
	t.ids[ref.FlightID] = tr.ID
	before := t.rejectionsNow()
	ev := t.mon.Observe(tr, wallS)
	admitted := t.rejectionsNow() == before
	if admitted {
		t.lastS[tr.ID] = wallS
	}
	out := t.events(ev, now)
	// A carried alert of this aircraft that core did not raise again:
	// an admitted, flying, judged sample is evidence; longer than the
	// hysteresis of it ends the alert. A landing ends it as core ends
	// its own.
	flying := tr.Flying != nil && *tr.Flying
	landed := tr.Flying != nil && !*tr.Flying
	clearAfter := t.mon.Config().ClearAfterS
	// Without a loaded zone set core judges no zone: its silence is no
	// evidence against a carried zone alert (an unknown CIS is not an
	// empty sky, SC-22).
	zonesKnown := t.set != nil && t.set.Loaded
	for _, k := range slices.Sorted(maps.Keys(t.carried)) {
		a := t.alerts[k]
		if a == nil || a.FlightID != ref.FlightID || !admitted {
			continue
		}
		c := t.carried[k]
		mismatch := a.Kind == KindIdentificationMismatch
		// Core's silence is evidence only about a zone it holds: one the
		// set lacks because it did not build or the set is over its bound
		// is not judged (withdrawn kept the alert).
		part, _ := a.Detail["zone_part"].(string)
		held := zonesKnown && t.set.Has(part)
		switch {
		case landed && !mismatch:
			out = append(out, t.end(k, string(alerting.ClearLanded), nil, now))
		case flying && !mismatch && held, mismatch && tr.Identification != nil:
			placed := math.Min(tr.CapturedAtS, wallS)
			if !c.judgedFlights {
				c.judgedSinceS, c.judgedFlights = placed, true
				continue
			}
			if placed-c.judgedSinceS > clearAfter {
				t.Counters.Inc(CounterNotReconfirmed)
				out = append(out, t.end(k, ClearNotReconfirmed, map[string]any{
					"reason": "not_judged_true_again", "carried_since": a.CarriedSince, "judged_for_s": placed - c.judgedSinceS,
				}, now))
			}
		}
	}
	// The alert's subject follows the aircraft.
	for _, a := range t.alerts {
		if a.FlightID == ref.FlightID && ref.Cell5 != "" {
			a.Cell5 = ref.Cell5
		}
	}
	return out
}

// Tick drops what core judges stale and ends the carried alerts of
// aircraft not heard for the stale time (stale, as core ends its own).
func (t *Tracker) Tick(wallS float64, now time.Time) []Event {
	if t.mon == nil {
		return nil
	}
	out := t.events(t.mon.Tick(wallS), now)
	staleAfter := t.mon.Config().StaleAfterS
	for _, k := range slices.Sorted(maps.Keys(t.carried)) {
		a := t.alerts[k]
		if a == nil {
			continue
		}
		last, heard := t.lastS[t.ids[a.FlightID]]
		since := t.carried[k].since
		if !heard {
			last = float64(since.UnixNano()) / 1e9
		}
		if wallS-last > staleAfter {
			out = append(out, t.end(k, string(alerting.ClearStale), nil, now))
		}
	}
	return out
}

// Drop ends every alert of the flight with reason (flight_ended,
// source_disabled, landed) and forgets it.
func (t *Tracker) Drop(flightID, reason string, wallS float64, now time.Time) []Event {
	var out []Event
	if id, ok := t.ids[flightID]; ok && t.mon != nil {
		out = t.events(t.mon.Drop(id, alerting.ClearReason(reason), wallS), now)
	}
	for _, k := range slices.Sorted(maps.Keys(t.alerts)) {
		if t.alerts[k].FlightID == flightID {
			out = append(out, t.end(k, reason, nil, now))
		}
	}
	t.forgetIDs(flightID)
	return out
}

// Forget stops tracking the flight without clearing anything: another
// instance took it over, or it was released to one (its alerts went
// with its saved state).
func (t *Tracker) Forget(flightID string, wallS float64) {
	if id, ok := t.ids[flightID]; ok && t.mon != nil {
		_ = t.mon.Drop(id, alerting.ClearReason("handover"), wallS)
	}
	for k, a := range t.alerts {
		if a.FlightID == flightID {
			delete(t.alerts, k)
			delete(t.carried, k)
		}
	}
	t.forgetIDs(flightID)
}

func (t *Tracker) forgetIDs(flightID string) {
	if id, ok := t.ids[flightID]; ok {
		delete(t.refs, id)
		delete(t.lastS, id)
	}
	delete(t.ids, flightID)
}

// Saved are the flight's active and carried alerts, the most severe
// first, at most MaxAlertsPerFlight (the rest counted).
func (t *Tracker) Saved(flightID string) []Alert {
	var out []Alert
	for _, k := range slices.Sorted(maps.Keys(t.alerts)) {
		if a := t.alerts[k]; a.FlightID == flightID {
			out = append(out, a.clone())
		}
	}
	slices.SortStableFunc(out, func(a, b Alert) int { return rank(a.Severity) - rank(b.Severity) })
	if len(out) > MaxAlertsPerFlight {
		t.Counters.Add(CounterSavedOverBound, uint64(len(out)-MaxAlertsPerFlight))
		out = out[:MaxAlertsPerFlight]
	}
	return out
}

func rank(s core.Severity) int {
	switch s {
	case core.SeverityCritical:
		return 0
	case core.SeverityWarning:
		return 1
	case core.SeverityInfo:
		return 2
	}
	return 3
}

// Restore carries the saved alerts of a flight this worker takes over
// (a restart, a handover) from now: each continues under its id when
// core raises it again, and ends only on evidence (not_reconfirmed,
// landed, stale, source_disabled, flight_ended). An alert already held
// is kept.
func (t *Tracker) Restore(flightID string, saved []Alert, now time.Time) {
	for i := range saved {
		a := saved[i].clone()
		if a.FlightID != flightID || a.Key == "" || a.ID == "" {
			continue
		}
		if _, ok := t.alerts[a.Key]; ok {
			continue
		}
		if a.Detail == nil {
			a.Detail = map[string]any{}
		}
		a.CarriedSince = ptr(now)
		t.alerts[a.Key] = &a
		t.carried[a.Key] = &carry{since: now}
		t.Counters.Inc(CounterRestoredAlerts)
	}
}

// Active are every active and carried alert, for the republish of each
// tick (C-08), ordered by key.
func (t *Tracker) Active() []Alert {
	out := make([]Alert, 0, len(t.alerts))
	for _, k := range slices.Sorted(maps.Keys(t.alerts)) {
		out = append(out, t.alerts[k].clone())
	}
	return out
}

// end clears the alert of key with reason and forgets it.
func (t *Tracker) end(key, reason string, clearing map[string]any, now time.Time) Event {
	a := t.alerts[key]
	delete(t.alerts, key)
	delete(t.carried, key)
	t.Counters.Inc(CounterCleared)
	a.UpdatedAt = now
	return Event{State: StateCleared, Alert: a.clone(), ClearReason: reason, ClearingDetail: clearing}
}

// events maps core's raises and clears onto alerts: a raise of a key
// already held (a carried alert core judged true again, a severity
// change) continues that alert under its id; a new key is a new alert
// whose id derives from the key and the raise time; a clear ends it with
// core's reason.
func (t *Tracker) events(ev alerting.Events, now time.Time) []Event {
	var out []Event
	for i := range ev.Raised {
		r := &ev.Raised[i]
		kind, ok := kindOf(r.Kind)
		if !ok {
			t.Counters.Inc(CounterOtherKind)
			continue
		}
		ref := t.refOf(r.Aircraft)
		if a, held := t.alerts[r.Key]; held {
			state := StateUpdated
			if a.Severity != r.Severity {
				state = StateRaised
			}
			if _, c := t.carried[r.Key]; c {
				delete(t.carried, r.Key)
				a.CarriedSince = nil
				t.Counters.Inc(CounterContinued)
			}
			a.Severity, a.Detail = r.Severity, t.detail(kind, r)
			a.UpdatedAt, a.CapturedAt, a.PolicyVersion = now, timeOfS(r.LastTrueS), t.pv
			if ref.Cell5 != "" {
				a.Cell5 = ref.Cell5
			}
			out = append(out, Event{State: state, Alert: a.clone()})
			continue
		}
		raised := timeOfS(r.RaisedAtS)
		a := &Alert{
			ID: alertID(r.Key, raised), Key: r.Key, Kind: kind, Severity: r.Severity, FlightID: ref.FlightID, IntentID: ref.IntentID,
			AuthorisationNumber: ref.AuthorisationNumber, Cell5: ref.Cell5, Detail: t.detail(kind, r),
			RaisedAt: raised, UpdatedAt: now, CapturedAt: timeOfS(r.LastTrueS), PolicyVersion: t.pv,
		}
		if a.FlightID == "" {
			// Only this USSP's flights reach the tracker; an alert with
			// no flight to name is nobody's to be told.
			t.Counters.Inc(CounterOtherKind)
			continue
		}
		t.alerts[r.Key] = a
		t.Counters.Inc(CounterRaised)
		out = append(out, Event{State: StateRaised, Alert: a.clone()})
	}
	for i := range ev.Cleared {
		c := &ev.Cleared[i]
		if _, held := t.alerts[c.Key]; !held {
			continue
		}
		e := t.end(c.Key, string(c.Reason), c.ClearingDetail, now)
		out = append(out, e)
	}
	return out
}

func (t *Tracker) refOf(aircraft []string) Ref {
	for _, id := range aircraft {
		if r, ok := t.refs[id]; ok {
			return r
		}
	}
	return Ref{}
}

func kindOf(coreKind string) (string, bool) {
	switch coreKind {
	case alerting.KindZone:
		return KindZoneIncursion, true
	case alerting.KindIdentification:
		return KindIdentification, true
	case alerting.KindIdentificationMismatch:
		return KindIdentificationMismatch, true
	}
	// alerting.KindHeight is never produced (no height limit here) and
	// a conflict never (conflicts are skipped): either is counted.
	return "", false
}

// detail is the alert/v1 detail of a raise (04 §3.3): the zone's id,
// type, dataset and versions beside core's judgement (vertical_known,
// limit_not_judged, within_band, not_judged and the heights it judged),
// for an identification alert the status and its reason.
func (t *Tracker) detail(kind string, r *alerting.Alert) map[string]any {
	d := maps.Clone(r.Detail)
	if d == nil {
		d = map[string]any{}
	}
	if kind == KindIdentificationMismatch {
		return d
	}
	part, _ := r.Detail["identifier"].(string)
	d["zone_part"] = part
	if m, ok := t.metaOf(part); ok {
		d["zone_id"] = m.Identifier
		d["zone_type"] = m.Type
		d["dataset"] = m.Dataset
		d["zone_version"] = m.Version
		if m.RestrictionState != "" {
			d["restriction_state"] = m.RestrictionState
		}
		if m.AlwaysApplies {
			d["applicability_not_built"] = true
		}
	} else {
		d["zone_id"] = strings.SplitN(part, "/L", 2)[0]
	}
	if t.set != nil {
		d["cis_version"] = t.set.CISVersion
	}
	if kind == KindZoneIncursion {
		if _, ok := d["vertical_known"]; !ok {
			// Core says nothing when the vertical was judged exactly.
			d["vertical_known"] = true
		}
		if _, ok := d["limit_not_judged"]; !ok {
			d["limit_not_judged"] = false
		}
	}
	return d
}

func (t *Tracker) metaOf(part string) (ZoneMeta, bool) {
	if t.set == nil {
		return ZoneMeta{}, false
	}
	for _, m := range t.set.Meta {
		if m.Part == part {
			return m, true
		}
	}
	return ZoneMeta{}, false
}

// alertID is the id of the alert raised for key at raisedAt: the same
// raise always has the same id, a later raise of the same condition
// another. UUID-shaped (version 4 bits) because the alerts table keys on
// a UUID.
func alertID(key string, raisedAt time.Time) string {
	sum := sha256.Sum256([]byte("zone|" + key + "|" + strconv.FormatInt(raisedAt.UnixNano(), 10)))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// timeOfS is the time of s seconds since the Unix epoch.
func timeOfS(s float64) time.Time {
	if !core.IsFinite(s) {
		return time.Time{}
	}
	sec, frac := math.Modf(s)
	return time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC()
}

func ptr[T any](v T) *T { return &v }
