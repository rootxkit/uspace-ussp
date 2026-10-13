"use client";

// One operational volume's outline (Annex IV item 5): a polygon's
// vertices or a circle's centre and radius, drawn on the map with the
// kit's DrawLayer (a click adds a vertex or places the centre, a drag
// moves one) or typed with its OutlineFields (the keyboard path), and
// sent as an ASTM F3548 Volume4D outline exactly as entered (brief
// WP-17). The page does no geometry: no area, no buffer, no circle drawn
// as a polygon, no check that a polygon is simple. The API judges the
// outline and returns the AMSL derivation and every problem by its field
// path.
//
// A circle is shown as its centre, with its centre and radius in words:
// the kit draws a circle's outline only when the API drew it
// (`circleOutline`), and this API draws none (docs/PLAN.md §15.1 Q28).
import { DrawLayer } from "@rootxkit/uspace-ui/layers";
import { OutlineFields } from "@rootxkit/uspace-ui/form";
import type { DrawOutline, DrawPoint } from "@rootxkit/uspace-ui/model";
import type { Schemas } from "@/lib/api";

export type Point = DrawPoint;

export type Outline = DrawOutline;

/**
 * The most vertices the editor takes: api/openapi.yaml IntentVolume4D's
 * outline_polygon.vertices maxItems (e2e/portal.spec.ts pins it).
 */
export const MAX_VERTICES = 1000;

/** The F3548 outline member of a Volume3D, as entered; null while it is incomplete. */
export function outlineOf(o: Outline): Schemas["IntentVolume4D"]["volume"] | null {
  if (o.kind === "polygon") {
    return o.vertices.length === 0
      ? null
      : ({ outline_polygon: { vertices: o.vertices.map((p) => ({ lat: p.lat, lng: p.lng })) } } as Schemas["IntentVolume4D"]["volume"]);
  }
  if (o.center === null || o.radiusM === null) return null;
  return { outline_circle: { center: { lat: o.center.lat, lng: o.center.lng }, radius: { value: o.radiusM, units: "M" } } } as Schemas["IntentVolume4D"]["volume"];
}

/** The outline on the enclosing map: the kit's drawing tool, no circle outline (the API draws none). */
export function VolumeDrawing({ outline, onChange }: { outline: Outline; onChange(o: Outline): void }) {
  return <DrawLayer id="portal-draw" outline={outline} onChange={onChange} maxVertices={MAX_VERTICES} circleOutline={null} />;
}

/** The outline's typed side: the kit's fields, the kind, the points or the circle, and the way to clear them. */
export function VolumeFields({ outline, onChange }: { outline: Outline; onChange(o: Outline): void }) {
  return (
    <div data-testid="outline">
      <OutlineFields outline={outline} onChange={onChange} maxVertices={MAX_VERTICES} circleOutlineShown={false} legendKey="portal.intent.outline.legend" />
    </div>
  );
}
