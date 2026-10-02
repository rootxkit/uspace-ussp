package pgstore

import (
	"strings"
	"testing"

	"github.com/rootxkit/uspace-core/geodesy"
)

// A box is one polygon; a box across the antimeridian is two; a box
// wider than 90 degrees is cut so no geography polygon is ambiguous.
func TestWKTSplitsBoxes(t *testing.T) {
	for _, c := range []struct {
		b    geodesy.BBox
		want string
	}{
		{geodesy.BBox{MinLat: 1, MinLon: 2, MaxLat: 3, MaxLon: 4}, "MULTIPOLYGON(((2 1,4 1,4 3,2 3,2 1)))"},
		{geodesy.BBox{MinLat: 0, MinLon: 179, MaxLat: 1, MaxLon: -179},
			"MULTIPOLYGON(((179 0,180 0,180 1,179 1,179 0)),((-180 0,-179 0,-179 1,-180 1,-180 0)))"},
		{geodesy.BBox{MinLat: 80, MinLon: -180, MaxLat: 90, MaxLon: 180},
			"MULTIPOLYGON(((-180 80,-90 80,-90 90,-180 90,-180 80)),((-90 80,0 80,0 90,-90 90,-90 80)),((0 80,90 80,90 90,0 90,0 80)),((90 80,180 80,180 90,90 90,90 80)))"},
		{geodesy.BBox{MinLat: 5, MinLon: 7, MaxLat: 6, MaxLon: 7}, "MULTIPOLYGON(((7 5,7 5,7 6,7 6,7 5)))"},
	} {
		if got := WKT([]geodesy.BBox{c.b}); got != c.want {
			t.Errorf("%+v:\n got %s\nwant %s", c.b, got, c.want)
		}
	}
	if got := WKT([]geodesy.BBox{{MinLat: 1, MinLon: 1, MaxLat: 2, MaxLon: 2}, {MinLat: 3, MinLon: 3, MaxLat: 4, MaxLon: 4}}); strings.Count(got, "((") != 2 {
		t.Errorf("two boxes: %s", got)
	}
}
