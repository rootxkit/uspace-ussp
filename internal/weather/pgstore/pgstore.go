// Package pgstore is internal/weather's Store on the relational
// database: weather_products and weather_source_status (migrations
// 00005, 00024) through the sqlc queries of
// internal/store/queries/relational/weather.sql. Every time is the
// database clock. Its tests are the integration tests
// (test/integration).
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
	"github.com/rootxkit/uspace-ussp/internal/weather"
)

// Store is weather.Store on PostgreSQL.
type Store struct{ S *store.Store }

var _ weather.Store = Store{}

// EventFetched is the events row of a fetch that stored products.
const EventFetched = "weather_products_stored"

// Now implements weather.Store.
func (p Store) Now(ctx context.Context) (time.Time, error) {
	t, err := p.S.Queries().WeatherNow(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("database clock: %w", err)
	}
	return t.UTC(), nil
}

// Save implements weather.Store: one transaction, its events row when a
// product was new.
func (p Store) Save(ctx context.Context, source string, ps []weather.NewProduct, radiusM float64) (int, error) {
	if len(ps) == 0 {
		return 0, nil
	}
	n := 0
	err := p.S.Tx(ctx, func(q *relational.Queries) error {
		n = 0
		for i := range ps {
			raw, err := json.Marshal(ps[i].Content)
			if err != nil {
				return fmt.Errorf("weather product: %w", err)
			}
			k, err := q.WeatherInsert(ctx, relational.WeatherInsertParams{
				LonDeg: ps[i].Content.Area.LonDeg, LatDeg: ps[i].Content.Area.LatDeg, RadiusM: radiusM,
				ObservedAt: ps[i].ObservedAt, ValidFrom: ps[i].ValidFrom, ValidTo: ps[i].ValidTo, Source: source,
				Product: raw, Station: ps[i].Station, Kind: string(ps[i].Kind),
			})
			if err != nil {
				return fmt.Errorf("insert weather product %s %s: %w", ps[i].Station, ps[i].Kind, err)
			}
			n += int(k)
		}
		if n == 0 {
			return nil
		}
		_, err := store.Audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "weather", EntityType: "weather_source",
			EntityID: source, EventType: EventFetched, Payload: map[string]any{"stored": n, "received": len(ps)}})
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RecordFetch implements weather.Store.
func (p Store) RecordFetch(ctx context.Context, source, failure string) error {
	var err error
	if failure == "" {
		err = p.S.Queries().WeatherFetchSucceeded(ctx, source)
	} else {
		err = p.S.Queries().WeatherFetchFailed(ctx, relational.WeatherFetchFailedParams{Source: source, LastError: &failure})
	}
	if err != nil {
		return fmt.Errorf("weather source status: %w", err)
	}
	return nil
}

// Status implements weather.Store.
func (p Store) Status(ctx context.Context, source string) (weather.SourceStatus, error) {
	r, err := p.S.Queries().WeatherSourceStatus(ctx, source)
	if store.IsNoRows(err) {
		return weather.SourceStatus{}, nil
	}
	if err != nil {
		return weather.SourceStatus{}, fmt.Errorf("weather source status: %w", err)
	}
	st := weather.SourceStatus{LastAttemptAt: &r.LastAttemptAt, LastSuccessAt: r.LastSuccessAt, LastFailureAt: r.LastFailureAt}
	if r.LastError != nil {
		st.LastError = *r.LastError
	}
	return st, nil
}

func productOf(id, station, kind, source string, observed, from, to, fetched time.Time, raw []byte) (weather.Product, error) {
	p := weather.Product{ID: id, Station: station, Kind: weather.Kind(kind), Source: source, ObservedAt: observed.UTC(),
		ValidFrom: from.UTC(), ValidTo: to.UTC(), FetchedAt: fetched.UTC()}
	if err := json.Unmarshal(raw, &p.Content); err != nil {
		return weather.Product{}, fmt.Errorf("weather product %s: %w", p.ID, err)
	}
	return p, nil
}

// Newest implements weather.Store.
func (p Store) Newest(ctx context.Context, source string, box geodesy.BBox, at time.Time, limit int) ([]weather.Product, error) {
	rows, err := p.S.Queries().WeatherNewest(ctx, relational.WeatherNewestParams{Source: source, AtTime: at,
		West: box.MinLon, South: box.MinLat, East: box.MaxLon, North: box.MaxLat, N: int32(min(max(limit, 1), weather.MaxAnswer))})
	if err != nil {
		return nil, fmt.Errorf("weather products: %w", err)
	}
	out := make([]weather.Product, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		pr, err := productOf(store.UUIDText(r.ID), r.Station, r.Kind, r.Source, r.ObservedAt, r.ValidFrom, r.ValidTo, r.FetchedAt, r.Product)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, nil
}

// InForce implements weather.Store.
func (p Store) InForce(ctx context.Context, source string, boxes []geodesy.BBox, from, to time.Time, limit int) ([]weather.Product, error) {
	if len(boxes) == 0 {
		return nil, nil
	}
	rows, err := p.S.Queries().WeatherInForce(ctx, relational.WeatherInForceParams{Source: source, FromTime: from, ToTime: to,
		BoxesWkt: BoxesWKT(boxes), N: int32(min(max(limit, 1), weather.MaxAnswer))})
	if err != nil {
		return nil, fmt.Errorf("weather products in force: %w", err)
	}
	out := make([]weather.Product, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		pr, err := productOf(store.UUIDText(r.ID), r.Station, r.Kind, r.Source, r.ObservedAt, r.ValidFrom, r.ValidTo, r.FetchedAt, r.Product)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, nil
}

// Prune implements weather.Store.
func (p Store) Prune(ctx context.Context, before time.Time, limit int) (int64, error) {
	n, err := p.S.Queries().WeatherPrune(ctx, relational.WeatherPruneParams{Before: before, N: int32(min(max(limit, 1), weather.PruneBatch))})
	if err != nil {
		return 0, fmt.Errorf("prune weather products: %w", err)
	}
	return n, nil
}

// BoxesWKT is the boxes as one WKT MULTIPOLYGON (longitude first).
func BoxesWKT(boxes []geodesy.BBox) string {
	var b strings.Builder
	b.WriteString("MULTIPOLYGON(")
	for i, x := range boxes {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "((%[1]g %[2]g,%[3]g %[2]g,%[3]g %[4]g,%[1]g %[4]g,%[1]g %[2]g))", x.MinLon, x.MinLat, x.MaxLon, x.MaxLat)
	}
	b.WriteByte(')')
	return b.String()
}
