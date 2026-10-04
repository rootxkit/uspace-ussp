package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed318"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

// CIS is what the decision reads of the CIS cache: the zones, U-space
// airspaces and restrictions whose boxes overlap a volume in its window
// (internal/cis.Evaluator.ZonesFor), with the basis they rest on, and
// the version the cache holds now (internal/cis.Evaluator.Age), which a
// decision compares at its commit with the version it judged.
type CIS interface {
	ZonesFor(envelope geodesy.BBox, from, to time.Time) cis.ZonesResult
	Age() (version string, ageS float64, stale bool)
}

// CISIntegrity says why the versions in use are known not to be the
// CISP's newest (internal/cis.Cache.Outdated): a newer version held
// untrusted, refused, or notified and not pulled yet. Empty when none.
type CISIntegrity interface {
	Outdated() []string
}

// Registry is the cached F8 lookup (internal/registry.Cache).
type Registry interface {
	Validate(ctx context.Context, qs []registry.Query, p registry.Purpose) ([]registry.Result, error)
}

// Terrain gives the lowest and highest ground (AMSL) under an outline.
// Without it, or when it cannot say, an AGL limit is not judged: a zone
// with one counts as overlapping and an airspace ceiling refuses (Z-09).
type Terrain interface {
	GroundRangeM(s deconflict.Shape) (minM, maxM float64, ok bool)
}

// DSS says whether the strategic coordination write to the DSS can be
// made now (WP-13); "" reason when it can.
type DSS interface {
	Available(ctx context.Context) (bool, string)
}

// Weather is what a decision consults of the weather service (Art.
// 10(3); internal/weather through the api process). It is never a
// judgement: what it says is recorded and becomes conditions the
// operator reads, never a conflict (brief WP-16).
type Weather interface {
	Check(ctx context.Context, boxes []geodesy.BBox, from, to time.Time) WeatherCheck
}

// WeatherCheck is what one consultation found: Ref is
// weather_checked_ref (the products consulted, nil when none),
// Unavailable why none was, Stale why those may not be the newest, and
// Advisories the products whose wind reaches the policy's advisory.
type WeatherCheck struct {
	Ref         *string
	Unavailable string
	Stale       string
	Advisories  []string
}

// MaxWeatherAdvisories bounds the weather_advisory conditions of one
// decision.
const MaxWeatherAdvisories = 16

// Decider runs the decision of brief WP-7 in its order: scope, registry,
// airspace, zones and restrictions (Assess, outside the intents lock),
// then deconfliction and the decision itself (Finish, inside it).
// Every dependency missing is a refusal or a hold, never a pass
// (CLAUDE.md rule 4).
type Decider struct {
	CIS       CIS
	Integrity CISIntegrity
	Registry  Registry
	Terrain   Terrain
	DSS       DSS
	// Weather is consulted on every decision (WP-16); nil records the
	// condition weather_unavailable.
	Weather Weather
	// SystemID is the USSP code of the authorisation number (M8).
	SystemID string
	Counters *core.Counters
}

// Assessment is a decision up to deconfliction.
type Assessment struct {
	n         *Normalised
	pol       policy.Record
	now       time.Time
	d         Decision
	dssOK     bool
	dssReason string
	// overrides are the airspaces' deviation thresholds (the most
	// restrictive applies).
	overrides []Thresholds
}

// Normalised is the request the assessment is about.
func (a *Assessment) Normalised() *Normalised { return a.n }

func (d *Decider) count(name string) {
	if d.Counters != nil {
		d.Counters.Inc("intent_" + name)
	}
}

func ptr[T any](v T) *T { return &v }

func (a *Assessment) conflict(c Conflict) {
	a.d.Conflicts = append(a.d.Conflicts, c)
}

func (a *Assessment) condition(c Condition) {
	for _, x := range a.d.Conditions {
		if x == c {
			return
		}
	}
	a.d.Conditions = append(a.d.Conditions, c)
}

