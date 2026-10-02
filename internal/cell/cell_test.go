package cell

import (
	"errors"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	corecell "github.com/rootxkit/uspace-core/geodesy/cell"
)

func TestKeyTbilisi(t *testing.T) {
	c5, c3, err := Key(core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271})
	if err != nil || c5 != "c5:1317:2248" || c3 != "c3:131:224" {
		t.Fatalf("Key = %q %q %v", c5, c3, err)
	}
	if p, err := Parent3(c5); err != nil || p != c3 {
		t.Fatalf("Parent3 = %q %v", p, err)
	}
}

// Invalid input is a field error, never a panic (and never a cell).
func TestKeyRefusesInvalidPositions(t *testing.T) {
	for _, p := range []core.LatLon{
		{LatDeg: math.NaN(), LonDeg: 44}, {LatDeg: 41, LonDeg: math.Inf(1)}, {LatDeg: 90.5, LonDeg: 0}, {LatDeg: -91, LonDeg: 0},
	} {
		c5, c3, err := Key(p)
		var fe *core.FieldError
		if !errors.As(err, &fe) || c5 != "" || c3 != "" {
			t.Errorf("Key(%v) = %q %q %v", p, c5, c3, err)
		}
	}
}

func TestParseLevels(t *testing.T) {
	if _, err := Parse5("c5:1317:2248"); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse3("c3:131:224"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"c3:131:224", "c5:01317:2248", "c5:1317", "", "c5:1317:2248:1", "c5:1800:0", "x"} {
		if _, err := Parse5(bad); err == nil {
			t.Errorf("Parse5(%q) accepted", bad)
		}
	}
	if _, err := Parse3("c5:1317:2248"); err == nil {
		t.Error("Parse3 accepted a c5 name")
	}
	if _, err := Parent3("c3:1:1"); err == nil {
		t.Error("Parent3 accepted a c3 name")
	}
	if _, err := Ring1("nope"); err == nil {
		t.Error("Ring1 accepted a bad name")
	}
}

func TestRing1(t *testing.T) {
	r, err := Ring1("c5:1317:2248")
	if err != nil || len(r) != 8 || slices.Contains(r, "c5:1317:2248") || !slices.Contains(r, "c5:1318:2249") {
		t.Fatalf("Ring1 = %v %v", r, err)
	}
	// The antimeridian wraps.
	r, err = Ring1("c5:900:0")
	if err != nil || !slices.Contains(r, "c5:900:3599") {
		t.Fatalf("Ring1 at the antimeridian = %v %v", r, err)
	}
}

// Every point maps to exactly one cell5, and that cell's box holds it
// (property, 10 000 random points over the globe).
func TestEveryPointHasOneCell(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 10_000 {
		p := core.LatLon{LatDeg: rng.Float64()*180 - 90, LonDeg: rng.Float64()*360 - 180}
		c5, c3, err := Key(p)
		if err != nil {
			t.Fatalf("Key(%v): %v", p, err)
		}
		id, err := Parse5(c5)
		if err != nil {
			t.Fatal(err)
		}
		b := id.BBox()
		if p.LatDeg < b.MinLat || p.LatDeg >= b.MaxLat || p.LonDeg < b.MinLon || p.LonDeg >= b.MaxLon {
			t.Fatalf("%v is not in its cell %s %+v", p, c5, b)
		}
		if id.Parent().String() != c3 {
			t.Fatalf("%v: cell3 %s is not the parent of %s", p, c3, c5)
		}
		// No neighbour holds it as well: the grid partitions.
		for _, n := range id.Ring1() {
			nb := n.BBox()
			if p.LatDeg >= nb.MinLat && p.LatDeg < nb.MaxLat && p.LonDeg >= nb.MinLon && p.LonDeg < nb.MaxLon {
				t.Fatalf("%v is in %s and in its neighbour %s", p, c5, n)
			}
		}
	}
}

const ringRadiusM = 800 // the CPA neighbour radius the ring must cover (C-15)

// offset is the point northM, eastM metres from p on the local tangent
// plane; the caller checks the distance with core's haversine.
func offset(p core.LatLon, northM, eastM float64) core.LatLon {
	const r = 6_371_008.8
	lat := p.LatDeg + northM/r*180/math.Pi
	lon := p.LonDeg + eastM/(r*math.Cos(p.LatDeg*math.Pi/180))*180/math.Pi
	return core.LatLon{LatDeg: lat, LonDeg: core.WrapLonDeg(lon)}
}

