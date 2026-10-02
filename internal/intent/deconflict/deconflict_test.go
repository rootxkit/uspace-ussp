package deconflict

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
)

var t0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func square(lat, lon, sizeDeg float64) Shape {
	return Shape{Polygon: []core.LatLon{
		{LatDeg: lat, LonDeg: lon}, {LatDeg: lat, LonDeg: lon + sizeDeg},
		{LatDeg: lat + sizeDeg, LonDeg: lon + sizeDeg}, {LatDeg: lat + sizeDeg, LonDeg: lon},
	}}
}

func vol(s Shape) Volume {
	return Volume{Shape: s, LowerAMSLM: 80, UpperAMSLM: 130, Start: t0, End: t0.Add(30 * time.Minute)}
}

func one(id string, prio int, rank time.Time, v Volume) Intent {
	return Intent{ID: id, Priority: prio, RankAt: rank, Volumes: []Volume{v}}
}

// Precedes is antisymmetric for different ids, whatever the order of
// the check (the pair decision of Art. 10(9)).
func TestPrecedesIsAntisymmetric(t *testing.T) {
	early, late := t0, t0.Add(time.Minute)
	for _, c := range []struct{ a, b Intent }{
		{one("a", 0, early, Volume{}), one("b", 0, late, Volume{})},
		{one("a", 0, early, Volume{}), one("b", 0, early, Volume{})},
		{one("a", 0, early, Volume{}), one("b", 100, late, Volume{})},
		{one("z", 100, late, Volume{}), one("y", 100, late, Volume{})},
	} {
		ab, r1 := Precedes(c.a, c.b)
		ba, r2 := Precedes(c.b, c.a)
		if ab == ba || r1 != r2 {
			t.Errorf("%s/%s: %v %s and %v %s", c.a.ID, c.b.ID, ab, r1, ba, r2)
		}
	}
}

// E-15 pair: a valid zero buffer judges; an invalid one refuses the
// whole check, never "no conflict".
func TestInvalidPolicyRefusesTheCheck(t *testing.T) {
	a := one("a", 0, t0, vol(square(41.7, 44.8, 0.01)))
	b := one("b", 0, t0.Add(time.Second), vol(square(41.7, 44.8, 0.01)))
	if cs, err := Check(a, []Intent{b}, Policy{}); err != nil || len(cs) != 1 {
		t.Fatalf("zero buffers: %v %v", cs, err)
	}
	for _, p := range []Policy{
		{HorizontalBufferM: math.NaN()}, {HorizontalBufferM: math.Inf(1)}, {HorizontalBufferM: -0.1},
		{VerticalBufferM: math.NaN()}, {VerticalBufferM: -1},
	} {
		if cs, err := Check(a, []Intent{b}, p); err == nil || cs != nil {
			t.Errorf("%+v: %v %v", p, cs, err)
		}
		if ok, _, err := VolumesConflict(a.Volumes[0], b.Volumes[0], p); err == nil || ok {
			t.Errorf("VolumesConflict %+v: %v %v", p, ok, err)
		}
	}
}

