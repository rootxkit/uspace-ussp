// Package pgstore is internal/dss's Store and Telemetry on the
// databases: peer_intents, constraints, dss_subscriptions, dss_state,
// dss_exchanges, uss_reports and the dss_outbox items oir_put, oir_delete
// and peer_notify of the relational tree (migrations 00003, 00017), and
// the telemetry hypertable of the time-series tree (read only), through
// the sqlc queries of internal/store/queries/*/dss.sql and
// peer_flights.sql, on the database clock. Only api uses it (the only
// relational writer, D5). Its tests are the integration tests
// (test/integration).
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/dss"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
	"github.com/rootxkit/uspace-ussp/internal/store/timeseries"
)

// Store is dss.Store on the relational database.
type Store struct{ S *store.Store }

var _ dss.Store = Store{}

func (p Store) q() *relational.Queries { return p.S.Queries() }

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Now is the database clock.
func (p Store) Now(ctx context.Context) (time.Time, error) { return p.q().DBNow(ctx) }

// Lock runs fn in a transaction holding the entity's advisory lock.
func (p Store) Lock(ctx context.Context, class int32, entityID string, fn func() error) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		if err := store.LockEntity(ctx, q, class, entityID); err != nil {
			return err
		}
		return fn()
	})
}

// Claim leases due items of the kinds.
func (p Store) Claim(ctx context.Context, kinds []string, n int) ([]store.OutboxItem, error) {
	return p.q().ClaimOutboxKinds(ctx, relational.ClaimOutboxKindsParams{
		LeaseS: store.DefaultLease.Seconds(), Kinds: kinds, N: int32(max(0, min(n, store.MaxClaim))),
	})
}

// Done marks an item done.
func (p Store) Done(ctx context.Context, id int64) error { return p.S.Outbox().Done(ctx, id) }

// Fail records an item's failure.
func (p Store) Fail(ctx context.Context, id int64, cause error, backoff time.Duration) error {
	return p.S.Outbox().Fail(ctx, id, cause, backoff)
}

// ClaimByKey leases the due item of a key, or one queued with a hold
// and never claimed; nil when none is (done, or held by the
// notification loop).
func (p Store) ClaimByKey(ctx context.Context, kind, entityID string, version int64) (*store.OutboxItem, error) {
	it, err := p.q().ClaimOutboxByKey(ctx, relational.ClaimOutboxByKeyParams{
		LeaseS: store.DefaultLease.Seconds(), Kind: kind, EntityID: entityID, EntityVersion: version,
	})
	if store.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// DoneByKey marks the item of a key done.
func (p Store) DoneByKey(ctx context.Context, kind, entityID string, version int64) (bool, error) {
	n, err := p.q().MarkOutboxDoneByKey(ctx, relational.MarkOutboxDoneByKeyParams{Kind: kind, EntityID: entityID, EntityVersion: version})
	return n > 0, err
}

// Backlog is the undone items of the kinds.
func (p Store) Backlog(ctx context.Context, kinds []string) (int64, float64, int32, error) {
	r, err := p.q().OutboxOldestDue(ctx, kinds)
	return r.Pending, r.OldestAgeS, r.MaxAttempts, err
}

// UpsertPeerIntent stores a peer's intent unless a newer version is held.
func (p Store) UpsertPeerIntent(ctx context.Context, r dss.PeerRecord) (bool, error) {
	v := r.Version
	ts, te := r.TimeStart, r.TimeEnd
	n, err := p.q().PeerIntentUpsert(ctx, relational.PeerIntentUpsertParams{
		EntityID: r.EntityID, Manager: r.Manager, UssBaseUrl: r.USSBaseURL, State: strp(r.State), Ovn: strp(r.OVN), Version: &v,
		TimeStart: &ts, TimeEnd: &te, Details: r.Details, Priority: int32(r.Priority),
	})
	if err != nil {
		return false, fmt.Errorf("peer intent %s: %w", r.EntityID, err)
	}
	return n > 0, nil
}

// PeerIntent reads a stored peer intent.
func (p Store) PeerIntent(ctx context.Context, entityID string) (*dss.PeerRecord, error) {
	r, err := p.q().PeerIntentGet(ctx, entityID)
	if store.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peer intent %s: %w", entityID, err)
	}
	out := &dss.PeerRecord{EntityID: r.EntityID, Manager: r.Manager, USSBaseURL: r.UssBaseUrl, State: deref(r.State), OVN: deref(r.Ovn),
		Priority: int(r.Priority), Details: r.Details, FetchedAt: r.FetchedAt, PeerUnavailable: r.PeerUnavailable}
	if r.Version != nil {
		out.Version = *r.Version
	}
	if r.TimeStart != nil {
		out.TimeStart = *r.TimeStart
	}
	if r.TimeEnd != nil {
		out.TimeEnd = *r.TimeEnd
	}
	return out, nil
}

