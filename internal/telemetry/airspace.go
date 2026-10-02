package telemetry

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/cis"
)

// AirspaceVerdict says whether a sample's position is inside a U-space
// airspace that applies (the uspace_airspace dataset of the CIS).
type AirspaceVerdict struct {
	// Judged is false when the CIS could not say: nothing loaded, or the
	// projection not readable. Reason says why.
	Judged bool
	// Inside is true inside an airspace, and when its vertical limits
	// could not be judged (an AGL limit with no terrain here): the
	// airspace is then one the aircraft may be inside, as the CIS
	// Evaluator keeps it (fail-safe).
	Inside     bool
	AirspaceID string
	Reason     string
}

// AirspaceJudge answers AirspaceVerdict for a position and its altitude.
type AirspaceJudge interface {
	At(p core.LatLon, ac zones.Aircraft, env zones.Env) AirspaceVerdict
}

// Reasons an airspace was not judged.
const (
	ReasonCISUnavailable = "cis_unavailable" // the cis_current bucket was never read
	ReasonCISNotLoaded   = "cis_not_loaded"  // read, but api has projected no CIS version yet
	ReasonCISUnreadable  = "cis_unreadable"  // an airspace here does not build into zones
)

// CISValue is one key of cis_current: the basis, or a cell's entry.
type CISValue struct {
	Basis *cis.BasisValue
	Cell  *cis.CellEntry
}

// DecodeCIS reads a cis_current value by its key (cis.BusProjector).
func DecodeCIS(key string, data []byte) (CISValue, error) {
	if key == cis.KeyBasis {
		var b cis.BasisValue
		if err := json.Unmarshal(data, &b); err != nil {
			return CISValue{}, core.Fieldf("basis", "not a CIS basis: %s", clipErr(err))
		}
		return CISValue{Basis: &b}, nil
	}
	var c cis.CellEntry
	if err := json.Unmarshal(data, &c); err != nil {
		return CISValue{}, core.Fieldf("cell", "not a CIS cell entry: %s", clipErr(err))
	}
	return CISValue{Cell: &c}, nil
}

// maxZoneCache bounds the zones built from features (E-10); beyond it
// the cache starts again.
const maxZoneCache = 10_000

// CISAirspace judges against the cis_current projection: the entries of
// the position's cell and of the "all" key, the USPACE features of the
// uspace_airspace dataset that apply (or whose applicability is
// unknown: kept, never dropped), built into zones by cis.FeatureZones
// and judged by uspace-core (Zone.ContainsHorizontally, zones.JudgeVertical)
// as the Evaluator's AirspacesAt does. Safe for concurrent use.
type CISAirspace struct {
	M *bus.Mirror[CISValue]
	// Policy is the zones judgement's policy (nil: zones.DefaultPolicy).
	Policy func() zones.Policy

	mu    sync.Mutex
	cache map[string]builtZones
}

type builtZones struct {
	zs  []*zones.Zone
	err error
}

// NewCISAirspace is the judge over a mirror of cis_current.
func NewCISAirspace(m *bus.Mirror[CISValue]) *CISAirspace {
	return &CISAirspace{M: m}
}

func (c *CISAirspace) zonesOf(z *cis.ApplicableZone) builtZones {
	key := z.Dataset + "/" + z.Identifier + "@" + z.Version
	c.mu.Lock()
	if c.cache == nil || len(c.cache) >= maxZoneCache {
		c.cache = map[string]builtZones{}
	}
	b, ok := c.cache[key]
	c.mu.Unlock()
	if ok {
		return b
	}
	zs, err := cis.FeatureZones(z.Feature)
	b = builtZones{zs: zs, err: err}
	c.mu.Lock()
	c.cache[key] = b
	c.mu.Unlock()
	return b
}

// At implements AirspaceJudge.
func (c *CISAirspace) At(p core.LatLon, ac zones.Aircraft, env zones.Env) AirspaceVerdict {
	basis, found, _, loaded := c.M.Get(cis.KeyBasis)
	switch {
	case !loaded:
		return AirspaceVerdict{Reason: ReasonCISUnavailable}
	case !found || basis.Basis == nil:
		return AirspaceVerdict{Reason: ReasonCISNotLoaded}
	}
	c5, _, err := cell.Key(p)
	if err != nil {
		return AirspaceVerdict{Reason: ReasonCISUnreadable}
	}
	pol := zones.DefaultPolicy()
	if c.Policy != nil {
		pol = c.Policy()
	}
	var maybe *AirspaceVerdict
	for _, key := range []string{cell.KVToken(c5), cis.AllCells} {
		v, ok, _, _ := c.M.Get(key)
		if !ok || v.Cell == nil {
			continue
		}
		for i := range v.Cell.Zones {
			z := &v.Cell.Zones[i]
			if z.Dataset != string(cis.USpaceAirspace) || z.Type != string(core.ZoneUSpace) {
				continue
			}
			if !z.Applies && z.CISApplicability != string(cis.Unknown) {
				continue
			}
			b := c.zonesOf(z)
			if b.err != nil {
				// An airspace here that cannot be built: not judged, said so.
				return AirspaceVerdict{Reason: ReasonCISUnreadable, AirspaceID: z.Identifier}
			}
			for _, part := range b.zs {
				in, err := part.ContainsHorizontally(p)
				if err != nil {
					return AirspaceVerdict{Reason: ReasonCISUnreadable, AirspaceID: z.Identifier}
				}
				if !in {
					continue
				}
				r := zones.JudgeVertical(part, ac, env, pol)
				switch {
				case r.Raise != nil:
					return AirspaceVerdict{Judged: true, Inside: true, AirspaceID: z.Identifier}
				case r.NotEvaluated || r.LimitNotJudged:
					// May be inside: kept, as the Evaluator keeps it.
					if maybe == nil {
						maybe = &AirspaceVerdict{Judged: true, Inside: true, AirspaceID: z.Identifier,
							Reason: "vertical_not_judged:" + strings.ReplaceAll(r.Reasons.String(), " ", "")}
					}
				}
			}
		}
	}
	if maybe != nil {
		return *maybe
	}
	return AirspaceVerdict{Judged: true}
}
