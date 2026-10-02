package cell

import (
	"slices"
	"strings"

	"github.com/rootxkit/uspace-core/core"
)

// OwnAll is the USSP_CELL_OWNERSHIP value of an instance that owns
// every cell (the demo, docs/PLAN.md §15 Q15).
const OwnAll = "all"

// MaxOwnedCells bounds an ownership list (E-10): Georgia spans about 30
// cell3 cells; 1000 is far beyond any deployment.
const MaxOwnedCells = 1000

// Ownership is the set of cell3 cells one monitor instance owns. The
// zero Ownership owns nothing.
type Ownership struct {
	all   bool
	cells map[string]struct{}
}

// ParseOwnership reads USSP_CELL_OWNERSHIP: "all", or a comma list of
// cell3 names (spaces around a name are ignored). An empty value, an
// empty entry, a name that is not a c3 cell, a duplicate or more than
// MaxOwnedCells names is refused naming USSP_CELL_OWNERSHIP: an instance
// never starts owning less than it was told by a typo.
func ParseOwnership(s string) (Ownership, error) {
	const field = "USSP_CELL_OWNERSHIP"
	s = strings.TrimSpace(s)
	if s == OwnAll {
		return Ownership{all: true}, nil
	}
	if s == "" {
		return Ownership{}, core.Fieldf(field, "empty: give %q or a comma list of c3 cells", OwnAll)
	}
	parts := strings.Split(s, ",")
	if len(parts) > MaxOwnedCells {
		return Ownership{}, core.Fieldf(field, "more than %d cells", MaxOwnedCells)
	}
	o := Ownership{cells: make(map[string]struct{}, len(parts))}
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if _, err := Parse3(p); err != nil {
			return Ownership{}, core.Fieldf(field, "entry %d (%q) is not a c3 cell", i+1, p)
		}
		if _, dup := o.cells[p]; dup {
			return Ownership{}, core.Fieldf(field, "entry %d (%q) is listed twice", i+1, p)
		}
		o.cells[p] = struct{}{}
	}
	return o, nil
}

// All reports whether the instance owns every cell.
func (o Ownership) All() bool { return o.all }

// Owns reports whether cell3 is owned.
func (o Ownership) Owns(cell3 string) bool {
	if o.all {
		return true
	}
	_, ok := o.cells[cell3]
	return ok
}

// Cells are the owned cell3 names, sorted; nil for All.
func (o Ownership) Cells() []string {
	if o.all {
		return nil
	}
	out := make([]string, 0, len(o.cells))
	for c := range o.cells {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// String is the ownership as USSP_CELL_OWNERSHIP spells it.
func (o Ownership) String() string {
	if o.all {
		return OwnAll
	}
	return strings.Join(o.Cells(), ",")
}
