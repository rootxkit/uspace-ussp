package api

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/terrain"

	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// DepTerrain is the readiness dependency of the DEM the intent decisions
// judge AGL limits with (USSP_TERRAIN_DIR): a restriction's or a zone's
// AGL limits, and a U-space airspace's ceiling above the ground.
const DepTerrain = "terrain"

// Counter of the api's ground lookups.
const (
	// CounterGroundUnknown: the ground under an intent's volume was not
	// known everywhere (no terrain, a cell outside the index, an
	// unreadable tile, a NoData sample, a box core refuses), so an AGL
	// limit was not judged and counted as reached (Z-09).
	CounterGroundUnknown = "intent_ground_unknown"
)

// demGround is the intent.Terrain of USSP_TERRAIN_DIR: the least and
// greatest ground over the box holding a volume's outline, from
// uspace-core's terrain.Store.RangeM, which bounds every elevation the
// DEM gives inside the box (a peak between the vertices included). Any
// unknown part of the box is no answer, never a partial range.
type demGround struct {
	store    *terrain.Store
	counters *core.Counters
}

// GroundRangeM is the least and greatest ground (AMSL) under s; ok is
// false when it is not known everywhere.
func (g demGround) GroundRangeM(s deconflict.Shape) (minM, maxM float64, ok bool) {
	r, err := g.store.RangeM(s.BBox(), 0)
	if err != nil || r == nil {
		g.counters.Inc(CounterGroundUnknown)
		return 0, 0, false
	}
	return r.MinM, r.MaxM, true
}

// loadTerrain is the DEM of USSP_TERRAIN_DIR (its index.json, and one
// tile per cell mapped read-only on first use with core's
// terrain.MappedDirOpener); nil with why when there is none, and an AGL
// limit is then not judged: a zone or restriction with one counts as
// overlapping and an airspace ceiling above the ground refuses.
func loadTerrain(dir string) (*terrain.Store, int, string) {
	if dir == "" {
		return nil, 0, "USSP_TERRAIN_DIR is not set: AGL limits of zones, restrictions and U-space airspace ceilings are not judged in intent decisions (counted as reached: an overlapping one refuses)"
	}
	raw, err := readIndex(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, 0, "the terrain index of USSP_TERRAIN_DIR does not read (" + err.Error() + "): AGL limits are not judged in intent decisions"
	}
	idx, err := terrain.ParseIndex(raw)
	if err != nil {
		return nil, 0, "the terrain index of USSP_TERRAIN_DIR does not parse (" + err.Error() + "): AGL limits are not judged in intent decisions"
	}
	return terrain.NewStore(idx, terrain.StoreOptions{OpenTile: terrain.MappedDirOpener(dir, 0)}), len(idx), ""
}

// terrainProbe is the readiness of the DEM: degraded with why without
// one (decisions still run, failing closed on AGL), up with the cells
// indexed otherwise.
func terrainProbe(s *terrain.Store, cells int, why string) obs.Probe {
	return func(context.Context) (obs.State, string) {
		if s == nil {
			return obs.StateDegraded, why
		}
		return obs.StateUp, fmt.Sprintf("%d cells indexed", cells)
	}
}

func readIndex(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's configured directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, terrain.MaxIndexBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > terrain.MaxIndexBytes {
		return nil, fmt.Errorf("more than %d bytes", terrain.MaxIndexBytes)
	}
	return buf, nil
}
