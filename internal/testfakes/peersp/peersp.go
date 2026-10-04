// Package peersp is a fake peer F3411 Service Provider for tests (brief
// WP-14): another USSP serving its flights to Display Providers. It
// registers an ISA in a DSS (PUT /rid/v2/dss/identification_service_areas/
// {id}) and notifies every subscriber the DSS names (POST {url}/uss/
// identification_service_areas/{id}), as a real Service Provider must,
// and serves GET /uss/flights?view= with the flights it is given. It
// counts the flight requests it answers (SC-16: a switched-off source's
// request count stops). Down makes it answer 503; Slow delays every
// answer; Pad adds flights to every answer.
package peersp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"
)

// TokenFunc gives a bearer token for a call to base with scope (aud the
// host of base).
type TokenFunc func(base string, scope f3411.Scope) string

// Flight is one flight the fake serves.
type Flight struct {
	ID       string
	Position core.LatLon
	// AltHAEM is the geodetic altitude above the ellipsoid.
	AltHAEM  float64
	SpeedMS  float64
	TrackDeg float64
	// Age is how long before the answer's timestamp the state was valid.
	Age time.Duration
}

// Fake is a running fake peer Service Provider.
type Fake struct {
	DSSURL string
	Token  TokenFunc
	HTTP   *http.Client

	srv *httptest.Server

	mu       sync.Mutex
	down     bool
	slow     time.Duration
	pad      int
	flights  map[string]Flight
	requests int
	auth     []string
	notified []string
}

