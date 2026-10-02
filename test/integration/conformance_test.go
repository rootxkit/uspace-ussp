//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/app/monitor"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	confstore "github.com/rootxkit/uspace-ussp/internal/conformance/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	flightstore "github.com/rootxkit/uspace-ussp/internal/flights/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// confRig is WP-7's intent rig (api's intent service against the real
// PostgreSQL and NATS, the fake CISP and authority), the monitor process
// against the same NATS, and api's conformance recorder in process;
// tracks are published on trk.v1 as telemetry-ingest publishes them.
type confRig struct {
	*intentRig
	nc      *bus.Conn
	pub     *bus.Publisher
	monitor string
	mu      sync.Mutex
	alerts  []recvAlert
	states  []conformance.StateBody
}

type recvAlert struct {
	at time.Time
	b  conformance.AlertBody
}

func newConfRig(t *testing.T) *confRig {
	t.Helper()
	g := &confRig{intentRig: newIntentRig(t), nc: busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)}
	g.pub = bus.NewPublisher(g.nc, &core.Counters{})
	g.monitor = "http://" + runAt(t, monitor.SpecWith(monitor.Options{}), map[string]string{
		"USSP_MONITOR_ADDR": "127.0.0.1:0", "USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
	})
	for subj, fn := range map[string]func(string, []byte){
		"alrt.v1.>": func(_ string, data []byte) {
			var a conformance.AlertMessage
			if json.Unmarshal(data, &a) == nil {
				g.mu.Lock()
				g.alerts = append(g.alerts, recvAlert{time.Now(), a.Body})
				g.mu.Unlock()
			}
		},
		"conf.v1.>": func(_ string, data []byte) {
			if s, err := conformance.DecodeState(data); err == nil {
				g.mu.Lock()
				g.states = append(g.states, s.Body)
				g.mu.Unlock()
			}
		},
	} {
		stop, err := g.nc.Listen(subj, fn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
	}
	return g
}

// recorder runs api's conformance recorder for one flight's subject.
func (g *confRig) recorder(flightID string) {
	g.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g.t.Cleanup(cancel)
	r := &conformance.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(g.nc.JetStream(), bus.DefaultTopology(), bus.StreamCONF, bus.PullSpec{
			Durable: "it-conformance-" + unique(), FilterSubject: "conf.v1." + flightID, MaxAckPending: 64,
		})},
		Store: confstore.Store{S: appStore(g.t)}, Intents: g.svc, Logger: quiet(),
	}
	go r.Run(ctx)
}

// policy is the policy the monitor follows (the policy bucket, which
// another test may have changed), or the defaults when there is none.
func (g *confRig) policy() policy.Values {
	g.t.Helper()
	kv, err := g.nc.JetStream().KeyValue(context.Background(), bus.BucketPolicy)
	if err != nil {
		return policy.Defaults()
	}
	e, err := kv.Get(context.Background(), bus.KeyPolicy)
	if err != nil {
		return policy.Defaults()
	}
	r, err := telemetry.DecodePolicy(e.Value())
	if err != nil {
		g.t.Fatal(err)
	}
	return r.Values
}

func (g *confRig) alertsOf(kind, state, flightID string) []recvAlert {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []recvAlert
	for _, a := range g.alerts {
		if a.b.Kind == kind && a.b.State == state && a.b.FlightID == flightID {
			out = append(out, a)
		}
	}
	return out
}

func (g *confRig) lastState(flightID string) (conformance.StateBody, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.states) - 1; i >= 0; i-- {
		if g.states[i].FlightID == flightID {
			return g.states[i], true
		}
	}
	return conformance.StateBody{}, false
}

// flyer publishes one flight's track every second at the position it
// holds, until stopped; it records the captured_at of each sample.
type flyer struct {
	g        *confRig
	id       string
	intentID *string
	altM     *float64
	altSrc   core.AltSource
	pos      atomic.Pointer[core.LatLon]
	last     atomic.Int64 // captured_at of the last sample, unix ns
	stop     chan struct{}
	stopped  sync.Once
	mu       sync.Mutex // one send or move at a time
	seq      atomic.Int64
}

func (g *confRig) fly(id string, intentID *string, p core.LatLon, altM *float64, src core.AltSource) *flyer {
	f := &flyer{g: g, id: id, intentID: intentID, altM: altM, altSrc: src, stop: make(chan struct{})}
	f.pos.Store(&p)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			f.send()
			select {
			case <-f.stop:
				return
			case <-t.C:
			}
		}
	}()
	g.t.Cleanup(f.halt)
	return f
}

func (f *flyer) halt() { f.stopped.Do(func() { close(f.stop) }) }

// move sets the next samples' position and returns the captured_at of
// the last sample sent at the old one.
func (f *flyer) move(p core.LatLon) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pos.Store(&p)
	return time.Unix(0, f.last.Load())
}

