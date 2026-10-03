package intent

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rootxkit/uspace-core/geodesy"
)

// normalisedOf rebuilds the judgement's view of a stored intent: its
// volumes as authorised (WireVolume with the undulation they were
// decided with, so no geoid is needed again), their boxes, windows and
// AMSL bands.
func normalisedOf(r *Record) (*Normalised, error) {
	n := &Normalised{Request: r.Request, TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, Priority: r.Priority, Exempt: r.Exempt, Cells: r.Cells}
	if len(r.VolumesAMSL) != len(r.Request.Volumes) {
		return nil, fmt.Errorf("intent %s: %d volumes and %d AMSL bands", r.ID, len(r.Request.Volumes), len(r.VolumesAMSL))
	}
	for i, w := range r.Request.Volumes {
		dv, err := WireVolume(w, r.VolumesAMSL[i].UndulationM)
		if err != nil {
			return nil, err
		}
		v := Volume{Index: i, Wire: w, Shape: dv.Shape, BBox: dv.Shape.BBox(), AMSL: r.VolumesAMSL[i], Start: dv.Start, End: dv.End}
		if dv.Shape.Circle != nil {
			v.Centroid = dv.Shape.Circle.Center
		} else if len(dv.Shape.Polygon) > 0 {
			v.Centroid = centroid(dv.Shape.Polygon)
		}
		n.Volumes = append(n.Volumes, v)
	}
	return n, nil
}

// Windows are the boxes and windows of the caller's own intent's
// volumes as authorised (GET /v1/geo/intents/{id}, brief WP-12):
// another operator's intent is not found, never forbidden.
func (s *Service) Windows(ctx context.Context, clientID, id string) ([]geodesy.BBox, []time.Time, []time.Time, error) {
	o, err := s.owner(ctx, clientID)
	if err != nil {
		return nil, nil, nil, err
	}
	if !validUUID(id) {
		return nil, nil, nil, refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	r, err := s.Store.Get(ctx, id)
	if err != nil {
		return nil, nil, nil, &UnavailableError{Dependency: "database", Detail: "the intent could not be read"}
	}
	if r == nil || r.OperatorID != o.OperatorID {
		return nil, nil, nil, refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	n, err := normalisedOf(r)
	if err != nil {
		return nil, nil, nil, &UnavailableError{Dependency: "database", Detail: "the intent's volumes could not be read"}
	}
	boxes := make([]geodesy.BBox, len(n.Volumes))
	from := make([]time.Time, len(n.Volumes))
	to := make([]time.Time, len(n.Volumes))
	for i := range n.Volumes {
		boxes[i], from[i], to[i] = n.Volumes[i].BBox, n.Volumes[i].Start, n.Volumes[i].End
	}
	return boxes, from, to, nil
}
