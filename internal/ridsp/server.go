package ridsp

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/stdapi/convert"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
)

// Counters of the Service Provider endpoints (E-09).
const (
	CounterFlightsRequests  = "rid_sp_flights_requests"
	CounterFlightsServed    = "rid_sp_flights_served"
	CounterViewTooLarge     = "rid_sp_view_too_large"
	CounterRequestRefused   = "rid_sp_request_refused"
	CounterFlightUnservable = "rid_sp_flight_unservable"
	CounterDetailsRequests  = "rid_sp_details_requests"
	CounterDetailsNotFound  = "rid_sp_details_not_found"
)

// MaxViewBytes bounds the view parameter read (E-10): four decimal
// coordinates fit in far less.
const MaxViewBytes = 256

// Server is the F3411 USS server of rid-sp (stdf3411.StrictServerInterface;
// see the package documentation). Its answers come from the window and
// the intent_active projection in memory and nothing else.
type Server struct {
	Window  *Window
	Intents Intents
	// Notifications keeps the ISA notifications peers send us.
	Notifications NotificationStore
	Now           func() time.Time
	Counters      *core.Counters
	// ObserveFlights, when set, receives the time each GET /uss/flights
	// took (the rid_sp_flights_seconds histogram).
	ObserveFlights func(time.Duration)
	Logger         *slog.Logger
}

var _ stdf3411.StrictServerInterface = (*Server)(nil)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func (s *Server) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

func message(format string, a ...any) *string {
	m := fmt.Sprintf(format, a...)
	return &m
}

// ParseView reads the standard's view, lat1,lng1,lat2,lng2: the smallest
// box bounded by the two corners, and its diagonal in metres (geodesic,
// corner to corner). A view that is not four finite coordinates within
// their ranges is refused naming what is wrong; a diagonal the geodesic
// cannot be computed for is +Inf.
func ParseView(v string) (geodesy.BBox, float64, error) {
	if len(v) > MaxViewBytes {
		return geodesy.BBox{}, 0, core.Fieldf("view", "longer than %d bytes", MaxViewBytes)
	}
	parts := strings.Split(v, ",")
	if len(parts) != 4 {
		return geodesy.BBox{}, 0, core.Fieldf("view", "must be lat1,lng1,lat2,lng2")
	}
	var c [4]float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return geodesy.BBox{}, 0, core.Fieldf("view", "coordinate %d is not a number", i+1)
		}
		c[i] = f
	}
	a, b := core.LatLon{LatDeg: c[0], LonDeg: c[1]}, core.LatLon{LatDeg: c[2], LonDeg: c[3]}
	if !a.Valid() || !b.Valid() {
		return geodesy.BBox{}, 0, core.Fieldf("view", "a corner is outside -90 to 90, -180 to 180")
	}
	box := geodesy.BBox{
		MinLat: math.Min(a.LatDeg, b.LatDeg), MaxLat: math.Max(a.LatDeg, b.LatDeg),
		MinLon: math.Min(a.LonDeg, b.LonDeg), MaxLon: math.Max(a.LonDeg, b.LonDeg),
	}
	d, err := geodesy.DistanceM(a, b)
	if err != nil {
		d = math.Inf(1)
	}
	return box, d, nil
}

// MaxViewDiagonalM is the largest view diagonal served:
// NetMaxDisplayAreaDiagonalKm.
const MaxViewDiagonalM = f3411.NetMaxDisplayAreaDiagonalKm * 1000

// recentOf is the requested recent_positions_duration: none when absent
// or not above zero, refused above NetMaxNearRealTimeDataPeriodSeconds
// (the file's maximum, 60) or when not a number.
func recentOf(d *float32) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	v := float64(*d)
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return 0, core.Fieldf("recent_positions_duration", "is not a number")
	case v > f3411.NetMaxNearRealTimeDataPeriodSeconds:
		return 0, core.Fieldf("recent_positions_duration", "is above %d s", f3411.NetMaxNearRealTimeDataPeriodSeconds)
	case v <= 0:
		return 0, nil
	}
	return time.Duration(v * float64(time.Second)), nil
}

// SearchFlights is GET /uss/flights.
func (s *Server) SearchFlights(ctx context.Context, req stdf3411.SearchFlightsRequestObject) (stdf3411.SearchFlightsResponseObject, error) {
	start := time.Now()
	if s.ObserveFlights != nil {
		defer func() { s.ObserveFlights(time.Since(start)) }()
	}
	s.count(CounterFlightsRequests)
	box, diagM, err := ParseView(req.Params.View)
	if err != nil {
		s.count(CounterRequestRefused)
		return stdf3411.SearchFlights400JSONResponse{Message: message("%v", err)}, nil
	}
	if diagM > MaxViewDiagonalM {
		s.count(CounterViewTooLarge)
		return stdf3411.SearchFlights413JSONResponse{Message: message("the view's diagonal is %.0f m; at most %d km is served",
			diagM, f3411.NetMaxDisplayAreaDiagonalKm)}, nil
	}
	recent, err := recentOf(req.Params.RecentPositionsDuration)
	if err != nil {
		s.count(CounterRequestRefused)
		return stdf3411.SearchFlights400JSONResponse{Message: message("%v", err)}, nil
	}
	views, err := s.Window.InView(box, recent)
	if err != nil {
		// A box within the diagonal always has a cell cover.
		return nil, err
	}
	now := s.now()
	flights := make([]f3411.RIDFlight, 0, len(views))
	for i := range views {
		v := &views[i]
		f := FlightOf(*v)
		if _, err := convert.RIDFlightToWire(&f); err != nil {
			// Never sent: a peer running core would refuse it. Counted
			// and logged so it is not silent.
			s.count(CounterFlightUnservable)
			s.logger().LogAttrs(ctx, slog.LevelError, "a flight in the window does not make a valid RIDFlight; it is not served",
				slog.String("flight_id", v.ID), obs.Err(err))
			continue
		}
		flights = append(flights, f)
	}
	if s.Counters != nil {
		s.Counters.Add(CounterFlightsServed, uint64(len(flights)))
	}
	return stdf3411.SearchFlights200JSONResponse{Timestamp: wireTime(now), Flights: &flights}, nil
}
