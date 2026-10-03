//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/serial"
	coresources "github.com/rootxkit/uspace-core/sources"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/alerts"
	alertstore "github.com/rootxkit/uspace-ussp/internal/alerts/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/app/monitor"
	"github.com/rootxkit/uspace-ussp/internal/app/trafficws"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	flightstore "github.com/rootxkit/uspace-ussp/internal/flights/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// trafficRig is WP-11 on the real bus and databases: the monitor process
// (its CPA path), the traffic-ws process, api's alerts record and
// service in process, two operators with an intent each in
// intent_active and their serials bound in client_bindings, and
// flights recorded in the flights table. Tracks are published on trk.v1
// and man.v1 as the ingest and the manned adapter publish them; every
// WebSocket frame read is validated against the envelope and its schema.
type trafficRig struct {
	t        *testing.T
	nc       *bus.Conn
	pub      *bus.Publisher
	kv       *bus.Projector
	own      *ownTokens
	ws       string
	monitor  string
	stopMon  func()
	svc      *alerts.Service
	ss       map[string]*jsonschema.Schema
	o        core.LatLon
	ops      [2]trafficOperator
	mu       sync.Mutex
	alerts   []recvProx
	recorder context.CancelFunc
	// db is the one owner pool of the rig's queries (a pool per poll
	// would exhaust the server's connections).
	db  *store.Pool
	app *store.Store
}

type trafficOperator struct {
	client, serial, intent, flight, token string
}

type recvProx struct {
	at time.Time
	b  traffic.AlertBody
}

