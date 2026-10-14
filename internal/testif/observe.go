package testif

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
	obsv "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/ridobservation"
)

// Counters of the RID observation (E-09).
const (
	CounterObservationServed      = "testif_observation_served"
	CounterObservationUnavailable = "testif_observation_unavailable"
)

// trustProvider is the trust class of a flight another Service Provider
// serves (envelope/v1 trust): what the Display Provider role shows.
const trustProvider = "provider"

// maxObservedTracks bounds the track ids remembered from the last views
// for GET /display_data/{id} (E-10).
const maxObservedTracks = 4096

// productTrack is what the observation reads of a traffic/product/v1
// track (schemas/traffic/product/v1; TestObservationReadsTheProductSchema
// reads the schema's examples through it).
type productTrack struct {
	TrackID      string    `json:"track_id"`
	Trust        string    `json:"trust"`
	TimeOfReport time.Time `json:"time_of_report"`
	Position     struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	} `json:"position"`
	SpeedMS   *float64 `json:"speed_ms"`
	TrackDeg  *float64 `json:"track_deg"`
	VspeedMS  *float64 `json:"vspeed_ms"`
	Emergency *bool    `json:"emergency"`
}

type product struct {
	Schema string `json:"schema"`
	Body   struct {
		Tracks []productTrack `json:"tracks"`
	} `json:"body"`
}

// observer is the RID observation interface: what the USSP's Display
// Provider role shows a person, read from GET /v1/traffic/snapshot?bbox=
// with a staff console session (the snapshot admits a bbox for a staff
// session only), mapped onto observation.yaml. It serves no more than a
// staff console shows: positions and motion, no identification.
type observer struct {
	s *Server

	mu   sync.Mutex
	seen map[string]bool
}

func newObserver(s *Server) *observer { return &observer{s: s, seen: map[string]bool{}} }

var _ obsv.StrictServerInterface = (*observer)(nil)

func (o *observer) iface() iface {
	return iface{name: "rid_observation", access: ObservationAccess(), register: func(g *httpx.GuardedMux) {
		h := obsv.NewStrictHandlerWithOptions(o, nil, obsv.StrictHTTPServerOptions{
			RequestErrorHandlerFunc: badObservation, ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
				httpx.NewProblem(http.StatusInternalServerError, httpx.SlugInternal, "", "").Write(w, r)
			},
		})
		obsv.HandlerWithOptions(h, obsv.StdHTTPServerOptions{BaseURL: BaseObservation, BaseRouter: g, ErrorHandlerFunc: badObservation})
	}}
}

func badObservation(w http.ResponseWriter, r *http.Request, _ error) {
	httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "the request is not the operation's", core.Fieldf("view", "malformed or missing")).Write(w, r)
}

// unavailable answers 503 with the reason the snapshot could not be read
// (a staff session not configured, the USSP's refusal or silence).
type unavailable struct{ detail string }

func (u unavailable) write(w http.ResponseWriter) error {
	httpx.RetryAfter(w, retryAfter)
	w.Header().Set("Content-Type", httpx.ProblemContentType)
	w.WriteHeader(http.StatusServiceUnavailable)
	return json.NewEncoder(w).Encode(httpx.NewProblem(http.StatusServiceUnavailable, "observation_unavailable", "", clip(u.detail, 512)))
}

// VisitGetDisplayDataResponse writes the 503.
func (u unavailable) VisitGetDisplayDataResponse(w http.ResponseWriter) error { return u.write(w) }

// viewBBox parses view (lat1,lng1,lat2,lng2: the box of two corners)
// into the snapshot's bbox (west,south,east,north).
func viewBBox(view string) (string, error) {
	parts := strings.Split(view, ",")
	if len(parts) != 4 {
		return "", core.Fieldf("view", "four numbers lat1,lng1,lat2,lng2")
	}
	v := make([]float64, 4)
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || !core.IsFinite(f) {
			return "", core.Fieldf("view", "not a number: %q", clip(p, 32))
		}
		v[i] = f
	}
	lat1, lng1, lat2, lng2 := v[0], v[1], v[2], v[3]
	if lat1 < -90 || lat1 > 90 || lat2 < -90 || lat2 > 90 || lng1 < -180 || lng1 > 180 || lng2 < -180 || lng2 > 180 {
		return "", core.Fieldf("view", "out of range")
	}
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }
	return strings.Join([]string{f(min(lng1, lng2)), f(min(lat1, lat2)), f(max(lng1, lng2)), f(max(lat1, lat2))}, ","), nil
}