// ringHolds checks the ring guarantee by brute force for points drawn
// in box: for a random point p and a random point q within 800 m of it,
// q's cell is p's cell or one of its Ring1.
func ringHolds(t *testing.T, rng *rand.Rand, minLat, maxLat, minLon, maxLon float64, n int) {
	t.Helper()
	checked := 0
	for checked < n {
		p := core.LatLon{LatDeg: minLat + rng.Float64()*(maxLat-minLat), LonDeg: minLon + rng.Float64()*(maxLon-minLon)}
		bearing := rng.Float64() * 2 * math.Pi
		d := rng.Float64() * ringRadiusM
		// Every tenth point sits on the circle itself, the worst case.
		if checked%10 == 0 {
			d = ringRadiusM * 0.999
		}
		q := offset(p, d*math.Cos(bearing), d*math.Sin(bearing))
		if q.LatDeg > 90 || q.LatDeg < -90 || geodesy.HaversineM(p, q) > ringRadiusM {
			continue
		}
		cp, _, err := Key(p)
		if err != nil {
			t.Fatal(err)
		}
		cq, _, err := Key(q)
		if err != nil {
			t.Fatal(err)
		}
		ring, err := Ring1(cp)
		if err != nil {
			t.Fatal(err)
		}
		if cq != cp && !slices.Contains(ring, cq) {
			t.Fatalf("%v (cell %s) and %v (cell %s) are %.1f m apart, but %s is outside the ring %v",
				p, cp, q, cq, geodesy.HaversineM(p, q), cq, ring)
		}
		checked++
	}
}

// The ring guarantee against brute force: 10 000 points over Georgia
// with a margin, and 10 000 over every latitude up to 85° (where a 0.1°
// column is still wider than 800 m).
func TestRingGuaranteeBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	ringHolds(t, rng, 40.5, 44.5, 39.5, 47.5, 10_000)
	ringHolds(t, rng, -85, 85, -180, 180, 10_000)
}

// Absence's twin (E-01): above 86° a 0.1° column is narrower than 800 m
// and the guarantee does not hold, which the same brute force finds.
func TestRingGuaranteeFailsNearThePole(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for range 100_000 {
		p := core.LatLon{LatDeg: 88 + rng.Float64(), LonDeg: rng.Float64()*360 - 180}
		q := offset(p, 0, ringRadiusM*0.99)
		if geodesy.HaversineM(p, q) > ringRadiusM {
			continue
		}
		cp, _, _ := Key(p)
		cq, _, _ := Key(q)
		ring, _ := Ring1(cp)
		if cq != cp && !slices.Contains(ring, cq) {
			return // found: the check can fail
		}
	}
	t.Fatal("no counterexample near the pole: the brute force cannot detect a broken ring")
}

func TestCellsFor(t *testing.T) {
	cells, err := CellsFor(geodesy.BBox{MinLat: 41.65, MinLon: 44.75, MaxLat: 41.75, MaxLon: 44.85})
	if err != nil || len(cells) != 4 || !slices.IsSorted(cells) || !slices.Contains(cells, "c5:1317:2248") {
		t.Fatalf("CellsFor = %v %v", cells, err)
	}
	// E-10: a box over the bound is refused, nothing allocated.
	if cells, err := CellsFor(geodesy.BBox{MinLat: -60, MinLon: -170, MaxLat: 60, MaxLon: 170}); err == nil || cells != nil {
		t.Fatalf("a continent-sized box: %d cells, %v", len(cells), err)
	}
	if _, err := CellsFor(geodesy.BBox{MinLat: math.NaN()}); err == nil {
		t.Fatal("a NaN box accepted")
	}
}