func newTrafficRig(t *testing.T) *trafficRig {
	t.Helper()
	ensureSchemas(t)
	g := &trafficRig{t: t, nc: busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true), own: newOwnTokens(t), ss: compileSchemas(t),
		db: relOwner(t), app: appStore(t)}
	if _, err := bus.Ensure(context.Background(), g.nc.JetStream(), bus.DefaultTopology()); err != nil {
		t.Fatal(err)
	}
	g.pub = bus.NewPublisher(g.nc, &core.Counters{})
	g.kv = bus.NewProjector(g.nc, nil)
	// A place of its own: the centre of a cell5 no other rig uses (10 km
	// from any other), so no other test's tracks pair with these, and the
	// alerts of this rig are the alrt.v1 messages of that cell.
	g.o = core.LatLon{LatDeg: 40.05 + float64(seq.Add(1)%30)*0.1, LonDeg: 44.95}
	c5, _, err := cell.Key(g.o)
	if err != nil {
		t.Fatal(err)
	}
	// Two operators with a client each (accounts in the database, for the
	// acknowledgement's ownership), a serial bound, an intent and a flight.
	st := newStack(t, newClock(), &logBuffer{})
	for i := range g.ops {
		opID, session, _ := st.operator(accounts.RegistryValid)
		clientID, _ := st.client(opID, session, auth.ScopeTraffic)
		op := trafficOperator{client: clientID, serial: "TEST-WP11-" + unique(), intent: newUUID(), flight: newUUID()}
		op.token = g.trafficToken(clientID)
		g.ops[i] = op
		g.putIntent(op)
		if err := g.kv.PutJSON(context.Background(), bus.BucketClientBindings, bus.KeyToken(clientID), []string{serial.FoldKey(op.serial)}); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := (flightstore.Store{S: g.app}).Record(context.Background(), flights.Body{
			FlightID: op.flight, Event: flights.EventStarted, At: bus.Stamp{Time: now}, StartedAt: bus.Stamp{Time: now},
			ClientID: clientID, UASSerial: op.serial,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// api's alerts record and service, in process.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g.recorder = cancel
	ast := alertstore.Store{S: g.app}
	rec := &alerts.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(g.nc.JetStream(), bus.DefaultTopology(), bus.StreamALRT, bus.PullSpec{
			Durable: "it-alerts-" + unique(), FilterSubject: "alrt.v1.*." + c5 + ".*", MaxAckPending: 1024,
		})},
		Store: ast, Logger: quiet(),
	}
	go rec.Run(ctx)
	g.svc = &alerts.Service{Store: ast, Bus: g.pub, Policy: func() policy.Values { return g.policy() }, Logger: quiet()}
	// Every proximity alert on the bus.
	stop, err := g.nc.Listen("alrt.v1.proximity."+c5+".*", func(_ string, data []byte) {
		var m traffic.AlertMessage
		if json.Unmarshal(data, &m) == nil {
			g.mu.Lock()
			g.alerts = append(g.alerts, recvProx{time.Now(), m.Body})
			g.mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	g.startMonitor()
	addr, _ := runStoppable(t, trafficws.SpecWith(trafficws.Options{}), map[string]string{
		"USSP_TRAFFIC_WS_ADDR": "127.0.0.1:0", "USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_AUDIENCES": testHost, "USSP_TOKEN_ISSUERS": testIssuer + "=" + g.own.jwks, "USSP_ISSUER_URL": testIssuer,
		"USSP_WS_ALLOWED_ORIGINS": "https://console.test",
	})
	g.ws = "ws://" + addr
	// Subscriptions are answered once the projections are read (503
	// before, which a client retries).
	within(t, 20*time.Second, func() bool {
		resp, err := http.Get("http://" + addr + "/readyz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var r struct {
			Dependencies map[string]struct {
				State string `json:"state"`
			} `json:"dependencies"`
		}
		if json.NewDecoder(resp.Body).Decode(&r) != nil {
			return false
		}
		return r.Dependencies["intent_active"].State == "up" && r.Dependencies["client_bindings"].State == "up" &&
			r.Dependencies["alerts"].State == "up"
	})
	return g
}

func (g *trafficRig) startMonitor() { g.startMonitorWith(monitor.Options{}) }

// startMonitorAs starts the monitor under another instance id, as a
// recreated container with a new hostname.
func (g *trafficRig) startMonitorAs(instance string) {
	g.startMonitorWith(monitor.Options{CPA: func(e *traffic.Engine) { e.InstanceID = instance }})
}

func (g *trafficRig) startMonitorWith(o monitor.Options) {
	g.t.Helper()
	addr, stop := runStoppable(g.t, monitor.SpecWith(o), map[string]string{
		"USSP_MONITOR_ADDR": "127.0.0.1:0", "USSP_NATS_URL": mustEnv(g.t, "USSP_TEST_NATS_URL"),
	})
	g.monitor, g.stopMon = "http://"+addr, stop
}

func (g *trafficRig) trafficToken(clientID string) string {
	g.t.Helper()
	tok, err := g.own.iss.Issue(clientID, testHost, []string{auth.ScopeTraffic}, time.Hour, time.Now())
	if err != nil {
		g.t.Fatal(err)
	}
	return tok
}

func (g *trafficRig) staffToken() string {
	g.t.Helper()
	now := time.Now().Truncate(time.Second)
	sub, jti := newUUID(), newUUID()
	tok, err := g.own.iss.IssueSession(coreauth.SessionClaims{Subject: sub, Audience: testHost, Realm: auth.RealmConsole,
		Roles: []string{auth.RoleSupervisor}, JTI: jti, IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		g.t.Fatal(err)
	}
	// As api does when the session starts: traffic-ws admits a session
	// only while sessions_live holds it (audit B2).
	if err := g.kv.PutJSON(context.Background(), bus.BucketSessionsLive, bus.KeyToken(jti), auth.LiveSession{Subject: sub,
		Realm: auth.RealmConsole, ExpiresAt: now.Add(time.Hour), IdleUntil: now.Add(30 * time.Minute)}); err != nil {
		g.t.Fatal(err)
	}
	return tok
}

// putIntent writes the operator's intent to intent_active: a 3 km
// circle around the rig's place, activated, with its flight.
func (g *trafficRig) putIntent(op trafficOperator) {
	g.t.Helper()
	now := time.Now().UTC()
	body := map[string]any{
		"intent_id": op.intent, "version": 2, "local_state": "activated", "uas_serial": op.serial, "operator_reg": "GEO-TEST-WP11",
		"authorisation_number": "USSP-DEV-WP11-" + op.intent[:8], "flight_id": op.flight,
		"volumes_amsl":         []any{map[string]any{"lower_amsl_m": 0, "upper_amsl_m": 2000, "undulation_m": 0, "lower_w84_m": 0, "upper_w84_m": 2000}},
		"deviation_thresholds": map[string]any{"h_m": 50, "v_m": 15, "t_s": 60},
		"volumes": []any{map[string]any{
			"volume": map[string]any{
				"outline_circle": map[string]any{"center": map[string]any{"lat": g.o.LatDeg, "lng": g.o.LonDeg}, "radius": map[string]any{"value": 3000, "units": "M"}},
				"altitude_lower": map[string]any{"value": 0, "reference": "W84", "units": "M"},
				"altitude_upper": map[string]any{"value": 2000, "reference": "W84", "units": "M"},
			},
			"time_start": map[string]any{"value": now.Add(-time.Minute).Format(time.RFC3339), "format": "RFC3339"},
			"time_end":   map[string]any{"value": now.Add(time.Hour).Format(time.RFC3339), "format": "RFC3339"},
		}},
		"cell_set": []string{}, "time_start": now.Format(time.RFC3339), "time_end": now.Add(time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	}
	if err := g.kv.PutJSON(context.Background(), bus.BucketIntentActive, op.intent, body); err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = g.kv.Delete(context.Background(), bus.BucketIntentActive, op.intent) })
}

func (g *trafficRig) policy() policy.Values {
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
		return policy.Defaults()
	}
	return r.Values
}

func (g *trafficRig) proximity(flightID, state string) []recvProx {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []recvProx
	for _, a := range g.alerts {
		if a.b.FlightID == flightID && a.b.State == state {
			out = append(out, a)
		}
	}
	return out
}

// aircraft publishes one flight's track every second at the position
// path gives for the seconds since it started, with the velocity given;
// it records the captured_at of every sample.
type aircraft struct {
	g       *trafficRig
	op      trafficOperator
	path    func(s float64) (core.LatLon, float64, float64)
	stop    chan struct{}
	once    sync.Once
	last    atomic.Int64
	started time.Time
	seqN    atomic.Int64
}

func (g *trafficRig) fly(op trafficOperator, path func(s float64) (p core.LatLon, speedMS, trackDeg float64)) *aircraft {
	a := &aircraft{g: g, op: op, path: path, stop: make(chan struct{}), started: time.Now()}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			a.send()
			select {
			case <-a.stop:
				return
			case <-t.C:
			}
		}
	}()
	g.t.Cleanup(a.halt)
	return a
}

