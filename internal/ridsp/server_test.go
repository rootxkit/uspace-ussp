package ridsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/stdapi"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
)

// openGuard admits every request: these tests are about the handlers;
// the scopes are internal/stdapi's and the process tests'.
func openGuard(httpx.Access) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler { return h }
}

// serve mounts s as rid-sp does and returns its base URL.
func serve(t *testing.T, s *Server) string {
	t.Helper()
	mux := http.NewServeMux()
	if err := stdapi.MountF3411(mux, s, stdapi.Options{Guard: openGuard}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}

// viewAround is a square view centred on p with the given diagonal.
func viewAround(p core.LatLon, diagonalM float64) string {
	h := diagonalM / 2 / 1.41421356
	a, b := offset(p, -h, -h), offset(p, h, h)
	return fmt.Sprintf("%.7f,%.7f,%.7f,%.7f", a.LatDeg, a.LonDeg, b.LatDeg, b.LonDeg)
}

// The view caps both ways (E-01): a 1.5 km view is served with the
// flight in it, a 7 km view too, an 8 km view is 413 with the
// standard's ErrorResponse and counted; a view that is not four
// coordinates, and a recent duration above 60 s, are 400.
func TestSearchFlightsViewCaps(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	w.Add(sample(1, origin, t0))
	s := &Server{Window: w, Now: c.now, Counters: &core.Counters{}}
	base := serve(t, s)
	for _, d := range []float64{1500, 6990} {
		code, b := get(t, base+"/uss/flights?view="+viewAround(origin, d))
		r, err := f3411.UnmarshalGetFlightsResponse(b)
		if code != 200 || err != nil || r.Flights == nil || len(*r.Flights) != 1 || (*r.Flights)[0].Id != flightN(1) {
			t.Fatalf("%.0f m view: %d %v %s", d, code, err, b)
		}
		if !r.Timestamp.Value.Equal(t0) {
			t.Errorf("response timestamp %v, want now", r.Timestamp.Value)
		}
	}
	code, b := get(t, base+"/uss/flights?view="+viewAround(origin, 8000))
	var e f3411.ErrorResponse
	if code != http.StatusRequestEntityTooLarge || json.Unmarshal(b, &e) != nil || e.Message == nil ||
		!strings.Contains(*e.Message, "diagonal") || s.Counters.Get(CounterViewTooLarge) != 1 {
		t.Fatalf("8 km view: %d %s %v", code, b, s.Counters.Snapshot())
	}
	for _, q := range []string{"view=41.7,44.8,41.71", "view=41.7,44.8,91,44.81", "view=a,b,c,d",
		"view=" + viewAround(origin, 1500) + "&recent_positions_duration=61"} {
		if code, b := get(t, base+"/uss/flights?"+q); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", q, code, b)
		}
	}
	if code, _ := get(t, base+"/uss/flights"); code != http.StatusBadRequest {
		t.Errorf("no view: %d", code)
	}
	if s.Counters.Get(CounterFlightsRequests) != 7 {
		t.Errorf("requests %v", s.Counters.Snapshot())
	}
}

// recent_positions are what was asked: none without a duration, the
// samples of the last d seconds with one (E-01 pair).
func TestSearchFlightsRecentPositions(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	for i := 0; i <= 30; i++ {
		w.Add(sample(1, offset(origin, float64(i), 0), t0.Add(time.Duration(i-30)*time.Second)))
	}
	s := &Server{Window: w, Now: c.now}
	view := viewAround(origin, 1500)
	ask := func(d *float32) f3411.RIDFlight {
		res, err := s.SearchFlights(context.Background(), stdf3411.SearchFlightsRequestObject{
			Params: f3411.SearchFlightsParams{View: view, RecentPositionsDuration: d}})
		if err != nil {
			t.Fatal(err)
		}
		r, ok := res.(stdf3411.SearchFlights200JSONResponse)
		if !ok || r.Flights == nil || len(*r.Flights) != 1 {
			t.Fatalf("%#v", res)
		}
		return (*r.Flights)[0]
	}
	if f := ask(nil); f.RecentPositions != nil {
		t.Fatalf("recent positions without a duration: %d", len(*f.RecentPositions))
	}
	zero := float32(0)
	if f := ask(&zero); f.RecentPositions != nil {
		t.Fatal("recent positions for a zero duration")
	}
	ten := float32(10)
	f := ask(&ten)
	if f.RecentPositions == nil || len(*f.RecentPositions) != 11 {
		t.Fatalf("10 s of 1 Hz samples: %v", f.RecentPositions)
	}
	for _, p := range *f.RecentPositions {
		if t0.Sub(p.Time.Value) > 10*time.Second {
			t.Errorf("a recent position older than asked: %v", p.Time.Value)
		}
	}
}

