// Package pgstore is internal/intent's Store on the relational database:
// operational_intents, intent_versions, peer_intents and the operator
// accounts (migrations 00002, 00003, 00011), through the sqlc queries of
// internal/store/queries/relational/intents.sql. Every time is the
// database clock; every decision runs under the intents advisory lock
// (store.LockIntents), so two overlapping requests are judged one after
// the other. Its tests are the integration tests (test/integration).
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/regnum"

	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Store is intent.Store on PostgreSQL.
type Store struct {
	S *store.Store
	// BeforeLock, when set, runs inside each InTx transaction after it
	// began and before it asks for the intents lock (the integration
	// tests hold a transaction there to race two decisions).
	BeforeLock func(ctx context.Context)
}

var _ intent.Store = Store{}

// Now is the database clock.
func (p Store) Now(ctx context.Context) (time.Time, error) {
	return p.S.Queries().IntentNow(ctx)
}

// Owner reads the client and its operator.
func (p Store) Owner(ctx context.Context, clientID string) (intent.Owner, error) {
	r, err := p.S.Queries().IntentOwner(ctx, clientID)
	if store.IsNoRows(err) {
		return intent.Owner{}, intent.ErrNotFound
	}
	if err != nil {
		return intent.Owner{}, fmt.Errorf("intent owner: %w", err)
	}
	return intent.Owner{
		ClientID: r.ClientID, OperatorID: store.UUIDText(r.OperatorID), OperatorKey: regnum.CompareKey(r.AuthorityRegistrationNumber),
		OperatorStatus: r.OperatorStatus, ClientStatus: r.ClientStatus,
	}, nil
}

// SerialBound reports a live binding of the fold key to the client.
func (p Store) SerialBound(ctx context.Context, clientID, fold string) (bool, error) {
	return p.S.Queries().IntentSerialBound(ctx, relational.IntentSerialBoundParams{ClientID: clientID, SerialFold: fold})
}

// ByClientRef is the client's intent of ref, nil when none.
func (p Store) ByClientRef(ctx context.Context, clientID, ref string) (*intent.Record, error) {
	r, err := p.S.Queries().IntentByClientRef(ctx, relational.IntentByClientRefParams{ClientID: clientID, ClientRef: &ref})
	if store.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("intent by client_ref: %w", err)
	}
	return recordOf(row(r))
}

// Get is the intent, nil when none.
func (p Store) Get(ctx context.Context, id string) (*intent.Record, error) {
	u, err := store.UUID("id", id)
	if err != nil {
		return nil, nil //nolint:nilerr // an id that is no UUID names no intent
	}
	r, err := p.S.Queries().IntentByID(ctx, u)
	if store.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("intent: %w", err)
	}
	return recordOf(r)
}

// List is an operator's intents, newest first.
func (p Store) List(ctx context.Context, operatorID string, f intent.ListFilter) ([]intent.Record, error) {
	op, err := store.UUID("operator_id", operatorID)
	if err != nil {
		return nil, err
	}
	params := relational.IntentListParams{OperatorID: op, FromAt: f.From, ToAt: f.To, MaxRows: int32(min(f.Limit, intent.MaxList))}
	if f.State != "" {
		params.State = &f.State
	}
	rs, err := p.S.Queries().IntentList(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("intent list: %w", err)
	}
	out := make([]intent.Record, 0, len(rs))
	for i := range rs {
		rec, err := recordOf(row(rs[i]))
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, nil
}

// InTx runs fn in one transaction holding store.LockIntents.
func (p Store) InTx(ctx context.Context, fn func(ctx context.Context, tx intent.Tx) error) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		if p.BeforeLock != nil {
			p.BeforeLock(ctx)
		}
		if err := store.Lock(ctx, q, store.LockIntents); err != nil {
			return err
		}
		return fn(ctx, tx{q: q})
	})
}

// row is the shape every intent query returns.
type row = relational.IntentByIDRow