func (a *aircraft) halt() { a.once.Do(func() { close(a.stop) }) }

func (a *aircraft) send() {
	now := time.Now()
	p, sp, td := a.path(now.Sub(a.started).Seconds())
	c5, _, err := cell.Key(p)
	if err != nil {
		return
	}
	alt, vs := 600.0, 0.0
	status := string(f3411.Airborne)
	id, intentID := a.op.flight, a.op.intent
	m := &telemetry.Track{
		Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer, core.Times{TS: &now, RxTS: now, CapturedAt: now, Source: core.TimeSourceClock}),
		Body: telemetry.TrackBody{
			TrackID: id, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, SourceInstance: a.op.client,
			Position: telemetry.Position{Lat: p.LatDeg, Lng: p.LonDeg}, AltAMSLM: &alt, AltSource: core.AltGeodetic,
			SpeedMS: &sp, TrackDeg: &td, VSpeedMS: &vs, Status: &status, FlightID: &id, IntentID: &intentID, Seq: a.seqN.Add(1),
			Identification: core.Identification{Status: core.IdentRegistered, Reason: core.ReasonSessionBinding, Basis: core.BasisAuthenticated},
		},
	}
	subject, err := bus.Trk(c5, id)
	if err != nil {
		return
	}
	if err := a.g.pub.Publish(context.Background(), subject, m); err == nil {
		a.last.Store(now.UnixNano())
	}
}

// manned publishes one manned aircraft every second along path.
func (g *trafficRig) manned(icao string, trust core.Trust, source string, path func(s float64) core.LatLon) func() {
	stop := make(chan struct{})
	start := time.Now()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			now := time.Now()
			p := path(now.Sub(start).Seconds())
			c5, _, err := cell.Key(p)
			if err == nil {
				gs, td, alt := 60.0, 90.0, 650.0
				m := &traffic.MannedTrack{
					Envelope: bus.NewEnvelope(traffic.SchemaManned, "ansp/manned-feed", core.Times{RxTS: now, CapturedAt: now, Source: core.TimeReceiver}),
					Body: traffic.MannedBody{ICAO24: icao, Position: traffic.MannedPosition{Lat: p.LatDeg, Lng: p.LonDeg}, AltPressureM: &alt,
						GSMS: &gs, TrackDeg: &td, SourceClass: "ads_b", Trust: trust, Source: source, SourceInstance: "adsb-wp11", State: traffic.StateLive},
				}
				if subject, err := bus.Man(c5, icao); err == nil {
					_ = g.pub.Publish(context.Background(), subject, m)
				}
			}
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	g.t.Cleanup(halt)
	return halt
}

// wsFrame is one frame read from a socket.
type wsFrame struct {
	Schema string          `json:"schema"`
	Body   json.RawMessage `json:"body"`
	at     time.Time
}

// socket keeps every frame of one connection, each validated.
type socket struct {
	g      *trafficRig
	c      *websocket.Conn
	mu     sync.Mutex
	frames []wsFrame
	errs   []string
}

