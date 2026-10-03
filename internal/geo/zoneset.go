package geo

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// Counters of the zone set (E-09).
const (
	// CounterZoneUnbuildable counts features of cis_current that do not
	// build into zones: left out of the judgement and named on the set
	// (Unbuildable), never silent.
	CounterZoneUnbuildable = "geo_zone_unbuildable"
	// CounterZoneAlwaysApplies counts features whose applicability
	// periods do not build (an open-ended daylight schedule): judged as
	// applying always, the fail-safe reading.
	CounterZoneAlwaysApplies = "geo_zone_applicability_always"
	// CounterZoneSetsBuilt counts the zone sets built from cis_current.
	CounterZoneSetsBuilt = "geo_zone_sets_built"
	// CounterZoneSetOverBound counts the zones left out of a set past
	// MaxZones.
	CounterZoneSetOverBound = "geo_zone_set_over_bound"
)

// MaxZones bounds the zone parts one set holds (E-10). Beyond it the
// rest are left out, counted, and the set says so (OverBound): the
// monitor's status and /readyz name it.
const MaxZones = 20_000

// zoneBound is MaxZones (a test lowers it to exceed it).
var zoneBound = MaxZones

// ZoneMeta is what a zone alert names of its zone beyond core's detail.
type ZoneMeta struct {
	Dataset    string
	Identifier string
	// Part is core's identifier of the part ("<id>/L<k>" for a layer of
	// a GeometryCollection, else the identifier).
	Part             string
	Type             string
	Version          string
	RestrictionState string
	// AlwaysApplies is set when the periods did not build.
	AlwaysApplies bool
}

// ZoneSet is the zones the monitor judges: every PROHIBITED,
// REQ_AUTHORIZATION and CONDITIONAL zone of the zones and restrictions
// datasets in cis_current (restrictions ended or cancelled left out),
// built by uspace-core with their applicability. USPACE features (the
// uspace_airspace dataset) are information, not incursions, and
// NO_RESTRICTION raises nothing: neither is held.
type ZoneSet struct {
	Zones []*zones.Zone
	Meta  map[*zones.Zone]ZoneMeta
	// Key identifies the projection the set was built from (its basis);
	// a new key is a new set.
	Key string
	// Loaded is false when cis_current was never read or holds no basis
	// (nothing projected): no zone is known, which is not "no zone".
	Loaded     bool
	CISVersion string
	CISAgeS    float64
	Stale      bool
	BuiltAt    time.Time
	// Unbuildable names the features left out ("dataset/identifier").
	Unbuildable []string
	OverBound   bool
}

// Has reports whether the set holds a zone part of this identifier
// (core's part identifier).
func (s *ZoneSet) Has(part string) bool {
	if s == nil {
		return false
	}
	for _, m := range s.Meta {
		if m.Part == part {
			return true
		}
	}
	return false
}

// judged reports whether a feature of the projection is one the monitor
// judges.
func judged(z *cis.ApplicableZone) bool {
	if z.Dataset != string(cis.Zones) && z.Dataset != string(cis.Restrictions) {
		return false
	}
	switch core.ZoneType(z.Type) {
	case core.ZoneUSpace, core.ZoneNoRestriction:
		return false
	case core.ZoneProhibited, core.ZoneReqAuthorization, core.ZoneConditional:
	}
	switch z.RestrictionState {
	case "ended", "cancelled":
		return false
	}
	return true
}

