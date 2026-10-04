package peers

import (
	"math"
	"strconv"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"
)

// Bounds of the views (02 F7, E-10).
const (
	// MaxViewDiagonalM is the largest view polled: F3411's
	// NetMaxDisplayAreaDiagonalKm, less a margin so a Service Provider
	// that measures the diagonal another way still serves it.
	MaxViewDiagonalM = f3411.NetMaxDisplayAreaDiagonalKm*1000 - 100
	// MaxDetailsDiagonalM is the largest view whose flight details are
	// asked (NetDetailsMaxDisplayAreaDiagonalKm).
	MaxDetailsDiagonalM = f3411.NetDetailsMaxDisplayAreaDiagonalKm * 1000
	// MaxTiles bounds the views one area is tiled into: an area that
	// needs more is refused, counted and said on /readyz.
	MaxTiles = 4096
	// maxSplit bounds the tiles along one side the search tries.
	maxSplit = 256
)

// DiagonalM is the geodesic diagonal of b, corner to corner (as rid-sp
// measures a view, ParseView); +Inf when it cannot be computed.
func DiagonalM(b geodesy.BBox) float64 {
	d, err := geodesy.DistanceM(core.LatLon{LatDeg: b.MinLat, LonDeg: b.MinLon}, core.LatLon{LatDeg: b.MaxLat, LonDeg: b.MaxLon})
	if err != nil || !core.IsFinite(d) {
		return math.Inf(1)
	}
	return d
}

// Tile cuts b into the fewest equal tiles, nx along the longitude and ny
// along the latitude, whose diagonals are each at most maxDiagM; a box
// already within it is its own one tile. ok is false for an empty or
// invalid box and for one that needs more than MaxTiles tiles. The
// tiles cover b exactly, row by row from the south-west.
func Tile(b geodesy.BBox, maxDiagM float64) (tiles []geodesy.BBox, ok bool) {
	if !validBox(b) || !(maxDiagM > 0) {
		return nil, false
	}
	if DiagonalM(b) <= maxDiagM {
		return []geodesy.BBox{b}, true
	}
	// The box's extent in metres: its height, and its width along the
	// latitude nearest the equator (the widest).
	lat := math.Max(b.MinLat, math.Min(b.MaxLat, 0))
	widthM := geodesy.HaversineM(core.LatLon{LatDeg: lat, LonDeg: b.MinLon}, core.LatLon{LatDeg: lat, LonDeg: b.MaxLon})
	heightM := geodesy.HaversineM(core.LatLon{LatDeg: b.MinLat, LonDeg: b.MinLon}, core.LatLon{LatDeg: b.MaxLat, LonDeg: b.MinLon})
	bestX, bestY := 0, 0
	for nx := 1; nx <= maxSplit; nx++ {
		w := widthM / float64(nx)
		if w >= maxDiagM {
			continue
		}
		// The fewest rows a flat tile of this width allows, then up to
		// the first that the geodesic confirms.
		ny := max(1, int(math.Ceil(heightM/math.Sqrt(maxDiagM*maxDiagM-w*w))))
		for ny <= maxSplit && !tilesFit(b, nx, ny, maxDiagM) {
			ny++
		}
		if ny > maxSplit {
			continue
		}
		if bestX == 0 || nx*ny < bestX*bestY {
			bestX, bestY = nx, ny
		}
		if nx > 1 && nx*1 >= bestX*bestY {
			break
		}
	}
	if bestX == 0 || bestX*bestY > MaxTiles {
		return nil, false
	}
	return split(b, bestX, bestY), true
}

func validBox(b geodesy.BBox) bool {
	for _, v := range []float64{b.MinLat, b.MaxLat, b.MinLon, b.MaxLon} {
		if !core.IsFinite(v) {
			return false
		}
	}
	return b.MinLat <= b.MaxLat && b.MinLon <= b.MaxLon && b.MinLat >= -90 && b.MaxLat <= 90 && b.MinLon >= -180 && b.MaxLon <= 180
}

// tilesFit reports whether every tile of an nx by ny split is within
// maxDiagM. The tiles of one row are alike (same latitudes, same width
// in degrees), and rows differ only by their latitude, so the first tile
// of each row is measured.
func tilesFit(b geodesy.BBox, nx, ny int, maxDiagM float64) bool {
	dLat, dLon := (b.MaxLat-b.MinLat)/float64(ny), (b.MaxLon-b.MinLon)/float64(nx)
	for j := 0; j < ny; j++ {
		t := geodesy.BBox{MinLat: b.MinLat + float64(j)*dLat, MaxLat: b.MinLat + float64(j+1)*dLat, MinLon: b.MinLon, MaxLon: b.MinLon + dLon}
		if DiagonalM(t) > maxDiagM {
			return false
		}
	}
	return true
}

// split cuts b into nx by ny equal tiles, row by row from the
// south-west; the last row and column end exactly on b's edges.
func split(b geodesy.BBox, nx, ny int) []geodesy.BBox {
	dLat, dLon := (b.MaxLat-b.MinLat)/float64(ny), (b.MaxLon-b.MinLon)/float64(nx)
	out := make([]geodesy.BBox, 0, nx*ny)
	for j := 0; j < ny; j++ {
		for i := 0; i < nx; i++ {
			t := geodesy.BBox{MinLat: b.MinLat + float64(j)*dLat, MaxLat: b.MinLat + float64(j+1)*dLat,
				MinLon: b.MinLon + float64(i)*dLon, MaxLon: b.MinLon + float64(i+1)*dLon}
			if j == ny-1 {
				t.MaxLat = b.MaxLat
			}
			if i == nx-1 {
				t.MaxLon = b.MaxLon
			}
			out = append(out, t)
		}
	}
	return out
}

// Overlaps reports whether a and b share a point.
func Overlaps(a, b geodesy.BBox) bool {
	return a.MinLat <= b.MaxLat && b.MinLat <= a.MaxLat && a.MinLon <= b.MaxLon && b.MinLon <= a.MaxLon
}

func coord(v float64) string { return strconv.FormatFloat(v, 'f', 7, 64) }

// ViewString is b as F3411's view, lat1,lng1,lat2,lng2 (south-west,
// north-east).
func ViewString(b geodesy.BBox) string {
	return coord(b.MinLat) + "," + coord(b.MinLon) + "," + coord(b.MaxLat) + "," + coord(b.MaxLon)
}

// AreaString is b as F3411's GeoPolygonString (lat,lng vertices,
// counter-clockwise from the south-west), the area of an ISA search.
func AreaString(b geodesy.BBox) string {
	return coord(b.MinLat) + "," + coord(b.MinLon) + "," + coord(b.MinLat) + "," + coord(b.MaxLon) + "," +
		coord(b.MaxLat) + "," + coord(b.MaxLon) + "," + coord(b.MaxLat) + "," + coord(b.MinLon)
}

// boxVolume is b as an F3411 outline polygon, counter-clockwise from the
// south-west corner, without altitude bounds (a Display Provider's
// interest has none).
func boxVolume(b geodesy.BBox) f3411.Volume3D {
	return f3411.Volume3D{OutlinePolygon: &f3411.Polygon{Vertices: []f3411.LatLngPoint{
		{Lat: b.MinLat, Lng: b.MinLon}, {Lat: b.MinLat, Lng: b.MaxLon},
		{Lat: b.MaxLat, Lng: b.MaxLon}, {Lat: b.MaxLat, Lng: b.MinLon},
	}}}
}