// An input that cannot be judged is an error naming the field, never a
// silent "clear"; its valid twin judges.
func TestUnjudgeableInputsAreErrors(t *testing.T) {
	good := vol(square(41.7, 44.8, 0.01))
	other := one("o", 0, t0, good)
	bad := map[string]Volume{
		"neither outline":   {LowerAMSLM: 1, UpperAMSLM: 2, Start: t0, End: t0},
		"both outlines":     {Shape: Shape{Polygon: square(1, 1, 1).Polygon, Circle: &geodesy.Circle{Center: core.LatLon{}, RadiusM: 1}}, UpperAMSLM: 1, Start: t0, End: t0},
		"two vertices":      {Shape: Shape{Polygon: square(1, 1, 1).Polygon[:2]}, UpperAMSLM: 1, Start: t0, End: t0},
		"invalid vertex":    {Shape: Shape{Polygon: []core.LatLon{{LatDeg: 91}, {}, {LatDeg: 1}}}, UpperAMSLM: 1, Start: t0, End: t0},
		"zero radius":       {Shape: Shape{Circle: &geodesy.Circle{RadiusM: 0}}, UpperAMSLM: 1, Start: t0, End: t0},
		"NaN radius":        {Shape: Shape{Circle: &geodesy.Circle{RadiusM: math.NaN()}}, UpperAMSLM: 1, Start: t0, End: t0},
		"invalid centre":    {Shape: Shape{Circle: &geodesy.Circle{Center: core.LatLon{LatDeg: math.Inf(1)}, RadiusM: 5}}, UpperAMSLM: 1, Start: t0, End: t0},
		"band upside down":  {Shape: good.Shape, LowerAMSLM: 10, UpperAMSLM: 5, Start: t0, End: t0},
		"NaN band":          {Shape: good.Shape, LowerAMSLM: math.NaN(), UpperAMSLM: 5, Start: t0, End: t0},
		"zero time":         {Shape: good.Shape, UpperAMSLM: 5},
		"end before start":  {Shape: good.Shape, UpperAMSLM: 5, Start: t0, End: t0.Add(-time.Second)},
		"too many vertices": {Shape: Shape{Polygon: make([]core.LatLon, MaxVertices+1)}, UpperAMSLM: 5, Start: t0, End: t0},
	}
	for name, v := range bad {
		if _, err := Check(one("m", 0, t0, v), []Intent{other}, Policy{}); err == nil {
			t.Errorf("%s as mine: no error", name)
		}
		if _, err := Check(other, []Intent{one("x", 0, t0, v)}, Policy{}); err == nil {
			t.Errorf("%s as other: no error", name)
		}
	}
	if _, err := Check(one("m", 0, t0, good), []Intent{other}, Policy{}); err != nil {
		t.Fatalf("the valid twin: %v", err)
	}
	for name, in := range map[string]Intent{
		"no id":      {RankAt: t0, Volumes: []Volume{good}},
		"no rank":    {ID: "x", Volumes: []Volume{good}},
		"no volumes": {ID: "x", RankAt: t0},
	} {
		if _, err := Check(in, nil, Policy{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// E-10: every bound refuses one past it and takes the bound itself.
func TestBoundsAreExceededAndHeld(t *testing.T) {
	v := vol(square(10, 10, 0.001))
	far := vol(square(-10, -10, 0.001))
	mine := one("m", 0, t0, v)
	others := make([]Intent, MaxOthers+1)
	for i := range others {
		others[i] = one(fmt.Sprintf("o%05d", i), 0, t0, far)
	}
	if _, err := Check(mine, others, Policy{}); err == nil || !strings.Contains(err.Error(), "others") {
		t.Fatalf("MaxOthers+1: %v", err)
	}
	if cs, err := Check(mine, others[:MaxOthers], Policy{}); err != nil || len(cs) != 0 {
		t.Fatalf("MaxOthers: %v %v", cs, err)
	}
	many := Intent{ID: "m", RankAt: t0, Volumes: make([]Volume, MaxVolumes+1)}
	for i := range many.Volumes {
		many.Volumes[i] = v
	}
	if _, err := Check(many, nil, Policy{}); err == nil {
		t.Fatal("MaxVolumes+1 accepted")
	}
	many.Volumes = many.Volumes[:MaxVolumes]
	if _, err := Check(many, nil, Policy{}); err != nil {
		t.Fatalf("MaxVolumes: %v", err)
	}
}

// The previous version of the intent (same id) is never a conflict; a
// different intent in the same place is.
func TestOwnPreviousVersionIsSkipped(t *testing.T) {
	v := vol(square(41.7, 44.8, 0.01))
	mine := one("same", 0, t0, v)
	if cs, err := Check(mine, []Intent{one("same", 0, t0.Add(-time.Hour), v)}, Policy{}); err != nil || len(cs) != 0 {
		t.Fatalf("own version: %v %v", cs, err)
	}
	if cs, err := Check(mine, []Intent{one("other", 0, t0.Add(-time.Hour), v)}, Policy{}); err != nil || len(cs) != 1 || cs[0].MineWins {
		t.Fatalf("other: %v %v", cs, err)
	}
}

// Conflicts come back sorted by the other's id with the overlap.
func TestConflictsAreSortedWithTheirOverlap(t *testing.T) {
	v := vol(square(41.7, 44.8, 0.01))
	mine := one("m", 100, t0, v)
	cs, err := Check(mine, []Intent{one("z", 0, t0, v), one("a", 0, t0, v)}, Policy{})
	if err != nil || len(cs) != 2 || cs[0].OtherID != "a" || cs[1].OtherID != "z" {
		t.Fatalf("%+v %v", cs, err)
	}
	if !cs[0].MineWins || cs[0].Rule != RulePriority || cs[0].Overlap != (Overlap{HM: 0, VM: 50, TS: 1800}) {
		t.Fatalf("%+v", cs[0])
	}
}

// Within: an outline 0 m apart meets; a separation just above the
// buffer is clear and one just inside the tolerance is not.
func TestWithinAtTheBuffer(t *testing.T) {
	a := Shape{Circle: &geodesy.Circle{Center: core.LatLon{LatDeg: 41.7, LonDeg: 44.8}, RadiusM: 100}}
	centre := core.LatLon{LatDeg: 41.7, LonDeg: 44.81}
	d, err := geodesy.DistanceM(a.Circle.Center, centre)
	if err != nil {
		t.Fatal(err)
	}
	gap := d - 100 - 100
	b := Shape{Circle: &geodesy.Circle{Center: centre, RadiusM: 100}}
	if ok, sep, err := Within(a, b, gap-0.5); err != nil || ok || math.Abs(sep-gap) > 1e-6 {
		t.Errorf("buffer 0.5 m short: %v %v %v", ok, sep, err)
	}
	if ok, _, err := Within(a, b, gap-0.005); err != nil || !ok {
		t.Errorf("buffer within the 1 cm tolerance: %v %v", ok, err)
	}
	if ok, _, err := Within(a, b, gap); err != nil || !ok {
		t.Errorf("buffer equal to the gap: %v %v", ok, err)
	}
	if _, _, err := Within(a, b, -1); err == nil {
		t.Error("negative buffer accepted")
	}
	if Tolerance(0) != 0.01 || Tolerance(1000) != 0.11 {
		t.Errorf("tolerance %v %v", Tolerance(0), Tolerance(1000))
	}
}

// A polygon edge that crosses a long straight edge of another without a
// vertex inside either, and a circle against a polygon by its edges.
func TestEdgesDecideWhenNoVertexIsInside(t *testing.T) {
	bar := Shape{Polygon: []core.LatLon{{LatDeg: 0, LonDeg: -1}, {LatDeg: 0, LonDeg: 1}, {LatDeg: 0.001, LonDeg: 1}, {LatDeg: 0.001, LonDeg: -1}}}
	cross := Shape{Polygon: []core.LatLon{{LatDeg: -1, LonDeg: 0}, {LatDeg: -1, LonDeg: 0.001}, {LatDeg: 1, LonDeg: 0.001}, {LatDeg: 1, LonDeg: 0}}}
	if ok, sep, err := Within(bar, cross, 0); err != nil || !ok || sep != 0 {
		t.Fatalf("crossing bars: %v %v %v", ok, sep, err)
	}
	// A circle centred 0.01 deg north of the bar's long edge (~1.1 km).
	c := Shape{Circle: &geodesy.Circle{Center: core.LatLon{LatDeg: 0.011, LonDeg: 0.5}, RadiusM: 1000}}
	if ok, _, err := Within(bar, c, 0); err != nil || ok {
		t.Fatalf("circle short of the bar: %v %v", ok, err)
	}
	if ok, sep, err := Within(bar, c, 200); err != nil || !ok || sep < 100 || sep > 120 {
		t.Fatalf("the same within a 200 m buffer: %v %v %v", ok, sep, err)
	}
	c.Circle.RadiusM = 1200
	if ok, _, err := Within(c, bar, 0); err != nil || !ok {
		t.Fatalf("circle reaching the bar: %v %v", ok, err)
	}
}

// BenchmarkDeconflictEnvelope is one intent against 1000 active intents
// spread over a 1 x 1 degree area (docs/bench-targets.txt).
func BenchmarkDeconflictEnvelope(b *testing.B) {
	mine := one("m", 0, t0, vol(square(41.5, 44.5, 0.01)))
	others := make([]Intent, 1000)
	for i := range others {
		lat := 41.0 + float64(i%32)/32
		lon := 44.0 + float64(i/32)/32
		others[i] = one(fmt.Sprintf("o%04d", i), 0, t0, vol(square(lat, lon, 0.01)))
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Check(mine, others, Policy{HorizontalBufferM: 50, VerticalBufferM: 10}); err != nil {
			b.Fatal(err)
		}
	}
}
