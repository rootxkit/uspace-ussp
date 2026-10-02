// Package deconflict is this USSP's strategic deconfliction judgement
// (Reg. (EU) 2021/664 Art. 10(2)(b), (8), (9); docs/PLAN.md D2, §15 Q1;
// brief WP-7): whether two operational intents meet in space, time and
// altitude, and which one has precedence. uspace-core has no
// Volume4D-intersection package; this one is pinned by the vectors of
// testdata/vectors/deconfliction.json and proposed upstream once the
// lab's scenarios have run it (CLAUDE.md rule 3). Nothing else in this
// repository judges two intents against each other.
//
// A Volume is one F3548 Volume4D as the judgement uses it: its outline
// (a polygon or a circle), its vertical band in AMSL (D-01: separation is
// judged in AMSL, never AGL) and its time window. Two volumes conflict
// when all three meet:
//
//   - horizontally, when the separation of the outlines is at most the
//     horizontal buffer (Policy.HorizontalBufferM, 0 by default): outlines
//     that touch, cross or contain one another are 0 apart. The edges of
//     a polygon are straight in latitude and longitude, as uspace-core's
//     geodesy.Polygon reads them, so whether two polygons meet is decided
//     in degrees with longitudes unwrapped about one reference (the
//     antimeridian is no edge). A separation is a geodesic distance
//     (Vincenty, geodesy.DistanceM) from a vertex to the nearest point of
//     an edge, the point found on the local tangent plane of the vertex;
//     a circle is its published centre and radius (Z-11). The comparison
//     is conservative by Tolerance: 1 cm (F3548
//     IntersectionMinimumPrecisionCm) plus 1e-4 of the separation, so a
//     pair at the edge of the buffer is a conflict, never the reverse;
//   - vertically, when the AMSL bands overlap or are at most the
//     vertical buffer apart (Policy.VerticalBufferM); bands that touch
//     overlap;
//   - in time, when the closed intervals [start, end] share an instant.
//
// Precedence (Art. 10(8), (9)): a higher priority wins; at equal
// priority the intent whose volumes were filed first wins (RankAt), and
// at an equal instant the smaller id, so the pair decision is the same
// whichever is filed or checked first. The loser of a pair is rejected
// when it is the one being decided, and flagged for an update when it is
// an authorisation already given (Art. 10(10); WP-12 does the update).
// The package informs: it never proposes a resolution (CLAUDE.md rule 2).
//
// Thresholds are data (INV-03): a buffer that is negative or not finite
// refuses the whole check with an error, never disarms it (E-15). An
// outline or a band that cannot be judged (an invalid vertex, a radius
// that is not a positive number, a band upside down, a zero time) is an
// error, never "no conflict". Every input is bounded (MaxVolumes,
// MaxVertices, MaxOthers) and nothing here panics.
package deconflict