// snapshot reads the traffic product of bbox with the staff session.
func (o *observer) snapshot(ctx context.Context, bbox string) ([]productTrack, error) {
	if o.s.USSP.Console == "" {
		return nil, errors.New("no staff console session is configured (USSP_TEST_INTERFACES_CONSOLE_SESSION_FILE): the snapshot admits a bbox for a staff session only")
	}
	c, err := o.s.USSP.Staff()
	if err != nil {
		return nil, err
	}
	resp, err := c.GetTrafficSnapshotWithResponse(ctx, &client.GetTrafficSnapshotParams{Bbox: &bbox})
	if err != nil {
		return nil, fmt.Errorf("GET /v1/traffic/snapshot: the USSP did not answer: %w", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, problemOf(resp.StatusCode(), resp.Body)
	}
	var p product
	if err := json.Unmarshal(resp.Body, &p); err != nil || p.Schema != "traffic/product/v1" {
		return nil, errors.New("GET /v1/traffic/snapshot: the answer is not a traffic/product/v1 message")
	}
	return p.Body.Tracks, nil
}

// flightOf maps one provider track onto an observation Flight.
func flightOf(t productTrack) obsv.Flight {
	ts := t.TimeOfReport.UTC().Format(time.RFC3339Nano)
	cs := &obsv.CurrentState{Timestamp: &ts}
	if t.SpeedMS != nil {
		v := float32(*t.SpeedMS)
		cs.Speed = &v
	}
	if t.TrackDeg != nil {
		v := float32(*t.TrackDeg)
		cs.Track = &v
	}
	if t.VspeedMS != nil {
		v := float32(*t.VspeedMS)
		cs.VerticalSpeed = &v
	}
	if t.Emergency != nil && *t.Emergency {
		st := obsv.RIDOperationalStatusEmergency
		cs.OperationalStatus = &st
	}
	return obsv.Flight{Id: t.TrackID, CurrentState: cs, MostRecentPosition: &obsv.Position{Lat: t.Position.Lat, Lng: t.Position.Lng}}
}

// GetDisplayData is GET /display_data?view=: the provider tracks of the
// view's box as the staff console shows them.
func (o *observer) GetDisplayData(ctx context.Context, req obsv.GetDisplayDataRequestObject) (obsv.GetDisplayDataResponseObject, error) {
	bbox, err := viewBBox(req.Params.View)
	if err != nil {
		return badView{err}, nil //nolint:nilerr // the refusal is the answer, written by badView
	}
	tracks, err := o.snapshot(ctx, bbox)
	if err != nil {
		o.s.Counters.Inc(CounterObservationUnavailable)
		return unavailable{err.Error()}, nil //nolint:nilerr // the failure is the answer, a 503 with its reason
	}
	flights := []obsv.Flight{}
	o.mu.Lock()
	for _, t := range tracks {
		if t.Trust != trustProvider {
			continue
		}
		flights = append(flights, flightOf(t))
		if len(o.seen) >= maxObservedTracks {
			clear(o.seen)
		}
		o.seen[t.TrackID] = true
	}
	o.mu.Unlock()
	o.s.Counters.Inc(CounterObservationServed)
	clusters := []obsv.Cluster{}
	return obsv.GetDisplayData200JSONResponse{Flights: &flights, Clusters: &clusters}, nil
}

// badView answers a view that is not four numbers.
type badView struct{ err error }

// VisitGetDisplayDataResponse writes the 400.
func (b badView) VisitGetDisplayDataResponse(w http.ResponseWriter) error {
	var fe *core.FieldError
	if !errors.As(b.err, &fe) {
		fe = core.Fieldf("view", "malformed")
	}
	w.Header().Set("Content-Type", httpx.ProblemContentType)
	w.WriteHeader(http.StatusBadRequest)
	return json.NewEncoder(w).Encode(httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "view is malformed", fe))
}

// GetDetails is GET /display_data/{id}: a flight of a view answers the
// details a staff console shows, which are none beyond the view (no
// operator or UAS identification, rule 8); another id is 404.
func (o *observer) GetDetails(_ context.Context, req obsv.GetDetailsRequestObject) (obsv.GetDetailsResponseObject, error) {
	o.mu.Lock()
	ok := o.seen[req.Id]
	o.mu.Unlock()
	if !ok {
		return obsv.GetDetails404Response{}, nil
	}
	return obsv.GetDetails200JSONResponse{}, nil
}