// DeletePeerIntent removes a manager's peer intent.
func (p Store) DeletePeerIntent(ctx context.Context, entityID, manager string) (bool, error) {
	n, err := p.q().PeerIntentDelete(ctx, relational.PeerIntentDeleteParams{EntityID: entityID, Manager: manager})
	return n > 0, err
}

// MarkPeerUnavailable marks a peer's stored intents.
func (p Store) MarkPeerUnavailable(ctx context.Context, base string, unavailable bool) (int64, error) {
	return p.q().PeerIntentsMarkUnavailable(ctx, relational.PeerIntentsMarkUnavailableParams{Unavailable: unavailable, UssBaseUrl: base})
}

// UpsertConstraint stores a constraint unless a newer version is held.
func (p Store) UpsertConstraint(ctx context.Context, c dss.ConstraintRecord) (bool, error) {
	v := c.Version
	ts, te := c.TimeStart, c.TimeEnd
	n, err := p.q().ConstraintUpsert(ctx, relational.ConstraintUpsertParams{
		EntityID: c.EntityID, Manager: c.Manager, Ovn: strp(c.OVN), Version: &v, TimeStart: &ts, TimeEnd: &te, Details: c.Details,
		CisRestrictionID: strp(c.CISRestrictionID), UssBaseUrl: strp(c.USSBaseURL),
	})
	if err != nil {
		return false, fmt.Errorf("constraint %s: %w", c.EntityID, err)
	}
	return n > 0, nil
}

// Constraint reads a stored constraint.
func (p Store) Constraint(ctx context.Context, entityID string) (*dss.ConstraintRecord, error) {
	r, err := p.q().ConstraintGet(ctx, entityID)
	if store.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("constraint %s: %w", entityID, err)
	}
	out := &dss.ConstraintRecord{EntityID: r.EntityID, Manager: r.Manager, OVN: deref(r.Ovn), Details: r.Details,
		CISRestrictionID: deref(r.CisRestrictionID), USSBaseURL: deref(r.UssBaseUrl), FetchedAt: r.FetchedAt}
	if r.Version != nil {
		out.Version = *r.Version
	}
	if r.TimeStart != nil {
		out.TimeStart = *r.TimeStart
	}
	if r.TimeEnd != nil {
		out.TimeEnd = *r.TimeEnd
	}
	return out, nil
}

// DeleteConstraint removes a manager's constraint.
func (p Store) DeleteConstraint(ctx context.Context, entityID, manager string) (bool, error) {
	n, err := p.q().ConstraintDelete(ctx, relational.ConstraintDeleteParams{EntityID: entityID, Manager: manager})
	return n > 0, err
}

// maxSubscriptions bounds the subscriptions read.
const maxSubscriptions = 1000

// Subscriptions reads our F3548 subscriptions.
func (p Store) Subscriptions(ctx context.Context) ([]dss.SubscriptionRecord, error) {
	rs, err := p.q().SubscriptionsUTM(ctx, maxSubscriptions)
	if err != nil {
		return nil, fmt.Errorf("subscriptions: %w", err)
	}
	out := make([]dss.SubscriptionRecord, 0, len(rs))
	for i := range rs {
		r := &rs[i]
		var a dss.Area
		if err := json.Unmarshal(r.Area, &a); err != nil {
			// A row this version cannot read is renewed from the areas.
			a = dss.Area{}
		}
		out = append(out, dss.SubscriptionRecord{ID: r.SubscriptionID, Area: a, Version: deref(r.Version),
			NotificationIndex: r.NotificationIndex, TimeEnd: r.TimeEnd, USSBaseURL: r.UssBaseUrl, RenewedAt: r.RenewedAt})
	}
	return out, nil
}

