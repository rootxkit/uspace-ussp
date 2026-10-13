package cis

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed318"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/cis/cispclient"
)

// RequirementsMember is the extendedProperties member of a USPACE
// feature that carries cis/uspace_requirements/v1 (the 2021/664 Art.
// 3(4) requirements; CISP plan Q32).
const RequirementsMember = "uspace_requirements"

// RestrictionMember is the extendedProperties member the CISP adds to a
// restriction feature (CisRestriction).
const RestrictionMember = "cis_restriction"

// Entry is one feature of a dataset version as the Evaluator uses it.
type Entry struct {
	Dataset    Dataset
	Version    int64
	Identifier string
	Type       core.ZoneType
	// Feature is the parsed feature (owned by the version's collection;
	// read only); Raw is it as ed318.Export writes it (equal by value to
	// the published feature).
	Feature *ed318.Feature
	Raw     json.RawMessage
	// Parts are the judgement's zones, one per geometry part, built by
	// ed318.ToZones (their Periods are empty: applicability is
	// ed318.Applies over Feature's limitedApplicability).
	Parts []*zones.Zone
	// Centres are where each part's applicability is evaluated (the
	// centre of its bounding box, as ed318.ToZones resolves daylight).
	Centres []core.LatLon
	// ValidFrom and ValidTo bound limitedApplicability (nil: an open
	// end, or no limitedApplicability).
	ValidFrom, ValidTo *time.Time
	// Requirements are the Art. 3(4) requirements of a U-space airspace
	// (uspace_airspace features); nil elsewhere, and nil with
	// RequirementsProblem when the block cannot be read (the feature is
	// kept and the problem reported, never dropped).
	Requirements        *Requirements
	RequirementsProblem string
	// Restriction is the CISP's cis_restriction of a restrictions
	// feature (its state); nil elsewhere or when absent. For a direct
	// restriction it is built from the ANSP's restriction/direct/v1.
	Restriction *cispclient.CisRestriction
	// Direct is true for a restriction from the ANSP's degraded direct
	// path that the CISP does not hold yet (direct.go).
	Direct bool
}

// Requirements is cis/uspace_requirements/v1 as the generated type reads
// it, with in_controlled_airspace from airspace_constraints when the
// publication gives it (nil: not said) and the block as published.
type Requirements struct {
	cispclient.UspaceRequirements
	InControlledAirspace *bool
	Raw                  json.RawMessage
}

// buildEntries builds the entries of an ED-318 version. A feature that
// ed318.ToZones cannot build (a ring, circle or limit it refuses) makes
// the whole version refused: a zone a consumer cannot judge is never
// dropped silently (spec 06 T9).
func buildEntries(v *Version) ([]*Entry, *RefusalError) {
	fc := v.Collection
	raw, err := exportFeatures(fc)
	if err != nil {
		return nil, refuse(v.Dataset, v.Number, "the collection does not export: "+short(err.Error()))
	}
	out := make([]*Entry, 0, len(fc.Features))
	for i := range fc.Features {
		f := &fc.Features[i]
		e, err := buildEntry(v, f)
		if err != nil {
			return nil, refuse(v.Dataset, v.Number, fmt.Sprintf("features[%d]: %v", i, err))
		}
		e.Raw = raw[i]
		out = append(out, e)
	}
	return out, nil
}

// exportFeatures is every feature of fc as ed318.Export writes it.
func exportFeatures(fc *ed318.FeatureCollection) ([]json.RawMessage, error) {
	b, err := ed318.Export(fc)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Features) != len(fc.Features) {
		return nil, fmt.Errorf("exported %d features of %d", len(doc.Features), len(fc.Features))
	}
	return doc.Features, nil
}

func buildEntry(v *Version, f *ed318.Feature) (*Entry, error) {
	// The uspace_airspace dataset holds U-space airspaces only. A feature
	// of another type there is neither an airspace nor a zone to its
	// readers (the decision reads zones from the other two datasets), so
	// it would be dropped silently: the version is refused instead.
	if v.Dataset == USpaceAirspace && f.Properties.Type != core.ZoneUSpace {
		return nil, fmt.Errorf("type %q in the %s dataset, which holds %s features only", string(f.Properties.Type), USpaceAirspace, string(core.ZoneUSpace))
	}
	// ToZones builds the shape and the limits; applicability is judged
	// by ed318.Applies, so the periods are left out of the copy it sees
	// (it refuses open-ended daylight schedules, which Applies judges).
	shape := *f
	shape.Properties.LimitedApplicability = nil
	zs, err := ed318.ToZones(&ed318.FeatureCollection{Type: "FeatureCollection", Features: []ed318.Feature{shape}}, ed318.NOAADaylight{})
	if err != nil {
		return nil, err
	}
	e := &Entry{
		Dataset: v.Dataset, Version: v.Number, Identifier: f.Properties.Identifier,
		Type: f.Properties.Type, Feature: f, Parts: zs, Centres: make([]core.LatLon, len(zs)),
	}
	for k, z := range zs {
		e.Centres[k] = centre(z.BBox)
	}
	e.ValidFrom, e.ValidTo = validity(f.Properties.LimitedApplicability)
	if raw, ok := f.Properties.ExtendedProperties[RequirementsMember]; ok {
		r, err := readRequirements(raw)
		if err != nil {
			e.RequirementsProblem = err.Error()
		} else {
			e.Requirements = r
		}
	} else if v.Dataset == USpaceAirspace {
		e.RequirementsProblem = "extendedProperties." + RequirementsMember + " is absent"
	}
	if raw, ok := f.Properties.ExtendedProperties[RestrictionMember]; ok {
		var r cispclient.CisRestriction
		if err := json.Unmarshal(raw, &r); err == nil {
			e.Restriction = &r
		}
	}
	return e, nil
}