func (g *trafficRig) dial(path, token string, staff bool) *socket {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	if staff {
		h.Set("Origin", "https://console.test")
		h.Set("Cookie", auth.CookieSession+"="+token)
	} else {
		h.Set("Authorization", "Bearer "+token)
	}
	c, _, err := websocket.Dial(ctx, g.ws+path, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		g.t.Fatal(err)
	}
	c.SetReadLimit(16 << 20)
	s := &socket{g: g, c: c}
	go func() {
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var f wsFrame
			_ = json.Unmarshal(data, &f)
			f.at = time.Now()
			bad := validateFrameRaw(g.ss, f.Schema, data)
			s.mu.Lock()
			s.frames = append(s.frames, f)
			if bad != "" {
				s.errs = append(s.errs, bad)
			}
			s.mu.Unlock()
		}
	}()
	g.t.Cleanup(func() {
		_ = c.Close(websocket.StatusNormalClosure, "")
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, e := range s.errs {
			g.t.Errorf("a frame does not validate: %s", e)
		}
		g.t.Logf("%s: %d frames, every one validated against envelope/v1 and its schema", path, len(s.frames))
	})
	return s
}

func (s *socket) of(schema string) []wsFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []wsFrame
	for _, f := range s.frames {
		if f.Schema == schema {
			out = append(out, f)
		}
	}
	return out
}

func (s *socket) lastProduct() (traffic.ProductBody, bool) {
	ps := s.of(traffic.SchemaProduct)
	if len(ps) == 0 {
		return traffic.ProductBody{}, false
	}
	var b traffic.ProductBody
	_ = json.Unmarshal(ps[len(ps)-1].Body, &b)
	return b, true
}

func compileSchemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	root := filepath.Join("..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "schema.json" {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		names = append(names, filepath.ToSlash(rel))
		return c.AddResource("https://schemas.uspace.ge/"+filepath.ToSlash(rel)+".json", doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*jsonschema.Schema{}
	for _, n := range names {
		s, err := c.Compile("https://schemas.uspace.ge/" + n + ".json")
		if err != nil {
			t.Fatal(err)
		}
		out[n] = s
	}
	return out
}

func validateFrameRaw(ss map[string]*jsonschema.Schema, schema string, data []byte) string {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err.Error()
	}
	if err := ss["envelope/v1"].Validate(inst); err != nil {
		return "envelope: " + err.Error()
	}
	s, ok := ss[schema]
	if !ok {
		return "unknown schema " + schema
	}
	if err := s.Validate(inst); err != nil {
		return schema + ": " + err.Error()
	}
	return ""
}