// UpsertSubscription records a subscription the DSS holds.
func (p Store) UpsertSubscription(ctx context.Context, s dss.SubscriptionRecord) error {
	area, err := json.Marshal(s.Area)
	if err != nil {
		return err
	}
	return p.q().SubscriptionUpsert(ctx, relational.SubscriptionUpsertParams{SubscriptionID: s.ID, Area: area, Version: strp(s.Version),
		NotificationIndex: s.NotificationIndex, TimeEnd: s.TimeEnd, UssBaseUrl: s.USSBaseURL})
}

// DeleteSubscription removes a subscription's row.
func (p Store) DeleteSubscription(ctx context.Context, id string) error {
	_, err := p.q().SubscriptionDelete(ctx, id)
	return err
}

// Notified advances a subscription's notification index.
func (p Store) Notified(ctx context.Context, id string, idx int32) (int32, bool, error) {
	prev, err := p.q().SubscriptionNotified(ctx, relational.SubscriptionNotifiedParams{Idx: idx, SubscriptionID: id})
	if store.IsNoRows(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return prev, true, nil
}

// State reads dss_state.
func (p Store) State(ctx context.Context) (dss.StateRecord, error) {
	r, err := p.q().DSSStateGet(ctx)
	if err != nil {
		return dss.StateRecord{}, err
	}
	return dss.StateRecord{Availability: deref(r.UssAvailability), SetBy: deref(r.SetBy), SetAt: r.SetAt,
		ReachableSince: r.DssReachableSince, UnreachableSince: r.DssUnreachableSince}, nil
}

// SetAvailability records the availability.
func (p Store) SetAvailability(ctx context.Context, availability, setBy string) (bool, error) {
	n, err := p.q().DSSStateSetAvailability(ctx, relational.DSSStateSetAvailabilityParams{Availability: &availability, SetBy: &setBy})
	return n > 0, err
}

// SetReachable records whether the DSS answers.
func (p Store) SetReachable(ctx context.Context, up bool) error {
	_, err := p.q().DSSStateSetReachable(ctx, up)
	return err
}

// InsertExchange records one exchange.
func (p Store) InsertExchange(ctx context.Context, e dss.Exchange) error {
	params := relational.ExchangeInsertParams{EntityID: strp(e.EntityID), RecorderRole: e.Role, Method: e.Method, Url: e.URL,
		RequestBody: strp(e.RequestBody), RequestTime: e.RequestTime, ResponseBody: strp(e.ResponseBody), Problem: strp(e.Problem)}
	if e.ResponseCode != 0 {
		c := int32(e.ResponseCode)
		params.ResponseCode = &c
	}
	if !e.ResponseTime.IsZero() {
		t := e.ResponseTime
		params.ResponseTime = &t
	}
	return p.q().ExchangeInsert(ctx, params)
}

// Exchanges reads an entity's exchanges.
func (p Store) Exchanges(ctx context.Context, entityID string, limit int) ([]dss.Exchange, error) {
	rs, err := p.q().ExchangesByEntity(ctx, relational.ExchangesByEntityParams{EntityID: &entityID, MaxRows: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]dss.Exchange, 0, len(rs))
	for i := range rs {
		r := &rs[i]
		e := dss.Exchange{EntityID: deref(r.EntityID), Role: r.RecorderRole, Method: r.Method, URL: r.Url, RequestBody: deref(r.RequestBody),
			RequestTime: r.RequestTime, ResponseBody: deref(r.ResponseBody), Problem: deref(r.Problem)}
		if r.ResponseCode != nil {
			e.ResponseCode = int(*r.ResponseCode)
		}
		if r.ResponseTime != nil {
			e.ResponseTime = *r.ResponseTime
		}
		out = append(out, e)
	}
	return out, nil
}

// InsertReport stores a report.
func (p Store) InsertReport(ctx context.Context, reportID, reporter string, exchange json.RawMessage) error {
	id, err := store.UUID("report_id", reportID)
	if err != nil {
		return err
	}
	return p.q().ReportInsert(ctx, relational.ReportInsertParams{ReportID: id, Reporter: reporter, Exchange: exchange})
}

// Purge removes peer data and the log past their retention.
func (p Store) Purge(ctx context.Context, peersBefore, logBefore time.Time, maxExchanges int64) (dss.PurgeCounts, error) {
	var out dss.PurgeCounts
	var err error
	q := p.q()
	if out.PeerIntents, err = q.PeerIntentsPurge(ctx, peersBefore); err != nil {
		return out, fmt.Errorf("peer intents purge: %w", err)
	}
	if out.Constraints, err = q.ConstraintsPurge(ctx, peersBefore); err != nil {
		return out, fmt.Errorf("constraints purge: %w", err)
	}
	n, err := q.ExchangesPurgeBefore(ctx, logBefore)
	if err != nil {
		return out, fmt.Errorf("exchanges purge: %w", err)
	}
	m, err := q.ExchangesPurgeBeyond(ctx, maxExchanges)
	if err != nil {
		return out, fmt.Errorf("exchanges bound: %w", err)
	}
	out.Exchanges = n + m
	if out.Reports, err = q.ReportsPurgeBefore(ctx, logBefore); err != nil {
		return out, fmt.Errorf("reports purge: %w", err)
	}
	return out, nil
}

// Audit appends an audit event in its own transaction.
func (p Store) Audit(ctx context.Context, e store.Event) error {
	return p.S.Tx(ctx, func(q *relational.Queries) error {
		_, err := store.Audit(ctx, q, e)
		return err
	})
}

// Telemetry is dss.Telemetry: the intent's newest flight from the
// relational tree, its newest sample from the time-series tree.
type Telemetry struct{ S *store.Store }

var _ dss.Telemetry = Telemetry{}

// Latest reads the newest sample of the intent's newest flight.
func (t Telemetry) Latest(ctx context.Context, intentID string, since time.Time) (*f3548.VehicleTelemetry, bool, error) {
	if t.S == nil || t.S.TS == nil {
		return nil, false, errors.New("no time-series database")
	}
	id, err := store.UUID("intent_id", intentID)
	if err != nil {
		return nil, false, nil //nolint:nilerr // an id that is no UUID names no flight
	}
	f, err := t.S.Queries().IntentNewestFlight(ctx, id)
	if store.IsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	r, err := t.S.TSQueries().LatestFlightSample(ctx, timeseries.LatestFlightSampleParams{FlightID: f.ID, Since: since})
	if store.IsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	lat, lng := r.Lat, r.Lng
	pos := &f3548.Position{Latitude: &lat, Longitude: &lng}
	if r.AltWgs84M != nil {
		pos.Altitude = &f3548.Altitude{Reference: f3548.W84, Units: f3548.AltitudeUnitsM, Value: *r.AltWgs84M}
	}
	if r.AccuracyH != nil {
		a := f3548.PositionAccuracyHorizontal(*r.AccuracyH)
		pos.AccuracyH = &a
	}
	if r.AccuracyV != nil {
		a := f3548.PositionAccuracyVertical(*r.AccuracyV)
		pos.AccuracyV = &a
	}
	out := &f3548.VehicleTelemetry{TimeMeasured: f3548.Time{Format: f3548.RFC3339, Value: r.CapturedAt.UTC()}, Position: pos}
	if r.SpeedMs != nil {
		v := &f3548.Velocity{Speed: float32(*r.SpeedMs), UnitsSpeed: f3548.MetersPerSecond}
		if r.TrackDeg != nil {
			tr := float32(*r.TrackDeg)
			v.Track = &tr
		}
		out.Velocity = v
	}
	return out, true, nil
}
