package deconflict

import (
	"fmt"
	"math"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
)

// MaxVertices bounds one polygon outline (F3548 OiMaxVertices).
const MaxVertices = f3548.OiMaxVertices

// Shape is a volume's outline: exactly one of Polygon (its vertices, at
// least three, the ring closing itself as F3548's Polygon does) and
// Circle.
type Shape struct {
	Polygon []core.LatLon
	Circle  *geodesy.Circle
}

// check refuses an outline that cannot be judged, naming the field.
func (s Shape) check(field string) error {
	switch {
	case s.Circle != nil && s.Polygon != nil:
		return core.Fieldf(field, "has both a polygon and a circle")
	case s.Circle != nil:
		if !s.Circle.Center.Valid() {
			return core.Fieldf(field+".circle.center", "is not a valid WGS84 position")
		}
		if !core.IsFinite(s.Circle.RadiusM) || s.Circle.RadiusM <= 0 {
			return core.Fieldf(field+".circle.radius_m", "must be a positive number of metres, got %v", s.Circle.RadiusM)
		}
		return nil
	case s.Polygon != nil:
		n := len(s.Polygon)
		if n < 3 || n > MaxVertices {
			return core.Fieldf(field+".polygon", "has %d vertices; 3 to %d", n, MaxVertices)
		}
		for i, p := range s.Polygon {
			if !p.Valid() {
				return core.Fieldf(fmt.Sprintf("%s.polygon[%d]", field, i), "is not a valid WGS84 position")
			}
		}
		return nil
	}
	return core.Fieldf(field, "has neither a polygon nor a circle")
}

// BBox is a box holding the whole outline (conservatively larger for a
// circle); a box across the antimeridian has MinLon > MaxLon.
func (s Shape) BBox() geodesy.BBox {
	if s.Circle != nil {
		return s.Circle.BBox()
	}
	r := make(geodesy.Ring, len(s.Polygon))
	copy(r, s.Polygon)
	return geodesy.Polygon{Rings: []geodesy.Ring{r}}.BBox()
}

// Tolerance is the conservative margin of a horizontal comparison at a
// separation of sepM metres: 1 cm (F3548 IntersectionMinimumPrecisionCm)
// plus 1e-4 of the separation, which covers the tangent-plane search for
// the nearest point of an edge.
func Tolerance(sepM float64) float64 {
	return float64(f3548.IntersectionMinimumPrecisionCm)/100 + 1e-4*math.Abs(sepM)
}

// Within reports whether a and b are at most bufferM apart horizontally
// (with Tolerance), and their separation in metres (0 when the outlines
// meet). The separation is exact only when it matters: a pair whose
// boxes, padded by the buffer, do not overlap is reported as not within
// with separation +Inf. Both outlines must pass their checks.
func Within(a, b Shape, bufferM float64) (bool, float64, error) {
	if !core.IsFinite(bufferM) || bufferM < 0 {
		return false, 0, core.Fieldf("buffer_m", "must be a finite number of at least 0, got %v", bufferM)
	}
	if err := a.check("a"); err != nil {
		return false, 0, err
	}
	if err := b.check("b"); err != nil {
		return false, 0, err
	}
	pad := bufferM + Tolerance(bufferM) + 1
	if !boxesOverlap(a.BBox().PadM(pad), b.BBox()) {
		return false, math.Inf(1), nil
	}
	sep, err := separation(a, b)
	if err != nil {
		return false, 0, err
	}
	return sep <= bufferM+Tolerance(sep), sep, nil
}

// separation is the horizontal distance between two checked outlines in
// metres, 0 when they meet.
func separation(a, b Shape) (float64, error) {
	switch {
	case a.Circle != nil && b.Circle != nil:
		d, err := geodesy.DistanceM(a.Circle.Center, b.Circle.Center)
		if err != nil {
			return 0, err
		}
		return math.Max(0, d-a.Circle.RadiusM-b.Circle.RadiusM), nil
	case a.Circle != nil:
		return circlePolygon(*a.Circle, b.Polygon)
	case b.Circle != nil:
		return circlePolygon(*b.Circle, a.Polygon)
	}
	return polygonPolygon(a.Polygon, b.Polygon)
}

func circlePolygon(c geodesy.Circle, poly []core.LatLon) (float64, error) {
	d, err := pointPolygon(c.Center, poly)
	if err != nil {
		return 0, err
	}
	return math.Max(0, d-c.RadiusM), nil
}

// pointPolygon is the distance from p to the polygon, 0 inside or on it.
func pointPolygon(p core.LatLon, poly []core.LatLon) (float64, error) {
	if contains(poly, p) {
		return 0, nil
	}
	best := math.Inf(1)
	n := len(poly)
	for i := range n {
		d, err := pointSegment(p, poly[i], poly[(i+1)%n])
		if err != nil {
			return 0, err
		}
		best = math.Min(best, d)
	}
	return best, nil
}

