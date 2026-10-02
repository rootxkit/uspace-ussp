package ridsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

const ourBase = "https://ussp.test"

func f3548Volume(minLat, minLon, maxLat, maxLon, lo, hi float64, start, end time.Time) f3548.Volume4D {
	alt := func(v float64) *f3548.Altitude { return &f3548.Altitude{Value: v, Reference: "W84", Units: "M"} }
	return f3548.Volume4D{
		Volume: f3548.Volume3D{
			AltitudeLower: alt(lo), AltitudeUpper: alt(hi),
			OutlinePolygon: &f3548.Polygon{Vertices: []f3548.LatLngPoint{
				{Lat: minLat, Lng: minLon}, {Lat: minLat, Lng: maxLon}, {Lat: maxLat, Lng: maxLon}, {Lat: maxLat, Lng: minLon},
			}},
		},
		TimeStart: &f3548.Time{Value: start, Format: "RFC3339"},
		TimeEnd:   &f3548.Time{Value: end, Format: "RFC3339"},
	}
}

// The intent's ISA spans every volume's box, the lowest lower and the
// highest upper W84 altitude, and ends a minute after the last volume.
// Volumes that cannot be bounded are refused, never an empty extent.
func TestIntentExtent(t *testing.T) {
	s, e := time.Now().Add(time.Minute), time.Now().Add(time.Hour)
	vol, end, err := IntentExtent([]f3548.Volume4D{
		f3548Volume(41.70, 44.80, 41.71, 44.81, 500, 550, s, e),
		f3548Volume(41.705, 44.805, 41.72, 44.83, 480, 600, s, e.Add(10*time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !end.Equal(e.Add(10*time.Minute + Horizon)) {
		t.Errorf("end %v", end)
	}
	if vol.AltitudeLower.Value != 480 || vol.AltitudeUpper.Value != 600 || vol.AltitudeLower.Reference != f3411.W84 {
		t.Errorf("altitudes %+v %+v", vol.AltitudeLower, vol.AltitudeUpper)
	}
	ext := f3411.Volume4D{Volume: vol}
	box, _, _, err := f3411.Volume4DToZonesEnvelope(ext)
	if err != nil {
		t.Fatal(err)
	}
	if box.MinLat > 41.70 || box.MaxLat < 41.72 || box.MinLon > 44.80 || box.MaxLon < 44.83 {
		t.Errorf("box %+v does not hold the volumes", box)
	}
	for name, vols := range map[string][]f3548.Volume4D{
		"none":         nil,
		"no altitude":  {{Volume: f3548.Volume3D{OutlinePolygon: f3548Volume(41.7, 44.8, 41.71, 44.81, 0, 1, s, e).Volume.OutlinePolygon}, TimeEnd: &f3548.Time{Value: e, Format: "RFC3339"}}},
		"no end":       {{Volume: f3548Volume(41.7, 44.8, 41.71, 44.81, 0, 1, s, e).Volume}},
		"no outline":   {{Volume: f3548.Volume3D{}}},
		"antimeridian": {f3548Volume(10, 179.9, 10.1, -179.9, 0, 1, s, e)},
		"not W84": {func() f3548.Volume4D {
			v := f3548Volume(41.7, 44.8, 41.71, 44.81, 0, 1, s, e)
			v.Volume.AltitudeLower.Reference = "SFC"
			return v
		}()},
	} {
		if _, _, err := IntentExtent(vols); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The session ISA holds the position at least the radius from its
// edges, is not centred on it (F3411: extents are not centred on the
// take-off point), and two flights close together share it.
func TestSessionExtent(t *testing.T) {
	r := 2000.0
	vol, err := SessionExtent(origin, r)
	if err != nil {
		t.Fatal(err)
	}
	box, _, _, err := f3411.Volume4DToZonesEnvelope(f3411.Volume4D{Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	if !box.Contains(origin) {
		t.Fatalf("box %+v misses the position", box)
	}
	for _, d := range []struct{ n, e float64 }{{r, 0}, {-r, 0}, {0, r}, {0, -r}} {
		if p := offset(origin, d.n*0.99, d.e*0.99); !box.Contains(p) {
			t.Errorf("a point %v m from the position is outside the ISA", d)
		}
	}
	centre := core.LatLon{LatDeg: (box.MinLat + box.MaxLat) / 2, LonDeg: (box.MinLon + box.MaxLon) / 2}
	if math.Abs(centre.LatDeg-origin.LatDeg) < 1e-9 && math.Abs(centre.LonDeg-origin.LonDeg) < 1e-9 {
		t.Error("the session ISA is centred on the position")
	}
	near := offset(origin, 1, 1)
	vol2, _ := SessionExtent(near, r)
	if a, b := vol.OutlinePolygon.Vertices, vol2.OutlinePolygon.Vertices; len(a) != 4 || a[0] != b[0] || a[2] != b[2] {
		t.Errorf("flights a metre apart get different ISAs: %v %v", a, b)
	}
	for name, c := range map[string]struct {
		p core.LatLon
		r float64
	}{
		"invalid":      {core.LatLon{LatDeg: 95}, r},
		"no radius":    {origin, 0},
		"inf radius":   {origin, math.Inf(1)},
		"antimeridian": {core.LatLon{LatDeg: 0, LonDeg: 179.99}, r},
		"pole":         {core.LatLon{LatDeg: 89.5, LonDeg: 0}, r},
	} {
		if _, err := SessionExtent(c.p, c.r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func started(id string, intent *string, pos *flights.Point) flights.Body {
	return flights.Body{FlightID: id, Event: flights.EventStarted, ClientID: "c", UASSerial: "TEST1", IntentID: intent, Position: pos}
}

// Planning both ways (E-01): a flight without an intent gets a session
// ISA around its position, one with an intent the intent's; a second
// fact plans nothing more; a flight with neither an intent nor a
// position plans nothing and that is counted; ended queues the delete of
// a planned ISA and nothing for a flight without one; a late fact of an
// ended flight plans nothing.
func TestPlanner(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	p := &Planner{Counters: &core.Counters{}}
	pos := &flights.Point{Lat: origin.LatDeg, Lng: origin.LonDeg}
	if err := p.Plan(ctx, m, started(flightN(1), nil, pos)); err != nil {
		t.Fatal(err)
	}
	if err := p.Plan(ctx, m, started(flightN(1), nil, pos)); err != nil {
		t.Fatal(err)
	}
	items := m.pending()
	if len(items) != 1 || items[0].Kind != store.OutboxISAPut || p.Counters.Get(CounterISAPlanned) != 1 {
		t.Fatalf("session: %+v", items)
	}
	var put ISAPut
	_ = json.Unmarshal(items[0].Payload, &put)
	rec := m.isas[put.ISAID]
	if rec == nil || rec.Kind != ISAKindSession || put.HorizonS != policy.Defaults().SessionISAHorizonS || put.TimeEnd != nil ||
		put.Volume.OutlinePolygon == nil || put.FlightID != flightN(1) {
		t.Fatalf("session ISA %+v %+v", rec, put)
	}

	intentID := "8c1f3f2e-7d0e-4a8b-9a51-0e4b7d6f2c11"
	s, e := time.Now(), time.Now().Add(time.Hour)
	m.volumes[intentID], _ = json.Marshal([]f3548.Volume4D{f3548Volume(41.70, 44.80, 41.71, 44.81, 500, 550, s, e)})
	if err := p.Plan(ctx, m, started(flightN(2), &intentID, pos)); err != nil {
		t.Fatal(err)
	}
	items = m.pending()
	_ = json.Unmarshal(items[1].Payload, &put)
	if m.isas[put.ISAID].Kind != ISAKindIntent || put.TimeEnd == nil || !put.TimeEnd.Equal(e.Add(Horizon)) || put.Volume.AltitudeUpper.Value != 550 {
		t.Fatalf("intent ISA %+v", put)
	}

	if err := p.Plan(ctx, m, started(flightN(3), nil, nil)); err != nil {
		t.Fatal(err)
	}
	if len(m.pending()) != 2 || p.Counters.Get(CounterISAUnplanned) != 1 {
		t.Fatalf("a flight without intent or position: %d %v", len(m.pending()), p.Counters.Snapshot())
	}

	end := flights.Body{FlightID: flightN(1), Event: flights.EventEnded}
	if err := p.Plan(ctx, m, end); err != nil {
		t.Fatal(err)
	}
	if err := p.Plan(ctx, m, flights.Body{FlightID: flightN(3), Event: flights.EventEnded}); err != nil {
		t.Fatal(err)
	}
	items = m.pending()
	if len(items) != 3 || items[2].Kind != store.OutboxISADelete {
		t.Fatalf("ended: %+v", items)
	}
	m.ended[flightN(4)] = true
	if err := p.Plan(ctx, m, started(flightN(4), nil, pos)); err != nil {
		t.Fatal(err)
	}
	if len(m.pending()) != 3 {
		t.Fatal("a late fact of an ended flight planned an ISA")
	}
}

// An intent that does not bound falls back to a session ISA, counted;
// a session ISA becomes the intent's when the flight is bound to an
// intent in flight; an intent that does not bound then leaves the
// session ISA as it is (both counted).
func TestPlannerIntentLater(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	p := &Planner{Counters: &core.Counters{}}
	pos := &flights.Point{Lat: origin.LatDeg, Lng: origin.LonDeg}
	bad := "11111111-1111-4111-8111-111111111111"
	m.volumes[bad] = []byte(`[]`)
	if err := p.Plan(ctx, m, started(flightN(1), &bad, pos)); err != nil {
		t.Fatal(err)
	}
	if p.Counters.Get(CounterISAIntentFallback) != 1 || len(m.pending()) != 1 {
		t.Fatalf("%v", p.Counters.Snapshot())
	}
	var put ISAPut
	_ = json.Unmarshal(m.pending()[0].Payload, &put)
	if m.isas[put.ISAID].Kind != ISAKindSession {
		t.Fatal("not a session ISA")
	}
	resumed := flights.Body{FlightID: flightN(1), Event: flights.EventTelemetryResumed, IntentID: &bad, Position: pos}
	if err := p.Plan(ctx, m, resumed); err != nil {
		t.Fatal(err)
	}
	if p.Counters.Get(CounterISAIntentFallback) != 2 || len(m.pending()) != 1 {
		t.Fatalf("the session ISA changed for an intent that does not bound: %v", p.Counters.Snapshot())
	}
	good := "8c1f3f2e-7d0e-4a8b-9a51-0e4b7d6f2c11"
	m.volumes[good], _ = json.Marshal([]f3548.Volume4D{f3548Volume(41.70, 44.80, 41.71, 44.81, 500, 550, time.Now(), time.Now().Add(time.Hour))})
	resumed.IntentID = &good
	if err := p.Plan(ctx, m, resumed); err != nil {
		t.Fatal(err)
	}
	items := m.pending()
	if len(items) != 2 || m.isas[put.ISAID].Kind != ISAKindIntent || items[1].EntityID != put.ISAID {
		t.Fatalf("the session ISA did not become the intent's: %+v", items)
	}
	if err := p.Plan(ctx, m, resumed); err != nil || len(m.pending()) != 2 {
		t.Fatalf("an intent ISA re-planned: %v %d", err, len(m.pending()))
	}
}

// A store that fails refuses the plan: the flight fact is not recorded
// and is retried with it (never half done).
func TestPlannerStoreFailure(t *testing.T) {
	m := newMemStore()
	p := &Planner{}
	for _, name := range []string{"FlightISA", "FlightEnded", "Now"} {
		m.errs = map[string]error{name: errors.New("db down")}
		if err := p.Plan(context.Background(), m, started(flightN(1), nil, &flights.Point{Lat: 41.7, Lng: 44.8})); err == nil {
			t.Errorf("%s failing: planned", name)
		}
	}
}

// subscriber is a peer Display Provider's notification endpoint.
type subscriber struct {
	srv   *httptest.Server
	mu    sync.Mutex
	got   []f3411.PutIdentificationServiceAreaNotificationParameters
	paths []string
	auth  []string
	fail  bool
}

func newSubscriber(t *testing.T) *subscriber {
	s := &subscriber{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var n f3411.PutIdentificationServiceAreaNotificationParameters
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &n)
		s.got = append(s.got, n)
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// subscribe registers a subscription of base over the test area in d.
func subscribe(t *testing.T, d *fakedss.DSS, id, base string) {
	t.Helper()
	ext := f3411.Volume4D{
		Volume:  boxVolume(geodeticBox(origin, 0.1)),
		TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: time.Now().Add(24 * time.Hour)},
	}
	b, _ := json.Marshal(f3411.CreateSubscriptionParameters{Extents: ext, UssBaseUrl: base})
	req, _ := http.NewRequest(http.MethodPut, d.URL()+"/rid/v2/dss/subscriptions/"+id, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer peer")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("subscription: %d", res.StatusCode)
	}
}

type workerRig struct {
	m   *memStore
	d   *fakedss.DSS
	tok *tokens
	w   *ISAWorker
	p   *Planner
}

func newWorkerRig(t *testing.T) *workerRig {
	d := fakedss.New()
	t.Cleanup(d.Close)
	r := &workerRig{m: newMemStore(), d: d, tok: &tokens{}, p: &Planner{}}
	r.w = &ISAWorker{Store: r.m, DSSBaseURL: d.URL(), USSBaseURL: ourBase, Tokens: r.tok, Counters: &core.Counters{}, MaxBackoff: time.Second}
	return r
}

func (r *workerRig) start(t *testing.T, n int) string {
	t.Helper()
	if err := r.p.Plan(context.Background(), r.m, started(flightN(n), nil, &flights.Point{Lat: origin.LatDeg, Lng: origin.LonDeg})); err != nil {
		t.Fatal(err)
	}
	isa, _, _ := r.m.FlightISA(context.Background(), flightN(n))
	return isa.ISAID
}

func (r *workerRig) once(t *testing.T) int {
	t.Helper()
	n, err := r.w.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// notify runs one notification pass.
func (r *workerRig) notify(t *testing.T) int {
	t.Helper()
	n, err := r.w.NotifyOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func probe(w *ISAWorker) (obs.State, string) { return w.Probe()(context.Background()) }

// Flight start puts the ISA in the DSS with our base URL, records its
// version and queues the notification of the subscriber the DSS lists,
// which the notification pass posts with aud = the subscriber's host and
// scope rid.service_provider; flight end deletes it with the version and
// notifies the deletion without service_area (E-01 both ways). /readyz
// goes unknown -> up.
func TestWorkerPutAndDelete(t *testing.T) {
	r := newWorkerRig(t)
	sub := newSubscriber(t)
	subscribe(t, r.d, "11111111-1111-4111-8111-111111111111", sub.srv.URL+"/rid")
	if st, _ := probe(r.w); st != obs.StateUnknown {
		t.Fatalf("before any call: %s", st)
	}
	id := r.start(t, 1)
	if r.once(t) != 1 {
		t.Fatal("nothing claimed")
	}
	isa, ok := r.d.ISAs()[id]
	if !ok || isa.UssBaseUrl != ourBase || r.w.Counters.Get(CounterISAWrites) != 1 {
		t.Fatalf("ISA not in the DSS: %+v %v", r.d.ISAs(), r.w.Counters.Snapshot())
	}
	rec := r.m.isas[id]
	if rec.Version == nil || *rec.Version != isa.Version || len(r.m.pending()) != 0 {
		t.Fatalf("version not recorded: %+v", rec)
	}
	if st, d := probe(r.w); st != obs.StateUp {
		t.Fatalf("after a write: %s %s", st, d)
	}
	sub.mu.Lock()
	if len(sub.got) != 0 || len(r.m.notes()) != 1 {
		t.Fatalf("notification not queued, or posted by the write: %d %d", len(sub.got), len(r.m.notes()))
	}
	sub.mu.Unlock()
	if r.notify(t) != 1 || len(r.m.notes()) != 0 {
		t.Fatal("notification not posted")
	}
	for _, c := range r.d.Calls() {
		if strings.Contains(c.Path, "/subscriptions/") {
			continue // the peer's own subscription
		}
		if c.Authorization != "Bearer aud=127.0.0.1;scope=rid.service_provider" {
			t.Errorf("DSS call with %q", c.Authorization)
		}
	}
	sub.mu.Lock()
	if len(sub.got) != 1 || sub.paths[0] != "POST /rid/uss/identification_service_areas/"+id || sub.got[0].ServiceArea == nil ||
		sub.got[0].Extents == nil || len(sub.got[0].Subscriptions) != 1 || *sub.got[0].Subscriptions[0].NotificationIndex != 1 ||
		sub.auth[0] != "Bearer aud=127.0.0.1;scope=rid.service_provider" {
		t.Fatalf("notification %+v %v %v", sub.got, sub.paths, sub.auth)
	}
	sub.mu.Unlock()

	r.m.ended[flightN(1)] = true
	if err := r.p.Plan(context.Background(), r.m, flights.Body{FlightID: flightN(1), Event: flights.EventEnded}); err != nil {
		t.Fatal(err)
	}
	r.once(t)
	if _, ok := r.d.ISAs()[id]; ok || r.m.isas[id].DeletedAt == nil || r.w.Counters.Get(CounterISADeletes) != 1 {
		t.Fatalf("ISA not deleted: %+v", r.d.ISAs())
	}
	r.notify(t)
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.got) != 2 || sub.got[1].ServiceArea != nil || sub.got[1].Extents != nil || *sub.got[1].Subscriptions[0].NotificationIndex != 2 {
		t.Fatalf("deletion notification %+v", sub.got)
	}
	if r.w.Counters.Get(CounterSubscriberNotified) != 2 {
		t.Errorf("%v", r.w.Counters.Snapshot())
	}
}

// E-02: the DSS down at flight start -> the write fails, /readyz says
// dss down since T with the waiting item, the item is retried with
// backoff; the DSS back -> the ISA is created, dss up, the outbox empty.
func TestWorkerDSSDownThenUp(t *testing.T) {
	r := newWorkerRig(t)
	r.d.Down(true)
	id := r.start(t, 1)
	r.once(t)
	st, detail := probe(r.w)
	if st != obs.StateDown || !strings.Contains(detail, "down since") || !strings.Contains(detail, "1 ISA writes waiting") ||
		r.w.Counters.Get(CounterISAFailed) != 1 || r.w.State().Up {
		t.Fatalf("DSS down: %s %q %v", st, detail, r.w.Counters.Snapshot())
	}
	if r.m.lastErr[id] == "" {
		t.Error("the ISA's last error is not recorded")
	}
	since := r.w.State().Since
	if r.once(t) != 0 {
		t.Fatal("retried before its backoff")
	}
	r.m.advance(2 * time.Second)
	r.once(t) // still down: the state keeps its since
	if r.w.State().Since != since {
		t.Error("down since moved while the DSS stayed down")
	}
	r.d.Down(false)
	r.m.advance(2 * time.Second)
	r.once(t)
	if _, ok := r.d.ISAs()[id]; !ok {
		t.Fatal("ISA not created on recovery")
	}
	if st, d := probe(r.w); st != obs.StateUp || d != "" || len(r.m.pending()) != 0 {
		t.Fatalf("after recovery: %s %q %d", st, d, len(r.m.pending()))
	}
}

// A 409 on create (our earlier create whose answer was lost) reads the
// ISA, records its version and retries at once as an update; an ISA the
// DSS holds for another Service Provider stays an error (E-01 pair).
func TestWorkerConflictRecovers(t *testing.T) {
	r := newWorkerRig(t)
	id := r.start(t, 1)
	other := &ISAWorker{Store: newMemStore(), DSSBaseURL: r.d.URL(), USSBaseURL: ourBase, Tokens: r.tok}
	_ = other.Store.(*memStore).InsertISA(context.Background(), ISARecord{ISAID: id, FlightID: flightN(1), Kind: ISAKindSession})
	_, _ = other.Store.(*memStore).Enqueue(context.Background(), store.OutboxISAPut, id, 1, r.payload(t, id))
	if _, err := other.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := r.d.ISAs()[id].Version
	r.once(t) // 409, version recorded, due again at once
	if r.m.isas[id].Version == nil || *r.m.isas[id].Version != first {
		t.Fatalf("version not learnt: %+v", r.m.isas[id])
	}
	r.once(t)
	if v := r.d.ISAs()[id].Version; v == first || len(r.m.pending()) != 0 {
		t.Fatalf("not updated after the conflict: %s %d", v, len(r.m.pending()))
	}

	r2 := newWorkerRig(t)
	id2 := r2.start(t, 2)
	foreign := &ISAWorker{Store: newMemStore(), DSSBaseURL: r2.d.URL(), USSBaseURL: "https://other.test", Tokens: r2.tok}
	_ = foreign.Store.(*memStore).InsertISA(context.Background(), ISARecord{ISAID: id2, FlightID: flightN(2), Kind: ISAKindSession})
	_, _ = foreign.Store.(*memStore).Enqueue(context.Background(), store.OutboxISAPut, id2, 1, r2.payload(t, id2))
	_, _ = foreign.Once(context.Background())
	r2.once(t)
	if r2.m.isas[id2].Version != nil || !strings.Contains(r2.m.lastErr[id2], "another Service Provider") {
		t.Fatalf("a foreign ISA was taken: %+v %q", r2.m.isas[id2], r2.m.lastErr[id2])
	}
}

func (r *workerRig) payload(t *testing.T, id string) ISAPut {
	t.Helper()
	items := r.m.pending()
	for i := range items {
		if it := &items[i]; it.EntityID == id {
			var p ISAPut
			_ = json.Unmarshal(it.Payload, &p)
			return p
		}
	}
	t.Fatal("no payload")
	return ISAPut{}
}

// Nothing is written for a flight that ended before its ISA was, nor
// for a window that passed; a delete of an ISA never written, or one the
// DSS no longer holds, records it deleted (each counted or logged).
func TestWorkerSkips(t *testing.T) {
	r := newWorkerRig(t)
	id := r.start(t, 1)
	r.m.ended[flightN(1)] = true
	r.once(t)
	if len(r.d.ISAs()) != 0 || r.w.Counters.Get(CounterISASkippedEnded) != 1 || len(r.m.pending()) != 0 {
		t.Fatalf("written for an ended flight: %v", r.w.Counters.Snapshot())
	}
	_ = r.p.Plan(context.Background(), r.m, flights.Body{FlightID: flightN(1), Event: flights.EventEnded})
	r.once(t)
	if r.m.isas[id].DeletedAt == nil || len(r.d.Calls()) != 0 {
		t.Fatalf("a never written ISA: %+v %v", r.m.isas[id], r.d.Calls())
	}

	past := time.Now().Add(-time.Minute)
	_ = r.m.InsertISA(context.Background(), ISARecord{ISAID: "isa-past", FlightID: flightN(2), Kind: ISAKindIntent})
	_, _ = r.m.Enqueue(context.Background(), store.OutboxISAPut, "isa-past", 1, ISAPut{ISAID: "isa-past", FlightID: flightN(2), TimeEnd: &past})
	r.once(t)
	if r.w.Counters.Get(CounterISAExpired) != 1 || len(r.m.pending()) != 0 {
		t.Fatalf("a passed window: %v", r.w.Counters.Snapshot())
	}

	id3 := r.start(t, 3)
	r.once(t)
	r.m.ended[flightN(3)] = true
	_ = r.p.Plan(context.Background(), r.m, flights.Body{FlightID: flightN(3), Event: flights.EventEnded})
	gone := r.d.ISAs()[id3]
	req, _ := http.NewRequest(http.MethodDelete, r.d.URL()+"/rid/v2/dss/identification_service_areas/"+id3+"/"+gone.Version, nil)
	req.Header.Set("Authorization", "Bearer x")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	r.once(t)
	if r.m.isas[id3].DeletedAt == nil || len(r.m.pending()) != 0 {
		t.Fatal("an ISA the DSS no longer holds is not recorded deleted")
	}
}

// A refused write (4xx) is retried and recorded, and the DSS is up: it
// answered. A token that cannot be had is a failure, retried.
func TestWorkerRefusals(t *testing.T) {
	r := newWorkerRig(t)
	r.w.USSBaseURL = "" // the fake refuses a create without uss_base_url
	id := r.start(t, 1)
	r.once(t)
	if !r.w.State().Up || !strings.Contains(r.m.lastErr[id], "refused") || len(r.m.pending()) != 1 {
		t.Fatalf("%+v %q", r.w.State(), r.m.lastErr[id])
	}
	r.w.USSBaseURL = ourBase
	r.tok.err = errors.New("token service down")
	r.m.due()
	r.once(t)
	if !strings.Contains(r.m.lastErr[id], "token") || len(r.d.ISAs()) != 0 {
		t.Fatalf("%q", r.m.lastErr[id])
	}
	r.tok.err = nil
	r.m.due()
	r.once(t)
	if len(r.d.ISAs()) != 1 {
		t.Fatal("not written once the token came")
	}
}

// A subscriber that does not take the notification is tried again,
// then dropped after MaxNotifyAttempts and counted, and the ISA stays
// written; one that takes it is counted notified at once (E-01). An
// unreadable item is dropped and counted.
func TestWorkerNotifyFailure(t *testing.T) {
	r := newWorkerRig(t)
	bad, good := newSubscriber(t), newSubscriber(t)
	bad.fail = true
	subscribe(t, r.d, "11111111-1111-4111-8111-111111111111", bad.srv.URL)
	subscribe(t, r.d, "22222222-2222-4222-8222-222222222222", good.srv.URL)
	r.start(t, 1)
	r.once(t)
	r.notify(t)
	if r.w.Counters.Get(CounterSubscriberNotifyErr) != 0 || r.w.Counters.Get(CounterSubscriberNotified) != 1 || len(r.m.notes()) != 1 {
		t.Fatalf("first pass: %v %d", r.w.Counters.Snapshot(), len(r.m.notes()))
	}
	for range MaxNotifyAttempts - 1 {
		if r.notify(t) != 0 {
			t.Fatal("retried before its wait")
		}
		r.m.advance(notifyRetry)
		r.notify(t)
	}
	if r.w.Counters.Get(CounterSubscriberNotifyErr) != 1 || len(r.m.notes()) != 0 || len(r.d.ISAs()) != 1 {
		t.Fatalf("after %d attempts: %v %d", MaxNotifyAttempts, r.w.Counters.Snapshot(), len(r.m.notes()))
	}
	bad.mu.Lock()
	if bad.got != nil {
		t.Error("the failing subscriber recorded a notification")
	}
	bad.mu.Unlock()

	_, _ = r.m.Enqueue(context.Background(), store.OutboxISANotify, "x", 0, "not a notification")
	r.notify(t)
	if r.w.Counters.Get(CounterSubscriberNotifyErr) != 2 || len(r.m.notes()) != 0 {
		t.Fatalf("unreadable: %v", r.w.Counters.Snapshot())
	}
	r.m.errs["Claim"] = errors.New("db down")
	if _, err := r.w.NotifyOnce(context.Background()); err == nil {
		t.Fatal("a claim failure not returned")
	}
}

// E-10: a DSS answer with more subscribers than MaxSubscribers, or one
// whose url is not an absolute http(s) URL, is refused; within the
// bound it is taken.
func TestCheckSubscribers(t *testing.T) {
	one := []f3411.SubscriptionState{{SubscriptionId: "s"}}
	ok := make([]f3411.SubscriberToNotify, MaxSubscribers)
	for i := range ok {
		ok[i] = f3411.SubscriberToNotify{Url: "https://dp.test/rid", Subscriptions: one}
	}
	if got, err := checkSubscribers(&ok); err != nil || len(got) != MaxSubscribers {
		t.Fatalf("at the bound: %v", err)
	}
	over := append(slices.Clone(ok), ok[0])
	if _, err := checkSubscribers(&over); err == nil {
		t.Fatal("over the bound accepted")
	}
	for _, u := range []string{"/relative", "ftp://dp.test", "https://", ""} {
		s := []f3411.SubscriberToNotify{{Url: u, Subscriptions: one}}
		if _, err := checkSubscribers(&s); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	s := []f3411.SubscriberToNotify{{Url: "https://dp.test"}}
	if _, err := checkSubscribers(&s); err == nil {
		t.Error("a subscriber without a subscription accepted")
	}
	if got, err := checkSubscribers(nil); err != nil || got != nil {
		t.Error("no subscribers")
	}
}

// The backoff doubles from 1 s and stops at MaxBackoff.
func TestBackoff(t *testing.T) {
	w := &ISAWorker{MaxBackoff: 30 * time.Second}
	for attempts, want := range map[int32]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 60: 30 * time.Second} {
		if got := w.backoff(attempts); got != want {
			t.Errorf("attempt %d: %v, want %v", attempts, got, want)
		}
	}
	if (&ISAWorker{}).backoff(100) != DefaultMaxBackoff {
		t.Error("default cap")
	}
}

// A session ISA near its end is renewed while its flight goes on: one
// renewal queued, none more while it waits (bounded during a DSS
// outage), the ISA's end pushed out once written; an ended flight's is
// not renewed.
func TestRenewal(t *testing.T) {
	r := newWorkerRig(t)
	id := r.start(t, 1)
	r.once(t)
	before := r.m.isas[id].TimeEnd
	if err := r.w.Renew(context.Background()); err != nil || len(r.m.pending()) != 0 {
		t.Fatalf("renewed early: %v %d", err, len(r.m.pending()))
	}
	r.m.advance(time.Duration(policy.Defaults().SessionISAHorizonS/2+1) * time.Second)
	r.d.Down(true)
	for range 3 {
		if err := r.w.Renew(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.m.pending()) != 1 || r.w.Counters.Get(CounterISARenewed) != 1 {
		t.Fatalf("renewals queued: %d %v", len(r.m.pending()), r.w.Counters.Snapshot())
	}
	r.once(t)
	r.d.Down(false)
	r.m.advance(2 * time.Second)
	r.once(t)
	if !r.m.isas[id].TimeEnd.After(before) || len(r.m.pending()) != 0 {
		t.Fatalf("not renewed: %v -> %v", before, r.m.isas[id].TimeEnd)
	}
	r.m.ended[flightN(1)] = true
	r.m.advance(time.Duration(policy.Defaults().SessionISAHorizonS) * time.Second)
	if err := r.w.Renew(context.Background()); err != nil || len(r.m.pending()) != 0 {
		t.Fatal("an ended flight's ISA renewed")
	}
}

// An item whose payload does not read is dropped, logged and counted,
// and does not block the next one.
func TestWorkerUnreadableItem(t *testing.T) {
	r := newWorkerRig(t)
	r.m.mu.Lock()
	r.m.nextID++
	r.m.items = append(r.m.items, &store.OutboxItem{ID: r.m.nextID, Kind: store.OutboxISAPut, EntityID: "x", Payload: []byte(`{`), NextAt: time.Now()})
	r.m.mu.Unlock()
	r.start(t, 1)
	if r.once(t) != 2 || len(r.d.ISAs()) != 1 || r.w.Counters.Get(CounterISAFailed) != 1 || len(r.m.pending()) != 0 {
		t.Fatalf("%v %d", r.w.Counters.Snapshot(), len(r.m.pending()))
	}
	r.m.errs["Claim"] = errors.New("db down")
	if _, err := r.w.Once(context.Background()); err == nil {
		t.Fatal("a claim failure not returned")
	}
	r.m.errs = map[string]error{"Backlog": errors.New("db down")}
	if _, d := probe(r.w); !strings.Contains(d, "depth unknown") {
		t.Fatalf("%q", d)
	}
}

// geodeticBox is a box of half side d degrees around p.
func geodeticBox(p core.LatLon, d float64) geodesy.BBox {
	return geodesy.BBox{MinLat: p.LatDeg - d, MaxLat: p.LatDeg + d, MinLon: p.LonDeg - d, MaxLon: p.LonDeg + d}
}

// The margin holds wherever a flight starts (latitudes 0 to 80, both
// hemispheres): a point radius away in any direction is inside its ISA.
func TestSessionExtentMarginEverywhere(t *testing.T) {
	r := 1500.0
	for i := range 2000 {
		lat := -80 + float64(i%161)
		lon := -170 + float64((i*37)%340) + float64(i%7)*0.013
		p := core.LatLon{LatDeg: lat + float64(i%13)*0.0371, LonDeg: lon}
		vol, err := SessionExtent(p, r)
		if err != nil {
			t.Fatalf("%v: %v", p, err)
		}
		box, _, _, _ := f3411.Volume4DToZonesEnvelope(f3411.Volume4D{Volume: vol})
		for _, brg := range []float64{0, 45, 90, 135, 180, 225, 270, 315} {
			if q := geodesy.Destination(p, brg, r); !box.Contains(q) {
				t.Fatalf("%v: %v m at %v° is outside %+v", p, r, brg, box)
			}
		}
	}
}

// Run works items and renewals until its context ends.
func TestWorkerRun(t *testing.T) {
	r := newWorkerRig(t)
	r.w.Every, r.w.RenewEvery = 10*time.Millisecond, 10*time.Millisecond
	id := r.start(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.w.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := r.d.ISAs()[id]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run wrote nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// Ping tells /readyz about the DSS before any flight: unknown, then up
// on an answer (a 404 for an ISA nobody wrote), degraded when the DSS
// refuses our token, down without an answer (E-02 both ways).
func TestPing(t *testing.T) {
	r := newWorkerRig(t)
	if st, _ := probe(r.w); st != obs.StateUnknown {
		t.Fatal(st)
	}
	if err := r.w.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, d := probe(r.w); st != obs.StateUp {
		t.Fatalf("%s %s", st, d)
	}
	plain := &ISAWorker{Store: r.m, DSSBaseURL: r.d.URL(), Tokens: tokenFunc(func() (string, error) { return "", nil })}
	_ = plain.Ping(context.Background()) // "Bearer " with an empty token: the fake says 401
	if st, d := probe(plain); st != obs.StateDegraded || !strings.Contains(d, "401 to our token") {
		t.Fatalf("token refused: %s %q", st, d)
	}
	r.d.Down(true)
	if err := r.w.Ping(context.Background()); err == nil {
		t.Fatal("no error from a DSS that is down")
	}
	if st, _ := probe(r.w); st != obs.StateDown {
		t.Fatal(st)
	}
	r.d.Close()
	if err := r.w.Ping(context.Background()); err == nil || !strings.Contains(r.w.State().Reason, "no answer") {
		t.Fatalf("closed: %v %+v", err, r.w.State())
	}
}

type tokenFunc func() (string, error)

func (f tokenFunc) Token(context.Context, string, ...string) (string, error) { return f() }

// hangingSubscriber takes a notification and never answers until the
// test ends.
func hangingSubscriber(t *testing.T) *httptest.Server {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: the handlers return, then the server closes
	return srv
}

// ISA writes never wait on subscribers: with subscribers that never
// answer, the put and the delete of an ISA each take well under one call
// timeout, and the ISA is created and deleted in the DSS.
func TestNotificationsDoNotBlockISAWrites(t *testing.T) {
	r := newWorkerRig(t)
	for i := range 3 {
		subscribe(t, r.d, fmt.Sprintf("%08d-1111-4111-8111-111111111111", i), hangingSubscriber(t).URL)
	}
	id := r.start(t, 1)
	began := time.Now()
	r.once(t)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the ISA put waited %v on subscribers", took)
	}
	if _, ok := r.d.ISAs()[id]; !ok {
		t.Fatal("ISA not written")
	}
	r.m.ended[flightN(1)] = true
	if err := r.p.Plan(context.Background(), r.m, flights.Body{FlightID: flightN(1), Event: flights.EventEnded}); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	r.once(t)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the ISA delete waited %v on subscribers", took)
	}
	if _, ok := r.d.ISAs()[id]; ok {
		t.Fatal("ISA not deleted")
	}
	// The notification pass is bounded by its budget, not by the
	// subscribers: six notifications to subscribers that never answer.
	if len(r.m.notes()) != 6 {
		t.Fatalf("notifications queued: %d", len(r.m.notes()))
	}
	r.w.NotifyBudget = 300 * time.Millisecond
	began = time.Now()
	r.notify(t)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the notification pass took %v with a budget of %v", took, r.w.NotifyBudget)
	}
	if r.w.Counters.Get(CounterSubscriberNotified) != 0 {
		t.Fatal("a hanging subscriber counted notified")
	}
}

// badSubscribers answers every successful ISA put and delete of the DSS
// with a subscriber list that cannot be notified (a relative url).
type badSubscribers struct{}

func (badSubscribers) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || res.StatusCode != http.StatusOK || req.Method == http.MethodGet ||
		!strings.Contains(req.URL.Path, "/dss/identification_service_areas/") {
		return res, err
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	var ans map[string]any
	if err := json.Unmarshal(b, &ans); err != nil {
		return nil, err
	}
	ans["subscribers"] = []any{map[string]any{"url": "/relative", "subscriptions": []any{map[string]any{"subscription_id": "s"}}}}
	b, _ = json.Marshal(ans)
	res.Body, res.ContentLength = io.NopCloser(strings.NewReader(string(b))), int64(len(b))
	res.Header.Del("Content-Length")
	return res, nil
}

// A DSS answer whose subscriber list is refused after a successful put
// or delete still records what the DSS holds: the item is done at the
// first attempt (no create -> 409 -> refresh loop), the ISA's version or
// deletion is recorded, nothing is notified and the refusal is counted.
func TestWorkerBadSubscribersRecordsFirst(t *testing.T) {
	r := newWorkerRig(t)
	r.w.HTTP = &http.Client{Transport: badSubscribers{}, Timeout: DefaultCallTimeout}
	id := r.start(t, 1)
	r.once(t)
	isa, ok := r.d.ISAs()[id]
	if !ok || r.m.isas[id].Version == nil || *r.m.isas[id].Version != isa.Version || len(r.m.pending()) != 0 {
		t.Fatalf("put not recorded at the first attempt: version %v pending %d", r.m.isas[id].Version, len(r.m.pending()))
	}
	r.m.ended[flightN(1)] = true
	if err := r.p.Plan(context.Background(), r.m, flights.Body{FlightID: flightN(1), Event: flights.EventEnded}); err != nil {
		t.Fatal(err)
	}
	r.once(t)
	if _, ok := r.d.ISAs()[id]; ok || r.m.isas[id].DeletedAt == nil || len(r.m.pending()) != 0 {
		t.Fatalf("delete not recorded at the first attempt: deleted %v pending %d", r.m.isas[id].DeletedAt, len(r.m.pending()))
	}
	if r.w.Counters.Get(CounterSubscribersRefused) != 2 || len(r.m.notes()) != 0 || r.w.Counters.Get(CounterISAFailed) != 0 {
		t.Fatalf("%v notes %d", r.w.Counters.Snapshot(), len(r.m.notes()))
	}
}

// gatedPuts holds every ISA PUT to the DSS until gate closes, and says
// on entered when one arrives.
type gatedPuts struct {
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (g *gatedPuts) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/dss/identification_service_areas/") {
		g.once.Do(func() { close(g.entered) })
		<-g.gate
	}
	return http.DefaultTransport.RoundTrip(req)
}

// Two workers (two api replicas) on one database: one is creating an
// ISA in the DSS when the flight ends and the other takes the delete.
// The delete waits for the put of the same ISA and then deletes what
// the put wrote, so no ISA is left in the DSS for an ended flight.
func TestWorkerPutAndDeleteSerialisedPerISA(t *testing.T) {
	r := newWorkerRig(t)
	g := &gatedPuts{gate: make(chan struct{}), entered: make(chan struct{})}
	a := &ISAWorker{Store: r.m, DSSBaseURL: r.d.URL(), USSBaseURL: ourBase, Tokens: r.tok, Counters: &core.Counters{},
		HTTP: &http.Client{Transport: g, Timeout: 10 * time.Second}, Batch: 1}
	b := &ISAWorker{Store: r.m, DSSBaseURL: r.d.URL(), USSBaseURL: ourBase, Tokens: r.tok, Counters: &core.Counters{}, Batch: 1}
	id := r.start(t, 1)
	aDone := make(chan error, 1)
	go func() { _, err := a.Once(context.Background()); aDone <- err }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the put never reached the DSS")
	}
	r.m.mu.Lock()
	r.m.ended[flightN(1)] = true
	r.m.mu.Unlock()
	if err := r.p.Plan(context.Background(), r.m, flights.Body{FlightID: flightN(1), Event: flights.EventEnded}); err != nil {
		t.Fatal(err)
	}
	bDone := make(chan error, 1)
	go func() { _, err := b.Once(context.Background()); bDone <- err }()
	select {
	case err := <-bDone:
		bDone <- err // the delete did not wait for the put
	case <-time.After(300 * time.Millisecond):
	}
	close(g.gate)
	for _, c := range []chan error{aDone, bDone} {
		if err := <-c; err != nil {
			t.Fatal(err)
		}
	}
	for range 3 { // whatever is still due
		r.m.due()
		r.once(t)
	}
	if _, ok := r.d.ISAs()[id]; ok {
		t.Fatalf("the ISA of an ended flight is left in the DSS (row deleted_at %v)", r.m.isas[id].DeletedAt)
	}
	if r.m.isas[id].DeletedAt == nil || len(r.m.pending()) != 0 {
		t.Fatalf("not recorded deleted: pending %d", len(r.m.pending()))
	}
}
