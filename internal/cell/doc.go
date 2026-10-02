// Package cell is the partition key of this system (docs/PLAN.md D7,
// §15 Q2, reconciliation M35): a thin wrapper over uspace-core
// geodesy/cell, the grid the authority uses too. cell5 is the 0.1° x
// 0.1° cell named c5:<lat_idx>:<lon_idx>, cell3 the 1° x 1° cell named
// c3:<lat_idx>:<lon_idx>; the grid, the names, the neighbours and the
// cover are core's and are never defined here a second time.
//
// What this package adds is local:
//
//   - Key, the cell5 and cell3 of a position, as the subjects of
//     docs/PLAN.md §7 carry them;
//   - Ring1, CellsFor and CellsForEnvelope, the cell sets of a cell's
//     neighbourhood, a viewport and an intent's envelope, bounded (E-10);
//   - KVToken, the form of a cell name inside a NATS KV key (KV keys
//     admit no colon);
//   - Ownership, the cell3 cells one monitor instance owns, from
//     USSP_CELL_OWNERSHIP (all, or a comma list of c3 names; Q15).
//
// The cell is internal: it is a key in subjects, KV keys and the
// ownership map, never a field of an external interface (spec 05 §3).
// A test in this package reads api/openapi.yaml and every schema under
// schemas/ and fails on a cell property.
//
// Ring guarantee (LESSONS C-15): a point within 800 m of any point of a
// cell5 lies in that cell or in its Ring1, for every latitude where a
// 0.1° column is wider than 800 m (|lat| up to 85°, Georgia's 41°-44°
// with a margin of more than 7 km). The property test checks it against
// brute force on 10 000 random points.
//
// Nothing here panics on any input: an invalid position, name or box is
// a *core.FieldError.
package cell
