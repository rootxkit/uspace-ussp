// Package pgstore is internal/conformance's StateStore on the relational
// database: the conformance_states timeline (migration 00002) through the
// sqlc queries of internal/store/queries/relational/conformance.sql. Its
// tests are the integration tests (test/integration).
package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is conformance.StateStore on PostgreSQL.
type Store struct{ S *store.Store }

var _ conformance.StateStore = Store{}

// RecordState implements conformance.StateStore: one appended row with
// the last position the monitor reported, false when the flight is not
// in the flights table yet.
func (p Store) RecordState(ctx context.Context, b conformance.StateBody, at time.Time) (bool, error) {
	id, err := store.UUID("flight_id", b.FlightID)
	if err != nil {
		return false, err
	}
	params := relational.InsertConformanceStateParams{
		At: at.UTC(), State: string(b.State), Reason: b.Reason, DistanceOutsideM: b.DistanceOutsideM,
		HeightOverM: b.HeightOverM, TimeOutsideS: b.TimeOutsideS, PolicyVersion: b.PolicyVersion, FlightID: id,
	}
	// A position outside WGS84 is left out rather than refused: the state
	// is the record, the position only its detail.
	if pos := b.Position; pos != nil && (core.LatLon{LatDeg: pos.Lat, LonDeg: pos.Lng}).Valid() {
		params.LastLatDeg, params.LastLngDeg = &pos.Lat, &pos.Lng
	}
	n, err := p.S.Queries().InsertConformanceState(ctx, params)
	if err != nil {
		return false, fmt.Errorf("conformance state: %w", err)
	}
	return n == 1, nil
}
