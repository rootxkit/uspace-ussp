package cis

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed318"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/zones"
)

// Applicability is whether a zone applies at an instant, in the
// vocabulary of cis_applicability (zone/applicable/v1).
type Applicability string

// The three answers. Unknown is never "does not apply": the zone is
// returned, marked.
const (
	Applies       Applicability = "applies"
	NotApplicable Applicability = "not_applicable"
	Unknown       Applicability = "unknown"
)

// Basis is what an answer rests on: the dataset versions, how old the
// oldest confirmation of them is, and whether that is beyond the
// policy's staleness bound (cis_version, cis_age_s, stale on every
// output that rests on this cache; brief WP-4 safety notes).
type Basis struct {
	CISVersion string  `json:"cis_version"`
	CISAgeS    float64 `json:"cis_age_s"`
	Stale      bool    `json:"stale"`
}

// Snapshot is an immutable view of every dataset's current version.
type Snapshot struct {
	versions map[Dataset]*Version
	entries  map[Dataset][]*Entry
	all      []*Entry
	index    *zones.Index
	owner    map[*zones.Zone]partRef
}

type partRef struct {
	entry *Entry
	part  int
}

func newSnapshot(versions map[Dataset]*Version, entries map[Dataset][]*Entry) *Snapshot {
	s := &Snapshot{versions: versions, entries: entries, owner: map[*zones.Zone]partRef{}}
	var parts []*zones.Zone
	for _, d := range ED318Datasets {
		for _, e := range entries[d] {
			s.all = append(s.all, e)
			for k, z := range e.Parts {
				parts = append(parts, z)
				s.owner[z] = partRef{entry: e, part: k}
			}
		}
	}
	s.index = zones.NewIndex(parts)
	return s
}

// with returns a copy of s with d's version and entries replaced.
func (s *Snapshot) with(d Dataset, v *Version, es []*Entry) *Snapshot {
	versions := make(map[Dataset]*Version, len(s.versions)+1)
	entries := make(map[Dataset][]*Entry, len(s.entries)+1)
	for k, x := range s.versions {
		versions[k] = x
	}
	for k, x := range s.entries {
		entries[k] = x
	}
	versions[d] = v
	entries[d] = es
	return newSnapshot(versions, entries)
}

// Version is the current version of d, or nil.
func (s *Snapshot) Version(d Dataset) *Version { return s.versions[d] }

// Entries are the entries of d's current version.
func (s *Snapshot) Entries(d Dataset) []*Entry { return s.entries[d] }

// Counter names of the Evaluator (E-09); the zones package's own
// counters (zone_checks_not_evaluated, zone_limits_not_judged) are added
// to the same set by Result.Count.
const (
	CounterApplicabilityUnknown   = "cis_applicability_unknown"
	CounterContainmentNotJudged   = "cis_containment_not_judged"
	CounterInvalidPointOrEnvelope = "cis_invalid_query"
)

// EvaluatorConfig configures an Evaluator.
type EvaluatorConfig struct {
	// StaleS is the policy's cis_stale_s, read on every call (INV-03).
	StaleS func() float64
	// ZonesPolicy is the zones judgement's policy (pressure margin,
	// CONDITIONAL severity); nil is zones.DefaultPolicy.
	ZonesPolicy func() zones.Policy
	// Daylight resolves ED-318 daylight events; nil is NOAADaylight.
	Daylight ed318.Daylight
	Counters *core.Counters
	Now      func() time.Time
}

// Evaluator answers what the cached CIS says. It is what intent, geo
// and monitor call (a package call, no hop), and is safe for concurrent
// use: the Cache installs a new Snapshot atomically.
type Evaluator struct {
	cfg  EvaluatorConfig
	snap atomic.Pointer[Snapshot]

	mu sync.Mutex
	// contact is when the CISP last confirmed each dataset's current
	// version (a 200, a 304, or a 404 for a dataset never published),
	// on this process's clock.
	contact map[Dataset]time.Time
	// empty marks a dataset the CISP says has no version yet.
	empty map[Dataset]bool
}

