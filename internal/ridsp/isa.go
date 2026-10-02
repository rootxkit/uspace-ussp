package ridsp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/flights"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/stdapi/convert"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// The two kinds of ISA (dss_isas.kind).
const (
	ISAKindIntent  = "intent"
	ISAKindSession = "session"
)

// Counters of the ISA planner and worker (E-09).
const (
	CounterISAPlanned          = "rid_sp_isa_planned"
	CounterISAUnplanned        = "rid_sp_isa_unplanned"
	CounterISAIntentFallback   = "rid_sp_isa_intent_extent_failed"
	CounterISAWrites           = "rid_sp_isa_writes"
	CounterISADeletes          = "rid_sp_isa_deletes"
	CounterISAFailed           = "rid_sp_isa_failed"
	CounterISAExpired          = "rid_sp_isa_expired_unwritten"
	CounterISASkippedEnded     = "rid_sp_isa_skipped_flight_ended"
	CounterISARenewed          = "rid_sp_isa_renewals_queued"
	CounterSubscriberNotified  = "rid_sp_subscriber_notified"
	CounterSubscriberNotifyErr = "rid_sp_subscriber_notify_failed"
	CounterSubscribersRefused  = "rid_sp_isa_subscribers_refused"
	CounterISAGivenUp          = "rid_sp_isa_refused_given_up"
)

// ISAPut is the payload of a dss_outbox isa_put item: the ISA's outline
// and altitudes, and either the fixed end of an intent's window
// (TimeEnd) or the horizon a session ISA reaches from the moment it is
// written (HorizonS). The time window is set when the item is sent, on
// the database clock, because the DSS refuses a start in the past.
type ISAPut struct {
	ISAID    string         `json:"isa_id"`
	FlightID string         `json:"flight_id"`
	Volume   f3411.Volume3D `json:"volume"`
	TimeEnd  *time.Time     `json:"time_end,omitempty"`
	HorizonS float64        `json:"horizon_s,omitempty"`
}

// ISADelete is the payload of a dss_outbox isa_delete item.
type ISADelete struct {
	ISAID    string `json:"isa_id"`
	FlightID string `json:"flight_id"`
}

// newUUID is a version 4 UUID (an ISA's EntityUUID).
func newUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// IntentExtent is the extent of an ISA over an intent's F3548 volumes:
// the union of their prefilter boxes (core's Volume4DToZonesEnvelope,
// conservative) as a four-vertex polygon, the lowest lower and the
// highest upper W84 altitude, and the latest end, plus
// NetMaxNearRealTimeDataPeriodSeconds: a Display Provider must still
// find the flight's last minute after the window closes (the DSS
// expects the retention in time_end). A volume that cannot be bounded,
// a box across the antimeridian, or no volume at all is an error.
func IntentExtent(vols []f3548.Volume4D) (f3411.Volume3D, time.Time, error) {
	if len(vols) == 0 {
		return f3411.Volume3D{}, time.Time{}, core.Fieldf("volumes", "none")
	}
	box := geodesy.BBox{MinLat: math.Inf(1), MinLon: math.Inf(1), MaxLat: math.Inf(-1), MaxLon: math.Inf(-1)}
	lower, upper := math.Inf(1), math.Inf(-1)
	var end time.Time
	for i, v := range vols {
		env, err := convert.Volume4DEnvelope(v)
		if err != nil {
			return f3411.Volume3D{}, time.Time{}, fmt.Errorf("volumes[%d]: %w", i, err)
		}
		b := env.BBox
		if b.MinLon > b.MaxLon {
			return f3411.Volume3D{}, time.Time{}, core.Fieldf(fmt.Sprintf("volumes[%d]", i), "crosses the antimeridian")
		}
		box.MinLat, box.MaxLat = math.Min(box.MinLat, b.MinLat), math.Max(box.MaxLat, b.MaxLat)
		box.MinLon, box.MaxLon = math.Min(box.MinLon, b.MinLon), math.Max(box.MaxLon, b.MaxLon)
		if env.End.IsZero() {
			return f3411.Volume3D{}, time.Time{}, core.Fieldf(fmt.Sprintf("volumes[%d].time_end", i), "missing")
		}
		if env.End.After(end) {
			end = env.End
		}
		for _, a := range []*f3548.Altitude{v.Volume.AltitudeLower, v.Volume.AltitudeUpper} {
			if a == nil {
				return f3411.Volume3D{}, time.Time{}, core.Fieldf(fmt.Sprintf("volumes[%d].volume", i), "has no altitude bounds")
			}
			h, err := a.HAEM()
			if err != nil {
				return f3411.Volume3D{}, time.Time{}, fmt.Errorf("volumes[%d]: %w", i, err)
			}
			lower, upper = math.Min(lower, h), math.Max(upper, h)
		}
	}
	vol := boxVolume(box)
	vol.AltitudeLower = &f3411.Altitude{Reference: f3411.W84, Units: f3411.AltitudeUnitsM, Value: lower}
	vol.AltitudeUpper = &f3411.Altitude{Reference: f3411.W84, Units: f3411.AltitudeUnitsM, Value: upper}
	return vol, end.Add(Horizon), nil
}