// Assess runs steps 1 to 4 for n under the policy version pol at the
// database time now.
func (d *Decider) Assess(ctx context.Context, n *Normalised, pol policy.Record, now time.Time) *Assessment {
	a := &Assessment{n: n, pol: pol, now: now}
	a.d = Decision{
		ClientRef: n.Request.ClientRef, ExemptArt13: n.Exempt, Priority: n.Priority,
		USpaceAirspaceIDs: []string{}, Conflicts: []Conflict{}, Conditions: []Condition{},
		Alternative: nullJSON, ValidFrom: n.TimeStart, ValidTo: n.TimeEnd, PolicyVersion: pol.Version,
		DecidedAt: now, UpdatedAt: now,
	}
	for i := range n.Volumes {
		a.d.VolumesAMSL = append(a.d.VolumesAMSL, n.Volumes[i].AMSL)
	}
	if n.SpecialUnverified {
		d.count(CondSpecialUnverified)
		a.condition(Condition{Code: CondSpecialUnverified,
			Detail: "flight_type special_operation is recorded as declared; nothing this USSP can check verifies it yet, so the intent is judged at priority 0"})
	}
	// Step 2: the registry (S8), for every intent, exempt or not; it
	// also confirms the class label and the exemption the request claims.
	d.registry(ctx, a)
	// Steps 3 and 4 run for an exempt intent too: it needs no
	// authorisation (Art. 1(3)), but the USSP never accepts, voluntarily
	// or not, a volume over a PROHIBITED zone or an active restriction,
	// nor on a CIS picture it cannot trust (CLAUDE.md rule 4; the brief
	// skips them, see the PR).
	d.cis(a)
	// Art. 10(3): the weather where applicable, consulted and recorded,
	// never a judgement (WP-16).
	d.weather(ctx, a)
	if n.Exempt {
		// Step 1: out of the regulation's scope; deconfliction, the DSS
		// and the number do not apply and nothing is authorised.
		return a
	}
	if a.d.InUSpaceAirspace {
		if d.DSS == nil {
			a.dssReason = "no DSS client is configured"
		} else {
			a.dssOK, a.dssReason = d.DSS.Available(ctx)
		}
	}
	return a
}

// weather consults the products in force over the volumes and their
// window and records their ids in weather_checked_ref; none, a stale
// source and each advisory are conditions, never conflicts.
func (d *Decider) weather(ctx context.Context, a *Assessment) {
	if d.Weather == nil {
		d.count(CondWeatherUnavailable)
		a.condition(Condition{Code: CondWeatherUnavailable, Detail: "no weather service is configured on this process"})
		return
	}
	boxes := make([]geodesy.BBox, len(a.n.Volumes))
	for i := range a.n.Volumes {
		boxes[i] = a.n.Volumes[i].BBox
	}
	c := d.Weather.Check(ctx, boxes, a.n.TimeStart, a.n.TimeEnd)
	a.d.WeatherCheckedRef = c.Ref
	if c.Ref == nil {
		detail := c.Unavailable
		if detail == "" {
			detail = "no weather product was consulted"
		}
		d.count(CondWeatherUnavailable)
		a.condition(Condition{Code: CondWeatherUnavailable, Detail: detail})
	}
	if c.Stale != "" {
		d.count(CondWeatherStale)
		a.condition(Condition{Code: CondWeatherStale, Detail: c.Stale})
	}
	for i, adv := range c.Advisories {
		if i == MaxWeatherAdvisories {
			break
		}
		d.count(CondWeatherAdvisory)
		a.condition(Condition{Code: CondWeatherAdvisory, Detail: adv})
	}
}

func (d *Decider) registry(ctx context.Context, a *Assessment) {
	n := a.n
	hold := func(reason, detail string) {
		d.count(reason)
		a.conflict(Conflict{Kind: KindRegistry, Reason: reason, Effect: EffectHolds, Detail: detail})
	}
	if d.Registry == nil {
		hold(ReasonRegistryUnavailable, "no registry lookup is configured on this process")
		return
	}
	q := registry.Query{Operator: n.Request.OperatorReg, Serial: n.Serial, Pilot: n.Request.PilotRef}
	rs, err := d.Registry.Validate(ctx, []registry.Query{q}, registry.PurposeAuthorisation)
	if err != nil || len(rs) != 1 {
		hold(ReasonRegistryUnavailable, "the registry lookup could not be made")
		return
	}
	oldest := 0.0
	for _, part := range []struct {
		ans    *registry.Answer
		entity string
		item   int
	}{{rs[0].Operator, "operator", 10}, {rs[0].UAS, "uas", 1}, {rs[0].Pilot, "pilot", 0}} {
		if part.ans == nil {
			continue
		}
		if part.ans.CacheAgeS != nil {
			oldest = math.Max(oldest, *part.ans.CacheAgeS)
		}
		var item *int
		if part.item > 0 {
			item = ptr(part.item)
		}
		switch part.ans.Status {
		case registry.StatusValid:
		case registry.StatusSuspended, registry.StatusRevoked:
			reason := part.entity + "_" + string(part.ans.Status)
			d.count(reason)
			a.conflict(Conflict{Kind: KindRegistry, Reason: reason, Effect: EffectRejects, Ref: part.ans.Key, Item: item,
				Detail: fmt.Sprintf("the registry says the %s is %s", part.entity, part.ans.Status)})
		case registry.StatusUnknown:
			d.unknown(a, part.ans, part.entity, item)
		default:
			// A status F8 does not define is never valid.
			d.unknown(a, part.ans, part.entity, item)
		}
	}
	a.d.RegistryCheckedAt = ptr(a.now.Add(-time.Duration(oldest * float64(time.Second))).UTC())
	d.uasClaims(a, rs[0].UAS)
}