// Special values (spec 04 §3.1) through core's constants: a track with
// unknown speed, track, vertical speed, altitude, pressure altitude and
// height serves SpecialSpeed (255), SpecialTrackDirection (361),
// SpecialVerticalSpeed (63) and SpecialHeight (-1000); reading our own
// output back with f3411.UnmarshalRIDFlight yields nils. The pair: a
// known track serves its values, read back as numbers.
func TestSpecialValuesBothWays(t *testing.T) {
	unknown := sample(1, origin, t0)
	b := &unknown.Body
	b.SpeedMS, b.TrackDeg, b.VSpeedMS, b.AltWGS84M, b.AltPressureM, b.HeightM, b.HeightRef = nil, nil, nil, nil, nil, nil, nil
	b.Status, b.TimestampAccuracyS = nil, nil
	f := FlightOf(View{ID: flightN(1), Current: unknown})
	st := f.CurrentState
	if *st.Speed != f3411.SpecialSpeed || *st.Track != f3411.SpecialTrackDirection || *st.VerticalSpeed != f3411.SpecialVerticalSpeed ||
		*st.Position.Alt != f3411.SpecialHeight || *st.Position.PressureAltitude != f3411.SpecialHeight ||
		*st.Position.Height.Distance != f3411.SpecialHeight || *st.OperationalStatus != f3411.Undeclared || st.TimestampAccuracy != 0 {
		t.Fatalf("special values not served: %+v", st)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	back, err := f3411.UnmarshalRIDFlight(raw)
	if err != nil {
		t.Fatalf("our own output is refused by core: %v %s", err, raw)
	}
	s := back.CurrentState
	if s.SpeedMS() != nil || s.TrackDeg() != nil || s.VerticalSpeedMS() != nil || s.Position.AltHAEM() != nil ||
		s.Position.PressureAltM() != nil || s.Position.Height.DistanceM() != nil {
		t.Fatalf("special values do not read back as nil: %s", raw)
	}

	known := FlightOf(View{ID: flightN(2), Current: sample(2, origin, t0)})
	raw, _ = json.Marshal(known)
	back, err = f3411.UnmarshalRIDFlight(raw)
	if err != nil {
		t.Fatal(err)
	}
	s = back.CurrentState
	if v := s.SpeedMS(); v == nil || *v != 12.5 {
		t.Errorf("speed %v", v)
	}
	if v := s.TrackDeg(); v == nil || *v != 90 {
		t.Errorf("track %v", v)
	}
	if v := s.VerticalSpeedMS(); v == nil || *v != 1.5 {
		t.Errorf("vertical speed %v", v)
	}
	if v := s.Position.AltHAEM(); v == nil || *v != 560 {
		t.Errorf("alt %v", v)
	}
	if v := s.Position.PressureAltM(); v == nil || *v != 545 {
		t.Errorf("pressure altitude %v", v)
	}
	if v := s.Position.Height.DistanceM(); v == nil || *v != 80 || s.Position.Height.Reference != f3411.GroundLevel {
		t.Errorf("height %v %v", v, s.Position.Height.Reference)
	}
	if *s.OperationalStatus != f3411.Airborne || *back.Simulated || *s.Position.Extrapolated || back.AircraftType != f3411.NotDeclared ||
		*s.Position.AccuracyH != f3411.HA10m || *s.Position.AccuracyV != f3411.VA10m || s.SpeedAccuracy != f3411.SAUnknown {
		t.Errorf("%s", raw)
	}
	if lat, lng := *s.Position.Lat, *s.Position.Lng; lat != origin.LatDeg || lng != origin.LonDeg {
		t.Errorf("position %v %v", lat, lng)
	}
}

// Values beyond the wire's range are held at its limits: a speed above
// MaxSpeed is MaxSpeed ("or more"), a vertical speed beyond
// MaxAbsVerticalSpeed is held at it; an emergency is the Emergency
// status whatever status was sent. Within range nothing is changed.
func TestWireLimitsAndEmergency(t *testing.T) {
	s := sample(1, origin, t0)
	s.Body.SpeedMS, s.Body.VSpeedMS, s.Body.Emergency = fptr(300), fptr(-90), true
	st := StateOf(s)
	if *st.Speed != f3411.MaxSpeed || *st.VerticalSpeed != -f3411.MaxAbsVerticalSpeed || *st.OperationalStatus != f3411.Emergency {
		t.Fatalf("%v %v %v", *st.Speed, *st.VerticalSpeed, *st.OperationalStatus)
	}
	raw, _ := json.Marshal(f3411.RIDFlight{Id: "x", AircraftType: f3411.NotDeclared, CurrentState: &st})
	back, err := f3411.UnmarshalRIDFlight(raw)
	if err != nil || !back.CurrentState.SpeedIsMax() {
		t.Fatalf("%v %s", err, raw)
	}
	s.Body.SpeedMS, s.Body.VSpeedMS = fptr(254), fptr(-62)
	st = StateOf(s)
	if *st.Speed != 254 || *st.VerticalSpeed != -62 {
		t.Fatalf("within range changed: %v %v", *st.Speed, *st.VerticalSpeed)
	}
}

// A flight that does not make a valid RIDFlight is not served, and that
// is counted, never silent; the valid one beside it is served (E-01).
func TestUnservableFlightIsCounted(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	w.Add(sample(1, origin, t0))
	bad := sample(2, origin, t0)
	bad.Body.AccuracyH = "HAWrong"
	bad.Body.TimestampAccuracyS = fptr(-1)
	w.Add(bad)
	// A negative timestamp accuracy is served as 0; force a refusal
	// through a status core does not know.
	w.flights[flightN(2)].samples[0].Body.Status = sptr("Flying")
	w.flights[flightN(2)].samples[0].Body.Emergency = false
	s := &Server{Window: w, Now: c.now, Counters: &core.Counters{}}
	res, err := s.SearchFlights(context.Background(), stdf3411.SearchFlightsRequestObject{Params: f3411.SearchFlightsParams{View: viewAround(origin, 1000)}})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(stdf3411.SearchFlights200JSONResponse)
	if len(*r.Flights) != 2 || s.Counters.Get(CounterFlightUnservable) != 0 {
		t.Fatalf("an unknown status or accuracy is served as unknown, not refused: %d %v", len(*r.Flights), s.Counters.Snapshot())
	}
	if *(*r.Flights)[1].CurrentState.OperationalStatus != f3411.Undeclared || *(*r.Flights)[1].CurrentState.Position.AccuracyH != f3411.HAUnknown {
		t.Fatalf("%+v", (*r.Flights)[1].CurrentState)
	}
	// A recent position that is not a latitude (a corrupted sample):
	// core refuses the flight.
	w.Add(sample(2, origin, t0.Add(-time.Second)))
	w.flights[flightN(2)].samples[0].Body.Position.Lat = 95
	sixty := float32(60)
	res, _ = s.SearchFlights(context.Background(), stdf3411.SearchFlightsRequestObject{Params: f3411.SearchFlightsParams{View: viewAround(origin, 1000), RecentPositionsDuration: &sixty}})
	r = res.(stdf3411.SearchFlights200JSONResponse)
	if len(*r.Flights) != 1 || (*r.Flights)[0].Id != flightN(1) || s.Counters.Get(CounterFlightUnservable) != 1 {
		t.Fatalf("unservable: %d %v", len(*r.Flights), s.Counters.Snapshot())
	}
}
