package peers

import (
	"math"
	"testing"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
)

// boxOfDiagonal is a box at 41.7 N whose sides are w by h metres.
func boxOf(w, h float64) geodesy.BBox {
	sw := core.LatLon{LatDeg: 41.7, LonDeg: 44.8}
	ne := geodesy.Destination(geodesy.Destination(sw, 0, h), 90, w)
	return geodesy.BBox{MinLat: sw.LatDeg, MinLon: sw.LonDeg, MaxLat: ne.LatDeg, MaxLon: ne.LonDeg}
}

// A view of 8 km is tiled into two views of at most 7 km (the brief's
// test); a view within the bound is its own one tile (E-01 pair); the
// tiles cover the box exactly.
func TestTileAnEightKilometreViewIntoTwo(t *testing.T) {
	b := boxOf(6400, 4800)
	if d := DiagonalM(b); d < 7900 || d > 8100 {
		t.Fatalf("diagonal %.0f m, want about 8 km", d)
	}
	ts, ok := Tile(b, MaxViewDiagonalM)
	if !ok || len(ts) != 2 {
		t.Fatalf("tiles %d %v", len(ts), ok)
	}
	for _, x := range ts {
		if DiagonalM(x) > f3411Max {
			t.Fatalf("a tile of %.0f m", DiagonalM(x))
		}
	}
	// Two halves that meet on one edge and together are the box.
	shared := ts[0].MaxLon == ts[1].MinLon && ts[0].MinLat == ts[1].MinLat && ts[0].MaxLat == ts[1].MaxLat ||
		ts[0].MaxLat == ts[1].MinLat && ts[0].MinLon == ts[1].MinLon && ts[0].MaxLon == ts[1].MaxLon
	if !shared || ts[0].MinLat != b.MinLat || ts[0].MinLon != b.MinLon || ts[1].MaxLat != b.MaxLat || ts[1].MaxLon != b.MaxLon {
		t.Fatalf("tiles do not cover the box: %+v", ts)
	}
	small := boxOf(3000, 3000)
	if ts, ok := Tile(small, MaxViewDiagonalM); !ok || len(ts) != 1 || ts[0] != small {
		t.Fatalf("a small view was cut: %+v", ts)
	}
}

const f3411Max = 7000.0

// A U-space airspace of 30 by 40 km: every tile within 7 km, the fewest
// the split allows; a box that needs more than MaxTiles, an empty box and
// one outside WGS84 are refused (E-10).
func TestTileBounds(t *testing.T) {
	b := boxOf(30000, 40000)
	ts, ok := Tile(b, MaxViewDiagonalM)
	if !ok {
		t.Fatal("refused")
	}
	for _, x := range ts {
		if DiagonalM(x) > MaxViewDiagonalM {
			t.Fatalf("a tile of %.0f m", DiagonalM(x))
		}
	}
	if lower := math.Ceil(30000 * 40000 / (MaxViewDiagonalM * MaxViewDiagonalM / 2)); float64(len(ts)) < lower {
		t.Fatalf("%d tiles, fewer than the %v squares can do", len(ts), lower)
	}
	if _, ok := Tile(geodesy.BBox{MinLat: -60, MinLon: -170, MaxLat: 60, MaxLon: 170}, MaxViewDiagonalM); ok {
		t.Fatal("a continent tiled")
	}
	for _, bad := range []geodesy.BBox{{MinLat: 2, MaxLat: 1}, {MinLat: 0, MaxLat: 1, MinLon: 0, MaxLon: math.NaN()}, {MinLat: -95, MaxLat: 1}} {
		if _, ok := Tile(bad, MaxViewDiagonalM); ok {
			t.Fatalf("%+v tiled", bad)
		}
	}
	if _, ok := Tile(b, 0); ok {
		t.Fatal("a zero bound tiled")
	}
}

func TestViewAndAreaStrings(t *testing.T) {
	b := geodesy.BBox{MinLat: 41.7, MinLon: 44.8, MaxLat: 41.75, MaxLon: 44.85}
	if ViewString(b) != "41.7000000,44.8000000,41.7500000,44.8500000" {
		t.Fatal(ViewString(b))
	}
	if AreaString(b) != "41.7000000,44.8000000,41.7000000,44.8500000,41.7500000,44.8500000,41.7500000,44.8000000" {
		t.Fatal(AreaString(b))
	}
	if !Overlaps(b, geodesy.BBox{MinLat: 41.75, MinLon: 44.85, MaxLat: 42, MaxLon: 45}) || Overlaps(b, geodesy.BBox{MinLat: 42, MaxLat: 43, MinLon: 44, MaxLon: 45}) {
		t.Fatal("Overlaps")
	}
	if v := boxVolume(b); v.OutlinePolygon == nil || len(v.OutlinePolygon.Vertices) != 4 {
		t.Fatal("boxVolume")
	}
	if !math.IsInf(DiagonalM(geodesy.BBox{MinLat: math.NaN()}), 1) {
		t.Fatal("DiagonalM of NaN")
	}
}