// uasClaims checks what the request says of the aircraft against the
// registry's answer for it, so that neither the class label nor the
// Art. 1(3) exemption rests on the request alone: a class_label the
// registry does not hold for the UAS refuses (item 4), and a privately
// built aircraft is exempt only when the registry's MTOM band puts it
// below 250 g. An answer that is not a status (unknown) already holds
// the intent, so nothing is confirmed from it.
func (d *Decider) uasClaims(a *Assessment, u *registry.Answer) {
	if u == nil || u.Status == registry.StatusUnknown || u.Reason != "" {
		return
	}
	r := a.n.Request
	reject := func(reason, detail string) {
		d.count(reason)
		a.conflict(Conflict{Kind: KindRegistry, Reason: reason, Effect: EffectRejects, Ref: u.Key, Item: ptr(4), Detail: detail})
	}
	if r.ClassLabel != "" && !strings.EqualFold(strings.TrimSpace(u.ClassLabel), r.ClassLabel) {
		held := "no class label"
		if u.ClassLabel != "" {
			held = "class label " + u.ClassLabel
		}
		reject(ReasonClassLabelMismatch, fmt.Sprintf("class_label %s is not what the registry holds for the UAS (%s)", r.ClassLabel, held))
		return
	}
	if a.n.Exempt && r.ClassLabel != "C0" {
		// Exempt as privately built below Art13MTOMKg: the registry's
		// band must put the aircraft there.
		if g, ok := bandBelowG(u.MTOMBand); !ok || g > Art13MTOMKg*1000 {
			band := u.MTOMBand
			if band == "" {
				band = "none"
			}
			reject(ReasonExemptionNotConfirmed, fmt.Sprintf("the registry's MTOM band for the UAS (%s) does not put it below %.0f g, so the Art. 1(3) exemption is not confirmed", band, Art13MTOMKg*1000))
		}
	}
}

// bandBelowG reads an authority MTOM band "under_<g>g" as its bound in
// grams; any other band ("from_<g>g", empty, malformed) is false.
func bandBelowG(band string) (float64, bool) {
	rest, ok := strings.CutPrefix(band, "under_")
	if !ok {
		return 0, false
	}
	rest, ok = strings.CutSuffix(rest, "g")
	if !ok || rest == "" {
		return 0, false
	}
	g, err := strconv.ParseFloat(rest, 64)
	if err != nil || !core.IsFinite(g) || g <= 0 {
		return 0, false
	}
	return g, true
}

// unknown holds the intent on an answer that is not a registry status
// of validity: the registry does not hold the key, could not be asked,
// or its answer was refused (02 F5, F8 failure rules).
func (d *Decider) unknown(a *Assessment, ans *registry.Answer, entity string, item *int) {
	reason, detail := ReasonRegistryUnknown, "the registry does not hold the "+entity
	switch ans.Reason {
	case registry.ReasonRegistryUnavailable:
		reason, detail = ReasonRegistryUnavailable, "the authority could not be asked about the "+entity+" and no answer is cached"
	case registry.ReasonAnswerRefused:
		reason, detail = ReasonRegistryRefused, "the answer of the authority about the "+entity+" was refused"
	}
	d.count(reason)
	a.conflict(Conflict{Kind: KindRegistry, Reason: reason, Effect: EffectHolds, Ref: ans.Key, Item: item, Detail: detail})
}

// overlapOf is a deconflict overlap with every member judged.
func overlapOf(o deconflict.Overlap) *Overlap {
	return &Overlap{HM: ptr(o.HM), VM: ptr(o.VM), TS: ptr(o.TS)}
}

// window intersects a volume's window with a zone candidate's (nil ends
// are open); false when they share no instant.
func window(v *Volume, c cis.ZoneCandidate) (float64, bool) {
	start, end := v.Start, v.End
	if c.Kind != cis.WindowAlways {
		if c.From != nil && c.From.After(start) {
			start = *c.From
		}
		if c.To != nil && c.To.Before(end) {
			end = *c.To
		}
	}
	if end.Before(start) {
		return 0, false
	}
	return end.Sub(start).Seconds(), true
}