func recordOf(r row) (*intent.Record, error) {
	out := &intent.Record{
		ID: store.UUIDText(r.ID), OperatorID: store.UUIDText(r.OperatorID), ClientID: r.ClientID,
		Version: int(r.Version), LocalState: r.LocalState, Exempt: r.ExemptArt13, Priority: int(r.Priority),
		TimeStart: r.TimeStart.UTC(), TimeEnd: r.TimeEnd.UTC(), FiledAt: r.FiledAt.UTC(), CreatedAt: r.CreatedAt.UTC(),
		Cells: r.CellSet,
	}
	if r.ClientRef != nil {
		out.ClientRef = *r.ClientRef
	}
	if r.RequestHash != nil {
		out.RequestHash = *r.RequestHash
	}
	if len(r.UpdateRequired) > 0 && string(r.UpdateRequired) != "null" {
		out.UpdateRequired = json.RawMessage(r.UpdateRequired)
	}
	if err := json.Unmarshal(r.Request, &out.Request); err != nil {
		return nil, fmt.Errorf("intent %s request: %w", out.ID, err)
	}
	if len(r.DecisionBody) > 0 {
		if err := json.Unmarshal(r.DecisionBody, &out.Decision); err != nil {
			return nil, fmt.Errorf("intent %s decision: %w", out.ID, err)
		}
	}
	if err := json.Unmarshal(r.VolumesAmsl, &out.VolumesAMSL); err != nil {
		return nil, fmt.Errorf("intent %s volumes_amsl: %w", out.ID, err)
	}
	out.Envelope = envelopeOfRecord(out)
	return out, nil
}

type tx struct{ q *relational.Queries }

// Now is the database clock when it is read (clock_timestamp), not when
// the transaction began: InTx asks for it after the intents lock, so the
// rank it gives follows the order in which decisions hold the lock.
func (t tx) Now(ctx context.Context) (time.Time, error) { return t.q.IntentLockedNow(ctx) }

