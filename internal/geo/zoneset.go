package geo

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/policy"
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
// datasets in cis_current (restrictions planned, ended or cancelled left out),
// built by uspace-core with their applicability. USPACE features (the
// uspace_airspace dataset) are information, not incursions, and
// NO_RESTRICTION raises nothing: neither is held.
type ZoneSet struct {
	Zones []*zones.Zone
	Meta  map[*zones.Zone]ZoneMeta
	// Key identifies the zones the set was built from: the CIS version
	// of the projection's basis, which names the dataset versions and so
	// the features. A new key is a new set; a projection that only
	// refreshes the basis of the same versions keeps the set (a new set
	// carries every active zone alert, Tracker.Configure).
	Key string
	// Loaded is false when cis_current was never read or holds no basis
	// (nothing projected): no zone is known, which is not "no zone".
	Loaded     bool
	CISVersion string
	// How old the CIS is, and whether it is stale, is not held here: it
	// changes with every projection of the same versions and with the
	// time since. ZoneSource.Freshness says it now.
	BuiltAt time.Time
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
	// A restriction is judged only while in force: a planned one is not
	// (spec 02 F2), nor an ended or cancelled one.
	return z.Dataset != string(cis.Restrictions) || cis.RestrictionStateInForce(z.RestrictionState)
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
		s.CISVersion = b.Basis.CISVersion
		s.Key = zoneSetKey(b.Basis)
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

// zoneSetKey is ZoneSet.Key of a basis.
func zoneSetKey(b *cis.BasisValue) string { return "cis:" + b.CISVersion }

// ZoneSource keeps the zone set of the current projection: Current
// rebuilds it when the projection's CIS version changed, so every
// worker that asks on its tick holds the new zones within one tick of
// the projection (Z-12), and Freshness says how old the CIS in force is
// now. Safe for concurrent use; the build runs outside the lock.
type ZoneSource struct {
	M        CISMirror
	Counters *core.Counters
	Now      func() time.Time
	// StaleS is the policy's cis_stale_s, read on every call (nil: the
	// policy default).
	StaleS func() float64

	// seen is the basis last received and seenAt when, on this
	// process's clock (Observe, or the first read that found it).
	rmu    sync.Mutex
	seen   cis.BasisValue
	seenAt time.Time
	seenOK bool

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
		key = zoneSetKey(basis.Basis)
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

// Freshness is how old the CIS the zone set rests on is now.
type Freshness struct {
	// Loaded is false while cis_current was never read or holds no
	// basis; such a CIS is stale.
	Loaded     bool
	CISVersion string
	// CISAgeS is the age the api projected (its oldest confirmation by
	// the CISP) plus the time since it projected it, on this process's
	// clock; never less than the age projected.
	CISAgeS float64
	// Stale is true when the api projected it stale (a dataset never
	// loaded, or already too old) or CISAgeS is beyond cis_stale_s.
	Stale bool
}

// Observe notes the receipt of a cis_current key on this process's
// clock (the mirror's OnChange): a new basis is aged from now. Without
// it the first read that finds a new basis notes it.
func (z *ZoneSource) Observe(key string) {
	if key == cis.KeyBasis || key == "" {
		z.received(z.now())
	}
}

func (z *ZoneSource) now() time.Time {
	if z.Now != nil {
		return z.Now()
	}
	return time.Now()
}

// received is the basis now in the mirror and when this process first
// saw it (now when it is new); ok false when there is none.
func (z *ZoneSource) received(now time.Time) (b cis.BasisValue, at time.Time, ok bool) {
	basis, found, _, loaded := z.M.Get(cis.KeyBasis)
	if !loaded || !found || basis.Basis == nil {
		return cis.BasisValue{}, time.Time{}, false
	}
	z.rmu.Lock()
	defer z.rmu.Unlock()
	if !z.seenOK || z.seen != *basis.Basis {
		z.seen, z.seenAt, z.seenOK = *basis.Basis, now, true
	}
	return z.seen, z.seenAt, true
}

// Freshness is the basis of the projection now in cis_current, aged to
// now: the age projected plus the time since this process received it,
// on its own clock, so a clock skew between hosts never makes it
// younger; or plus the time since its at when that is longer (a basis
// that sat in the KV before this process read it, as traffic-ws ages
// it). The api projects again on every confirmation by the CISP
// (cis.Cache), so a basis that is not refreshed is one whose writer
// stopped: it goes stale here past cis_stale_s, never frozen at the age
// it had when written.
func (z *ZoneSource) Freshness() Freshness {
	now := z.now()
	b, at, ok := z.received(now)
	if !ok {
		return Freshness{Stale: true}
	}
	bound := policy.Defaults().CISStaleS
	if z.StaleS != nil {
		bound = z.StaleS()
	}
	since := max(0, now.Sub(at).Seconds(), now.Sub(b.At).Seconds())
	age := b.CISAgeS + since
	return Freshness{Loaded: true, CISVersion: b.CISVersion, CISAgeS: age, Stale: b.Stale || !(age <= bound)}
}