// zoneShape is the zone's outer outline as a deconfliction shape (holes
// are not subtracted: an intent in a hole is judged as overlapping,
// which refuses more, never less).
func zoneShape(z *zones.Zone) deconflict.Shape {
	if z.Circle != nil {
		c := *z.Circle
		return deconflict.Shape{Circle: &c}
	}
	if z.Polygon == nil || len(z.Polygon.Rings) == 0 {
		return deconflict.Shape{}
	}
	r := z.Polygon.Rings[0]
	if n := len(r); n > 1 && r[0] == r[n-1] {
		r = r[:n-1]
	}
	return deconflict.Shape{Polygon: slices.Clone(r)}
}

// band judges a zone's vertical limits against a volume: whether they
// may overlap (a limit that cannot be judged counts as overlapping), the
// overlap in AMSL when both limits are AMSL or absent, and the
// references not judged.
func (d *Decider) band(z *zones.Zone, v *Volume) (bool, *float64, []string) {
	var ground struct {
		minM, maxM float64
		ok         bool
	}
	if d.Terrain != nil {
		ground.minM, ground.maxM, ground.ok = d.Terrain.GroundRangeM(v.Shape)
		ground.ok = ground.ok && core.IsFinite(ground.minM) && core.IsFinite(ground.maxM)
	}
	var notJudged []string
	reach := func(l *zones.Limit, lower bool) bool {
		if l == nil {
			return true
		}
		if !core.IsFinite(l.ValueM) {
			notJudged = append(notJudged, string(l.Ref))
			return true
		}
		switch l.Ref {
		case core.RefAMSL:
			if lower {
				return v.AMSL.UpperAMSLM >= l.ValueM
			}
			return v.AMSL.LowerAMSLM <= l.ValueM
		case core.RefWGS84:
			if lower {
				return v.AMSL.UpperW84M >= l.ValueM
			}
			return v.AMSL.LowerW84M <= l.ValueM
		case core.RefAGL:
			if lower && l.ValueM <= 0 {
				return true
			}
			if !ground.ok {
				notJudged = append(notJudged, string(l.Ref))
				return true
			}
			if lower {
				return v.AMSL.UpperAMSLM >= ground.minM+l.ValueM
			}
			return v.AMSL.LowerAMSLM <= ground.maxM+l.ValueM
		}
		notJudged = append(notJudged, string(l.Ref))
		return true
	}
	lo, hi := reach(z.Lower, true), reach(z.Upper, false)
	if !lo || !hi {
		return false, nil, nil
	}
	amsl := func(l *zones.Limit) bool { return l == nil || l.Ref == core.RefAMSL && core.IsFinite(l.ValueM) }
	var vm *float64
	if amsl(z.Lower) && amsl(z.Upper) {
		top, bottom := v.AMSL.UpperAMSLM, v.AMSL.LowerAMSLM
		if z.Upper != nil {
			top = math.Min(top, z.Upper.ValueM)
		}
		if z.Lower != nil {
			bottom = math.Max(bottom, z.Lower.ValueM)
		}
		vm = ptr(top - bottom)
	}
	slices.Sort(notJudged)
	return true, vm, slices.Compact(notJudged)
}

// meets judges one candidate against one volume in time, space and
// altitude.
func (d *Decider) meets(c cis.ZoneCandidate, v *Volume) (bool, *Overlap, []string) {
	ts, ok := window(v, c)
	if !ok {
		return false, nil, nil
	}
	within, sep, err := deconflict.Within(v.Shape, zoneShape(c.Zone), 0)
	var notJudged []string
	var hm *float64
	switch {
	case err != nil:
		// An outline that cannot be judged counts as meeting (Z-09).
		notJudged = append(notJudged, "outline")
	case !within:
		return false, nil, nil
	default:
		hm = ptr(sep)
	}
	ok, vm, nj := d.band(c.Zone, v)
	if !ok {
		return false, nil, nil
	}
	return true, &Overlap{HM: hm, VM: vm, TS: ptr(ts)}, append(notJudged, nj...)
}

// messages are a zone's ED-318 message texts.
func messages(e *cis.Entry) []ed318.Text {
	if e.Feature == nil {
		return nil
	}
	return e.Feature.Properties.Message
}

// textOf is the English text of a message, else the first one.
func textOf(ts []ed318.Text) string {
	for _, t := range ts {
		if t.Text != nil && t.Lang == "en-GB" || t.Text != nil && t.Lang == "en" {
			return *t.Text
		}
	}
	for _, t := range ts {
		if t.Text != nil {
			return *t.Text
		}
	}
	return ""
}

