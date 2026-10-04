// Package pgstore is internal/records' Reader on the relational database
// and Series on the time-series database (read only: tsdb-writer is the
// only writer of the hypertables), through the sqlc queries of
// internal/store/queries/{relational,timeseries}/records.sql, plus the
// daily bundles' table and the gap records api keeps from src.v1. Its
// tests are the integration tests (test/integration).
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/records"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
	"github.com/rootxkit/uspace-ussp/internal/store/timeseries"
)

// Reader is records.Reader on PostgreSQL.
type Reader struct{ S *store.Store }

// Series is records.Series on TimescaleDB.
type Series struct{ S *store.Store }

var (
	_ records.Reader = Reader{}
	_ records.Series = Series{}
)

func n32(n int) int32 { return int32(min(max(n, 1), 100_000)) }

func opt(u string) *string {
	if u == "" {
		return nil
	}
	return &u
}

func raw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

// Flight implements records.Reader.
func (p Reader) Flight(ctx context.Context, id string) (records.FlightRow, error) {
	fid, err := store.UUID("flight_id", id)
	if err != nil {
		return records.FlightRow{}, records.ErrNotFound
	}
	r, err := p.S.Queries().RecordFlight(ctx, fid)
	if store.IsNoRows(err) {
		return records.FlightRow{}, records.ErrNotFound
	}
	if err != nil {
		return records.FlightRow{}, fmt.Errorf("flight: %w", err)
	}
	return records.FlightRow{ID: store.UUIDText(r.ID), IntentID: opt(store.UUIDText(r.IntentID)), AuthorisationNumber: r.AuthorisationNumber,
		UASSerial: r.UasSerial, OperatorReg: r.OperatorReg, ClientID: r.ClientID, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
		EndReason: r.EndReason, RIDFlightID: r.RidFlightID, Emergency: r.Emergency, LastState: r.LastState}, nil
}

