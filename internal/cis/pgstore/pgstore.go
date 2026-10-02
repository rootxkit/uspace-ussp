// Package pgstore is internal/cis's Store on the relational database:
// the cis_* tables of migrations 00004 and 00009, through the sqlc
// queries of internal/store/queries/relational/cis.sql. Its tests are
// the integration tests (test/integration), against PostgreSQL +
// PostGIS.
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed318"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is cis.Store on the relational database (the cis_* tables of
// migrations 00004 and 00009).
type Store struct{ S *store.Store }

var _ cis.Store = Store{}

// SaveVersion stores v, its features and the pruning of older versions
// in one transaction.
func (p Store) SaveVersion(ctx context.Context, v *cis.Version, es []*cis.Entry) error {
	meta, err := json.Marshal(v.Meta)
	if err != nil {
		return err
	}
	var etag *string
	if v.ETag != "" {
		etag = &v.ETag
	}
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		row, err := q.InsertCISDataset(ctx, relational.InsertCISDatasetParams{
			Dataset: string(v.Dataset), Version: v.Number, Etag: etag, Metadata: meta, SignatureOk: v.SignatureOK,
		})
		if err != nil {
			return fmt.Errorf("cis_datasets: %w", err)
		}
		if !row.Inserted {
			return nil // stored before: a version is written once
		}
		for _, e := range es {
			fp, err := featureParams(v, e)
			if err != nil {
				return err
			}
			if err := q.InsertCISFeature(ctx, fp); err != nil {
				return fmt.Errorf("cis_features %s: %w", e.Identifier, err)
			}
		}
		if _, err := q.PruneCISVersions(ctx, string(v.Dataset)); err != nil {
			return fmt.Errorf("pruning cis_datasets: %w", err)
		}
		return nil
	})
}

type circleParam struct {
	LonDeg  float64 `json:"lon_deg"`
	LatDeg  float64 `json:"lat_deg"`
	RadiusM float64 `json:"radius_m"`
}

type polygonParam struct {
	Type        string         `json:"type"`
	Coordinates [][][2]float64 `json:"coordinates"`
}

func featureParams(v *cis.Version, e *cis.Entry) (relational.InsertCISFeatureParams, error) {
	polys := []polygonParam{}
	circles := []circleParam{}
	var walk func(g *ed318.Geometry)
	walk = func(g *ed318.Geometry) {
		switch {
		case g.Type == ed318.GeometryCollection:
			for i := range g.Geometries {
				walk(&g.Geometries[i])
			}
		case g.Center != nil && g.RadiusM != nil:
			circles = append(circles, circleParam{LonDeg: g.Center.LonDeg, LatDeg: g.Center.LatDeg, RadiusM: *g.RadiusM})
		default:
			pp := polygonParam{Type: "Polygon"}
			for _, ring := range g.Rings {
				r := make([][2]float64, len(ring))
				for k, pt := range ring {
					r[k] = [2]float64{pt.LonDeg, pt.LatDeg} // GeoJSON order (Z-03)
				}
				pp.Coordinates = append(pp.Coordinates, r)
			}
			polys = append(polys, pp)
		}
	}
	walk(&e.Feature.Geometry)
	pj, err := json.Marshal(polys)
	if err != nil {
		return relational.InsertCISFeatureParams{}, err
	}
	cj, err := json.Marshal(circles)
	if err != nil {
		return relational.InsertCISFeatureParams{}, err
	}
	fp := relational.InsertCISFeatureParams{
		Dataset: string(v.Dataset), Version: v.Number, FeatureID: e.Identifier, Feature: e.Raw,
		Polygons: pj, Circles: cj, ApplicableFrom: e.ValidFrom, ApplicableTo: e.ValidTo,
	}
	if e.Type != "" {
		t := string(e.Type)
		fp.ZoneType = &t
	}
	// Limits as ED-318 gives them, in metres with their reference
	// (D-02: never a stored AGL conversion), for a single-layer zone; a
	// zone of several layers keeps them in the feature only.
	if l := e.Feature.Geometry.Layer; l != nil && e.Feature.Geometry.Type != ed318.GeometryCollection {
		if m := l.LowerM(); m != nil && l.LowerReference != "" {
			ref := string(l.LowerReference)
			fp.LowerM, fp.LowerRef = m, &ref
		}
		if m := l.UpperM(); m != nil && l.UpperReference != "" {
			ref := string(l.UpperReference)
			fp.UpperM, fp.UpperRef = m, &ref
		}
	}
	return fp, nil
}