func peerOf(b traffic.AlertBody) map[string]any {
	raw, _ := json.Marshal(b.Detail["peer"])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// Head-on at 10 m/s from 600 m (SC-02 step 1-2 in unit form, brief
// WP-11): the proximity alert is raised for both flights within 2 s of
// the triggering sample, t_cpa under 60 s, naming the other as
// authenticated; both operators' alert streams receive it; after the
// pass it clears resolved at least the hysteresis after the last sample
// in conflict, with the final numbers and the clearing separation. The
// alerts record holds one row per flight.
func TestIntegrationProximityHeadOn(t *testing.T) {
	g := newTrafficRig(t)
	pv := g.policy()
	a, b := g.ops[0], g.ops[1]
	sa := g.dial("/v1/alerts?intent_id="+a.intent, a.token, false)
	sb := g.dial("/v1/alerts?intent_id="+b.intent, b.token, false)
	along := func(bearing float64) func(s float64) (core.LatLon, float64, float64) {
		start := geodesy.Destination(g.o, bearing, 300)
		return func(s float64) (core.LatLon, float64, float64) {
			return geodesy.Destination(start, bearing+180, 10*s), 10, float64(int(bearing+180) % 360)
		}
	}
	fa := g.fly(a, along(180))
	fb := g.fly(b, along(0))
	within(t, 10*time.Second, func() bool {
		return len(g.proximity(a.flight, "raised")) == 1 && len(g.proximity(b.flight, "raised")) == 1
	})
	ra := g.proximity(a.flight, "raised")[0]
	latency := ra.at.Sub(ra.b.CapturedAt.Time)
	tcpa := ra.b.Detail["t_cpa_s"].(float64)
	t.Logf("proximity raised %v after the triggering sample's captured_at, t_cpa_s %.1f, d_cpa_h_m %.1f, d_horizontal_now_m %.1f",
		latency, tcpa, ra.b.Detail["d_cpa_h_m"], ra.b.Detail["d_horizontal_now_m"])
	if latency > 2*time.Second || latency < 0 || tcpa >= 60 {
		t.Fatalf("latency %v t_cpa %v", latency, tcpa)
	}
	if p := peerOf(ra.b); p["track_id"] != b.flight || p["trust"] != string(core.TrustAuthenticated) {
		t.Fatalf("peer %v", p)
	}
	within(t, 5*time.Second, func() bool {
		return len(sa.of(alerts.SchemaAlert)) > 0 && len(sb.of(alerts.SchemaAlert)) > 0
	})
	var ab traffic.AlertBody
	_ = json.Unmarshal(sb.of(alerts.SchemaAlert)[0].Body, &ab)
	if p := peerOf(ab); ab.FlightID != b.flight || p["track_id"] != a.flight || p["trust"] != string(core.TrustAuthenticated) {
		t.Fatalf("operator B received %+v", ab)
	}
	within(t, 60*time.Second, func() bool { return len(g.proximity(a.flight, "cleared")) == 1 })
	cl := g.proximity(a.flight, "cleared")[0]
	lastTrue := cl.b.CapturedAt.Time
	held := cl.b.UpdatedAt.Sub(lastTrue)
	t.Logf("cleared %s %v after the last sample in conflict; final d_cpa_h_m %v d_horizontal_now_m %v; clearing %v",
		*cl.b.ClearReason, held, cl.b.Detail["d_cpa_h_m"], cl.b.Detail["d_horizontal_now_m"], cl.b.ClearingDetail)
	if *cl.b.ClearReason != "resolved" || held < secs(pv.CPAClearAfterS) || cl.b.ClearingDetail == nil || cl.b.AlertID != ra.b.AlertID {
		t.Fatalf("%+v", cl.b)
	}
	fa.halt()
	fb.halt()
	within(t, 10*time.Second, func() bool {
		return count(t, g.db, "SELECT count(*) FROM alerts WHERE flight_id = $1 AND kind = 'proximity' AND state = 'cleared' AND clear_reason = 'resolved'", a.flight) == 1 &&
			count(t, g.db, "SELECT count(*) FROM alerts WHERE flight_id = $1 AND kind = 'proximity' AND peer_ref = $2", a.flight, b.flight) == 1
	})
	if n := len(g.proximity(a.flight, "raised")); n != 1 {
		t.Fatalf("%d raises", n)
	}
}

// SC-01 in unit form (C-03): two flights hovering 25 m apart are raised
// and never cleared for 30 s. E-01 twin: two flights 2 km apart and
// diverging raise nothing.
func TestIntegrationProximityHoverAndDiverging(t *testing.T) {
	g := newTrafficRig(t)
	a, b := g.ops[0], g.ops[1]
	still := func(p core.LatLon) func(float64) (core.LatLon, float64, float64) {
		return func(float64) (core.LatLon, float64, float64) { return p, 0.03, 90 }
	}
	g.fly(a, still(g.o))
	g.fly(b, still(geodesy.Destination(g.o, 90, 25)))
	within(t, 10*time.Second, func() bool { return len(g.proximity(a.flight, "raised")) == 1 })
	raised := time.Now()
	for time.Since(raised) < 30*time.Second {
		if c := g.proximity(a.flight, "cleared"); len(c) != 0 {
			t.Fatalf("cleared after %v while 25 m apart: %+v", time.Since(raised), c[0].b)
		}
		time.Sleep(time.Second) // the absence of a clear is observed over the 30 s
	}
	t.Logf("held %v while hovering 25 m apart, %d updates", time.Since(raised), len(g.proximity(a.flight, "updated")))

	h := newTrafficRig(t)
	c, d := h.ops[0], h.ops[1]
	away := func(bearing float64) func(float64) (core.LatLon, float64, float64) {
		return func(s float64) (core.LatLon, float64, float64) {
			return geodesy.Destination(h.o, bearing, 1000+10*s), 10, bearing
		}
	}
	h.fly(c, away(0))
	h.fly(d, away(180))
	// Both flights are heard (the monitor tracks them), and for six
	// seconds nothing is raised.
	within(t, 10*time.Second, func() bool { return metric(t, h.monitor, "traffic_samples") >= 4 })
	time.Sleep(6 * time.Second) // the absence of a raise is observed over 6 s
	if n := len(h.proximity(c.flight, "raised")) + len(h.proximity(d.flight, "raised")); n != 0 {
		for _, r := range append(h.proximity(c.flight, "raised"), h.proximity(d.flight, "raised")...) {
			t.Logf("raised %+v", r.b)
		}
		t.Fatalf("2 km apart and diverging raised %d", n)
	}
}

// A manned aircraft on man.v1 crossing a flight raises proximity with
// peer.trust surveillance; an e-conspicuity track (broadcast) is shown
// as broadcast in the product (R-05); manned degraded both ways.
func TestIntegrationProximityMannedAndDegraded(t *testing.T) {
	g := newTrafficRig(t)
	a := g.ops[0]
	s := g.dial("/v1/traffic?intent_id="+a.intent, a.token, false)
	hasManned := func() bool {
		p, ok := s.lastProduct()
		if !ok {
			return false
		}
		for _, d := range p.Degraded {
			if d.Input == "manned" {
				return true
			}
		}
		return false
	}
	within(t, 10*time.Second, func() bool { _, ok := s.lastProduct(); return ok && hasManned() })
	g.fly(a, func(float64) (core.LatLon, float64, float64) { return g.o, 0, 0 })
	icao := "a" + unique()[len(unique())-5:]
	stop := g.manned(icao, core.TrustSurveillance, traffic.SourceANSPFeed, func(s float64) core.LatLon {
		return geodesy.Destination(g.o, 270, 600-60*s)
	})
	within(t, 10*time.Second, func() bool {
		for _, r := range g.proximity(a.flight, "raised") {
			if p := peerOf(r.b); p["track_id"] == icao && p["trust"] == string(core.TrustSurveillance) {
				return true
			}
		}
		return false
	})
	within(t, 5*time.Second, func() bool { return !hasManned() })
	broadcast := "b" + unique()[len(unique())-5:]
	g.manned(broadcast, core.TrustBroadcast, traffic.SourceAdsbRx, func(float64) core.LatLon { return geodesy.Destination(g.o, 0, 1500) })
	within(t, 5*time.Second, func() bool {
		p, _ := s.lastProduct()
		for _, tr := range p.Tracks {
			if tr.TrackID == broadcast && tr.Trust == core.TrustBroadcast && tr.Source == traffic.SourceAdsbRx {
				return true
			}
		}
		return false
	})
	stop()
	within(t, 20*time.Second, hasManned)
}

// A source switched off by the admin: its flight's track is
// source_disabled in the product within a tick and its proximity alert
// clears source_disabled (SC-08 steps 2-3 in unit form); switched on,
// the track is live again within a second of its next sample and the
// alert is raised again.
func TestIntegrationProximitySourceSwitch(t *testing.T) {
	g := newTrafficRig(t)
	a, b := g.ops[0], g.ops[1]
	s := g.dial("/v1/traffic?intent_id="+a.intent, a.token, false)
	g.fly(a, func(float64) (core.LatLon, float64, float64) { return g.o, 0, 0 })
	g.fly(b, func(float64) (core.LatLon, float64, float64) { return geodesy.Destination(g.o, 90, 30), 0, 0 })
	within(t, 10*time.Second, func() bool { return len(g.proximity(a.flight, "raised")) == 1 })
	epoch := "wp11-" + unique()
	inst := b.client
	off := coresources.State{Epoch: epoch, Version: 1, Controls: []coresources.Control{{SourceType: telemetry.SourceOperatorWS, InstanceID: &inst, Enabled: false}}}
	if err := g.kv.ProjectSources(context.Background(), off); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.kv.ProjectSources(context.Background(), coresources.State{Epoch: "wp11-reset-" + unique(), Version: 1})
	})
	switched := time.Now()
	stateOf := func() string {
		p, _ := s.lastProduct()
		for _, tr := range p.Tracks {
			if tr.TrackID == b.flight {
				return tr.State
			}
		}
		return ""
	}
	within(t, 3*time.Second, func() bool { return stateOf() == traffic.StateSourceDisabled })
	within(t, 3*time.Second, func() bool {
		c := g.proximity(a.flight, "cleared")
		return len(c) == 1 && *c[0].b.ClearReason == "source_disabled"
	})
	t.Logf("source_disabled in the product and the alert cleared source_disabled %v after the switch", time.Since(switched))
	on := coresources.State{Epoch: epoch, Version: 2, Controls: []coresources.Control{{SourceType: telemetry.SourceOperatorWS, InstanceID: &inst, Enabled: true}}}
	if err := g.kv.ProjectSources(context.Background(), on); err != nil {
		t.Fatal(err)
	}
	within(t, 3*time.Second, func() bool { return stateOf() == traffic.StateLive })
	within(t, 5*time.Second, func() bool { return len(g.proximity(a.flight, "raised")) == 2 })
}