// NewEvaluator returns an Evaluator with nothing loaded.
func NewEvaluator(cfg EvaluatorConfig) *Evaluator {
	if cfg.ZonesPolicy == nil {
		cfg.ZonesPolicy = zones.DefaultPolicy
	}
	if cfg.Daylight == nil {
		cfg.Daylight = ed318.NOAADaylight{}
	}
	if cfg.Counters == nil {
		cfg.Counters = &core.Counters{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.StaleS == nil {
		cfg.StaleS = func() float64 { return 0 }
	}
	e := &Evaluator{cfg: cfg, contact: map[Dataset]time.Time{}, empty: map[Dataset]bool{}}
	e.snap.Store(newSnapshot(map[Dataset]*Version{}, map[Dataset][]*Entry{}))
	return e
}

// Counters are the Evaluator's counters.
func (e *Evaluator) Counters() *core.Counters { return e.cfg.Counters }

// Snapshot is the current snapshot.
func (e *Evaluator) Snapshot() *Snapshot { return e.snap.Load() }

// install makes v current for its dataset, confirmed at at.
func (e *Evaluator) install(v *Version, es []*Entry, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.snap.Store(e.snap.Load().with(v.Dataset, v, es))
	e.contact[v.Dataset] = at
	delete(e.empty, v.Dataset)
}

// replace makes v and es current for d without touching when the CISP
// last confirmed it (the ANSP's direct path changed the restrictions,
// not the CISP's version); v nil leaves d unloaded, its entries served.
func (e *Evaluator) replace(d Dataset, v *Version, es []*Entry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.snap.Store(e.snap.Load().with(d, v, es))
}

// confirm records that the CISP confirmed d's current version at at.
// empty is true when the CISP says the dataset has no version yet: an
// empty dataset that is known to be empty.
func (e *Evaluator) confirm(d Dataset, at time.Time, empty bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if empty && e.snap.Load().versions[d] != nil {
		// A version is held: a 404 neither empties the cache nor
		// confirms what it holds (Cache.heldNotFound); it ages.
		return
	}
	e.contact[d] = at
	if empty {
		e.empty[d] = true
	}
}

// DatasetAge is one dataset's version and age.
type DatasetAge struct {
	Dataset Dataset
	// Version is the current version; 0 with Empty when the CISP says
	// there is none, 0 without it when none was ever loaded.
	Version int64
	Empty   bool
	Loaded  bool
	// AgeS is the time since the CISP last confirmed the version;
	// meaningless when !Loaded && !Empty.
	AgeS float64
}

// Ages is the version and age of every dataset, in AllDatasets order.
func (e *Evaluator) Ages() []DatasetAge {
	now := e.cfg.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.snap.Load()
	out := make([]DatasetAge, 0, len(AllDatasets))
	for _, d := range AllDatasets {
		a := DatasetAge{Dataset: d, Empty: e.empty[d]}
		if v := s.versions[d]; v != nil {
			a.Version, a.Loaded = v.Number, true
		}
		if c, ok := e.contact[d]; ok && (a.Loaded || a.Empty) {
			a.AgeS = math.Max(0, now.Sub(c).Seconds())
		}
		out = append(out, a)
	}
	return out
}

// Age is the CIS cache's version, age and staleness over the ED-318
// datasets (zones, uspace_airspace, restrictions): version
// "zones:5,uspace_airspace:3,restrictions:7" (":0" for a dataset the
// CISP says has no version yet), ageS the oldest confirmation, and stale
// true when a dataset was never loaded or ageS exceeds the policy's
// cis_stale_s. With nothing loaded the version is "", ageS 0 and stale
// true: there is no age to report, and nothing is current.
func (e *Evaluator) Age() (version string, ageS float64, stale bool) {
	b := e.basis()
	return b.CISVersion, b.CISAgeS, b.Stale
}

func (e *Evaluator) basis() Basis {
	var labels []string
	var b Basis
	missing := false
	for _, a := range e.Ages() {
		if !a.Dataset.ED318() {
			continue
		}
		if !a.Loaded && !a.Empty {
			missing = true
			continue
		}
		labels = append(labels, versionLabel(a.Dataset, a.Version))
		b.CISAgeS = math.Max(b.CISAgeS, a.AgeS)
	}
	b.CISVersion = joinLabels(labels)
	bound := e.cfg.StaleS()
	b.Stale = missing || !(b.CISAgeS <= bound)
	return b
}

// applicability judges whether entry's part applies at at with
// ed318.Applies at the part's centre. A zero at is "when is not known":
// the zone applies (fail-safe, as zones.Zone.AppliesAt).
func (e *Evaluator) applicability(en *Entry, part int, at time.Time) (Applicability, error) {
	if at.IsZero() {
		return Applies, nil
	}
	ok, err := ed318.Applies(en.Feature.Properties.LimitedApplicability, at.UTC(), en.Centres[part], e.cfg.Daylight)
	switch {
	case err != nil:
		e.cfg.Counters.Inc(CounterApplicabilityUnknown)
		return Unknown, err
	case ok:
		return Applies, nil
	default:
		return NotApplicable, nil
	}
}

// ApplicabilityAt is whether entry's part applies at at, judged by
// ed318.Applies at the part's centre (counted when unknown): what
// GET /v1/geo says of a zone at an instant (brief WP-12).
func (e *Evaluator) ApplicabilityAt(en *Entry, part int, at time.Time) (Applicability, error) {
	if en == nil || part < 0 || part >= len(en.Centres) {
		return Unknown, errors.New("no such part")
	}
	return e.applicability(en, part, at)
}

// PointJudgement is one zone part containing the point that applies (or
// may apply) at the instant, with core's vertical judgement.
type PointJudgement struct {
	Entry *Entry
	Part  int
	Zone  *zones.Zone
	// Applicability is Applies or Unknown; a part that does not apply is
	// left out.
	Applicability Applicability
	// ApplicabilityError says why it is Unknown.
	ApplicabilityError string
	// HorizontalError is set when containment could not be judged
	// (Zone.ContainsHorizontally returned an error): the part is
	// returned, not evaluated, never taken as outside.
	HorizontalError string
	Result          zones.Result
}

// PointResult is JudgePoint's answer.
type PointResult struct {
	Basis
	Zones []PointJudgement
	// Error is set when the point itself is not valid (nothing was
	// judged; never "no zone here").
	Error string
}

// ErrInvalidPoint is the error of a query at an invalid position.
var ErrInvalidPoint = errors.New("the point is not a valid WGS84 position")

// JudgePoint returns the zones of every ED-318 dataset that contain pt
// and apply at at (the aircraft's placed time), each with
// zones.JudgeVertical's result for aircraft in env: within_band,
// limit_not_judged and not_judged come from core (SC-13: a PROHIBITED
// zone with an AGL limit and no terrain warns with not_judged ["AGL"];
// a CONDITIONAL one is not evaluated). Every result is added to the
// Evaluator's counters.
func (e *Evaluator) JudgePoint(pt core.LatLon, aircraft zones.Aircraft, env zones.Env, at time.Time) PointResult {
	out := PointResult{Basis: e.basis()}
	if !pt.Valid() {
		e.cfg.Counters.Inc(CounterInvalidPointOrEnvelope)
		out.Error = ErrInvalidPoint.Error()
		return out
	}
	s := e.snap.Load()
	pol := e.cfg.ZonesPolicy()
	for _, z := range s.index.Candidates(pt) {
		ref := s.owner[z]
		pj := PointJudgement{Entry: ref.entry, Part: ref.part, Zone: z}
		inside, err := z.ContainsHorizontally(pt)
		if err != nil {
			e.cfg.Counters.Inc(CounterContainmentNotJudged)
			pj.HorizontalError = err.Error()
			pj.Applicability = Unknown
			pj.Result = zones.Result{NotEvaluated: true, Reasons: zones.ReasonsOf(zones.ReasonInvalidZone)}
			pj.Result.Count(e.cfg.Counters)
			out.Zones = append(out.Zones, pj)
			continue
		}
		if !inside {
			continue
		}
		a, aerr := e.applicability(ref.entry, ref.part, at)
		if a == NotApplicable {
			continue
		}
		pj.Applicability = a
		if aerr != nil {
			pj.ApplicabilityError = aerr.Error()
		}
		pj.Result = zones.JudgeVertical(z, aircraft, env, pol)
		pj.Result.Count(e.cfg.Counters)
		out.Zones = append(out.Zones, pj)
	}
	return out
}

// AirspaceMatch is one U-space airspace containing a point.
type AirspaceMatch struct {
	Entry *Entry
	Part  int
	// Applicability is Applies or Unknown.
	Applicability      Applicability
	ApplicabilityError string
	// VerticalKnown is false when a limit of the airspace could not be
	// judged (an AGL limit without terrain, a WGS84 one without a geoid)
	// or containment could not be judged: the airspace is returned, as
	// one the point may be inside (fail-safe).
	VerticalKnown bool
	Reasons       zones.Reasons
}

// AirspaceResult is AirspacesAt's answer.
type AirspaceResult struct {
	Basis
	Airspaces []AirspaceMatch
	Error     string
}

// AirspacesAt returns the U-space airspaces (USPACE features of the
// uspace_airspace dataset) containing the point at altAMSLM that apply
// at at: horizontal containment and the vertical limits in the
// airspace's own reference through core zones (an AMSL altitude known
// exactly, AltGeodetic, with no terrain and no geoid: an AGL or WGS84
// limit is not judged and the airspace is kept with VerticalKnown
// false, never left out).
func (e *Evaluator) AirspacesAt(pt core.LatLon, altAMSLM float64, at time.Time) AirspaceResult {
	out := AirspaceResult{Basis: e.basis()}
	if !pt.Valid() || math.IsNaN(altAMSLM) || math.IsInf(altAMSLM, 0) {
		e.cfg.Counters.Inc(CounterInvalidPointOrEnvelope)
		out.Error = "the point or its altitude is not valid"
		return out
	}
	s := e.snap.Load()
	pol := e.cfg.ZonesPolicy()
	ac := zones.Aircraft{AltAMSLM: &altAMSLM, AltSource: core.AltGeodetic}
	for _, z := range s.index.Candidates(pt) {
		ref := s.owner[z]
		if ref.entry.Dataset != USpaceAirspace || ref.entry.Type != core.ZoneUSpace {
			continue
		}
		m := AirspaceMatch{Entry: ref.entry, Part: ref.part}
		inside, err := z.ContainsHorizontally(pt)
		if err != nil {
			e.cfg.Counters.Inc(CounterContainmentNotJudged)
			m.Applicability, m.ApplicabilityError = Unknown, err.Error()
			m.Reasons = zones.ReasonsOf(zones.ReasonInvalidZone)
			out.Airspaces = append(out.Airspaces, m)
			continue
		}
		if !inside {
			continue
		}
		a, aerr := e.applicability(ref.entry, ref.part, at)
		if a == NotApplicable {
			continue
		}
		m.Applicability = a
		if aerr != nil {
			m.ApplicabilityError = aerr.Error()
		}
		r := zones.JudgeVertical(z, ac, zones.Env{}, pol)
		r.Count(e.cfg.Counters)
		switch {
		case r.NotEvaluated || r.LimitNotJudged:
			m.Reasons = r.Reasons
		case r.Raise != nil:
			m.VerticalKnown = true
		default:
			continue // judged outside the airspace's vertical limits
		}
		out.Airspaces = append(out.Airspaces, m)
	}
	return out
}

// JudgeHeightLimit judges a height limit over the ground (for example a
// U-space airspace's airspace_constraints.max_height_agl_m) with core's
// zones.JudgeHeightLimit: only strictly above the limit raises, and
// unknown ground is not evaluated, never judged against 0 (D-04). A nil
// limit is no limit. The result is added to the Evaluator's counters.
func (e *Evaluator) JudgeHeightLimit(aircraft zones.Aircraft, env zones.Env, maxHeightAGLM *float64) zones.Result {
	pol := e.cfg.ZonesPolicy()
	pol.MaxHeightAGLM = maxHeightAGLM
	r := zones.JudgeHeightLimit(aircraft, env, pol)
	r.Count(e.cfg.Counters)
	return r
}

// WindowKind says how a zone applies over a window.
type WindowKind string

// The kinds. A zone that does not apply at any time of the window is
// left out of ZonesFor.
const (
	// WindowAlways: no limitedApplicability; the zone always applies.
	WindowAlways WindowKind = "always"
	// WindowDuring: a period without a daily schedule overlaps the
	// window: the zone applies from From to To (nil: an open end).
	WindowDuring WindowKind = "during"
	// WindowScheduled: only periods with daily schedules overlap the
	// window: the zone applies at the daily times of its schedule
	// between From and To. Judge each instant with Applicability; a
	// strategic check treats it as applying.
	WindowScheduled WindowKind = "scheduled"
)

// ZoneCandidate is one zone part whose bounding box overlaps an
// envelope and whose dates overlap a window.
type ZoneCandidate struct {
	Entry    *Entry
	Part     int
	Zone     *zones.Zone
	Kind     WindowKind
	From, To *time.Time
}

// ZonesResult is ZonesFor's answer.
type ZonesResult struct {
	Basis
	Zones []ZoneCandidate
	Error string
}

// ZonesFor returns the candidate zones for an intent: every part, of
// every ED-318 dataset, whose bounding box overlaps envelope (a
// prefilter; the intent's own volumes are judged against the zone's
// shape through geodesy by the caller) and whose limitedApplicability
// dates overlap [from, to], with how it applies over the window. A zone
// is left out only when every period ends before from or starts after
// to.
func (e *Evaluator) ZonesFor(envelope geodesy.BBox, from, to time.Time) ZonesResult {
	out := ZonesResult{Basis: e.basis()}
	if !validBox(envelope) || to.Before(from) {
		e.cfg.Counters.Inc(CounterInvalidPointOrEnvelope)
		out.Error = "the envelope or the window is not valid"
		return out
	}
	s := e.snap.Load()
	for _, en := range s.all {
		kind, wf, wt, ok := window(en.Feature.Properties.LimitedApplicability, from, to)
		if !ok {
			continue
		}
		for k, z := range en.Parts {
			if !overlaps(z.BBox, envelope) {
				continue
			}
			out.Zones = append(out.Zones, ZoneCandidate{Entry: en, Part: k, Zone: z, Kind: kind, From: wf, To: wt})
		}
	}
	return out
}

// window is how periods apply over [from, to].
func window(tp []ed318.TimePeriod, from, to time.Time) (kind WindowKind, wf, wt *time.Time, ok bool) {
	if len(tp) == 0 {
		return WindowAlways, nil, nil, true
	}
	kind = WindowScheduled
	first := true
	openFrom, openTo := false, false
	for _, p := range tp {
		var s, en *time.Time
		if p.StartDateTime != nil {
			t := p.StartDateTime.Time.UTC()
			s = &t
		}
		if p.EndDateTime != nil {
			t := p.EndDateTime.Time.UTC()
			en = &t
		}
		if (s != nil && s.After(to)) || (en != nil && en.Before(from)) {
			continue
		}
		ok = true
		if len(p.Schedule) == 0 {
			kind = WindowDuring
		}
		// The overlap of the period with the window, widened over the
		// periods that overlap.
		lo, hi := from.UTC(), to.UTC()
		if s != nil && s.After(lo) {
			lo = *s
		}
		if en != nil && en.Before(hi) {
			hi = *en
		}
		if s == nil {
			openFrom = true
		}
		if en == nil {
			openTo = true
		}
		if first || lo.Before(*wf) {
			wf = &lo
		}
		if first || hi.After(*wt) {
			wt = &hi
		}
		first = false
	}
	if !ok {
		return "", nil, nil, false
	}
	if openFrom && !wf.After(from.UTC()) {
		wf = nil
	}
	if openTo && !wt.Before(to.UTC()) {
		wt = nil
	}
	return kind, wf, wt, true
}

func validBox(b geodesy.BBox) bool {
	for _, v := range []float64{b.MinLat, b.MinLon, b.MaxLat, b.MaxLon} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return b.MinLat <= b.MaxLat && b.MinLat >= -90 && b.MaxLat <= 90 &&
		b.MinLon >= -180 && b.MinLon <= 180 && b.MaxLon >= -180 && b.MaxLon <= 180
}

// overlaps reports whether two boxes share a point (a prefilter). A box
// with MinLon above MaxLon crosses the antimeridian and is taken as its
// two halves.
func overlaps(a, b geodesy.BBox) bool {
	if a.MinLat > a.MaxLat || b.MinLat > b.MaxLat || a.MaxLat < b.MinLat || b.MaxLat < a.MinLat {
		return false
	}
	for _, x := range lonSpans(a) {
		for _, y := range lonSpans(b) {
			if x[0] <= y[1] && y[0] <= x[1] {
				return true
			}
		}
	}
	return false
}

func lonSpans(b geodesy.BBox) [][2]float64 {
	if b.MinLon <= b.MaxLon {
		return [][2]float64{{b.MinLon, b.MaxLon}}
	}
	return [][2]float64{{b.MinLon, 180}, {-180, b.MaxLon}}
}