// boxVolume is b as an outline polygon, counter-clockwise from the
// south-west corner.
func boxVolume(b geodesy.BBox) f3411.Volume3D {
	return f3411.Volume3D{OutlinePolygon: &f3411.Polygon{Vertices: []f3411.LatLngPoint{
		{Lat: b.MinLat, Lng: b.MinLon}, {Lat: b.MinLat, Lng: b.MaxLon},
		{Lat: b.MaxLat, Lng: b.MaxLon}, {Lat: b.MaxLat, Lng: b.MinLon},
	}}}
}

// SessionExtent is the outline of the ISA of a flight without an intent
// around p: a square of a fixed grid whose step is a little over
// 2*radiusM, of side two steps, that holds p at least radiusM from its
// edges. F3411 asks that extents not be centred on the take-off point of
// a single flight, so the square is the grid's, not the flight's:
// flights near each other share it. The grid's degrees per step are
// taken at the whole degree of latitude nearest p, the same for every
// flight there. No altitude bounds: a session declares none.
func SessionExtent(p core.LatLon, radiusM float64) (f3411.Volume3D, error) {
	if !p.Valid() {
		return f3411.Volume3D{}, core.Fieldf("position", "not a WGS84 position")
	}
	if !(radiusM > 0) || math.IsInf(radiusM, 0) {
		return f3411.Volume3D{}, core.Fieldf("session_isa_radius_m", "must be a positive number of metres")
	}
	// 5 % over 2r: within a degree of latitude the metres of a degree of
	// longitude differ from the reference's by less than that.
	step := 2.1 * radiusM
	ref := core.LatLon{LatDeg: math.Max(-89, math.Min(89, math.Round(p.LatDeg)))}
	dLat := geodesy.Destination(ref, 0, step).LatDeg - ref.LatDeg
	dLon := core.WrapLonDeg(geodesy.Destination(ref, 90, step).LonDeg - ref.LonDeg)
	if !(dLat > 0) || !(dLon > 0) || math.Abs(p.LatDeg) > 85 {
		return f3411.Volume3D{}, core.Fieldf("position", "too close to a pole for a session ISA")
	}
	lat0 := math.Floor(p.LatDeg/dLat) * dLat
	lon0 := math.Floor(p.LonDeg/dLon) * dLon
	b := geodesy.BBox{MinLat: lat0 - dLat/2, MaxLat: lat0 + 1.5*dLat, MinLon: lon0 - dLon/2, MaxLon: lon0 + 1.5*dLon}
	if b.MinLon < -180 || b.MaxLon > 180 {
		return f3411.Volume3D{}, core.Fieldf("position", "too close to the antimeridian for a session ISA")
	}
	return boxVolume(b), nil
}

// Planner plans the ISA of every flight inside the transaction that
// records the flight's fact (api), so a fact and its DSS work commit
// together (B-05): the first fact of a flight that is not ended plans
// its ISA (dss_isas) and queues isa_put; a later fact that brings an
// intent to a session ISA queues the intent's extent; ended queues
// isa_delete for an ISA that is not deleted.
type Planner struct {
	Policy   func() policy.Values
	Counters *core.Counters
	Logger   *slog.Logger
}

func (p *Planner) count(name string) {
	if p.Counters != nil {
		p.Counters.Inc(name)
	}
}

func (p *Planner) logger() *slog.Logger {
	if p.Logger == nil {
		return obs.Discard()
	}
	return p.Logger
}

func (p *Planner) policy() policy.Values {
	if p.Policy != nil {
		return p.Policy()
	}
	return policy.Defaults()
}

// Plan plans b's ISA work in st, the caller's transaction, after the
// flight row was recorded.
func (p *Planner) Plan(ctx context.Context, st PlanStore, b flights.Body) error {
	cur, found, err := st.FlightISA(ctx, b.FlightID)
	if err != nil {
		return fmt.Errorf("flight %s ISA: %w", b.FlightID, err)
	}
	if b.Event == flights.EventEnded {
		if !found || cur.DeletedAt != nil {
			return nil
		}
		_, err := st.Enqueue(ctx, store.OutboxISADelete, cur.ISAID, 0, ISADelete{ISAID: cur.ISAID, FlightID: b.FlightID})
		return err
	}
	ended, err := st.FlightEnded(ctx, b.FlightID)
	if err != nil {
		return err
	}
	if ended {
		return nil // a late fact of an ended flight plans nothing
	}
	now, err := st.Now(ctx)
	if err != nil {
		return err
	}
	switch {
	case !found:
		return p.planNew(ctx, st, b, now)
	case cur.DeletedAt == nil && cur.Kind == ISAKindSession && b.IntentID != nil:
		return p.planIntent(ctx, st, b, cur, now)
	}
	return nil
}