func contains(poly []core.LatLon, p core.LatLon) bool {
	r := make(geodesy.Ring, len(poly))
	copy(r, poly)
	return geodesy.Polygon{Rings: []geodesy.Ring{r}}.Contains(p)
}

// pointSegment is the geodesic distance from p to the nearest point of
// the edge s0-s1 (straight in latitude and longitude), the point found
// on p's tangent plane.
func pointSegment(p, s0, s1 core.LatLon) (float64, error) {
	k := math.Cos(p.LatDeg * math.Pi / 180)
	ax, ay := core.WrapLonDeg(s0.LonDeg-p.LonDeg), s0.LatDeg-p.LatDeg
	bx := ax + core.WrapLonDeg(s1.LonDeg-s0.LonDeg)
	by := s1.LatDeg - p.LatDeg
	dx, dy := (bx-ax)*k, by-ay
	t := 0.0
	if l2 := dx*dx + dy*dy; l2 > 0 {
		t = math.Max(0, math.Min(1, -(ax*k*dx+ay*dy)/l2))
	}
	c := core.LatLon{LatDeg: p.LatDeg + ay + t*(by-ay), LonDeg: core.WrapLonDeg(p.LonDeg + ax + t*(bx-ax))}
	c.LatDeg = math.Max(-90, math.Min(90, c.LatDeg))
	return geodesy.DistanceM(p, c)
}

// polygonPolygon is 0 when the polygons meet (an edge crosses or touches
// an edge, or one holds a vertex of the other), else the smallest
// vertex-to-edge distance either way.
func polygonPolygon(a, b []core.LatLon) (float64, error) {
	if contains(b, a[0]) || contains(a, b[0]) {
		return 0, nil
	}
	ref := a[0].LonDeg
	ua, ub := unwrap(a, ref), unwrap(b, ref)
	for i := range ua {
		p1, p2 := ua[i], ua[(i+1)%len(ua)]
		for j := range ub {
			if segmentsMeet(p1, p2, ub[j], ub[(j+1)%len(ub)]) {
				return 0, nil
			}
		}
	}
	best := math.Inf(1)
	for _, pair := range [][2][]core.LatLon{{a, b}, {b, a}} {
		for _, v := range pair[0] {
			d, err := pointPolygon(v, pair[1])
			if err != nil {
				return 0, err
			}
			best = math.Min(best, d)
		}
	}
	return best, nil
}

// xy is a vertex with its longitude unwrapped about a reference.
type xy struct{ x, y float64 }

func unwrap(ps []core.LatLon, ref float64) []xy {
	out := make([]xy, len(ps))
	for i, p := range ps {
		out[i] = xy{x: ref + core.WrapLonDeg(p.LonDeg-ref), y: p.LatDeg}
	}
	return out
}

func orient(a, b, c xy) float64 { return (b.x-a.x)*(c.y-a.y) - (b.y-a.y)*(c.x-a.x) }

func onSeg(a, b, p xy) bool {
	return math.Min(a.x, b.x) <= p.x && p.x <= math.Max(a.x, b.x) && math.Min(a.y, b.y) <= p.y && p.y <= math.Max(a.y, b.y)
}

// segmentsMeet reports whether two closed segments share a point
// (crossing, touching or collinear overlap).
func segmentsMeet(p1, p2, q1, q2 xy) bool {
	d1, d2 := orient(q1, q2, p1), orient(q1, q2, p2)
	d3, d4 := orient(p1, p2, q1), orient(p1, p2, q2)
	if ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) && ((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0)) {
		return true
	}
	return d1 == 0 && onSeg(q1, q2, p1) || d2 == 0 && onSeg(q1, q2, p2) ||
		d3 == 0 && onSeg(p1, p2, q1) || d4 == 0 && onSeg(p1, p2, q2)
}

// boxesOverlap reports whether two boxes share a point; a box with
// MinLon above MaxLon crosses the antimeridian and is taken as its two
// halves. An empty box overlaps nothing.
func boxesOverlap(a, b geodesy.BBox) bool {
	if !(a.MinLat <= a.MaxLat) || !(b.MinLat <= b.MaxLat) || a.MaxLat < b.MinLat || b.MaxLat < a.MinLat {
		return false
	}
	for _, x := range lonSpans(a) {
		for _, y := range lonSpans(b) {
			if x[0] <= y[1] && y[0] <= x[1] {
				return true
			}
		}
	}
	return false
}

func lonSpans(b geodesy.BBox) [][2]float64 {
	if b.MinLon <= b.MaxLon {
		return [][2]float64{{b.MinLon, b.MaxLon}}
	}
	return [][2]float64{{b.MinLon, 180}, {-180, b.MaxLon}}
}