// Escalation (02 F5): a critical alert unacknowledged for
// escalation_after_s is escalated (escalated_at, republished, seen by a
// console subscriber); one acknowledged at 10 s is not.
func TestIntegrationAlertEscalation(t *testing.T) {
	g := newTrafficRig(t)
	pv := g.policy()
	a, b := g.ops[0], g.ops[1]
	console := g.dial("/v1/traffic", g.staffToken(), true)
	box := geodesy.BBox{MinLat: g.o.LatDeg, MaxLat: g.o.LatDeg, MinLon: g.o.LonDeg, MaxLon: g.o.LonDeg}.PadM(5000)
	sub, _ := json.Marshal(map[string]any{"schema": "console/subscribe/v1", "body": map[string]any{
		"bbox": []float64{box.MinLon, box.MinLat, box.MaxLon, box.MaxLat}, "layers": []string{"tracks", "alerts"}}})
	if err := console.c.Write(context.Background(), websocket.MessageText, sub); err != nil {
		t.Fatal(err)
	}
	g.fly(a, func(float64) (core.LatLon, float64, float64) { return g.o, 0, 0 })
	g.fly(b, func(float64) (core.LatLon, float64, float64) { return geodesy.Destination(g.o, 90, 30), 0, 0 })
	within(t, 10*time.Second, func() bool {
		return len(g.proximity(a.flight, "raised")) == 1 && len(g.proximity(b.flight, "raised")) == 1
	})
	idA, idB := g.proximity(a.flight, "raised")[0].b.AlertID, g.proximity(b.flight, "raised")[0].b.AlertID
	raised := time.Now()
	within(t, 10*time.Second, func() bool {
		return count(t, g.db, "SELECT count(*) FROM alerts WHERE id = ANY($1::uuid[])", []string{idA, idB}) == 2
	})
	time.Sleep(time.Until(raised.Add(10 * time.Second)))
	res, err := g.svc.Ack(context.Background(), idB, b.client)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("B acknowledged at %v", res.AckedAt.Sub(raised))
	if _, err := g.svc.Ack(context.Background(), idA, b.client); err == nil {
		t.Fatal("another operator acknowledged A's alert")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.svc.RunEscalation(ctx, time.Second)
	within(t, secs(pv.EscalationAfterS)+10*time.Second, func() bool {
		return count(t, g.db, "SELECT count(*) FROM alerts WHERE id = $1 AND escalated_at IS NOT NULL", idA) == 1
	})
	t.Logf("A escalated %v after its raise (escalation_after_s %v)", time.Since(raised), pv.EscalationAfterS)
	if time.Since(raised) < secs(pv.EscalationAfterS) {
		t.Fatal("escalated before escalation_after_s")
	}
	if n := count(t, g.db, "SELECT count(*) FROM alerts WHERE id = $1 AND escalated_at IS NULL AND acked_at IS NOT NULL", idB); n != 1 {
		t.Fatal("the acknowledged alert was escalated")
	}
	within(t, 5*time.Second, func() bool {
		for _, f := range console.of(alerts.SchemaAlert) {
			if strings.Contains(string(f.Body), idA) && strings.Contains(string(f.Body), "escalated_at") {
				return true
			}
		}
		return false
	})
}

// A monitor restart carries the active alert under its id (no second
// raise) and the pair is judged again after it.
func TestIntegrationProximityRestart(t *testing.T) {
	g := newTrafficRig(t)
	a, b := g.ops[0], g.ops[1]
	g.fly(a, func(float64) (core.LatLon, float64, float64) { return g.o, 0, 0 })
	g.fly(b, func(float64) (core.LatLon, float64, float64) { return geodesy.Destination(g.o, 90, 30), 0, 0 })
	within(t, 10*time.Second, func() bool { return len(g.proximity(a.flight, "raised")) == 1 })
	id := g.proximity(a.flight, "raised")[0].b.AlertID
	within(t, 5*time.Second, func() bool { return g.saved(a.flight) })
	g.stopMon()
	restarted := time.Now()
	g.startMonitor()
	within(t, 15*time.Second, func() bool {
		for _, u := range g.proximity(a.flight, "updated") {
			if u.at.After(restarted) && u.b.AlertID == id && u.b.Detail["carried_since"] == nil {
				return true
			}
		}
		return false
	})
	if n := len(g.proximity(a.flight, "raised")); n != 1 || len(g.proximity(a.flight, "cleared")) != 0 {
		t.Fatalf("%d raises and a clear across the restart", n)
	}
	t.Logf("alert %s continued across the monitor restart, judged again %v after it", id, time.Since(restarted))
}

// A rollover under a changed instance id (a rolling update, a container
// recreated with a new hostname): monitor A holds a proximity alert; B
// starts under another id while A is up, its feed holding nothing of the
// pair (both aircraft silent for longer than the live age), so B leaves
// the alert to its live owner; A stops; the aircraft fly again, apart,
// so the conflict has ended. B carries the alert under its id and clears
// it on the evidence (not_reconfirmed); nothing is left active.
func TestIntegrationProximityRolloverNewInstance(t *testing.T) {
	g := newTrafficRig(t)
	pv := g.policy()
	a, b := g.ops[0], g.ops[1]
	fa := g.fly(a, func(float64) (core.LatLon, float64, float64) { return g.o, 0, 0 })
	fb := g.fly(b, func(float64) (core.LatLon, float64, float64) { return geodesy.Destination(g.o, 90, 30), 0, 0 })
	within(t, 10*time.Second, func() bool { return len(g.proximity(a.flight, "raised")) == 1 })
	id := g.proximity(a.flight, "raised")[0].b.AlertID
	within(t, 5*time.Second, func() bool { return g.saved(a.flight) })
	fa.halt()
	fb.halt()
	silent := time.Unix(0, max(fa.last.Load(), fb.last.Load()))
	// Past the live age nothing B replays is judged; well inside the
	// stale time A still holds the alert.
	time.Sleep(time.Until(silent.Add(secs(pv.MonitorLiveMaxAgeS) + 500*time.Millisecond)))
	stopA := g.stopMon
	g.startMonitorAs("it-monitor-b-" + unique())
	if n := len(g.proximity(a.flight, "cleared")); n != 0 {
		t.Fatalf("A cleared the alert before the rollover (%v after the last sample): the run is too slow for this test", time.Since(silent))
	}
	stopA()
	stopped := time.Now()
	if since := stopped.Sub(silent); since >= secs(pv.CPAStaleAfterS) {
		t.Fatalf("A stopped %v after the last sample, past the stale time", since)
	}
	g.fly(a, func(float64) (core.LatLon, float64, float64) { return g.o, 0, 0 })
	g.fly(b, func(float64) (core.LatLon, float64, float64) { return geodesy.Destination(g.o, 90, 3000), 0, 0 })
	took := within(t, 20*time.Second, func() bool {
		for _, c := range g.proximity(a.flight, "cleared") {
			if c.at.After(stopped) && c.b.AlertID == id {
				return true
			}
		}
		return false
	})
	var carried bool
	for _, u := range g.proximity(a.flight, "updated") {
		if u.at.After(stopped) && u.b.AlertID == id && u.b.Detail["carried_since"] != nil {
			carried = true
		}
	}
	cl := g.proximity(a.flight, "cleared")
	if !carried || len(cl) != 1 || cl[0].b.ClearReason == nil || *cl[0].b.ClearReason != traffic.ClearNotReconfirmed {
		t.Fatalf("carried %v, clears %+v", carried, cl)
	}
	if n := len(g.proximity(a.flight, "raised")); n != 1 {
		t.Fatalf("%d raises", n)
	}
	within(t, 5*time.Second, func() bool { return !g.saved(a.flight) })
	t.Logf("alert %s carried by the new instance and cleared %s %v after the old one stopped", id, *cl[0].b.ClearReason, took)
}

// saved reports whether proximity_state holds an alert of flightID.
func (g *trafficRig) saved(flightID string) bool {
	kv, err := g.nc.JetStream().KeyValue(context.Background(), bus.BucketProximityState)
	if err != nil {
		return false
	}
	keys, err := kv.ListKeys(context.Background())
	if err != nil {
		return false
	}
	for k := range keys.Keys() {
		e, err := kv.Get(context.Background(), k)
		if err != nil {
			continue
		}
		if s, err := traffic.DecodeSaved(k, e.Value()); err == nil && (s.Aircraft[0].FlightID == flightID || s.Aircraft[1].FlightID == flightID) {
			return true
		}
	}
	return false
}

// metric is a counter of a process's /metrics, summed over its labels.
func metric(t *testing.T, base, name string) float64 {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	sum := 0.0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "#") || !strings.Contains(line, name) {
			continue
		}
		f := strings.Fields(line)
		var v float64
		if _, err := fmt.Sscan(f[len(f)-1], &v); err == nil {
			sum += v
		}
	}
	return sum
}