// New starts a fake peer filing its ISAs in the DSS at dssURL.
func New(dssURL string, tok TokenFunc) *Fake {
	f := &Fake{DSSURL: strings.TrimRight(dssURL, "/"), Token: tok, HTTP: &http.Client{Timeout: 5 * time.Second}, flights: map[string]Flight{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is its uss_base_url.
func (f *Fake) URL() string { return f.srv.URL }

// Close stops it.
func (f *Fake) Close() { f.srv.Close() }

// Down makes it answer 503 (down true) or serve again.
func (f *Fake) Down(down bool) { f.mu.Lock(); f.down = down; f.mu.Unlock() }

// Slow delays every answer by d.
func (f *Fake) Slow(d time.Duration) { f.mu.Lock(); f.slow = d; f.mu.Unlock() }

// Pad adds n flights near the first one to every answer (0: none).
func (f *Fake) Pad(n int) { f.mu.Lock(); f.pad = n; f.mu.Unlock() }

// SetFlight serves fl (replacing one of its id).
func (f *Fake) SetFlight(fl Flight) { f.mu.Lock(); f.flights[fl.ID] = fl; f.mu.Unlock() }

// RemoveFlight stops serving the flight id.
func (f *Fake) RemoveFlight(id string) { f.mu.Lock(); delete(f.flights, id); f.mu.Unlock() }

// Requests is how many GET /uss/flights it answered (not while down).
func (f *Fake) Requests() int { f.mu.Lock(); defer f.mu.Unlock(); return f.requests }

// Auth is every Authorization header of the flight requests, in order.
func (f *Fake) Auth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

// Notified is every subscriber URL it notified, in order.
func (f *Fake) Notified() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notified...)
}

func boxVolume(b geodesy.BBox, start, end time.Time) f3411.Volume4D {
	return f3411.Volume4D{
		Volume: f3411.Volume3D{OutlinePolygon: &f3411.Polygon{Vertices: []f3411.LatLngPoint{
			{Lat: b.MinLat, Lng: b.MinLon}, {Lat: b.MinLat, Lng: b.MaxLon}, {Lat: b.MaxLat, Lng: b.MaxLon}, {Lat: b.MaxLat, Lng: b.MinLon},
		}}},
		TimeStart: &f3411.Time{Format: f3411.RFC3339, Value: start.UTC()},
		TimeEnd:   &f3411.Time{Format: f3411.RFC3339, Value: end.UTC()},
	}
}

func (f *Fake) do(ctx context.Context, method, u string, scope f3411.Scope, base string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.Token != nil {
		req.Header.Set("Authorization", "Bearer "+f.Token(base, scope))
	}
	res, err := f.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, b, err
}

// RegisterISA creates the ISA id over b from now to end in the DSS and
// notifies every subscriber the DSS names; it returns the ISA.
func (f *Fake) RegisterISA(ctx context.Context, id string, b geodesy.BBox, end time.Time) (f3411.IdentificationServiceArea, error) {
	ext := boxVolume(b, time.Now(), end)
	code, body, err := f.do(ctx, http.MethodPut, f.DSSURL+"/rid/v2/dss/identification_service_areas/"+id, f3411.ScopeServiceProvider, f.DSSURL,
		f3411.CreateIdentificationServiceAreaParameters{Extents: ext, UssBaseUrl: f.URL()})
	if err != nil {
		return f3411.IdentificationServiceArea{}, err
	}
	if code != http.StatusOK {
		return f3411.IdentificationServiceArea{}, fmt.Errorf("the DSS answered %d: %s", code, body)
	}
	var ans f3411.PutIdentificationServiceAreaResponse
	if err := json.Unmarshal(body, &ans); err != nil {
		return f3411.IdentificationServiceArea{}, err
	}
	if ans.Subscribers != nil {
		for _, s := range *ans.Subscribers {
			sa := ans.ServiceArea
			n := f3411.PutIdentificationServiceAreaNotificationParameters{ServiceArea: &sa, Extents: &ext, Subscriptions: s.Subscriptions}
			u := strings.TrimRight(s.Url, "/") + "/uss/identification_service_areas/" + id
			code, body, err := f.do(ctx, http.MethodPost, u, f3411.ScopeServiceProvider, s.Url, n)
			if err != nil {
				return ans.ServiceArea, fmt.Errorf("notify %s: %w", u, err)
			}
			if code != http.StatusNoContent && code != http.StatusOK {
				return ans.ServiceArea, fmt.Errorf("notify %s answered %d: %s", u, code, body)
			}
			f.mu.Lock()
			f.notified = append(f.notified, s.Url)
			f.mu.Unlock()
		}
	}
	return ans.ServiceArea, nil
}

func f32(v float64) *float32 { x := float32(v); return &x }

// state is fl's current state at ts.
func state(fl Flight, ts time.Time) f3411.RIDAircraftState {
	lat, lng := fl.Position.LatDeg, fl.Position.LonDeg
	ha, va := f3411.HA10m, f3411.VA3m
	airborne := f3411.Airborne
	return f3411.RIDAircraftState{
		Timestamp: f3411.Time{Format: f3411.RFC3339, Value: ts.Add(-fl.Age).UTC()}, TimestampAccuracy: 0.1,
		OperationalStatus: &airborne, SpeedAccuracy: f3411.SA1mps,
		Position: f3411.RIDAircraftPosition{Lat: &lat, Lng: &lng, Alt: f32(fl.AltHAEM), AccuracyH: &ha, AccuracyV: &va},
		Speed:    f32(fl.SpeedMS), Track: f32(math.Mod(fl.TrackDeg, 360)), VerticalSpeed: f32(0),
	}
}

func parseView(v string) (geodesy.BBox, bool) {
	p := strings.Split(v, ",")
	if len(p) != 4 {
		return geodesy.BBox{}, false
	}
	var c [4]float64
	for i := range p {
		x, err := strconv.ParseFloat(p[i], 64)
		if err != nil {
			return geodesy.BBox{}, false
		}
		c[i] = x
	}
	return geodesy.BBox{MinLat: math.Min(c[0], c[2]), MaxLat: math.Max(c[0], c[2]), MinLon: math.Min(c[1], c[3]), MaxLon: math.Max(c[1], c[3])}, true
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down, slow := f.down, f.slow
	f.mu.Unlock()
	if slow > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(slow):
		}
	}
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/uss/flights/"); ok && strings.HasSuffix(id, "/details") && r.Method == http.MethodGet {
		id = strings.TrimSuffix(id, "/details")
		f.mu.Lock()
		_, known := f.flights[id]
		f.mu.Unlock()
		if !known {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f3411.GetFlightDetailsResponse{Details: f3411.RIDFlightDetails{Id: id}})
		return
	}
	if r.URL.Path != "/uss/flights" || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	box, ok := parseView(r.URL.Query().Get("view"))
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	now := time.Now()
	f.mu.Lock()
	f.requests++
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	var out []f3411.RIDFlight
	var first *Flight
	for _, fl := range f.flights {
		if box.Contains(fl.Position) {
			fl := fl
			if first == nil {
				first = &fl
			}
			s := state(fl, now)
			out = append(out, f3411.RIDFlight{Id: fl.ID, AircraftType: f3411.Helicopter, CurrentState: &s})
		}
	}
	for i := 0; first != nil && i < f.pad; i++ {
		fl := *first
		fl.ID = fmt.Sprintf("%s-pad-%d", first.ID, i)
		s := state(fl, now)
		out = append(out, f3411.RIDFlight{Id: fl.ID, AircraftType: f3411.Helicopter, CurrentState: &s})
	}
	f.mu.Unlock()
	if out == nil {
		out = []f3411.RIDFlight{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f3411.GetFlightsResponse{Flights: &out, Timestamp: f3411.Time{Format: f3411.RFC3339, Value: now.UTC()}})
}
