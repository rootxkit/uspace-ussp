package monitor

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/terrain"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// pgmFile writes a 16-bit GeographicLib PGM with the header lines.
func pgmFile(t *testing.T, path string, width, height int, header [][2]string, sample func(r, c int) uint16) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("P5\n")
	for _, h := range header {
		b.WriteString("# " + h[0] + " " + h[1] + "\n")
	}
	b.WriteString(strconv.Itoa(width) + " " + strconv.Itoa(height) + "\n65535\n")
	for r := range height {
		for c := range width {
			v := sample(r, c)
			b.WriteByte(byte(v >> 8))
			b.WriteByte(byte(v))
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gridPos is inside the test tile N41E044, at sample (2, 3).
var gridPos = core.LatLon{LatDeg: 41.5, LonDeg: 44.75}

// writeGrids writes a constant geoid (N = 20 m) and a terrain directory
// with one 5 x 5 tile N41E044 at 0.25 deg, elevation 400 + 10*row + col.
func writeGrids(t *testing.T) (geoidPath, terrainDir string) {
	t.Helper()
	dir := t.TempDir()
	geoidPath = filepath.Join(dir, "geoid.pgm")
	pgmFile(t, geoidPath, 2, 3, [][2]string{{"Description", "monitor test grid"}, {"Offset", "20"}, {"Scale", "1"}},
		func(int, int) uint16 { return 0 })
	terrainDir = filepath.Join(dir, "terrain")
	if err := os.Mkdir(terrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pgmFile(t, filepath.Join(terrainDir, "N41E044.pgm"), 5, 5, [][2]string{
		{"Description", "monitor test tile"}, {"Dataset", "COP-DEM GLO-30"},
		{"Offset", "-500.0"}, {"Scale", "0.2"}, {"Nodata", "65535"},
		{"LatFirst", "42"}, {"LonFirst", "44"}, {"LatStep", "0.25"}, {"LonStep", "0.25"},
	}, func(r, c int) uint16 { return uint16((float64(400+10*r+c) - terrain.OffsetM) / terrain.ScaleM) })
	if err := os.WriteFile(filepath.Join(terrainDir, "index.json"), []byte(`{"cells": {"N41E044": "COP-DEM GLO-30"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return geoidPath, terrainDir
}

func mappedDetail(mapped bool) string {
	if mapped {
		return "mapped: true"
	}
	return "mapped: false"
}

// checkGridLoad loads USSP_GEOID_FILE and USSP_TERRAIN_DIR as the
// monitor process does and checks that the grid and the tile it reads
// say wantMapped, and /readyz's geoid and terrain details say it
// (WP-19). grids_unix_test.go wants a mapping, grids_other_test.go the
// read into memory core falls back to elsewhere.
func checkGridLoad(t *testing.T, wantMapped bool) {
	t.Helper()
	geoidPath, terrainDir := writeGrids(t)
	und, mapped, why := loadGeoid(geoidPath)
	if und == nil || why != "" || mapped != wantMapped {
		t.Fatalf("loadGeoid: %v, mapped %v, %q; want mapped %v", und, mapped, why, wantMapped)
	}
	if st, detail := geoidProbe(und, mapped, why)(t.Context()); st != obs.StateUp || detail != mappedDetail(wantMapped) {
		t.Fatalf("readyz geoid %s %q", st, detail)
	}
	tiles, why := loadTerrain(terrainDir)
	if tiles == nil || why != "" {
		t.Fatalf("loadTerrain: %q", why)
	}
	if st, detail := terrainProbe(tiles, why)(t.Context()); st != obs.StateUp || detail != "mapped: no tile read yet" {
		t.Fatalf("readyz terrain before a tile is read: %s %q", st, detail)
	}
	env := NewEnv(und, tiles)(gridPos)
	if env.UndulationM == nil || *env.UndulationM != 20 || env.Ground != zones.GroundKnown ||
		math.Abs(env.GroundM-423) > 1e-9 {
		t.Fatalf("env %+v", env)
	}
	if got, known := tiles.Mapped(); !known || got != wantMapped {
		t.Fatalf("tile Mapped() %v, %v; want %v", got, known, wantMapped)
	}
	if st, detail := terrainProbe(tiles, why)(t.Context()); st != obs.StateUp || detail != mappedDetail(wantMapped) {
		t.Fatalf("readyz terrain %s %q", st, detail)
	}
}

// A tile read into memory says so on every platform, and the probes of
// inputs that are not there say why.
func TestGridProbesOtherStates(t *testing.T) {
	_, terrainDir := writeGrids(t)
	raw, err := os.ReadFile(filepath.Join(terrainDir, "N41E044.pgm"))
	if err != nil {
		t.Fatal(err)
	}
	mem := newMappedTerrain(terrain.Index{"N41E044": "COP-DEM GLO-30"}, func(string) (*terrain.Tile, error) {
		return terrain.ParseTile(raw)
	})
	if e, err := mem.Elevation(gridPos); err != nil || e == nil {
		t.Fatalf("elevation %v, %v", e, err)
	}
	if st, detail := terrainProbe(mem, "")(t.Context()); st != obs.StateUp || detail != "mapped: false" {
		t.Fatalf("readyz terrain in memory %s %q", st, detail)
	}
	if st, detail := terrainProbe(nil, "no terrain")(t.Context()); st != obs.StateDegraded || detail != "no terrain" {
		t.Fatalf("readyz without terrain %s %q", st, detail)
	}
	und, mapped, why := loadGeoid("")
	if st, detail := geoidProbe(und, mapped, why)(t.Context()); und != nil || st != obs.StateDown || detail != why || why == "" {
		t.Fatalf("readyz without a geoid %s %q", st, detail)
	}
}