// FeatureZones builds the judgement's zones of one ED-318 feature as the
// projection lists it (ApplicableZone.Feature, ed318.Export's form):
// ed318.Parse of the feature alone, then ed318.ToZones without its
// limitedApplicability, exactly as buildEntry builds an Entry's Parts
// (applicability is the projection's verdict, Applies). A feature Parse
// or ToZones refuses is an error, never zones that judge nothing.
func FeatureZones(raw json.RawMessage) ([]*zones.Zone, error) {
	f, err := parseFeature(raw)
	if err != nil {
		return nil, err
	}
	f.Properties.LimitedApplicability = nil
	return ed318.ToZones(&ed318.FeatureCollection{Type: "FeatureCollection", Features: []ed318.Feature{*f}}, ed318.NOAADaylight{})
}

// FeatureZonesApplicable builds the zones of one feature as
// FeatureZones does, but with its limitedApplicability as the zones'
// periods (ed318.ToZones resolves daylight events into fixed windows),
// so uspace-core judges each sample's applicability at its placement
// (Zone.AppliesAt, T-09). It returns ErrApplicabilityNotBuilt, with the
// zones built without periods, when ToZones refuses the periods (an
// open-ended daylight schedule, which only ed318.Applies judges): the
// caller then holds zones that apply always, the fail-safe reading, and
// says so. Any other refusal is an error with no zones.
func FeatureZonesApplicable(raw json.RawMessage) ([]*zones.Zone, error) {
	f, err := parseFeature(raw)
	if err != nil {
		return nil, err
	}
	zs, err := ed318.ToZones(&ed318.FeatureCollection{Type: "FeatureCollection", Features: []ed318.Feature{*f}}, ed318.NOAADaylight{})
	if err == nil || len(f.Properties.LimitedApplicability) == 0 {
		return zs, err
	}
	shape := *f
	shape.Properties.LimitedApplicability = nil
	always, serr := ed318.ToZones(&ed318.FeatureCollection{Type: "FeatureCollection", Features: []ed318.Feature{shape}}, ed318.NOAADaylight{})
	if serr != nil {
		return nil, serr
	}
	return always, fmt.Errorf("%w: %s", ErrApplicabilityNotBuilt, short(err.Error()))
}

// ErrApplicabilityNotBuilt is FeatureZonesApplicable's answer for a
// feature whose periods ed318.ToZones refuses.
var ErrApplicabilityNotBuilt = errors.New("the applicability periods do not build; the zone is held as applying always")

// parseFeature parses one feature as published (ed318.Parse of a
// collection holding it alone, with ProblemLimits).
func parseFeature(raw json.RawMessage) (*ed318.Feature, error) {
	doc := make([]byte, 0, len(raw)+48)
	doc = append(doc, `{"type":"FeatureCollection","features":[`...)
	doc = append(doc, raw...)
	doc = append(doc, "]}"...)
	fc, problems := ed318.Parse(doc, ProblemLimits)
	if problems != nil {
		return nil, fmt.Errorf("the feature does not parse: %s", short(problems.Error()))
	}
	if len(fc.Features) != 1 {
		return nil, fmt.Errorf("%d features, want 1", len(fc.Features))
	}
	return &fc.Features[0], nil
}

func readRequirements(raw json.RawMessage) (*Requirements, error) {
	r := &Requirements{Raw: raw}
	if err := json.Unmarshal(raw, &r.UspaceRequirements); err != nil {
		return nil, fmt.Errorf("extendedProperties.%s does not read: %s", RequirementsMember, short(err.Error()))
	}
	var ac struct {
		AirspaceConstraints struct {
			InControlledAirspace *bool `json:"in_controlled_airspace"`
		} `json:"airspace_constraints"`
	}
	if err := json.Unmarshal(raw, &ac); err == nil {
		r.InControlledAirspace = ac.AirspaceConstraints.InControlledAirspace
	}
	return r, nil
}

// centre is the centre of a box that does not cross the antimeridian
// (ed318.Parse refuses a shape across it).
func centre(b geodesy.BBox) core.LatLon {
	return core.LatLon{LatDeg: (b.MinLat + b.MaxLat) / 2, LonDeg: (b.MinLon + b.MaxLon) / 2}
}

// validity is the earliest start and the latest end of the periods; an
// open start or end on any period leaves that bound nil, and so does a
// zone without limitedApplicability (it applies always).
func validity(tp []ed318.TimePeriod) (from, to *time.Time) {
	if len(tp) == 0 {
		return nil, nil
	}
	openFrom, openTo := false, false
	for _, p := range tp {
		if p.StartDateTime == nil {
			openFrom = true
		} else if t := p.StartDateTime.Time.UTC(); from == nil || t.Before(*from) {
			from = &t
		}
		if p.EndDateTime == nil {
			openTo = true
		} else if t := p.EndDateTime.Time.UTC(); to == nil || t.After(*to) {
			to = &t
		}
	}
	if openFrom {
		from = nil
	}
	if openTo {
		to = nil
	}
	return from, to
}