// TouchVersion moves fetched_at to now on the database clock.
func (p Store) TouchVersion(ctx context.Context, d cis.Dataset, version int64) error {
	_, err := p.S.Queries().TouchCISDataset(ctx, relational.TouchCISDatasetParams{Dataset: string(d), Version: version})
	if store.IsNoRows(err) {
		return nil
	}
	return err
}

// LoadCurrent reads the newest version of every dataset back and parses
// it as a served body (its features, and the cis_* members from its
// metadata), so a stored version is judged exactly like a pulled one.
func (p Store) LoadCurrent(ctx context.Context) ([]cis.StoredVersion, error) {
	rows, err := p.S.Queries().CurrentCISDatasets(ctx)
	if err != nil {
		return nil, err
	}
	var out []cis.StoredVersion
	for _, r := range rows {
		d, ok := cis.ParseDataset(r.Dataset)
		if !ok {
			continue
		}
		var meta cis.Metadata
		if err := json.Unmarshal(r.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("cis_datasets %s:%d metadata: %w", r.Dataset, r.Version, err)
		}
		etag := ""
		if r.Etag != nil {
			etag = *r.Etag
		}
		var body []byte
		if d == cis.USSPList {
			body = meta.USSPList
		} else {
			feats, err := p.S.Queries().CISFeatures(ctx, relational.CISFeaturesParams{Dataset: r.Dataset, Version: r.Version})
			if err != nil {
				return nil, err
			}
			fs := make([]json.RawMessage, len(feats))
			for i, f := range feats {
				fs[i] = f.Feature
			}
			doc := map[string]any{"type": "FeatureCollection", "features": fs, "cis_dataset": r.Dataset, "cis_version": r.Version}
			if meta.CISUpdatedAt != nil {
				doc["cis_updated_at"] = meta.CISUpdatedAt.Format(time.RFC3339Nano)
			}
			if len(meta.PublisherStaleSince) > 0 {
				doc["cis_publisher_stale_since"] = meta.PublisherStaleSince
			}
			if body, err = json.Marshal(doc); err != nil {
				return nil, err
			}
		}
		v, rf := cis.ParseVersion(d, body, etag, r.Version)
		if rf != nil {
			return nil, fmt.Errorf("stored %s: %w", fmt.Sprintf("%s:%d", d, r.Version), rf)
		}
		v.Meta.Issued, v.Meta.Provider, v.Meta.Delta = meta.Issued, meta.Provider, meta.Delta
		v.SignatureOK = r.SignatureOk
		out = append(out, cis.StoredVersion{Version: v, AgeS: r.AgeS})
	}
	return out, nil
}

// MarkPulled marks the notifications of d up to version as pulled.
func (p Store) MarkPulled(ctx context.Context, d cis.Dataset, version int64) error {
	_, err := p.S.Queries().MarkCISNotificationsPulled(ctx, relational.MarkCISNotificationsPulledParams{Dataset: string(d), Version: &version})
	return err
}

// RememberJTI records a delivery id (RememberCISJTI).
func (p Store) RememberJTI(ctx context.Context, issuer, jti string, ttl time.Duration, maxLive int64) (fresh, full bool, err error) {
	if len(jti) > 256 || len(issuer) > 2048 {
		return false, false, core.Fieldf("jti", "longer than this receiver records")
	}
	row, err := p.S.Queries().RememberCISJTI(ctx, relational.RememberCISJTIParams{Issuer: issuer, Jti: jti, TtlS: ttl.Seconds(), MaxRows: maxLive})
	if err != nil {
		return false, false, err
	}
	if row.Inserted {
		return true, false, nil
	}
	return false, row.Live >= maxLive, nil
}

// InsertNotification logs n.
func (p Store) InsertNotification(ctx context.Context, n cis.Notification) error {
	ids := n.FeatureIDs
	if ids == nil {
		ids = []string{}
	}
	v := n.Version
	_, err := p.S.Queries().InsertCISNotification(ctx, relational.InsertCISNotificationParams{
		Dataset: string(n.Dataset), Version: &v, FeatureIds: ids, Reason: &n.Reason, Issuer: &n.Issuer,
		Jti: &n.JTI, Subscription: &n.Subscription, MsgID: &n.MsgID,
	})
	return err
}

// Sweep deletes expired delivery ids and notification log rows older
// than keepDays.
func (p Store) Sweep(ctx context.Context, keepDays int) error {
	if _, err := p.S.Queries().SweepCISJTIs(ctx); err != nil {
		return err
	}
	_, err := p.S.Queries().SweepCISNotifications(ctx, int32(max(1, keepDays)))
	return err
}