// Intent implements records.Reader.
func (p Reader) Intent(ctx context.Context, id string) (records.IntentRow, error) {
	iid, err := store.UUID("intent_id", id)
	if err != nil {
		return records.IntentRow{}, err
	}
	r, err := p.S.Queries().RecordIntent(ctx, iid)
	if err != nil {
		return records.IntentRow{}, fmt.Errorf("intent: %w", err)
	}
	v := records.IntentView{IntentID: store.UUIDText(r.ID), Version: int(r.Version), LocalState: r.LocalState, DSSState: r.DssState,
		Decision: r.Decision, AuthorisationNumber: r.AuthorisationNumber, Priority: int(r.Priority), TimeStart: r.TimeStart, TimeEnd: r.TimeEnd,
		Volumes: raw(r.Volumes), DeviationThresholds: raw(r.DeviationThresholds), Conflicts: raw(r.Conflicts), Conditions: r.Conditions,
		CISVersionChecked: r.CisVersionChecked, RegistryCheckedAt: r.RegistryCheckedAt, PolicyVersion: r.PolicyVersion,
		InUSpaceAirspace: r.InUspaceAirspace, USpaceAirspaceIDs: r.UspaceAirspaceIds, ExemptArt13: r.ExemptArt13, Mode: r.Mode,
		FlightType: r.FlightType, Category: r.Category, ClassLabel: r.ClassLabel, IdentificationTechnology: r.IdentificationTechnology,
		ConnectivityMethods: r.ConnectivityMethods, UASSerial: r.UasSerial, UARegistration: r.UaRegistration, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.EnduranceS != nil {
		e := int(*r.EnduranceS)
		v.EnduranceS = &e
	}
	return records.IntentRow{IntentView: v, OperatorReg: r.OperatorReg}, nil
}

// Versions implements records.Reader.
func (p Reader) Versions(ctx context.Context, intentID string, n int) ([]records.Version, error) {
	iid, err := store.UUID("intent_id", intentID)
	if err != nil {
		return nil, err
	}
	rows, err := p.S.Queries().RecordIntentVersions(ctx, relational.RecordIntentVersionsParams{IntentID: iid, N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("intent versions: %w", err)
	}
	out := make([]records.Version, 0, len(rows))
	for _, r := range rows {
		out = append(out, records.Version{Version: int(r.Version), At: r.At, Actor: r.Actor, ChangeReason: r.ChangeReason, Decision: raw(r.Decision)})
	}
	return out, nil
}

// Alerts implements records.Reader.
func (p Reader) Alerts(ctx context.Context, flightID string, n int) ([]records.Alert, error) {
	fid, err := store.UUID("flight_id", flightID)
	if err != nil {
		return nil, err
	}
	rows, err := p.S.Queries().RecordAlerts(ctx, relational.RecordAlertsParams{FlightID: fid, N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("alerts: %w", err)
	}
	out := make([]records.Alert, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, records.Alert{AlertID: store.UUIDText(r.ID), Kind: r.Kind, Severity: r.Severity, State: r.State, RaisedAt: r.RaisedAt,
			UpdatedAt: r.UpdatedAt, ClearedAt: r.ClearedAt, ClearReason: r.ClearReason, CapturedAt: r.CapturedAt, AckedAt: r.AckedAt,
			EscalatedAt: r.EscalatedAt, PeerRef: r.PeerRef, PolicyVersion: r.PolicyVersion, Detail: raw(r.Detail)})
	}
	return out, nil
}

// Conformance implements records.Reader.
func (p Reader) Conformance(ctx context.Context, flightID string, n int) ([]records.ConformanceState, error) {
	fid, err := store.UUID("flight_id", flightID)
	if err != nil {
		return nil, err
	}
	rows, err := p.S.Queries().RecordConformance(ctx, relational.RecordConformanceParams{FlightID: fid, N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("conformance: %w", err)
	}
	out := make([]records.ConformanceState, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, records.ConformanceState{At: r.At, State: r.State, Reason: r.Reason, DistanceOutsideM: r.DistanceOutsideM,
			HeightOverM: r.HeightOverM, TimeOutsideS: r.TimeOutsideS, PolicyVersion: r.PolicyVersion, ATSNotifiedAt: r.AtsNotifiedAt, ATSAckRef: r.AtsAckRef})
	}
	return out, nil
}

// Notices implements records.Reader.
func (p Reader) Notices(ctx context.Context, flightID, intentID string, n int) ([]records.Notice, error) {
	fid, err := store.UUID("flight_id", flightID)
	if err != nil {
		return nil, err
	}
	params := relational.RecordNoticesParams{FlightID: fid, N: n32(n)}
	if intentID != "" {
		if params.IntentID, err = store.UUID("intent_id", intentID); err != nil {
			return nil, err
		}
	}
	rows, err := p.S.Queries().RecordNotices(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("coordination notices: %w", err)
	}
	out := make([]records.Notice, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, records.Notice{NoticeRef: r.NoticeRef, Kind: r.Kind, State: r.State, CreatedAt: r.CreatedAt, ReceivedAt: r.ReceivedAt,
			AckID: r.AckID, AcknowledgedAt: r.AcknowledgedAt, AcknowledgedBy: r.AcknowledgedBy, EscalatedAt: r.EscalatedAt, FailedAt: r.FailedAt})
	}
	return out, nil
}

// IngestGaps implements records.Reader.
func (p Reader) IngestGaps(ctx context.Context, clientID string, from, to time.Time, n int) ([]records.IngestGap, error) {
	rows, err := p.S.Queries().RecordIngestGaps(ctx, relational.RecordIngestGapsParams{SourceInstance: clientID, FromAt: &from, ToAt: &to, N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("ingest gaps: %w", err)
	}
	out := make([]records.IngestGap, 0, len(rows))
	for _, r := range rows {
		if r.GapStarted == nil || r.GapEnded == nil {
			continue
		}
		out = append(out, records.IngestGap{Cause: r.Cause, Started: *r.GapStarted, Ended: *r.GapEnded, Dropped: int(r.Dropped), RecordedAt: r.RecordedAt})
	}
	return out, nil
}

// Policies implements records.Reader.
func (p Reader) Policies(ctx context.Context, versions []int64) ([]records.PolicyVersion, error) {
	rows, err := p.S.Queries().RecordPolicies(ctx, versions)
	if err != nil {
		return nil, fmt.Errorf("policy versions: %w", err)
	}
	out := make([]records.PolicyVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, records.PolicyVersion{PolicyVersion: r.Version, CreatedAt: r.CreatedAt, Values: raw(r.Values)})
	}
	return out, nil
}

// Now implements records.Reader: the database clock.
func (p Reader) Now(ctx context.Context) (time.Time, error) {
	return p.S.Queries().DBNow(ctx)
}

func (p Series) q() (*timeseries.Queries, error) {
	if _, err := p.S.Pool(store.TreeTimeseries); err != nil {
		return nil, err
	}
	return p.S.TSQueries(), nil
}

// Summary implements records.Series.
func (p Series) Summary(ctx context.Context, flightID string) (records.Summary, error) {
	q, err := p.q()
	if err != nil {
		return records.Summary{}, err
	}
	fid, err := store.UUID("flight_id", flightID)
	if err != nil {
		return records.Summary{}, err
	}
	r, err := q.RecordTelemetrySummary(ctx, fid)
	if err != nil {
		return records.Summary{}, fmt.Errorf("telemetry summary: %w", err)
	}
	out := records.Summary{Samples: r.Samples}
	if r.Samples == 0 {
		return out, nil
	}
	first, last := r.FirstAt.UTC(), r.LastAt.UTC()
	out.FirstAt, out.LastAt = &first, &last
	out.BBox = &[4]float64{r.MinLng, r.MinLat, r.MaxLng, r.MaxLat}
	if r.HasAltAmsl {
		a := r.MaxAltAmslM
		out.MaxAltAMSLM = &a
	}
	return out, nil
}

// Silences implements records.Series.
func (p Series) Silences(ctx context.Context, flightID string, gapS float64, n int) ([]records.Silence, error) {
	q, err := p.q()
	if err != nil {
		return nil, err
	}
	fid, err := store.UUID("flight_id", flightID)
	if err != nil {
		return nil, err
	}
	rows, err := q.RecordTelemetrySilences(ctx, timeseries.RecordTelemetrySilencesParams{FlightID: fid, GapS: gapS, N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("telemetry silences: %w", err)
	}
	out := make([]records.Silence, 0, len(rows))
	for _, r := range rows {
		out = append(out, records.Silence{After: r.AfterAt, Before: r.BeforeAt})
	}
	return out, nil
}

// WriterGaps implements records.Series.
func (p Series) WriterGaps(ctx context.Context, from, to time.Time, n int) ([]records.WriterGap, error) {
	q, err := p.q()
	if err != nil {
		return nil, err
	}
	rows, err := q.RecordWriterGaps(ctx, timeseries.RecordWriterGapsParams{FromAt: &from, ToAt: &to, N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("writer gaps: %w", err)
	}
	out := make([]records.WriterGap, 0, len(rows))
	for _, r := range rows {
		out = append(out, records.WriterGap{Cause: r.Cause, Count: r.Count, CountUnit: r.CountUnit, AfterAt: r.AfterAt, BeforeAt: r.BeforeAt, Detail: r.Detail, At: r.At})
	}
	return out, nil
}

// Products implements records.Series.
func (p Series) Products(ctx context.Context, clientID string, from, to time.Time, n int) ([]records.Product, int64, error) {
	q, err := p.q()
	if err != nil {
		return nil, 0, err
	}
	total, err := q.CountTrafficProducts(ctx, timeseries.CountTrafficProductsParams{ClientID: clientID, FromAt: from, ToAt: to})
	if err != nil {
		return nil, 0, fmt.Errorf("traffic products: %w", err)
	}
	rows, err := q.RecordTrafficProducts(ctx, timeseries.RecordTrafficProductsParams{ClientID: clientID, FromAt: from, ToAt: to, N: n32(n)})
	if err != nil {
		return nil, 0, fmt.Errorf("traffic products: %w", err)
	}
	out := make([]records.Product, 0, len(rows))
	for _, r := range rows {
		out = append(out, records.Product{At: r.At, IntentID: opt(store.UUIDText(r.IntentID)), TracksShown: raw(r.TracksShown), Degraded: r.Degraded, PolicyVersion: r.PolicyVersion})
	}
	return out, total, nil
}

var (
	_ records.GapStore    = Reader{}
	_ records.BundleStore = Reader{}
)

// RecordGap implements records.GapStore (idempotent by msg_id).
func (p Reader) RecordGap(ctx context.Context, g records.Gap) error {
	params := relational.InsertIngestGapParams{MsgID: g.MsgID, SourceInstance: g.SourceInstance, Cause: g.Cause, Dropped: int32(min(max(g.Dropped, 0), math.MaxInt32))}
	if !g.Started.IsZero() {
		s, e := g.Started.UTC(), g.Ended.UTC()
		params.GapStarted, params.GapEnded = &s, &e
	}
	if g.ToSeq > 0 && g.ToSeq <= math.MaxInt64 && g.FromSeq <= math.MaxInt64 {
		f, t := int64(g.FromSeq), int64(g.ToSeq)
		params.FromSeq, params.ToSeq = &f, &t
	}
	if _, err := p.S.Queries().InsertIngestGap(ctx, params); err != nil {
		return fmt.Errorf("ingest gap: %w", err)
	}
	return nil
}

// PurgeGaps deletes the gap records older than days and returns how
// many.
func (p Reader) PurgeGaps(ctx context.Context, days int) (int64, error) {
	n, err := p.S.Queries().PurgeIngestGaps(ctx, int32(min(max(days, 1), 100_000)))
	if err != nil {
		return 0, fmt.Errorf("purge ingest gaps: %w", err)
	}
	return n, nil
}

// DayFlights implements records.BundleStore.
func (p Reader) DayFlights(ctx context.Context, start, end, afterAt time.Time, afterID string, n int) ([]records.DayFlight, error) {
	aid, err := store.UUID("after_id", afterID)
	if err != nil {
		return nil, err
	}
	rows, err := p.S.Queries().RecordDayFlights(ctx, relational.RecordDayFlightsParams{DayStart: start, DayEnd: end, AfterAt: afterAt, AfterID: aid, N: n32(n)})
	if err != nil {
		return nil, fmt.Errorf("flights of the day: %w", err)
	}
	out := make([]records.DayFlight, 0, len(rows))
	for _, r := range rows {
		out = append(out, records.DayFlight{ID: store.UUIDText(r.ID), StartedAt: r.StartedAt})
	}
	return out, nil
}

// InsertBundle implements records.BundleStore.
func (p Reader) InsertBundle(ctx context.Context, b records.Bundle) (bool, error) {
	n, err := p.S.Queries().InsertRecordBundle(ctx, relational.InsertRecordBundleParams{Date: store.Date(b.Date), ContentHash: b.Hash, StorageRef: b.Ref,
		Flights: int32(min(max(b.Flights, 0), math.MaxInt32))})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Bundle implements records.BundleStore.
func (p Reader) Bundle(ctx context.Context, day time.Time) (records.Bundle, error) {
	r, err := p.S.Queries().GetRecordBundle(ctx, store.Date(day))
	if store.IsNoRows(err) {
		return records.Bundle{}, records.ErrNotFound
	}
	if err != nil {
		return records.Bundle{}, fmt.Errorf("record bundle: %w", err)
	}
	return records.Bundle{Date: store.DateTime(r.Date), BuiltAt: r.BuiltAt, Hash: r.ContentHash, Ref: r.StorageRef, Flights: int(r.Flights)}, nil
}

// MissingDays implements records.BundleStore.
func (p Reader) MissingDays(ctx context.Context, first, last time.Time) ([]time.Time, error) {
	if last.Before(first) {
		return nil, nil
	}
	rows, err := p.S.Queries().MissingRecordDays(ctx, relational.MissingRecordDaysParams{FirstDay: store.Date(first), LastDay: store.Date(last)})
	if err != nil {
		return nil, fmt.Errorf("missing record days: %w", err)
	}
	out := make([]time.Time, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.DateTime(r))
	}
	return out, nil
}

// EntityBundle and EntityFlight are the events entities of the records.
const (
	EntityBundle = "record_bundle"
	EntityFlight = "flight"
)

// AuditBundle implements records.BundleStore.
func (p Reader) AuditBundle(ctx context.Context, b records.Bundle) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		_, err := store.Audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "records", EntityType: EntityBundle,
			EntityID: b.Date.Format(time.DateOnly), EventType: "record_bundle_built",
			Payload: map[string]any{"content_hash": b.Hash, "storage_ref": b.Ref, "flights": b.Flights}})
		return err
	})
}

// AuditRead writes the events row of a record read by actorID (the
// token's subject): committed before a byte is served.
func (p Reader) AuditRead(ctx context.Context, actorID, entity, entityID string) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		_, err := store.Audit(ctx, q, store.Event{ActorType: store.ActorPeer, ActorID: actorID, Purpose: "service_record",
			EntityType: entity, EntityID: entityID, EventType: "record_read"})
		return err
	})
}