func TestCellsForEnvelope(t *testing.T) {
	cells, err := CellsForEnvelope(geodesy.BBox{MinLat: 41.71, MinLon: 44.82, MaxLat: 41.72, MaxLon: 44.83})
	if err != nil || len(cells) != 9 || !slices.Contains(cells, "c5:1317:2248") || !slices.Contains(cells, "c5:1316:2247") {
		t.Fatalf("CellsForEnvelope = %v %v", cells, err)
	}
	if !slices.IsSorted(cells) || len(slices.Compact(slices.Clone(cells))) != len(cells) {
		t.Fatalf("not sorted or duplicated: %v", cells)
	}
	// E-10: under MaxCells as a cover, over it with the ring.
	b := geodesy.BBox{MinLat: 30, MinLon: 30, MaxLat: 39.95, MaxLon: 39.95} // 100 x 100 = 10 000 cells
	if c, err := CellsFor(b); err != nil || len(c) != MaxCells {
		t.Fatalf("cover %d %v", len(c), err)
	}
	if c, err := CellsForEnvelope(b); err == nil || c != nil {
		t.Fatalf("envelope with ring over the bound: %d %v", len(c), err)
	}
	if _, err := CellsForEnvelope(geodesy.BBox{MinLat: math.Inf(1)}); err == nil {
		t.Fatal("an infinite box accepted")
	}
}

func TestKVToken(t *testing.T) {
	if got := KVToken("c5:1317:2248"); got != "c5.1317.2248" {
		t.Fatal(got)
	}
	if !regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`).MatchString(KVToken("c3:131:224")) {
		t.Fatal("not a valid KV key")
	}
}

func TestOwnership(t *testing.T) {
	all, err := ParseOwnership("all")
	if err != nil || !all.All() || !all.Owns("c3:131:224") || all.Cells() != nil || all.String() != "all" {
		t.Fatalf("all: %+v %v", all, err)
	}
	o, err := ParseOwnership(" c3:131:224 ,c3:131:225")
	if err != nil || o.All() || !o.Owns("c3:131:224") || !o.Owns("c3:131:225") || o.Owns("c3:132:224") {
		t.Fatalf("list: %+v %v", o, err)
	}
	if o.String() != "c3:131:224,c3:131:225" {
		t.Fatal(o.String())
	}
	var zero Ownership
	if zero.Owns("c3:131:224") {
		t.Fatal("the zero ownership owns a cell")
	}
	for _, bad := range []string{"", " ", "c5:1317:2248", "c3:131:224,", "c3:131:224,c3:131:224", "ALL", "c3:999:0"} {
		_, err := ParseOwnership(bad)
		var fe *core.FieldError
		if !errors.As(err, &fe) || fe.Field != "USSP_CELL_OWNERSHIP" {
			t.Errorf("ParseOwnership(%q) = %v", bad, err)
		}
	}
	// E-10: more than MaxOwnedCells names.
	many := make([]string, 0, MaxOwnedCells+1)
	for i := range MaxOwnedCells + 1 {
		many = append(many, corecell.ID{Level: corecell.Level3, LatIdx: i / 360, LonIdx: i % 360}.String())
	}
	if _, err := ParseOwnership(strings.Join(many, ",")); err == nil {
		t.Fatal("over the bound accepted")
	}
	if _, err := ParseOwnership(strings.Join(many[:MaxOwnedCells], ",")); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
}

// The cell is internal (spec 05 §3, brief WP-6 safety note): no
// property named cell, cell3 or cell5 in the published OpenAPI file or
// in any schema under schemas/. The twin below shows the scan finds one.
func TestNoCellOnAnExternalInterface(t *testing.T) {
	root := filepath.Join("..", "..")
	files := []string{filepath.Join(root, "api", "openapi.yaml")}
	err := filepath.WalkDir(filepath.Join(root, "schemas"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".json") || strings.HasSuffix(p, ".yaml")) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if names := cellProperties(string(b)); len(names) > 0 {
			t.Errorf("%s carries %v: the partition cell never crosses an external interface", f, names)
		}
	}
	t.Logf("scanned %d files", len(files))
}

func TestCellPropertyScanFindsOne(t *testing.T) {
	yaml := "properties:\n  position: {type: object}\n  cell5:\n    type: string\n"
	json := `{"properties": {"cell": {"type": "string"}}}`
	if got := cellProperties(yaml); !slices.Equal(got, []string{"cell5"}) {
		t.Fatalf("yaml: %v", got)
	}
	if got := cellProperties(json); !slices.Equal(got, []string{"cell"}) {
		t.Fatalf("json: %v", got)
	}
	if got := cellProperties("description: the cell of a spreadsheet\nexcellent: true\n"); got != nil {
		t.Fatalf("prose matched: %v", got)
	}
}

var cellProp = regexp.MustCompile(`(?m)(?:^\s*|[{,]\s*)"?(cell|cell3|cell5)"?\s*:`)

func cellProperties(s string) []string {
	var out []string
	for _, m := range cellProp.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}
