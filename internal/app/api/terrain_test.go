package api

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/terrain"

	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// writeTerrain writes a DEM directory in the lab fetch tool's layout:
// index.json naming N41E044, and its tile, 5 x 5 samples at 0.25 deg
// from 42N 44E, elevation 580 + 20*row m. The tile is parsed with core's
// terrain.ParseTile before it is written, so a layout written wrong here
// fails here.
func writeTerrain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var b bytes.Buffer
	b.WriteString("P5\n")
	f := func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }
	for _, h := range [][2]string{
		{"Description", "ussp test tile"}, {"Dataset", "COP-DEM GLO-30"},
		{"Offset", f(terrain.OffsetM)}, {"Scale", f(terrain.ScaleM)}, {"Nodata", "65535"},
		{"LatFirst", "42"}, {"LonFirst", "44"}, {"LatStep", "0.25"}, {"LonStep", "0.25"},
	} {
		b.WriteString("# " + h[0] + " " + h[1] + "\n")
	}
	b.WriteString("5 5\n65535\n")
	for row := 0; row < 5; row++ {
		for col := 0; col < 5; col++ {
			raw := uint16(math.Round((580 + 20*float64(row) - terrain.OffsetM) / terrain.ScaleM))
			b.WriteByte(byte(raw >> 8))
			b.WriteByte(byte(raw))
		}
	}
	if _, err := terrain.ParseTile(b.Bytes()); err != nil {
		t.Fatalf("the test tile does not parse in core: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "N41E044.pgm"), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"cells":{"N41E044":"COP-DEM GLO-30"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The api's ground: the range under a volume's outline from the tiles
// (presence), no answer and counted where a part of it is outside the
// index (absence), and the readiness that says which.
func TestTerrainForIntentDecisions(t *testing.T) {
	store, cells, why := loadTerrain(writeTerrain(t))
	if store == nil || cells != 1 || why != "" {
		t.Fatalf("%v %d %q", store, cells, why)
	}
	var c core.Counters
	g := demGround{store: store, counters: &c}
	in := deconflict.Shape{Polygon: []core.LatLon{{LatDeg: 41.70, LonDeg: 44.78}, {LatDeg: 41.70, LonDeg: 44.82}, {LatDeg: 41.73, LonDeg: 44.82}, {LatDeg: 41.73, LonDeg: 44.78}}}
	lo, hi, ok := g.GroundRangeM(in)
	if !ok || math.Abs(lo-600) > 1e-9 || math.Abs(hi-620) > 1e-9 || c.Get(CounterGroundUnknown) != 0 {
		t.Fatalf("%v..%v %v %v", lo, hi, ok, c.Snapshot())
	}
	// A circle reaching across 45E into a cell the index lacks.
	out := deconflict.Shape{Circle: &geodesy.Circle{Center: core.LatLon{LatDeg: 41.7, LonDeg: 44.999}, RadiusM: 500}}
	if _, _, ok := g.GroundRangeM(out); ok || c.Get(CounterGroundUnknown) != 1 {
		t.Fatalf("outside the index: %v %v", ok, c.Snapshot())
	}
	state, detail := terrainProbe(store, cells, why)(context.Background())
	if state != obs.StateUp || detail != "1 cells indexed" {
		t.Fatalf("%s %q", state, detail)
	}
}

// Without USSP_TERRAIN_DIR, or with an index that does not read or
// parse, there is no ground and the readiness is degraded with why.
func TestTerrainNotConfigured(t *testing.T) {
	for name, dir := range map[string]string{"unset": "", "no index": t.TempDir()} {
		store, _, why := loadTerrain(dir)
		state, detail := terrainProbe(store, 0, why)(context.Background())
		if store != nil || state != obs.StateDegraded || !strings.Contains(detail, "USSP_TERRAIN_DIR") || !strings.Contains(detail, "not judged") {
			t.Fatalf("%s: %v %s %q", name, store, state, detail)
		}
	}
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "index.json"), []byte(`{"cells":{"X":"a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if store, _, why := loadTerrain(bad); store != nil || !strings.Contains(why, "does not parse") {
		t.Fatalf("%v %q", store, why)
	}
	big := t.TempDir()
	if err := os.WriteFile(filepath.Join(big, "index.json"), bytes.Repeat([]byte(" "), terrain.MaxIndexBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if store, _, why := loadTerrain(big); store != nil || !strings.Contains(why, "more than") {
		t.Fatalf("%v %q", store, why)
	}
}