// Overlapping reads the active, non-exempt intents near the boxes.
func (t tx) Overlapping(ctx context.Context, boxes []geodesy.BBox, distM float64, from, to time.Time, excludeID string, limit int) ([]intent.Record, error) {
	ex, err := store.UUID("exclude_id", excludeID)
	if err != nil {
		return nil, err
	}
	rs, err := t.q.IntentOverlapping(ctx, relational.IntentOverlappingParams{
		ToAt: to, FromAt: from, ExcludeID: ex, EnvelopeWkt: WKT(boxes), DistM: distM, MaxRows: int32(limit + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("intents overlapping: %w", err)
	}
	out := make([]intent.Record, 0, len(rs))
	for i := range rs {
		rec, err := recordOf(row(rs[i]))
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, nil
}

// PeerIntents reads the peers' intents of the window.
func (t tx) PeerIntents(ctx context.Context, since, from, to time.Time, limit int) ([]intent.PeerIntent, error) {
	rs, err := t.q.IntentPeerOverlapping(ctx, relational.IntentPeerOverlappingParams{Since: since, ToAt: to, FromAt: from, MaxRows: int32(limit + 1)})
	if err != nil {
		return nil, fmt.Errorf("peer intents: %w", err)
	}
	out := make([]intent.PeerIntent, 0, len(rs))
	for i := range rs {
		r := &rs[i]
		var d f3548.OperationalIntentDetails
		p := intent.PeerIntent{EntityID: r.EntityID, FetchedAt: r.FetchedAt.UTC()}
		if len(r.Details) > 0 {
			if err := json.Unmarshal(r.Details, &d); err != nil {
				// Kept with no volumes: the deconfliction refuses it as
				// not judgeable rather than passing over it.
				out = append(out, p)
				continue
			}
		}
		if d.Volumes != nil {
			p.Volumes = *d.Volumes
		}
		if d.Priority != nil {
			p.Priority = *d.Priority
		}
		out = append(out, p)
	}
	return out, nil
}

// CountOpen is the operator's open intents.
func (t tx) CountOpen(ctx context.Context, operatorID string) (int, error) {
	op, err := store.UUID("operator_id", operatorID)
	if err != nil {
		return 0, err
	}
	n, err := t.q.IntentCountOpen(ctx, op)
	return int(n), err
}

// Lock reads the intent FOR UPDATE.
func (t tx) Lock(ctx context.Context, id string) (*intent.Record, error) {
	u, err := store.UUID("id", id)
	if err != nil {
		return nil, nil //nolint:nilerr // an id that is no UUID names no intent
	}
	r, err := t.q.IntentForUpdate(ctx, u)
	if store.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("intent for update: %w", err)
	}
	return recordOf(row(r))
}

func str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func jsonOf(v any) ([]byte, error) { return json.Marshal(v) }

// columns are the values both writes share.
type columns struct {
	volumes, volumesAMSL, decisionBody, request, conflicts, contingency []byte
	thresholds                                                          []byte
	conditions                                                          []string
	wkt                                                                 string
}

func columnsOf(r *intent.Record) (columns, error) {
	var c columns
	var err error
	for _, f := range []struct {
		dst *[]byte
		v   any
	}{
		{&c.volumes, r.Request.Volumes}, {&c.volumesAMSL, r.VolumesAMSL}, {&c.decisionBody, r.Decision},
		{&c.request, r.Request}, {&c.conflicts, r.Decision.Conflicts}, {&c.contingency, r.Request.Contingency},
	} {
		if *f.dst, err = jsonOf(f.v); err != nil {
			return c, err
		}
	}
	if r.Decision.DeviationThresholds != nil {
		if c.thresholds, err = jsonOf(r.Decision.DeviationThresholds); err != nil {
			return c, err
		}
	}
	c.conditions = make([]string, 0, len(r.Decision.Conditions))
	for _, x := range r.Decision.Conditions {
		c.conditions = append(c.conditions, x.Code)
	}
	if len(r.Envelope) == 0 {
		return c, errors.New("intent has no envelope")
	}
	c.wkt = WKT(r.Envelope)
	return c, nil
}

// Insert writes a new intent, its first version and its audit row.
func (t tx) Insert(ctx context.Context, r *intent.Record) error {
	id, err := store.UUID("id", r.ID)
	if err != nil {
		return err
	}
	op, err := store.UUID("operator_id", r.OperatorID)
	if err != nil {
		return err
	}
	c, err := columnsOf(r)
	if err != nil {
		return err
	}
	q, d := r.Request, r.Decision
	end := int32(min(q.EnduranceS, intent.MaxEnduranceS))
	err = t.q.IntentInsert(ctx, relational.IntentInsertParams{
		ID: id, OperatorID: op, ClientID: r.ClientID, UasSerial: q.UASSerial, PilotRef: str(q.PilotRef),
		Priority: int32(r.Priority), DssState: d.DSSState, LocalState: r.LocalState, Volumes: c.volumes,
		VolumesAmsl: c.volumesAMSL, EnvelopeWkt: c.wkt, TimeStart: r.TimeStart, TimeEnd: r.TimeEnd,
		Mode: str(q.Mode), FlightType: str(q.FlightType), Category: str(q.Category), Subcategory: str(q.Subcategory),
		ClassLabel: str(q.ClassLabel), TypeCertificate: str(q.TypeCertificate), PrivatelyBuilt: q.PrivatelyBuilt,
		MtomKg: q.MTOMKg, IdentificationTechnology: str(q.IdentificationTechnology), ConnectivityMethods: q.ConnectivityMethods,
		EnduranceS: &end, LossOfC2Procedure: str(q.LossOfC2Procedure), OperatorReg: str(q.OperatorReg),
		UaRegistration: str(q.UARegistration), Contingency: c.contingency, EmergencyContactRef: str(q.EmergencyContactRef),
		AuthorisationRef: str(q.AuthorisationRef), ClientRef: str(r.ClientRef), InUspaceAirspace: &d.InUSpaceAirspace,
		UspaceAirspaceIds: d.USpaceAirspaceIDs, ExemptArt13: r.Exempt, Decision: str(d.Decision),
		AuthorisationNumber: d.AuthorisationNumber, DeviationThresholds: c.thresholds, Alternative: []byte("null"),
		Conflicts: c.conflicts, Conditions: c.conditions, CisVersionChecked: d.CISVersionChecked,
		RegistryCheckedAt: d.RegistryCheckedAt, PolicyVersion: &d.PolicyVersion, Version: int32(r.Version),
		Request: c.request, RequestHash: str(r.RequestHash), DecisionBody: c.decisionBody, FiledAt: r.FiledAt,
		CellSet: r.Cells, CreatedAt: r.CreatedAt,
	})
	if store.SQLState(err) == store.StateUniqueViolation {
		return intent.ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("insert intent: %w", err)
	}
	return t.version(ctx, r, intent.EventSubmitted)
}

// Update writes a new version guarded by the previous one.
func (t tx) Update(ctx context.Context, r *intent.Record, event string) error {
	id, err := store.UUID("id", r.ID)
	if err != nil {
		return err
	}
	c, err := columnsOf(r)
	if err != nil {
		return err
	}
	d := r.Decision
	n, err := t.q.IntentUpdate(ctx, relational.IntentUpdateParams{
		Priority: int32(r.Priority), DssState: d.DSSState, LocalState: r.LocalState, Volumes: c.volumes,
		VolumesAmsl: c.volumesAMSL, EnvelopeWkt: c.wkt, TimeStart: r.TimeStart, TimeEnd: r.TimeEnd,
		InUspaceAirspace: &d.InUSpaceAirspace, UspaceAirspaceIds: d.USpaceAirspaceIDs, ExemptArt13: r.Exempt,
		Decision: str(d.Decision), AuthorisationNumber: d.AuthorisationNumber, DeviationThresholds: c.thresholds,
		Conflicts: c.conflicts, Conditions: c.conditions, CisVersionChecked: d.CISVersionChecked,
		RegistryCheckedAt: d.RegistryCheckedAt, PolicyVersion: &d.PolicyVersion, Version: int32(r.Version),
		Request: c.request, DecisionBody: c.decisionBody, FiledAt: r.FiledAt, CellSet: r.Cells, UpdatedAt: d.UpdatedAt, ID: id,
	})
	if err != nil {
		return fmt.Errorf("update intent: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("update intent %s: version %d is not the next one", r.ID, r.Version)
	}
	return t.version(ctx, r, event)
}

func (t tx) version(ctx context.Context, r *intent.Record, event string) error {
	id, err := store.UUID("id", r.ID)
	if err != nil {
		return err
	}
	snap, err := json.Marshal(map[string]any{"request": r.Request, "decision": r.Decision})
	if err != nil {
		return err
	}
	at := r.Decision.UpdatedAt
	if at.IsZero() {
		at = r.FiledAt
	}
	if err := t.q.IntentVersionInsert(ctx, relational.IntentVersionInsertParams{
		IntentID: id, Version: int32(r.Version), At: at, Actor: r.Actor, ChangeReason: r.ChangeReason, Snapshot: snap,
	}); err != nil {
		return fmt.Errorf("intent version: %w", err)
	}
	actorType := store.ActorClient
	if r.Actor == "system" {
		actorType = store.ActorSystem
	}
	_, err = store.Audit(ctx, t.q, store.Event{
		ActorType: actorType, ActorID: r.Actor, Purpose: intent.PurposeAuthorisation,
		EntityType: "operational_intent", EntityID: r.ID, EventType: event,
		Payload: map[string]any{
			"version": r.Version, "local_state": r.LocalState, "decision": r.Decision.Decision,
			"authorisation_number": r.Decision.AuthorisationNumber, "change_reason": r.ChangeReason,
			"policy_version": r.Decision.PolicyVersion, "cis_version_checked": r.Decision.CISVersionChecked,
		},
	})
	return err
}

// FlagUpdate records the precedence of by on each authorisation.
func (t tx) FlagUpdate(ctx context.Context, ids []string, by string, at time.Time) error {
	flag, err := json.Marshal(map[string]any{"by_intent_id": by, "at": at.UTC(), "reason": intent.ReasonIntentFlagged})
	if err != nil {
		return err
	}
	params := relational.IntentFlagUpdateParams{UpdateRequired: flag}
	for _, id := range ids {
		u, err := store.UUID("id", id)
		if err != nil {
			return err
		}
		params.Ids = append(params.Ids, u)
	}
	if _, err := t.q.IntentFlagUpdate(ctx, params); err != nil {
		return fmt.Errorf("flag intents: %w", err)
	}
	for _, id := range ids {
		if _, err := store.Audit(ctx, t.q, store.Event{
			ActorType: store.ActorSystem, ActorID: "intent", Purpose: intent.PurposeAuthorisation,
			EntityType: "operational_intent", EntityID: id, EventType: intent.EventFlagged,
			Payload: map[string]any{"by_intent_id": by, "reason": intent.ReasonIntentFlagged},
		}); err != nil {
			return err
		}
	}
	return nil
}

// DueToEnd reads the open intents past time_end, locked.
func (t tx) DueToEnd(ctx context.Context, now time.Time, limit int) ([]intent.Record, error) {
	rs, err := t.q.IntentDueToEnd(ctx, relational.IntentDueToEndParams{NowAt: now, MaxRows: int32(limit)})
	if err != nil {
		return nil, fmt.Errorf("intents due: %w", err)
	}
	out := make([]intent.Record, 0, len(rs))
	for i := range rs {
		rec, err := recordOf(row(rs[i]))
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, nil
}

// envelopeOfRecord rebuilds the stored envelope of an intent read back
// (its volumes' boxes padded as the service pads them).
func envelopeOfRecord(r *intent.Record) []geodesy.BBox {
	out := make([]geodesy.BBox, 0, len(r.Request.Volumes))
	for _, v := range r.Request.Volumes {
		var b geodesy.BBox
		switch {
		case v.Volume.OutlineCircle != nil && v.Volume.OutlineCircle.Center != nil && v.Volume.OutlineCircle.Radius != nil:
			b = geodesy.Circle{Center: v.Volume.OutlineCircle.Center.LatLon(), RadiusM: float64(v.Volume.OutlineCircle.Radius.Value)}.BBox()
		case v.Volume.OutlinePolygon != nil:
			r := make(geodesy.Ring, 0, len(v.Volume.OutlinePolygon.Vertices))
			for _, p := range v.Volume.OutlinePolygon.Vertices {
				r = append(r, p.LatLon())
			}
			b = geodesy.Polygon{Rings: []geodesy.Ring{r}}.BBox()
		default:
			continue
		}
		out = append(out, intent.PadForGeography(b))
	}
	return out
}

// maxSpanDeg is the widest longitude span of one WKT polygon: a
// geography edge is a great circle, and a polygon 180 degrees wide or
// more is ambiguous, so wider boxes are cut into pieces.
const maxSpanDeg = 90

// WKT is the boxes as a MULTIPOLYGON in WGS84 (lon lat): a box across
// the antimeridian is split into its two halves, and every piece wider
// than maxSpanDeg is cut again, so no polygon is ambiguous on the
// sphere (the boxes come padded by intent.PadForGeography for the bulge
// of their edges).
func WKT(boxes []geodesy.BBox) string {
	var polys []string
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }
	pt := func(lon, lat float64) string { return f(lon) + " " + f(lat) }
	ring := func(minLon, minLat, maxLon, maxLat float64) {
		for lo := minLon; lo < maxLon || lo == minLon; lo += maxSpanDeg {
			hi := min(lo+maxSpanDeg, maxLon)
			polys = append(polys, "(("+strings.Join([]string{pt(lo, minLat), pt(hi, minLat), pt(hi, maxLat), pt(lo, maxLat), pt(lo, minLat)}, ",")+"))")
			if hi >= maxLon {
				break
			}
		}
	}
	for _, b := range boxes {
		lat0, lat1 := clampLat(b.MinLat), clampLat(b.MaxLat)
		if b.MinLon <= b.MaxLon {
			ring(b.MinLon, lat0, b.MaxLon, lat1)
			continue
		}
		ring(b.MinLon, lat0, 180, lat1)
		ring(-180, lat0, b.MaxLon, lat1)
	}
	return "MULTIPOLYGON(" + strings.Join(polys, ",") + ")"
}

func clampLat(v float64) float64 {
	if !core.IsFinite(v) {
		return 0
	}
	return max(-90, min(90, v))
}