func (f *flyer) send() {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	p := *f.pos.Load()
	c5, _, err := cell.Key(p)
	if err != nil {
		return
	}
	status := string(f3411.Airborne)
	id := f.id
	m := &telemetry.Track{
		Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer, core.Times{TS: &now, RxTS: now, CapturedAt: now, Source: core.TimeSourceClock}),
		Body: telemetry.TrackBody{
			TrackID: id, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, SourceInstance: "wp10-client",
			Position: telemetry.Position{Lat: p.LatDeg, Lng: p.LonDeg}, AltAMSLM: f.altM, AltSource: f.altSrc,
			Status: &status, FlightID: &id, IntentID: f.intentID, Seq: f.seq.Add(1),
		},
	}
	subject, err := bus.Trk(c5, id)
	if err != nil {
		return
	}
	if err := f.g.pub.Publish(context.Background(), subject, m); err == nil {
		f.last.Store(now.UnixNano())
	}
}

// S-M2 conformance on the real bus and databases (brief WP-10 done-when,
// integration): an activated intent; the aircraft leaves its volume and
// the nonconformance is raised within 2 s of the sample's captured_at
// (measured and printed), conf.v1 carries it and the intent goes
// nonconforming; a flight 500 m away gets nonconformance_nearby and one
// 5 km away nothing (E-01); back inside, it clears resolved after the
// 3 s hysteresis with its numbers and the intent is activated again; out
// for 60 s it goes contingent; 15 s of silence raises lost_link.
func TestIntegrationConformanceLifecycle(t *testing.T) {
	g := newConfRig(t)
	pv := g.policy()
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	now := time.Now().UTC().Truncate(time.Second)
	box := g.box(0, 0, 0.01)
	req := g.request(number, serial, "conf-"+unique(), box)
	req["volumes"] = []any{g.volume(box, 120, 170, now.Add(-time.Minute), now.Add(30*time.Minute))}
	d := g.file(token, req)
	if d.status != 201 || d.str("decision") != "authorised" {
		t.Fatalf("%d %s", d.status, d.raw)
	}
	intentID := d.str("intent_id")
	a := g.stack.call("PATCH", "/v1/intents/"+intentID, map[string]any{"action": "activate"}, bearer(token))
	if a.status != 200 || a.str("state") != "activated" {
		t.Fatalf("activate: %d %s", a.status, a.raw)
	}
	band := d.body["volumes_amsl"].([]any)[0].(map[string]any)
	midM := (band["lower_amsl_m"].(float64) + band["upper_amsl_m"].(float64)) / 2
	centre := core.LatLon{LatDeg: box[0] + 0.005, LonDeg: box[1] + 0.005}
	eastEdge := core.LatLon{LatDeg: box[0] + 0.005, LonDeg: box[3]}
	out := geodesy.Destination(eastEdge, 90, 100) // beyond h_m 50: threshold_exceeded at once
	flightA, flightB, flightC := newUUID(), newUUID(), newUUID()
	if err := (flightstore.Store{S: appStore(t)}).Record(context.Background(), flights.Body{
		FlightID: flightA, Event: flights.EventStarted, At: bus.Stamp{Time: now}, StartedAt: bus.Stamp{Time: now},
		ClientID: "wp10-client", UASSerial: serial, IntentID: &intentID,
	}); err != nil {
		t.Fatal(err)
	}
	g.recorder(flightA)
	localState := func() string {
		r, err := g.svc.Store.Get(context.Background(), intentID)
		if err != nil || r == nil {
			return ""
		}
		return r.LocalState
	}

	fa := g.fly(flightA, &intentID, centre, &midM, core.AltGeodetic)
	within(t, 10*time.Second, func() bool {
		s, ok := g.lastState(flightA)
		return ok && s.State == conformance.StateConforming && s.Judged
	})
	g.fly(flightB, nil, geodesy.Destination(out, 0, 500), &midM, core.AltGeodetic)
	g.fly(flightC, nil, geodesy.Destination(out, 0, 5000), &midM, core.AltGeodetic)
	time.Sleep(1500 * time.Millisecond)

	fa.move(out)
	within(t, 5*time.Second, func() bool { return len(g.alertsOf("nonconformance", "raised", flightA)) == 1 })
	raised := g.alertsOf("nonconformance", "raised", flightA)[0]
	latency := raised.at.Sub(raised.b.CapturedAt)
	t.Logf("nonconformance raised %v after the triggering sample's captured_at (budget 2 s, spec 05 §7)", latency)
	if latency > 2*time.Second || latency < 0 {
		t.Fatalf("alert latency %v", latency)
	}
	if raised.b.Detail["reason"] != conformance.ReasonThresholdExceeded || raised.b.Severity != core.SeverityCritical {
		t.Fatalf("%+v", raised.b)
	}
	within(t, 5*time.Second, func() bool { return localState() == "nonconforming" })
	within(t, 5*time.Second, func() bool { return len(g.alertsOf("nonconformance_nearby", "raised", flightB)) == 1 })
	time.Sleep(2 * time.Second)
	if n := g.alertsOf("nonconformance_nearby", "raised", flightC); len(n) != 0 {
		t.Fatalf("a flight 5 km away received %+v", n)
	}

	back := fa.move(centre) // the last sample outside
	within(t, 10*time.Second, func() bool { return len(g.alertsOf("nonconformance", "cleared", flightA)) == 1 })
	cl := g.alertsOf("nonconformance", "cleared", flightA)[0]
	if *cl.b.ClearReason != conformance.ClearResolved || cl.b.ClearingDetail == nil || cl.b.Detail["distance_outside_m"] == nil {
		t.Fatalf("%+v", cl.b)
	}
	if held := cl.at.Sub(back); held < secs(pv.ConformanceClearAfterS) {
		t.Fatalf("cleared %v after the last sample outside, inside the %v s hysteresis", held, pv.ConformanceClearAfterS)
	}
	t.Logf("cleared resolved %v after the last sample outside, distance_outside_m %v", cl.at.Sub(back), cl.b.Detail["distance_outside_m"])
	within(t, 5*time.Second, func() bool {
		c := g.alertsOf("nonconformance_nearby", "cleared", flightB)
		return len(c) == 1 && *c[0].b.ClearReason == conformance.ClearResolved
	})
	within(t, 5*time.Second, func() bool { return localState() == "activated" })

	left := time.Now()
	fa.move(out)
	within(t, 75*time.Second, func() bool {
		s, ok := g.lastState(flightA)
		return ok && s.State == conformance.StateContingent
	})
	t.Logf("contingent %v after leaving (F3548 60 s)", time.Since(left))
	if time.Since(left) < 59*time.Second {
		t.Fatal("contingent before 60 s")
	}
	within(t, 5*time.Second, func() bool { return localState() == "contingent" })

	fa.halt()
	within(t, secs(pv.LostLinkS)+5*time.Second, func() bool { return len(g.alertsOf("lost_link", "raised", flightA)) == 1 })
	ll := g.alertsOf("lost_link", "raised", flightA)[0]
	silence := ll.at.Sub(time.Unix(0, fa.last.Load()))
	t.Logf("lost_link raised %v after the last sample's captured_at (lost_link_s %v)", silence, pv.LostLinkS)
	if silence < secs(pv.LostLinkS) || silence > secs(pv.LostLinkS)+2*time.Second {
		t.Fatalf("lost_link after %v of silence", silence)
	}
	within(t, 5*time.Second, func() bool {
		return count(t, relOwner(t), "SELECT count(*) FROM conformance_states WHERE flight_id = $1 AND state = 'contingent'", flightA) >= 1
	})
	n := count(t, relOwner(t), "SELECT count(*) FROM conformance_states WHERE flight_id = $1", flightA)
	t.Logf("conformance_states rows for the flight: %d", n)
	if n < 4 {
		t.Fatalf("%d timeline rows", n)
	}
}

