package cis

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/geodesy/cell"
)

// BucketCISCurrent is the KV bucket the projection is written to
// (docs/PLAN.md §7).
const BucketCISCurrent = "cis_current"

// AllCells is the key of the entry holding the zones too large to be
// listed per cell (more than MaxCellsPerZone cells): every follower
// reads it beside its own cells, so a large zone is never missed.
const AllCells = "all"

// MaxCellsPerZone bounds the cell5 cells one zone is listed in (E-10);
// a larger zone goes to AllCells. 400 cells is about 2 x 2 degrees.
const MaxCellsPerZone = 400

// ApplicableZone is one zone as the projection lists it: the
// zone/applicable/v1 body (uspace-lab schemas/common/zone/applicable/v1)
// evaluated when the projection was built, with the dataset, the
// restriction state and the feature as published, so a follower that
// judges (monitor) holds what it needs. cis_applicability carries this
// system's own verdict in the CISP's vocabulary: the cache pulls whole
// datasets, which carry no CISP annotation.
type ApplicableZone struct {
	Identifier       string          `json:"identifier"`
	Type             string          `json:"type"`
	Applies          bool            `json:"applies"`
	CISApplicability string          `json:"cis_applicability"`
	Version          string          `json:"version"`
	ValidFrom        *time.Time      `json:"valid_from"`
	ValidTo          *time.Time      `json:"valid_to"`
	At               time.Time       `json:"at"`
	CISVersion       string          `json:"cis_version"`
	Dataset          string          `json:"dataset"`
	RestrictionState string          `json:"restriction_state,omitempty"`
	Feature          json.RawMessage `json:"feature"`
}

// CellEntry is the value of one key of cis_current.
type CellEntry struct {
	Cell       string           `json:"cell"`
	CISVersion string           `json:"cis_version"`
	CISAgeS    float64          `json:"cis_age_s"`
	Stale      bool             `json:"stale"`
	At         time.Time        `json:"at"`
	Zones      []ApplicableZone `json:"zones"`
}

// Projection is the whole cis_current content for one snapshot.
type Projection struct {
	Basis
	At    time.Time
	Cells map[string]CellEntry
}

// Projector writes a projection to the KV bucket the hot path reads
// (WP-6 implements it on internal/bus). An error leaves the cache
// installed in this process and is counted and shown on /readyz.
type Projector interface {
	ProjectCIS(ctx context.Context, p *Projection) error
}

// BasisProjector is a Projector that can rewrite the basis of the last
// projection alone (a confirmation of the same versions: the cells are
// unchanged). ErrNotProjected when it holds no whole projection of its
// own to rewrite the basis of.
type BasisProjector interface {
	ProjectCISBasis(ctx context.Context, b Basis, at time.Time) error
}

// ErrNotProjected is a basis rewrite with no whole projection written
// before it by this projector.
var ErrNotProjected = errors.New("no projection written yet: the basis alone cannot be rewritten")

// MemoryProjector keeps the last projection in memory: the projector of
// one process until WP-6's bus projector exists.
type MemoryProjector struct {
	mu   sync.Mutex
	last *Projection
	n    int
}

// ProjectCIS keeps p.
func (m *MemoryProjector) ProjectCIS(_ context.Context, p *Projection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = p
	m.n++
	return nil
}

// ProjectCISBasis implements BasisProjector: the last projection with
// b and at, counted as a write.
func (m *MemoryProjector) ProjectCISBasis(_ context.Context, b Basis, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		return ErrNotProjected
	}
	p := *m.last
	p.Basis, p.At = b, at.UTC()
	m.last = &p
	m.n++
	return nil
}

// Last is the last projection (nil before the first) and how many were
// written.
func (m *MemoryProjector) Last() (*Projection, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.n
}

// Project builds the projection of the current snapshot at at: every
// feature of every ED-318 dataset, listed under each cell5 its parts'
// bounding boxes cover (AllCells beyond MaxCellsPerZone), with whether
// it applies at at.
func (e *Evaluator) Project(at time.Time) *Projection {
	b := e.basis()
	p := &Projection{Basis: b, At: at.UTC(), Cells: map[string]CellEntry{}}
	s := e.snap.Load()
	for _, en := range s.all {
		// The feature applies when any of its parts does; it is unknown
		// when none does and one is unknown.
		appl := NotApplicable
		for k := range en.Parts {
			switch a, _ := e.applicability(en, k, at); {
			case a == Applies:
				appl = Applies
			case a == Unknown && appl == NotApplicable:
				appl = Unknown
			}
		}
		z := ApplicableZone{
			Identifier: en.Identifier, Type: string(en.Type), Applies: appl == Applies,
			CISApplicability: string(appl), Version: versionLabel(en.Dataset, en.Version),
			ValidFrom: en.ValidFrom, ValidTo: en.ValidTo, At: p.At, CISVersion: b.CISVersion,
			Dataset: string(en.Dataset), Feature: en.Raw,
		}
		if en.Restriction != nil {
			z.RestrictionState = string(en.Restriction.State)
		}
		keys := map[string]bool{}
		for _, part := range en.Parts {
			ids, cerr := cell.Cover(part.BBox, cell.Level5, MaxCellsPerZone)
			if cerr != nil {
				keys[AllCells] = true
				continue
			}
			for _, id := range ids {
				keys[id.String()] = true
			}
		}
		for k := range keys {
			ce := p.Cells[k]
			ce.Zones = append(ce.Zones, z)
			p.Cells[k] = ce
		}
	}
	for k, ce := range p.Cells {
		ce.Cell, ce.CISVersion, ce.CISAgeS, ce.Stale, ce.At = k, b.CISVersion, b.CISAgeS, b.Stale, p.At
		sort.Slice(ce.Zones, func(i, j int) bool {
			if ce.Zones[i].Dataset != ce.Zones[j].Dataset {
				return ce.Zones[i].Dataset < ce.Zones[j].Dataset
			}
			return ce.Zones[i].Identifier < ce.Zones[j].Identifier
		})
		p.Cells[k] = ce
	}
	return p
}