// extent is the volume, end and kind of b's ISA: the intent's when b
// flies one whose volumes bound, else a session ISA around b's
// position; ok false when neither is possible.
func (p *Planner) extent(ctx context.Context, st PlanStore, b flights.Body) (put ISAPut, kind string, ok bool) {
	if b.IntentID != nil {
		vol, end, err := p.intentExtent(ctx, st, *b.IntentID)
		if err == nil {
			return ISAPut{FlightID: b.FlightID, Volume: vol, TimeEnd: &end}, ISAKindIntent, true
		}
		p.count(CounterISAIntentFallback)
		p.logger().LogAttrs(ctx, slog.LevelError, "the intent's volumes do not make an ISA extent; a session ISA stands in",
			slog.String("flight_id", b.FlightID), slog.String("intent_id", *b.IntentID), obs.Err(err))
	}
	if b.Position == nil {
		return ISAPut{}, "", false
	}
	pol := p.policy()
	vol, err := SessionExtent(b.Position.LatLon(), pol.SessionISARadiusM)
	if err != nil {
		p.logger().LogAttrs(ctx, slog.LevelError, "no session ISA extent", slog.String("flight_id", b.FlightID), obs.Err(err))
		return ISAPut{}, "", false
	}
	return ISAPut{FlightID: b.FlightID, Volume: vol, HorizonS: pol.SessionISAHorizonS}, ISAKindSession, true
}

func (p *Planner) intentExtent(ctx context.Context, st PlanStore, intentID string) (f3411.Volume3D, time.Time, error) {
	raw, err := st.IntentVolumes(ctx, intentID)
	if err != nil {
		return f3411.Volume3D{}, time.Time{}, err
	}
	var vols []f3548.Volume4D
	if err := json.Unmarshal(raw, &vols); err != nil {
		return f3411.Volume3D{}, time.Time{}, core.Fieldf("volumes", "not F3548 volumes")
	}
	return IntentExtent(vols)
}

// planned is the dss_isas window of put at now.
func planned(put ISAPut, now time.Time) (time.Time, time.Time) {
	if put.TimeEnd != nil {
		return now, *put.TimeEnd
	}
	return now, now.Add(time.Duration(put.HorizonS * float64(time.Second)))
}

func (p *Planner) planNew(ctx context.Context, st PlanStore, b flights.Body, now time.Time) error {
	put, kind, ok := p.extent(ctx, st, b)
	if !ok {
		// Nothing to place an ISA on: the flight is served on
		// /uss/flights but no Display Provider will find it. Never
		// silent.
		p.count(CounterISAUnplanned)
		p.logger().LogAttrs(ctx, slog.LevelError, "no ISA planned: the flight has neither an intent nor a position",
			slog.String("flight_id", b.FlightID))
		return nil
	}
	put.ISAID = newUUID()
	start, end := planned(put, now)
	if !end.After(start) {
		p.count(CounterISAUnplanned)
		p.logger().LogAttrs(ctx, slog.LevelError, "no ISA planned: the intent's window has passed",
			slog.String("flight_id", b.FlightID))
		return nil
	}
	ext, err := json.Marshal(f3411.Volume4D{Volume: put.Volume, TimeStart: &f3411.Time{Format: f3411.RFC3339, Value: start.UTC()},
		TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: end.UTC()}})
	if err != nil {
		return err
	}
	if err := st.InsertISA(ctx, ISARecord{ISAID: put.ISAID, FlightID: b.FlightID, Kind: kind, TimeStart: start, TimeEnd: end, Extents: ext}); err != nil {
		return err
	}
	if _, err := st.Enqueue(ctx, store.OutboxISAPut, put.ISAID, 1, put); err != nil {
		return err
	}
	p.count(CounterISAPlanned)
	return nil
}

// planIntent turns a session ISA into its flight's intent ISA (the
// flight was bound to an intent in flight).
func (p *Planner) planIntent(ctx context.Context, st PlanStore, b flights.Body, cur ISARecord, now time.Time) error {
	vol, end, err := p.intentExtent(ctx, st, *b.IntentID)
	if err != nil {
		p.count(CounterISAIntentFallback)
		p.logger().LogAttrs(ctx, slog.LevelError, "the intent's volumes do not make an ISA extent; the session ISA stays",
			slog.String("flight_id", b.FlightID), slog.String("intent_id", *b.IntentID), obs.Err(err))
		return nil
	}
	if err := st.SetKind(ctx, cur.ISAID, ISAKindIntent); err != nil {
		return err
	}
	put := ISAPut{ISAID: cur.ISAID, FlightID: b.FlightID, Volume: vol, TimeEnd: &end}
	if _, err := st.Enqueue(ctx, store.OutboxISAPut, cur.ISAID, now.UnixMilli(), put); err != nil {
		return err
	}
	p.count(CounterISAPlanned)
	return nil
}