// E-02, SC-22: a flight whose intent is not in intent_active is unknown
// and says why, and judged within one sample once the entry appears; a
// track without a vertical position is judged horizontally with
// vertical_known false on its state; /readyz names intent_active.
func TestIntegrationConformanceMissingInputs(t *testing.T) {
	g := newConfRig(t)
	intentID := newUUID()
	flight := newUUID()
	p := origin(t)
	f := g.fly(flight, &intentID, p, nil, core.AltNone)
	within(t, 10*time.Second, func() bool {
		s, ok := g.lastState(flight)
		return ok && s.State == conformance.StateUnknown && s.Reason != nil && *s.Reason == conformance.UnknownAuthorisationMissing
	})
	body := map[string]any{
		"intent_id": intentID, "version": 2, "local_state": "activated", "volumes_amsl": []any{
			map[string]any{"lower_amsl_m": 0, "upper_amsl_m": 500, "undulation_m": 0, "lower_w84_m": 0, "upper_w84_m": 500}},
		"deviation_thresholds": map[string]any{"h_m": 50, "v_m": 15, "t_s": 60},
		"volumes": []any{map[string]any{
			"volume": map[string]any{
				"outline_circle": map[string]any{"center": map[string]any{"lat": p.LatDeg, "lng": p.LonDeg}, "radius": map[string]any{"value": 300, "units": "M"}},
				"altitude_lower": map[string]any{"value": 0, "reference": "W84", "units": "M"},
				"altitude_upper": map[string]any{"value": 500, "reference": "W84", "units": "M"},
			},
			"time_start": map[string]any{"value": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), "format": "RFC3339"},
			"time_end":   map[string]any{"value": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "format": "RFC3339"},
		}},
		"cell_set": []string{}, "time_start": time.Now().UTC().Format(time.RFC3339), "time_end": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := g.kv.PutJSON(context.Background(), bus.BucketIntentActive, intentID, body); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.kv.Delete(context.Background(), bus.BucketIntentActive, intentID) })
	put := time.Now()
	within(t, 3*time.Second, func() bool {
		s, ok := g.lastState(flight)
		return ok && s.State == conformance.StateConforming && s.VerticalKnown != nil && !*s.VerticalKnown && s.HeightOverM == nil
	})
	t.Logf("judged %v after the intent_active entry appeared (the next sample)", time.Since(put))
	f.halt()
	resp, err := http.Get(g.monitor + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(raw), `"intent_active"`) || !strings.Contains(string(raw), `"trk"`) {
		t.Fatalf("readyz: %s", raw)
	}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