// BuildZoneSet builds the set from cis_current's values (the cell
// entries and the basis, keyed as cis.BusProjector writes them): each
// feature once, whatever the cells it is listed under, in a fixed order
// (dataset, identifier) so core keys the same zone the same way at every
// build. A feature that does not build is left out and named.
func BuildZoneSet(vals map[string]telemetry.CISValue, counters *core.Counters, now time.Time) *ZoneSet {
	if counters == nil {
		counters = &core.Counters{}
	}
	s := &ZoneSet{Meta: map[*zones.Zone]ZoneMeta{}, BuiltAt: now}
	if b, ok := vals[cis.KeyBasis]; ok && b.Basis != nil {
		s.Loaded = true
		s.CISVersion, s.CISAgeS, s.Stale = b.Basis.CISVersion, b.Basis.CISAgeS, b.Basis.Stale
		s.Key = b.Basis.CISVersion + "@" + b.Basis.At.UTC().Format(time.RFC3339Nano)
	}
	type feature struct {
		z   *cis.ApplicableZone
		key string
	}
	seen := map[string]bool{}
	var fs []feature
	for k, v := range vals {
		if k == cis.KeyBasis || v.Cell == nil {
			continue
		}
		for i := range v.Cell.Zones {
			z := &v.Cell.Zones[i]
			if !judged(z) {
				continue
			}
			key := z.Dataset + "/" + z.Identifier + "@" + z.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			fs = append(fs, feature{z: z, key: key})
		}
	}
	slices.SortFunc(fs, func(a, b feature) int {
		if a.z.Dataset != b.z.Dataset {
			if a.z.Dataset < b.z.Dataset {
				return -1
			}
			return 1
		}
		switch {
		case a.z.Identifier < b.z.Identifier:
			return -1
		case a.z.Identifier > b.z.Identifier:
			return 1
		}
		return 0
	})
	for _, f := range fs {
		parts, err := cis.FeatureZonesApplicable(f.z.Feature)
		always := false
		switch {
		case errors.Is(err, cis.ErrApplicabilityNotBuilt):
			counters.Inc(CounterZoneAlwaysApplies)
			always = true
		case err != nil:
			counters.Inc(CounterZoneUnbuildable)
			s.Unbuildable = append(s.Unbuildable, f.z.Dataset+"/"+f.z.Identifier)
			continue
		}
		for _, p := range parts {
			if len(s.Zones) >= zoneBound {
				counters.Inc(CounterZoneSetOverBound)
				s.OverBound = true
				break
			}
			s.Zones = append(s.Zones, p)
			s.Meta[p] = ZoneMeta{
				Dataset: f.z.Dataset, Identifier: f.z.Identifier, Part: p.Identifier, Type: f.z.Type, Version: f.z.Version,
				RestrictionState: f.z.RestrictionState, AlwaysApplies: always,
			}
		}
	}
	counters.Inc(CounterZoneSetsBuilt)
	return s
}

// CISMirror is the cis_current projection as the monitor follows it
// (bus.Mirror[telemetry.CISValue]).
type CISMirror interface {
	Snapshot() (map[string]telemetry.CISValue, float64, bool)
	Get(key string) (v telemetry.CISValue, found bool, ageS float64, loaded bool)
}

// ZoneSource keeps the zone set of the current projection: Current
// rebuilds it when the projection's basis changed, so every worker that
// asks on its tick holds the new zones within one tick of the
// projection (Z-12). Safe for concurrent use; the build runs outside
// the lock.
type ZoneSource struct {
	M        CISMirror
	Counters *core.Counters
	Now      func() time.Time

	mu  sync.Mutex
	cur *ZoneSet
}

// Current is the set of the projection now; a set with Loaded false
// while cis_current was never read or holds no basis.
func (z *ZoneSource) Current() *ZoneSet {
	now := time.Now()
	if z.Now != nil {
		now = z.Now()
	}
	basis, found, _, loaded := z.M.Get(cis.KeyBasis)
	key := ""
	if loaded && found && basis.Basis != nil {
		key = basis.Basis.CISVersion + "@" + basis.Basis.At.UTC().Format(time.RFC3339Nano)
	}
	z.mu.Lock()
	cur := z.cur
	z.mu.Unlock()
	if cur != nil && cur.Key == key && (key != "" || !cur.Loaded) {
		return cur
	}
	var s *ZoneSet
	if key == "" {
		s = &ZoneSet{Meta: map[*zones.Zone]ZoneMeta{}, BuiltAt: now}
	} else {
		vals, _, _ := z.M.Snapshot()
		s = BuildZoneSet(vals, z.Counters, now)
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.cur == nil || z.cur.Key != s.Key || z.cur.Loaded != s.Loaded {
		z.cur = s
	}
	return z.cur
}
