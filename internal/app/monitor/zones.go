package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/terrain"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Readiness dependencies of the zone path (brief WP-12).
const (
	DepCISCurrent = "cis_current"
	DepTerrain    = "terrain"
)

// ZoneProvider is the zone set in force (geo.ZoneSource over the
// cis_current mirror).
type ZoneProvider interface {
	Current() *geo.ZoneSet
}

// EnvFunc resolves the ground and the geoid at a position for core's
// vertical judgement (zones does no I/O).
type EnvFunc func(p core.LatLon) zones.Env

// NewEnv is the environment of the zone path: the geoid undulation when
// a grid is loaded (nil otherwise: a WGS84 limit is not judged), and the
// ground from the terrain tiles when they are configured
// (GroundNotConfigured otherwise; GroundUnknown where a tile does not
// answer, never 0, D-04): an AGL limit without the ground warns in a
// PROHIBITED or REQ_AUTHORIZATION zone and is not evaluated in a
// CONDITIONAL one (core, SC-13).
func NewEnv(und geoid.Undulator, ground terrain.Ground) EnvFunc {
	return func(p core.LatLon) zones.Env {
		var env zones.Env
		if und != nil {
			if n, err := und.UndulationM(p); err == nil && core.IsFinite(n) {
				env.UndulationM = &n
			}
		}
		if ground == nil {
			return env
		}
		env.Ground = zones.GroundUnknown
		if e, err := ground.Elevation(p); err == nil && e != nil && core.IsFinite(e.ElevationM) {
			env.Ground, env.GroundM = zones.GroundKnown, e.ElevationM
		}
		return env
	}
}

// loadTerrain is the terrain of USSP_TERRAIN_DIR (its index.json and one
// tile per cell); nil with why when there is none.
func loadTerrain(dir string) (terrain.Ground, string) {
	if dir == "" {
		return nil, "USSP_TERRAIN_DIR is not set: AGL zone limits are not judged (a PROHIBITED or REQ_AUTHORIZATION zone warns with limit_not_judged, a CONDITIONAL one is not evaluated)"
	}
	raw, err := readBounded(filepath.Join(dir, "index.json"), terrain.MaxIndexBytes)
	if err != nil {
		return nil, "the terrain index of USSP_TERRAIN_DIR does not read (" + err.Error() + "): AGL zone limits are not judged"
	}
	idx, err := terrain.ParseIndex(raw)
	if err != nil {
		return nil, "the terrain index of USSP_TERRAIN_DIR does not parse (" + err.Error() + "): AGL zone limits are not judged"
	}
	return terrain.NewStore(idx, terrain.StoreOptions{Open: terrain.DirOpener(dir, 0)}), ""
}

// errTooLarge is a file over its bound.
var errTooLarge = errors.New("file too large")

func readBounded(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's configured directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > maxBytes {
		return nil, fmt.Errorf("%w: more than %d bytes", errTooLarge, maxBytes)
	}
	return buf, nil
}

// zoneProbe is the readiness of the zone path's input: unknown while
// cis_current was never read (no zone is judged, said so: an empty sky
// is never claimed, SC-22), degraded with its age when the CIS it holds
// is stale or a feature did not build.
func zoneProbe(src ZoneProvider) obs.Probe {
	return func(context.Context) (obs.State, string) {
		s := src.Current()
		switch {
		case s == nil || !s.Loaded:
			return obs.StateUnknown, "cis_current not read or nothing projected: no zone is judged"
		case len(s.Unbuildable) > 0:
			return obs.StateDegraded, fmt.Sprintf("%d CIS features do not build into zones and are not judged: %v", len(s.Unbuildable), s.Unbuildable)
		case s.OverBound:
			return obs.StateDegraded, fmt.Sprintf("more than %d zone parts: the rest are not judged", geo.MaxZones)
		case s.Stale:
			return obs.StateDegraded, fmt.Sprintf("the CIS in force (%s) is stale: age %.0f s", s.CISVersion, s.CISAgeS)
		}
		return obs.StateUp, ""
	}
}

// zoneAlertEvent is a zone path event as the conformance path's
// alert/v1 mapping takes it (conformance.AlertMessageOf, one message
// shape for every kind the monitor publishes).
func zoneAlertEvent(e geo.Event) conformance.AlertEvent {
	a := e.Alert
	detail := a.Detail
	if a.CarriedSince != nil {
		detail = make(map[string]any, len(a.Detail)+1)
		for k, v := range a.Detail {
			detail[k] = v
		}
		detail["carried_since"] = a.CarriedSince.UTC()
	}
	return conformance.AlertEvent{
		State: e.State, ClearReason: e.ClearReason, ClearingDetail: e.ClearingDetail,
		Alert: conformance.Alert{
			ID: a.ID, Kind: a.Kind, Severity: a.Severity, FlightID: a.FlightID, IntentID: a.IntentID,
			AuthorisationNumber: a.AuthorisationNumber, Cell5: a.Cell5, Detail: detail,
			RaisedAt: a.RaisedAt, UpdatedAt: a.UpdatedAt, CapturedAt: a.CapturedAt, PolicyVersion: a.PolicyVersion,
		},
	}
}

// ZoneStatusAttrs are the zone path's attributes of the status line:
// what the judgement rests on, said every period (SC-22).
func ZoneStatusAttrs(s *geo.ZoneSet, terrainKnown, geoidKnown bool) []slog.Attr {
	if s == nil {
		s = &geo.ZoneSet{}
	}
	return []slog.Attr{
		slog.Bool("cis_loaded", s.Loaded), slog.String("cis_version", s.CISVersion), slog.Float64("cis_age_s", s.CISAgeS),
		slog.Bool("cis_stale", s.Stale), slog.Int("zones_judged", len(s.Zones)), slog.Int("zones_unbuildable", len(s.Unbuildable)),
		slog.Bool("terrain", terrainKnown), slog.Bool("geoid", geoidKnown),
	}
}
