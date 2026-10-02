package cell

import (
	"slices"
	"strings"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	corecell "github.com/rootxkit/uspace-core/geodesy/cell"
)

// MaxCells bounds every cell set this package returns (E-10): a
// viewport or an envelope covering more cell5 cells is refused. 10 000
// cell5 cells are about 10° x 10°, larger than Georgia.
const MaxCells = 10_000

// Key returns the names of the cell5 and the cell3 holding p. An
// invalid position is refused with a *core.FieldError naming "lat_deg"
// or "lon_deg".
func Key(p core.LatLon) (cell5, cell3 string, err error) {
	c5, err := corecell.Of(p, corecell.Level5)
	if err != nil {
		return "", "", err
	}
	return c5.String(), c5.Parent().String(), nil
}

// Parse5 parses a cell5 name; a cell3 name or anything String never
// produces is refused naming "cell".
func Parse5(name string) (corecell.ID, error) {
	id, err := corecell.Parse(name)
	if err != nil {
		return corecell.ID{}, err
	}
	if id.Level != corecell.Level5 {
		return corecell.ID{}, core.Fieldf("cell", "%q is not a c5 cell", name)
	}
	return id, nil
}

// Parse3 parses a cell3 name; a cell5 name or anything String never
// produces is refused naming "cell".
func Parse3(name string) (corecell.ID, error) {
	id, err := corecell.Parse(name)
	if err != nil {
		return corecell.ID{}, err
	}
	if id.Level != corecell.Level3 {
		return corecell.ID{}, core.Fieldf("cell", "%q is not a c3 cell", name)
	}
	return id, nil
}

// Parent3 is the name of the cell3 holding the cell5 cell5.
func Parent3(cell5 string) (string, error) {
	id, err := Parse5(cell5)
	if err != nil {
		return "", err
	}
	return id.Parent().String(), nil
}

// Ring1 are the names of the cell5 cells sharing an edge or a corner
// with cell5, sorted (core's order). The cell itself is not in it.
func Ring1(cell5 string) ([]string, error) {
	id, err := Parse5(cell5)
	if err != nil {
		return nil, err
	}
	return names(id.Ring1()), nil
}

// CellsFor are the names of every cell5 the box intersects, sorted; a
// box crossing the antimeridian has MinLon > MaxLon. More than MaxCells
// is refused naming "bbox".
func CellsFor(b geodesy.BBox) ([]string, error) {
	ids, err := corecell.Cover(b, corecell.Level5, MaxCells)
	if err != nil {
		return nil, err
	}
	out := names(ids)
	slices.Sort(out)
	return out, nil
}

// CellsForEnvelope are the cell5 cells an intent's envelope (its
// bounding box) is listed under in intent_active: the cover of the box
// and the ring 1 of every cell in it, so a monitor that owns a cell
// beside the envelope sees an intent whose aircraft may cross into it.
// Sorted, without duplicates; more than MaxCells is refused naming
// "bbox".
func CellsForEnvelope(b geodesy.BBox) ([]string, error) {
	ids, err := corecell.Cover(b, corecell.Level5, MaxCells)
	if err != nil {
		return nil, err
	}
	set := make(map[corecell.ID]struct{}, len(ids)*2)
	for _, id := range ids {
		set[id] = struct{}{}
		for _, n := range id.Ring1() {
			set[n] = struct{}{}
		}
		if len(set) > MaxCells {
			return nil, core.Fieldf("bbox", "the envelope and its ring cover more than %d cells", MaxCells)
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id.String())
	}
	slices.Sort(out)
	return out, nil
}

// KVToken is the form of a cell name inside a NATS KV key, which admits
// no colon: c5:1317:2248 is c5.1317.2248. Subjects carry the name
// itself.
func KVToken(name string) string { return strings.ReplaceAll(name, ":", ".") }

func names(ids []corecell.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