func (d *Decider) cis(a *Assessment) {
	reject := func(reason, ref, detail string) {
		d.count(reason)
		a.conflict(Conflict{Kind: KindCIS, Reason: reason, Effect: EffectRejects, Ref: ref, Detail: detail})
	}
	if d.CIS == nil {
		reject(ReasonCISUnavailable, "", "no CIS cache is configured on this process")
		return
	}
	if d.Integrity == nil {
		reject(ReasonCISOutdated, "", "whether the CIS versions in use are current cannot be told on this process")
		return
	}
	if out := d.Integrity.Outdated(); len(out) > 0 {
		reject(ReasonCISOutdated, "", "the CIS versions in use are not the newest: "+strings.Join(out, "; "))
		return
	}
	results := make([]cis.ZonesResult, len(a.n.Volumes))
	for i := range a.n.Volumes {
		v := &a.n.Volumes[i]
		results[i] = d.CIS.ZonesFor(v.BBox, v.Start, v.End)
		r := results[i]
		if i == 0 {
			a.d.CISVersionChecked = ptr(r.CISVersion)
			a.d.CISAgeS = ptr(r.CISAgeS)
		}
		switch {
		case r.Error != "":
			reject(ReasonCISUnavailable, "", "the CIS cache did not answer for volumes["+fmt.Sprint(i)+"]: "+r.Error)
			return
		case r.Stale:
			// 02 F3: stale beyond the bound refuses. Whether the volume is
			// inside U-space airspace is itself read from the stale cache,
			// so a stale CIS refuses everywhere.
			reject(ReasonCISStale, r.CISVersion, fmt.Sprintf("the CIS cache is stale (age %.0f s beyond the policy's cis_stale_s, or a dataset never loaded)", r.CISAgeS))
			return
		}
	}
	// Step 3: the U-space airspaces and their constraints.
	seen := map[string]bool{}
	for i := range a.n.Volumes {
		v := &a.n.Volumes[i]
		for _, c := range results[i].Zones {
			if c.Entry.Dataset != cis.USpaceAirspace || c.Entry.Type != core.ZoneUSpace {
				continue
			}
			ok, ov, _ := d.meets(c, v)
			if !ok {
				continue
			}
			id := c.Entry.Identifier
			if !seen[id] {
				seen[id] = true
				a.d.USpaceAirspaceIDs = append(a.d.USpaceAirspaceIDs, id)
			}
			a.d.InUSpaceAirspace = true
			d.airspace(a, c.Entry, v, ov)
		}
	}
	slices.Sort(a.d.USpaceAirspaceIDs)
	// Step 4: zones and restrictions (Art. 10(7)).
	for i := range a.n.Volumes {
		v := &a.n.Volumes[i]
		for _, c := range results[i].Zones {
			if c.Entry.Dataset == cis.USpaceAirspace {
				continue
			}
			ok, ov, nj := d.meets(c, v)
			if !ok {
				continue
			}
			d.zone(a, c, v, ov, nj)
		}
	}
}

// airspace applies an overlapping U-space airspace's Art. 3(4)
// constraints: its ceiling over the ground caps the volume, and its
// service performance may tighten the deviation thresholds.
func (d *Decider) airspace(a *Assessment, e *cis.Entry, v *Volume, ov *Overlap) {
	vol := ptr(v.Index)
	if e.Requirements == nil {
		d.count(ReasonRequirementsUnread)
		a.conflict(Conflict{Kind: KindAirspace, Reason: ReasonRequirementsUnread, Effect: EffectRejects, Ref: e.Identifier, Item: ptr(5), Volume: vol, Overlap: ov,
			Detail: "the Art. 3(4) requirements of the U-space airspace cannot be read (" + e.RequirementsProblem + "), so its constraints cannot be applied"})
		return
	}
	a.condition(Condition{Code: CondAirspaceRequirements, Ref: e.Identifier, Detail: "the Art. 3(4) requirements of the U-space airspace apply"})
	if ceiling := e.Requirements.AirspaceConstraints.MaxHeightAglM; ceiling != nil {
		minG, _, ok := 0.0, 0.0, false
		if d.Terrain != nil {
			minG, _, ok = d.Terrain.GroundRangeM(v.Shape)
			ok = ok && core.IsFinite(minG)
		}
		switch {
		case !core.IsFinite(*ceiling) || *ceiling <= 0:
			d.count(ReasonCeilingNotJudged)
			a.conflict(Conflict{Kind: KindAirspace, Reason: ReasonCeilingNotJudged, Effect: EffectRejects, Ref: e.Identifier, Item: ptr(5), Volume: vol, Overlap: ov,
				Detail: fmt.Sprintf("the U-space airspace's max_height_agl_m %v cannot be applied", *ceiling)})
		case !ok:
			d.count(ReasonCeilingNotJudged)
			a.conflict(Conflict{Kind: KindAirspace, Reason: ReasonCeilingNotJudged, Effect: EffectRejects, Ref: e.Identifier, Item: ptr(5), Volume: vol, Overlap: ov,
				Detail: fmt.Sprintf("the ground under the volume is not known, so its height against the U-space airspace's ceiling of %.1f m above the ground cannot be judged", *ceiling)})
		case v.AMSL.UpperAMSLM-minG > *ceiling:
			d.count(ReasonCeilingExceeded)
			a.conflict(Conflict{Kind: KindAirspace, Reason: ReasonCeilingExceeded, Effect: EffectRejects, Ref: e.Identifier, Item: ptr(5), Volume: vol, Overlap: ov,
				Detail: fmt.Sprintf("the volume reaches %.1f m above the lowest ground under it, above the U-space airspace's ceiling of %.1f m", v.AMSL.UpperAMSLM-minG, *ceiling)})
		}
	}
	if t, ok := thresholdsOf(e.Requirements.Raw); ok {
		a.overrides = append(a.overrides, t)
	}
}

