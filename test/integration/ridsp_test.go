//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/app/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
	sp "github.com/rootxkit/uspace-ussp/internal/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// ridRig is rid-sp and api against the real NATS and PostgreSQL, a fake
// DSS and the fake authority as the token service and as the Display
// Provider; tracks and flight facts are published as telemetry-ingest
// publishes them.
type ridRig struct {
	t       *testing.T
	a       *fakeAuthority
	dss     *fakedss.DSS
	nc      *bus.Conn
	pub     *bus.Publisher
	sp      string // rid-sp base URL
	api     *client.ClientWithResponses
	dpToken string
	spToken string
}

func newRIDRig(t *testing.T) *ridRig {
	t.Helper()
	ensureSchemas(t)
	r := &ridRig{t: t, a: newFakeAuthority(t), nc: busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)}
	r.pub = bus.NewPublisher(r.nc, &core.Counters{})
	r.sp = "http://" + runAt(t, ridsp.Spec, map[string]string{
		"USSP_RID_SP_ADDR":   "127.0.0.1:0",
		"USSP_NATS_URL":      mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_AUDIENCES":     testHost + "," + testAlias,
		"USSP_TOKEN_ISSUERS": r.a.url + "=" + r.a.jwks,
	})
	vars := withAuth(t, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
	}, r.a)
	r.dss = fakedss.New()
	t.Cleanup(r.dss.Close)
	vars["USSP_DSS_BASE_URL"] = r.dss.URL()
	vars["USSP_USS_BASE_URL"] = r.sp // what a Display Provider finds in the ISA
	// The CIS signs a notification's aud as the host of the callback
	// built from USSP_USS_BASE_URL, so api refuses to start unless that
	// host is an audience (system audit F-5).
	vars["USSP_AUDIENCES"] += ",127.0.0.1"
	r.api = run(t, api.Spec, vars)
	var err error
	if r.dpToken, err = r.a.iss.Issue("authority-dp", testHost, []string{string(f3411.ScopeDisplayProvider)}, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.spToken, err = r.a.iss.Issue("peer-sp", testHost, []string{string(f3411.ScopeServiceProvider)}, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	// rid-sp is ready to serve once its TRK consumer is open.
	within(t, 15*time.Second, func() bool {
		res, err := http.Get(r.sp + "/readyz")
		if err != nil {
			return false
		}
		defer res.Body.Close()
		var body struct {
			Dependencies map[string]struct{ State string } `json:"dependencies"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		return body.Dependencies["trk"].State == "up"
	})
	return r
}

// track publishes one sample of flight id at p, captured now, as
// telemetry-ingest does (trk.v1, core NATS, captured by TRK).
func (r *ridRig) track(id string, p core.LatLon, seq int64, op *telemetry.OperatorPosition) {
	r.t.Helper()
	now := time.Now()
	c5, _, err := cell.Key(p)
	if err != nil {
		r.t.Fatal(err)
	}
	serial, reg := "TEST-RID-"+id[len(id)-6:], "GEOTESTOP0001"
	status, ref := string(f3411.Airborne), string(f3411.GroundLevel)
	alt, hgt, spd, trk, vs, acc := 560.0, 80.0, 12.5, 90.0, 0.5, 0.1
	m := &telemetry.Track{
		Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer, core.Times{TS: &now, RxTS: now, CapturedAt: now, Source: core.TimeSourceClock}),
		Body: telemetry.TrackBody{
			TrackID: id, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, SourceInstance: "wp9-client",
			Position: telemetry.Position{Lat: p.LatDeg, Lng: p.LonDeg}, AltWGS84M: &alt, AltSource: core.AltGeodetic,
			HeightM: &hgt, HeightRef: &ref, SpeedMS: &spd, TrackDeg: &trk, VSpeedMS: &vs, Status: &status,
			Identification: core.Identification{Status: core.IdentRegistered, Reason: core.ReasonSessionBinding, Serial: &serial, OperatorReg: &reg},
			FlightID:       &id, OperatorPosition: op, AccuracyH: f3411.HA10m, AccuracyV: f3411.VA10m, TimestampAccuracyS: &acc, Seq: seq,
		},
	}
	subject, err := bus.Trk(c5, id)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.pub.Publish(context.Background(), subject, m); err != nil {
		r.t.Fatal(err)
	}
}

// fact publishes a flight fact on flight.v1 (JetStream FLIGHT), as the
// binder of telemetry-ingest does.
func (r *ridRig) fact(id, event string, p core.LatLon) {
	r.t.Helper()
	now := time.Now()
	var reason *string
	if event == flights.EventEnded {
		s := flights.EndOperator
		reason = &s
	}
	reg := "GEOTESTOP0001"
	e := &flights.Event{Envelope: bus.SystemEnvelope(flights.Schema, flights.Producer, now), Body: flights.Body{
		FlightID: id, Event: event, At: bus.Stamp{Time: now}, StartedAt: bus.Stamp{Time: now}, ClientID: "wp9-client",
		UASSerial: "TEST-RID-" + id[len(id)-6:], OperatorReg: &reg, EndReason: reason,
		Position: &flights.Point{Lat: p.LatDeg, Lng: p.LonDeg},
	}}
	subject, err := e.Subject()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.pub.Publish(context.Background(), subject, e); err != nil {
		r.t.Fatal(err)
	}
}

func (r *ridRig) do(method, url, token string, body any) (int, []byte) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// view is a square view around p with the given diagonal.
func view(p core.LatLon, diagonalM float64) string {
	a := geodesy.Destination(p, 225, diagonalM/2)
	b := geodesy.Destination(p, 45, diagonalM/2)
	return fmt.Sprintf("%.7f,%.7f,%.7f,%.7f", a.LatDeg, a.LonDeg, b.LatDeg, b.LonDeg)
}

// area is the GeoPolygonString of a box of half side d degrees.
func area(p core.LatLon, d float64) string {
	return fmt.Sprintf("%f,%f,%f,%f,%f,%f,%f,%f", p.LatDeg-d, p.LonDeg-d, p.LatDeg-d, p.LonDeg+d, p.LatDeg+d, p.LonDeg+d, p.LatDeg+d, p.LonDeg-d)
}

// dpDiscovers is the Display Provider's discovery: the ISAs the DSS
// holds over the area.
func (r *ridRig) dpDiscovers(p core.LatLon) []f3411.IdentificationServiceArea {
	code, b := r.do(http.MethodGet, r.dss.URL()+"/rid/v2/dss/identification_service_areas?area="+area(p, 0.05), r.dpToken, nil)
	var res f3411.SearchIdentificationServiceAreasResponse
	if code != 200 || json.Unmarshal(b, &res) != nil || res.ServiceAreas == nil {
		r.t.Fatalf("search %d %s", code, b)
	}
	return *res.ServiceAreas
}

// dpSubscriber is the authority's Display Provider endpoint for the
// ISA notifications its DSS subscription asks for.
type dpSubscriber struct {
	mu   sync.Mutex
	got  []f3411.PutIdentificationServiceAreaNotificationParameters
	auth []string
}

func (r *ridRig) subscribe(p core.LatLon) *dpSubscriber {
	s := &dpSubscriber{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var n f3411.PutIdentificationServiceAreaNotificationParameters
		_ = json.NewDecoder(req.Body).Decode(&n)
		s.mu.Lock()
		s.got, s.auth = append(s.got, n), append(s.auth, req.Header.Get("Authorization"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	r.t.Cleanup(srv.Close)
	b := geodesy.BBox{MinLat: p.LatDeg - 0.05, MaxLat: p.LatDeg + 0.05, MinLon: p.LonDeg - 0.05, MaxLon: p.LonDeg + 0.05}
	ext := f3411.Volume4D{
		Volume: f3411.Volume3D{OutlinePolygon: &f3411.Polygon{Vertices: []f3411.LatLngPoint{
			{Lat: b.MinLat, Lng: b.MinLon}, {Lat: b.MinLat, Lng: b.MaxLon}, {Lat: b.MaxLat, Lng: b.MaxLon}, {Lat: b.MaxLat, Lng: b.MinLon},
		}}},
		TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: time.Now().Add(time.Hour)},
	}
	code, body := r.do(http.MethodPut, r.dss.URL()+"/rid/v2/dss/subscriptions/"+newUUID(), r.dpToken,
		f3411.CreateSubscriptionParameters{Extents: ext, UssBaseUrl: srv.URL})
	if code != 200 {
		r.t.Fatalf("subscription %d %s", code, body)
	}
	return s
}

func (s *dpSubscriber) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.got) }

// newUUID is a version 4 UUID.
func newUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// readyzDSS is api's dss entry.
func (r *ridRig) readyzDSS() (string, string) {
	res, err := r.api.GetReadyzWithResponse(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	body := res.JSON200
	if body == nil {
		body = res.JSON503
	}
	if body == nil {
		return "", ""
	}
	d, ok := body.Dependencies["dss"]
	if !ok {
		return "", ""
	}
	detail := ""
	if d.Detail != nil {
		detail = *d.Detail
	}
	return string(d.State), detail
}

// The S-M2 network identification path end to end (brief WP-9): a
// flight starts -> its ISA is in the DSS with our base URL and the
// authority's DP subscription is notified (aud = its host,
// rid.service_provider); the DP discovers the ISA and GET /uss/flights
// on its base URL returns the flight with every Art. 8(2) item; details
// for a 1.5 km view; 413 for an 8 km view; the flight ends -> the ISA
// is deleted and the DP notified again (E-01 both ways on every cap).
func TestIntegrationRIDSPNetworkIdentification(t *testing.T) {
	r := newRIDRig(t)
	at := core.LatLon{LatDeg: 41.6411, LonDeg: 44.9137}
	dp := r.subscribe(at)
	id := newUUID()
	op := &telemetry.OperatorPosition{Lat: 41.6405, Lng: 44.9120}
	r.fact(id, flights.EventStarted, at)
	r.track(id, at, 1, op)
	var isa f3411.IdentificationServiceArea
	took := within(t, 20*time.Second, func() bool {
		for _, a := range r.dss.ISAs() {
			if a.UssBaseUrl == r.sp {
				isa = a
				return true
			}
		}
		return false
	})
	t.Logf("ISA %s in the DSS with uss_base_url %s %v after the flight started", isa.Id, isa.UssBaseUrl, took.Round(time.Millisecond))
	for i := 2; i <= 3; i++ {
		time.Sleep(time.Second)
		r.track(id, at, int64(i), op)
	}
	within(t, 10*time.Second, func() bool { return dp.count() >= 1 })
	dp.mu.Lock()
	if dp.got[0].ServiceArea == nil || dp.got[0].ServiceArea.Id != isa.Id || !strings.Contains(dp.auth[0], "Bearer ") {
		t.Fatalf("DP notification %+v", dp.got[0])
	}
	dp.mu.Unlock()

	found := r.dpDiscovers(at)
	if !slices.ContainsFunc(found, func(a f3411.IdentificationServiceArea) bool { return a.Id == isa.Id && a.UssBaseUrl == r.sp }) {
		t.Fatalf("the DP does not discover our ISA: %+v", found)
	}
	// The third sample reaches rid-sp through NATS a moment after it was
	// published: poll until the answer holds it.
	var (
		res *f3411.GetFlightsResponse
		b   []byte
		i   int
	)
	within(t, 5*time.Second, func() bool {
		var code int
		code, b = r.do(http.MethodGet, isa.UssBaseUrl+"/uss/flights?view="+view(at, 1500)+"&recent_positions_duration=60", r.dpToken, nil)
		var err error
		res, err = f3411.UnmarshalGetFlightsResponse(b)
		if code != 200 || err != nil || res.Flights == nil {
			t.Fatalf("/uss/flights %d %v %s", code, err, b)
		}
		i = slices.IndexFunc(*res.Flights, func(f f3411.RIDFlight) bool { return f.Id == id })
		return i >= 0 && (*res.Flights)[i].RecentPositions != nil && len(*(*res.Flights)[i].RecentPositions) >= 3
	})
	f := (*res.Flights)[i]
	st := f.CurrentState
	// Art. 8(2): (c) position with alt and height, (d) track and speed,
	// (f) operational status, (g) timestamp.
	ll := st.Position.LatLon()
	switch {
	case math.Abs(ll.LatDeg-at.LatDeg) > 1e-6 || math.Abs(ll.LonDeg-at.LonDeg) > 1e-6:
		t.Errorf("position %v", ll)
	case st.Position.AltHAEM() == nil || *st.Position.AltHAEM() != 560 || st.Position.Height.DistanceM() == nil:
		t.Errorf("altitude/height %s", b)
	case st.TrackDeg() == nil || st.SpeedMS() == nil || *st.SpeedMS() != 12.5:
		t.Errorf("track/speed %s", b)
	case st.OperationalStatus == nil || *st.OperationalStatus != f3411.Airborne || *f.Simulated:
		t.Errorf("status %s", b)
	case res.Timestamp.Value.Sub(st.Timestamp.Value) > sp.Horizon:
		t.Errorf("timestamp %s", b)
	}
	// (a) operator_id, (b) serial, (e) operator location in the details.
	code, b := r.do(http.MethodGet, isa.UssBaseUrl+"/uss/flights/"+id+"/details", r.dpToken, nil)
	var det f3411.GetFlightDetailsResponse
	if code != 200 || json.Unmarshal(b, &det) != nil || det.Details.OperatorId == nil || *det.Details.OperatorId != "GEOTESTOP0001" ||
		det.Details.UasId == nil || det.Details.UasId.SerialNumber == nil || det.Details.OperatorLocation == nil ||
		det.Details.OperatorLocation.Position.Lat != op.Lat || *det.Details.UasId.UtmId != id {
		t.Fatalf("details %d %s", code, b)
	}
	if code, b := r.do(http.MethodGet, isa.UssBaseUrl+"/uss/flights?view="+view(at, 8000), r.dpToken, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("8 km view: %d %s", code, b)
	}
	if code, _ := r.do(http.MethodGet, isa.UssBaseUrl+"/uss/flights/"+id+"/details", r.spToken, nil); code != http.StatusForbidden {
		t.Fatalf("details without rid.display_provider: %d", code)
	}
	if state, detail := r.readyzDSS(); state != "up" {
		t.Errorf("dss %s %q", state, detail)
	}

	r.fact(id, flights.EventEnded, at)
	took = within(t, 20*time.Second, func() bool { _, ok := r.dss.ISAs()[isa.Id]; return !ok })
	t.Logf("ISA deleted %v after the flight ended", took.Round(time.Millisecond))
	within(t, 10*time.Second, func() bool { return dp.count() >= 2 })
	dp.mu.Lock()
	if last := dp.got[len(dp.got)-1]; last.ServiceArea != nil {
		t.Errorf("deletion notified with a service_area: %+v", last)
	}
	dp.mu.Unlock()
	if v := count(t, relOwner(t), "SELECT count(*) FROM dss_isas WHERE isa_id = $1 AND deleted_at IS NOT NULL AND version IS NOT NULL", isa.Id); v != 1 {
		t.Errorf("dss_isas row %d", v)
	}
	if v := count(t, relOwner(t), "SELECT count(*) FROM flights WHERE id = $1 AND isa_id = $2", id, isa.Id); v != 1 {
		t.Errorf("flights.isa_id %d", v)
	}
}

// E-02: the DSS down for 30 s while a flight starts -> /uss/flights
// still serves it, api's /readyz says dss down since T with the ISA
// write waiting; the DSS back -> the ISA is created, dss up and the
// outbox empty.
func TestIntegrationRIDSPDSSDownAtFlightStart(t *testing.T) {
	r := newRIDRig(t)
	within(t, 15*time.Second, func() bool { s, _ := r.readyzDSS(); return s == "up" })
	r.dss.Down(true)
	at := core.LatLon{LatDeg: 41.6911, LonDeg: 44.7337}
	id := newUUID()
	r.fact(id, flights.EventStarted, at)
	down := time.Now()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := int64(1); ; n++ {
			r.track(id, at, n, nil)
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
		}
	}()
	defer func() { close(stop); <-done }()
	within(t, 15*time.Second, func() bool {
		state, detail := r.readyzDSS()
		return state == "down" && strings.Contains(detail, "down since") && strings.Contains(detail, "ISA writes waiting")
	})
	code, b := r.do(http.MethodGet, r.sp+"/uss/flights?view="+view(at, 1000), r.dpToken, nil)
	if code != 200 || !strings.Contains(string(b), id) {
		t.Fatalf("/uss/flights with the DSS down: %d %s", code, b)
	}
	time.Sleep(time.Until(down.Add(30 * time.Second)))
	r.dss.Down(false)
	up := time.Now()
	within(t, 60*time.Second, func() bool {
		for _, a := range r.dss.ISAs() {
			if a.UssBaseUrl == r.sp {
				return true
			}
		}
		return false
	})
	t.Logf("ISA created %v after the DSS came back (down %v)", time.Since(up).Round(time.Millisecond), up.Sub(down).Round(time.Second))
	within(t, 30*time.Second, func() bool { s, d := r.readyzDSS(); return s == "up" && d == "" })
	if v := count(t, relOwner(t), "SELECT count(*) FROM dss_outbox WHERE kind IN ('isa_put','isa_delete') AND done_at IS NULL AND entity_id IN (SELECT isa_id FROM dss_isas WHERE flight_id = $1)", id); v != 0 {
		t.Errorf("outbox not empty: %d", v)
	}
}

// A peer Service Provider's ISA notification is stored in the
// rid_isa_notifications bucket (shared, surviving a restart) and
// answered 204; another sender for the same ISA is 403 (E-01 pair).
func TestIntegrationRIDSPISANotification(t *testing.T) {
	r := newRIDRig(t)
	isa := newUUID()
	now := time.Now()
	ext := f3411.Volume4D{
		Volume:    f3411.Volume3D{OutlineCircle: &f3411.Circle{Center: &f3411.LatLngPoint{Lat: 41.7, Lng: 44.8}, Radius: &f3411.Radius{Value: 500, Units: f3411.RadiusUnitsM}}},
		TimeStart: &f3411.Time{Format: f3411.RFC3339, Value: now}, TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: now.Add(time.Hour)},
	}
	body := f3411.PutIdentificationServiceAreaNotificationParameters{
		Subscriptions: []f3411.SubscriptionState{{SubscriptionId: newUUID()}},
		ServiceArea: &f3411.IdentificationServiceArea{Id: isa, Owner: "peer", UssBaseUrl: "https://peer.test/rid", Version: "v1",
			TimeStart: *ext.TimeStart, TimeEnd: *ext.TimeEnd},
		Extents: &ext,
	}
	if code, b := r.do(http.MethodPost, r.sp+"/uss/identification_service_areas/"+isa, r.spToken, body); code != http.StatusNoContent {
		t.Fatalf("%d %s", code, b)
	}
	raw, found, err := bus.KVStore{JS: r.nc.JetStream(), Bucket: bus.BucketISANotifications}.Get(context.Background(), isa)
	var n sp.ISANotification
	if err != nil || !found || json.Unmarshal(raw, &n) != nil || n.Sender != "peer-sp" || n.ServiceArea.Version != "v1" {
		t.Fatalf("stored %v %v %s", err, found, raw)
	}
	other, err := r.a.iss.Issue("intruder", testHost, []string{string(f3411.ScopeServiceProvider)}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := r.do(http.MethodPost, r.sp+"/uss/identification_service_areas/"+isa, other, body); code != http.StatusForbidden {
		t.Fatalf("another sender: %d", code)
	}
	if code, _ := r.do(http.MethodPost, r.sp+"/uss/identification_service_areas/"+isa, r.dpToken, body); code != http.StatusForbidden {
		t.Fatalf("a display provider token: %d", code)
	}
}

// The Service Provider budget (brief WP-9, PLAN §9): 100 flights at
// 1 Hz, 10 views polled at 1 Hz with 60 s of recent positions, for 60 s:
// p95 <= 1 s, p99 <= 3 s (NetSpDataResponseTime*), measured and printed;
// no position older than 60 s in any answer.
func TestIntegrationRIDSPLoad(t *testing.T) {
	r := newRIDRig(t)
	centre := core.LatLon{LatDeg: 41.5511, LonDeg: 45.0337}
	const nFlights, nViews = 100, 10
	run := 60 * time.Second
	ids := make([]string, nFlights)
	pos := make([]core.LatLon, nFlights)
	for i := range ids {
		ids[i] = newUUID()
		pos[i] = geodesy.Destination(centre, float64(i*37%360), float64(100+(i*53)%3000))
	}
	ctx, cancel := context.WithTimeout(context.Background(), run)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		for n := int64(1); ctx.Err() == nil; n++ {
			for i := range ids {
				pos[i] = geodesy.Destination(pos[i], 90, 10)
				r.track(ids[i], pos[i], n, nil)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	})
	time.Sleep(3 * time.Second)
	var mu sync.Mutex
	var lat []time.Duration
	var served, old, failed int
	for v := range nViews {
		at := geodesy.Destination(centre, float64(v*36), 1200)
		diag := 1500.0 + float64(v)*550 // 1.5 km to 6.45 km
		wg.Go(func() {
			for ctx.Err() == nil {
				start := time.Now()
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
					r.sp+"/uss/flights?view="+view(at, diag)+"&recent_positions_duration=60", nil)
				req.Header.Set("Authorization", "Bearer "+r.dpToken)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					continue
				}
				b, _ := io.ReadAll(res.Body)
				res.Body.Close()
				d := time.Since(start)
				resp, err := f3411.UnmarshalGetFlightsResponse(b)
				mu.Lock()
				lat = append(lat, d)
				if res.StatusCode != 200 || err != nil {
					failed++
				} else if resp.Flights != nil {
					for _, f := range *resp.Flights {
						served++
						if resp.Timestamp.Value.Sub(f.CurrentState.Timestamp.Value) > sp.Horizon {
							old++
						}
						if f.RecentPositions != nil {
							for _, p := range *f.RecentPositions {
								if resp.Timestamp.Value.Sub(p.Time.Value) > sp.Horizon {
									old++
								}
							}
						}
					}
				}
				mu.Unlock()
				select {
				case <-ctx.Done():
				case <-time.After(time.Second - min(d, time.Second)):
				}
			}
		})
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration { return lat[min(len(lat)-1, int(math.Ceil(p*float64(len(lat))))-1)] }
	p50, p95, p99 := pct(0.50), pct(0.95), pct(0.99)
	t.Logf("GET /uss/flights under load: %d flights at 1 Hz, %d views at 1 Hz for %v: %d requests, %d flights served, p50 %v p95 %v p99 %v max %v, failed %d, older than 60 s %d",
		nFlights, nViews, run, len(lat), served, p50, p95, p99, lat[len(lat)-1], failed, old)
	if len(lat) < nViews*int(run/time.Second)/2 || failed > 0 || old > 0 || served == 0 {
		t.Fatalf("requests %d, failed %d, old %d, served %d", len(lat), failed, old, served)
	}
	if p95 > f3411.NetSpDataResponseTime95thPercentileSeconds*time.Second || p99 > f3411.NetSpDataResponseTime99thPercentileSeconds*time.Second {
		t.Fatalf("p95 %v p99 %v over the F3411 budget", p95, p99)
	}
}