// thresholdsOf reads deviation thresholds an airspace's
// service_performance may carry (deviation_h_m, deviation_v_m,
// deviation_t_s, all three positive), and false otherwise.
func thresholdsOf(raw json.RawMessage) (Thresholds, bool) {
	var doc struct {
		ServicePerformance struct {
			HM *float64 `json:"deviation_h_m"`
			VM *float64 `json:"deviation_v_m"`
			TS *float64 `json:"deviation_t_s"`
		} `json:"service_performance"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return Thresholds{}, false
	}
	sp := doc.ServicePerformance
	for _, p := range []*float64{sp.HM, sp.VM, sp.TS} {
		if p == nil || !core.IsFinite(*p) || *p <= 0 {
			return Thresholds{}, false
		}
	}
	return Thresholds{HM: *sp.HM, VM: *sp.VM, TS: *sp.TS}, true
}

func (d *Decider) zone(a *Assessment, c cis.ZoneCandidate, v *Volume, ov *Overlap, notJudged []string) {
	e := c.Entry
	t := c.Zone.Type
	if t == "" {
		t = e.Type
	}
	vol := ptr(v.Index)
	detail := ""
	if len(notJudged) > 0 {
		detail = " (not judged: " + strings.Join(notJudged, ", ") + "; counted as overlapping)"
	}
	add := func(kind, reason, effect, text string) {
		d.count(reason)
		a.conflict(Conflict{Kind: kind, Reason: reason, Effect: effect, Ref: e.Identifier, Item: ptr(5), Volume: vol, Overlap: ov, Detail: text + detail})
	}
	if e.Dataset == cis.Restrictions {
		if t == core.ZoneNoRestriction {
			return
		}
		if e.Planned() {
			// Planned, not in force (02 F2): told, never refused. Its
			// activation is a new version, and the standing re-check
			// withdraws or marks this intent then (Art. 10(10)).
			a.condition(Condition{Code: CondRestrictionPlanned, Ref: e.Identifier,
				Detail: "the volume overlaps a restriction the ANSP has planned and not activated; if it is activated, the authorisation is withdrawn"})
			return
		}
		if !e.InForce() {
			return
		}
		add(KindRestriction, ReasonRestrictionActive, EffectRejects, "the volume overlaps a restriction of the ANSP in space, altitude and time")
		return
	}
	switch t {
	case core.ZoneNoRestriction, core.ZoneUSpace:
		return
	case core.ZoneProhibited:
		add(KindZone, ReasonZoneProhibited, EffectRejects, "the volume overlaps a PROHIBITED zone in space, altitude and time")
	case core.ZoneReqAuthorization:
		if a.n.Request.AuthorisationRef != "" {
			a.condition(Condition{Code: CondAuthorisationRef, Ref: e.Identifier,
				Detail: "the zone requires an authorisation; the flight relies on authorisation_ref " + a.n.Request.AuthorisationRef})
			return
		}
		add(KindZone, ReasonZoneReqAuthorisation, EffectHolds, "the volume overlaps a zone that requires an authorisation, and no authorisation_ref is given")
	case core.ZoneConditional:
		msg := textOf(messages(e))
		if msg == "" {
			msg = "the zone's conditions apply"
		}
		a.condition(Condition{Code: CondConditionalZone, Ref: e.Identifier, Detail: msg})
		if len(notJudged) > 0 {
			a.condition(Condition{Code: CondLimitNotJudged, Ref: e.Identifier, Detail: "a limit of the zone could not be judged: " + strings.Join(notJudged, ", ")})
		}
	default:
		add(KindZone, ReasonZoneUnknownType, EffectRejects, fmt.Sprintf("the volume overlaps a zone of a type this USSP does not know (%q); it is enforced", string(t)))
	}
}

// Finish runs steps 5 and 6: deconfliction against others (the active
// local intents and the peers' intents whose envelope and window overlap)
// and the decision. id is the intent id, rankAt when its volumes were
// filed. It returns the decision and the ids of the authorisations this
// one takes precedence over (to flag for an update, Art. 10(10)).
func (d *Decider) Finish(a *Assessment, id string, rankAt time.Time, others []deconflict.Intent) (Decision, []string) {
	a.d.IntentID = id
	var flagged []string
	if !a.n.Exempt {
		flagged = d.deconflict(a, id, rankAt, others)
	}
	effects := map[string]bool{}
	for _, c := range a.d.Conflicts {
		effects[c.Effect] = true
	}
	holds := func(kind string) bool {
		for _, c := range a.d.Conflicts {
			if c.Effect == EffectHolds && c.Kind == kind {
				return true
			}
		}
		return false
	}
	switch {
	case effects[EffectRejects]:
		a.d.Decision, a.d.State = DecisionRejected, StateRejected
		flagged = nil
	case holds(KindRegistry):
		a.d.Decision, a.d.State = DecisionPendingValidation, StatePendingValidation
		flagged = nil
	case holds(KindZone):
		a.d.Decision, a.d.State = DecisionPendingAuthority, StatePendingAuthority
		flagged = nil
	case a.n.Exempt:
		a.d.Decision, a.d.State = DecisionAcceptedVoluntary, StateAccepted
	default:
		d.authorise(a)
		if a.d.Decision != DecisionAuthorised {
			flagged = nil
		}
	}
	d.count("decision_" + a.d.Decision)
	return a.d, flagged
}

func (d *Decider) deconflict(a *Assessment, id string, rankAt time.Time, others []deconflict.Intent) []string {
	p := deconflict.Policy{HorizontalBufferM: a.pol.Values.DeconflictBufferM, VerticalBufferM: a.pol.Values.DeconflictVerticalBufferM}
	mine := deconflict.Intent{ID: id, Priority: a.n.Priority, RankAt: rankAt}
	for i := range a.n.Volumes {
		mine.Volumes = append(mine.Volumes, a.n.Volumes[i].Deconflict())
	}
	cs, err := deconflict.Check(mine, others, p)
	if err != nil {
		reason := ReasonDeconflictNotJudged
		if p.Validate() != nil {
			reason = ReasonThresholdInvalid
		}
		d.count(reason)
		a.conflict(Conflict{Kind: KindPolicy, Reason: reason, Effect: EffectRejects, Detail: "the strategic deconfliction did not run: " + reasonOf(err)})
		return nil
	}
	out, flagged := d.conflictsOf(cs)
	for _, c := range out {
		a.conflict(c)
	}
	return flagged
}

// PeerPrefix and ConstraintPrefix name the peers' intents and the F3548
// constraints among the intents a decision is checked against, so that a
// conflict with one names what it is (WP-13).
const (
	PeerPrefix       = "peer:"
	ConstraintPrefix = "constraint:"
)

// conflictsOf turns the deconfliction's pairs into conflicts[]: a pair
// this intent wins by priority flags the other (Art. 10(8), (10)) and
// names it in flagged; every other pair refuses this intent. A
// constraint always refuses (it is a restriction, never displaced).
func (d *Decider) conflictsOf(cs []deconflict.Conflict) ([]Conflict, []string) {
	var out []Conflict
	var flagged []string
	for _, c := range cs {
		ov := overlapOf(c.Overlap)
		if strings.HasPrefix(c.OtherID, ConstraintPrefix) {
			d.count(ReasonConstraintActive)
			out = append(out, Conflict{Kind: KindConstraint, Reason: ReasonConstraintActive, Effect: EffectRejects,
				Ref: strings.TrimPrefix(c.OtherID, ConstraintPrefix), Item: ptr(5), Volume: ptr(c.MineVolume), Overlap: ov,
				Detail: "the volume overlaps an F3548 constraint the DSS holds (an airspace restriction)"})
			continue
		}
		// Only priority lets a request take space another intent already
		// holds (Art. 10(8), (10)). At equal priority the others were
		// accepted before this decision, so they came first whatever the
		// ranks say (a clock stepped back, a tie broken on the id): the
		// request is refused, never granted over them.
		if c.MineWins && c.Rule == deconflict.RulePriority {
			flagged = append(flagged, c.OtherID)
			d.count(ReasonIntentFlagged)
			out = append(out, Conflict{Kind: KindIntent, Reason: ReasonIntentFlagged, Effect: EffectFlagsOther, Ref: c.OtherID, Volume: ptr(c.MineVolume), Overlap: ov,
				Detail: "this intent has precedence (" + c.Rule + "); the other authorisation is flagged for an update (Art. 10(10))"})
			continue
		}
		reason := ReasonIntentFirstCome
		if c.Rule == deconflict.RulePriority {
			reason = ReasonIntentPriority
		}
		if c.MineWins {
			d.count("first_come_rank_inverted")
		}
		d.count(reason)
		out = append(out, Conflict{Kind: KindIntent, Reason: reason, Effect: EffectRejects, Ref: c.OtherID, Item: ptr(5), Volume: ptr(c.MineVolume), Overlap: ov,
			Detail: "the volume conflicts with an authorised intent that has precedence (" + c.Rule + ")"})
	}
	return out, flagged
}

// authorise is step 6: the thresholds, the DSS and the number.
func (d *Decider) authorise(a *Assessment) {
	pv := a.pol.Values
	th := Thresholds{HM: pv.DeviationHM, VM: pv.DeviationVM, TS: pv.DeviationTS}
	for _, o := range a.overrides {
		th = Thresholds{HM: math.Min(th.HM, o.HM), VM: math.Min(th.VM, o.VM), TS: math.Min(th.TS, o.TS)}
	}
	for _, v := range []float64{th.HM, th.VM, th.TS} {
		if !core.IsFinite(v) || v <= 0 {
			// E-15: a zero or invalid threshold refuses, never disarms.
			d.count(ReasonThresholdInvalid)
			a.conflict(Conflict{Kind: KindPolicy, Reason: ReasonThresholdInvalid, Effect: EffectRejects,
				Detail: fmt.Sprintf("the deviation thresholds %+v of policy version %d cannot be used", th, a.pol.Version)})
			a.d.Decision, a.d.State = DecisionRejected, StateRejected
			return
		}
	}
	if a.d.InUSpaceAirspace && !a.dssOK {
		// 02 F5: inside U-space airspace cross-USSP deconfliction through
		// the DSS is required; while it cannot be made the intent waits.
		reason := ReasonDSSUnavailable
		if strings.HasPrefix(a.dssReason, ReasonUSSAvailabilityDown) {
			reason = ReasonUSSAvailabilityDown
		}
		d.count(reason)
		a.conflict(Conflict{Kind: KindDSS, Reason: reason, Effect: EffectHolds,
			Detail: "inside U-space airspace the intent is deconflicted through the DSS, which cannot be written now: " + a.dssReason})
		a.d.Decision, a.d.State = DecisionPendingDSS, StatePendingDSS
		return
	}
	if !a.d.InUSpaceAirspace {
		a.condition(Condition{Code: CondLocalDeconfliction, Detail: "outside U-space airspace: deconflicted against this USSP's intents and the peers' intents it holds (02 F5)"})
	}
	if d.SystemID == "" || a.n.OperatorPublic == "" {
		d.count("number_unavailable")
		a.conflict(Conflict{Kind: KindPolicy, Reason: "authorisation_number_unavailable", Effect: EffectRejects,
			Detail: "no USSP code (USSP_SYSTEM_ID) to number the authorisation with"})
		a.d.Decision, a.d.State = DecisionRejected, StateRejected
		return
	}
	a.d.Decision, a.d.State = DecisionAuthorised, StateAccepted
	a.d.DSSState = ptr("Accepted")
	a.d.DeviationThresholds = &th
	if a.d.AuthorisationNumber == nil {
		a.d.AuthorisationNumber = ptr(d.SystemID + "-" + a.n.OperatorPublic + "-" + bus.NewULID(a.now))
	}
}

// KeepNumber makes a modification that stays authorised keep the
// intent's authorisation number (Art. 6(6)).
func (a *Assessment) KeepNumber(number string) {
	if number != "" {
		a.d.AuthorisationNumber = ptr(number)
	}
}

// Decide is Assess and Finish in one call (the vectors and the tests).
func (d *Decider) Decide(ctx context.Context, n *Normalised, pol policy.Record, now time.Time, id string, rankAt time.Time, others []deconflict.Intent) (Decision, []string) {
	return d.Finish(d.Assess(ctx, n, pol, now), id, rankAt, others)
}
