package trafficws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/serial"
	coresources "github.com/rootxkit/uspace-core/sources"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

const (
	ownIss   = "https://ussp.test"
	clientA  = "client-a"
	clientB  = "client-b"
	intentA  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	flightA  = "11111111-1111-4111-8111-111111111111"
	flightB  = "22222222-2222-4222-8222-222222222222"
	serialA  = "TEST0001"
	alertA   = "5f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c"
	staffSub = "0d9f8e7c-6b5a-4c3d-8e2f-1a0b9c8d7e6f"
)

var origin = core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271}

type verifier map[string]coreauth.Claims

func (v verifier) Verify(_ context.Context, token string) (coreauth.Claims, error) {
	c, ok := v[token]
	if !ok {
		return coreauth.Claims{}, &coreauth.TokenError{Counter: "rejected_signature", Claim: "sig", Reason: "unknown token"}
	}
	return c, nil
}

type gate struct {
	mu  sync.Mutex
	off map[string]bool
}

func (g *gate) Query(typ string, inst *string) coresources.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := typ
	if inst != nil {
		k += "/" + *inst
	}
	if g.off[typ] || g.off[k] {
		w := coresources.WhyInstance
		return coresources.Decision{WhyDisabled: &w}
	}
	return coresources.Decision{Enabled: true}
}

func (g *gate) set(key string, off bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.off == nil {
		g.off = map[string]bool{}
	}
	g.off[key] = off
}

type pub struct {
	mu   sync.Mutex
	msgs map[string][]string
}

func (p *pub) Publish(_ context.Context, subject string, m bus.Enveloped) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.msgs == nil {
		p.msgs = map[string][]string{}
	}
	s, _ := bus.Parse(subject)
	p.msgs[s.Kind+"/"+s.Sub] = append(p.msgs[s.Kind+"/"+s.Sub], string(raw))
	return nil
}

func (p *pub) count(k string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.msgs[k])
}

type rig struct {
	t    *testing.T
	hub  *Hub
	srv  *Server
	http *httptest.Server
	gate *gate
	pub  *pub
	pv   policy.Values
	mu   sync.Mutex
}

func circle(p core.LatLon, r float32) f3548.Volume4D {
	c := f3548.Circle{Center: &f3548.LatLngPoint{Lat: f3548.Latitude(p.LatDeg), Lng: f3548.Longitude(p.LonDeg)}, Radius: &f3548.Radius{Units: "M", Value: r}}
	return f3548.Volume4D{Volume: f3548.Volume3D{OutlineCircle: &c}}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, gate: &gate{}, pub: &pub{}, pv: policy.Defaults()}
	intents := &bus.Mirror[intent.StateBody]{}
	fa := flightA
	intents.Seed(map[string]intent.StateBody{intentA: {IntentID: intentA, UASSerial: serialA, FlightID: &fa, Volumes: []f3548.Volume4D{circle(origin, 500)}}})
	bindings := &bus.Mirror[[]string]{}
	bindings.Seed(map[string][]string{bus.KeyToken(clientA): {serial.FoldKey(serialA)}, bus.KeyToken(clientB): {"OTHER"}})
	r.hub = &Hub{
		Picture: &traffic.Picture{}, Book: &traffic.Book{}, Gate: r.gate, Intents: intents, Bindings: bindings, Pub: r.pub,
		Policy: func() policy.Record { r.mu.Lock(); defer r.mu.Unlock(); return policy.Record{Version: 4, Values: r.pv} },
	}
	r.hub.Started()
	r.hub.AlertFeed(true, "")
	v := verifier{
		"op-a":  {Issuer: ownIss, Subject: clientA, Scopes: []string{auth.ScopeTraffic}, ExpiresAt: time.Now().Add(time.Hour)},
		"op-b":  {Issuer: ownIss, Subject: clientB, Scopes: []string{auth.ScopeTraffic}, ExpiresAt: time.Now().Add(time.Hour)},
		"staff": {Issuer: ownIss, Subject: staffSub, Scopes: []string{auth.SessionScope}, Realm: auth.RealmConsole, Roles: []string{auth.RoleSupervisor}, ExpiresAt: time.Now().Add(time.Hour)},
		"short": {Issuer: ownIss, Subject: clientA, Scopes: []string{auth.ScopeTraffic}, ExpiresAt: time.Now().Add(1500 * time.Millisecond)},
	}
	guard := &auth.Guard{Verifier: v, OwnIssuer: ownIss}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.srv = &Server{Hub: r.hub, WS: &auth.WSAuth{Guard: guard, AllowedOrigins: []string{"https://console.test"}}, Ctx: ctx,
		ProductEvery: 100 * time.Millisecond, StatusEvery: 200 * time.Millisecond, RecordEvery: 300 * time.Millisecond, RepeatEvery: 300 * time.Millisecond,
		Health: healthStub{}}
	mux := http.NewServeMux()
	if err := Register(mux, r.srv, guard.Require); err != nil {
		t.Fatal(err)
	}
	r.http = httptest.NewServer(httpx.TrackRoute(mux))
	t.Cleanup(r.http.Close)
	return r
}

type healthStub struct{}

func (healthStub) GetHealthz(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }
func (healthStub) GetReadyz(w http.ResponseWriter, _ *http.Request)  { w.WriteHeader(200) }

// track publishes a track sample into the hub as the bus would.
func (r *rig) track(id string, trust core.Trust, source, instance string, p core.LatLon, at time.Time, flight bool) {
	r.t.Helper()
	alt, sp, td := 550.0, 0.0, 0.0
	status := "Airborne"
	tr := telemetry.Track{
		Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer, core.Times{RxTS: at, CapturedAt: at, Source: core.TimeSourceClock}),
		Body: telemetry.TrackBody{TrackID: id, Trust: trust, Source: source, SourceInstance: instance, Position: telemetry.Position{Lat: p.LatDeg, Lng: p.LonDeg},
			AltAMSLM: &alt, AltSource: core.AltGeodetic, SpeedMS: &sp, TrackDeg: &td, Status: &status,
			Identification: core.Identification{Status: core.IdentRegistered, Reason: core.ReasonSessionBinding, Basis: core.BasisAuthenticated}},
	}
	if flight {
		f := id
		tr.Body.FlightID = &f
	}
	raw, err := json.Marshal(&tr)
	if err != nil {
		r.t.Fatal(err)
	}
	r.hub.TakeTrack(traffic.NSTrack, raw)
}

func (r *rig) manned(icao string, trust core.Trust, source string, p core.LatLon, at time.Time) {
	r.t.Helper()
	gs, td := 60.0, 90.0
	m := traffic.MannedTrack{Envelope: bus.NewEnvelope(traffic.SchemaManned, "ansp/manned-feed", core.Times{RxTS: at, CapturedAt: at, Source: core.TimeReceiver}),
		Body: traffic.MannedBody{ICAO24: icao, Position: traffic.MannedPosition{Lat: p.LatDeg, Lng: p.LonDeg}, GSMS: &gs, TrackDeg: &td,
			SourceClass: "ads_b", Trust: trust, Source: source, SourceInstance: "adsb-tbs", State: traffic.StateLive}}
	raw, err := json.Marshal(&m)
	if err != nil {
		r.t.Fatal(err)
	}
	r.hub.TakeManned(raw)
}

func (r *rig) alert(state string, acked bool, updated time.Time) {
	r.t.Helper()
	b := traffic.AlertBody{AlertID: alertA, Kind: traffic.KindProximity, Severity: core.SeverityCritical, State: state, FlightID: flightA,
		CapturedAt: bus.Stamp{Time: updated}, RaisedAt: bus.Stamp{Time: updated}, UpdatedAt: bus.Stamp{Time: updated}, PolicyVersion: 4,
		Detail: map[string]any{"t_cpa_s": 10.0, "d_cpa_h_m": 1.0, "d_alt_m": nil, "los_start_s": 5.0, "evaluation_period_s": 1,
			"peer": traffic.Peer{TrackID: flightB, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS}}}
	if state == traffic.AlertCleared {
		cr := "resolved"
		b.ClearReason = &cr
	}
	raw, _ := json.Marshal(&traffic.AlertMessage{Envelope: bus.SystemEnvelope(traffic.SchemaAlert, traffic.Producer, updated), Body: b})
	if acked {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["body"].(map[string]any)["acked_at"] = updated.Format(time.RFC3339Nano)
		raw, _ = json.Marshal(m)
	}
	r.hub.TakeAlert("alrt.v1.proximity.c5:1317:2248."+alertA, raw)
}

func (r *rig) dial(path, token, origin string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	if token != "" && origin == "" {
		h.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		h.Set("Origin", origin)
		h.Set("Cookie", auth.CookieSession+"="+token)
	}
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.http.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
	if c != nil {
		c.SetReadLimit(16 << 20)
	}
	return c, resp, err
}

type frame struct {
	Schema string          `json:"schema"`
	Body   json.RawMessage `json:"body"`
	raw    []byte
}

// next reads frames until cond holds (every frame validates).
func next(t *testing.T, ss map[string]*jsonschema.Schema, c *websocket.Conn, within time.Duration, cond func(frame) bool) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no frame satisfied the condition: %v", err)
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatal(err)
		}
		f.raw = data
		validateFrame(t, ss, f)
		if cond(f) {
			return f
		}
	}
}

func product(t *testing.T, f frame) traffic.ProductBody {
	t.Helper()
	var b traffic.ProductBody
	if err := json.Unmarshal(f.Body, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func statusOf(t *testing.T, f frame) StatusBody {
	t.Helper()
	var b StatusBody
	if err := json.Unmarshal(f.Body, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

var schemaCache struct {
	once sync.Once
	ss   map[string]*jsonschema.Schema
	err  error
}

func schemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	schemaCache.once.Do(func() {
		root := filepath.Join("..", "..", "..", "schemas")
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		var names []string
		schemaCache.err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		schemaCache.ss = map[string]*jsonschema.Schema{}
		for _, n := range names {
			s, err := c.Compile("https://schemas.uspace.ge/" + n + ".json")
			if err != nil {
				schemaCache.err = err
				return
			}
			schemaCache.ss[n] = s
		}
	})
	if schemaCache.err != nil {
		t.Fatal(schemaCache.err)
	}
	return schemaCache.ss
}

// validateFrame checks a frame against the envelope and its schema
// (every frame of the integration run does the same, brief WP-11).
func validateFrame(t *testing.T, ss map[string]*jsonschema.Schema, f frame) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(f.raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := ss["envelope/v1"].Validate(inst); err != nil {
		t.Fatalf("envelope: %v\n%s", err, f.raw)
	}
	s, ok := ss[f.Schema]
	if !ok {
		t.Fatalf("a frame of an unknown schema %q", f.Schema)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("%s: %v\n%s", f.Schema, err, f.raw)
	}
}

// An operator connects to its intent: status first, then the snapshot,
// then products with its own flight, the other flight in the radius and
// its proximity alert; every frame validates; a far track is not in it.
func TestOperatorStreamFramesValidate(t *testing.T) {
	ss := schemas(t)
	r := newRig(t)
	now := time.Now()
	r.track(flightA, core.TrustAuthenticated, telemetry.SourceOperatorWS, clientA, origin, now, true)
	r.track(flightB, core.TrustAuthenticated, telemetry.SourceOperatorWS, clientB, geodesy.Destination(origin, 90, 800), now, true)
	far := "33333333-3333-4333-8333-333333333333"
	r.track(far, core.TrustAuthenticated, telemetry.SourceOperatorWS, clientB, geodesy.Destination(origin, 90, 10_000), now, true)
	r.alert(traffic.AlertRaised, false, now)
	c, _, err := r.dial("/v1/traffic?intent_id="+intentA, "op-a", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	first := next(t, ss, c, 2*time.Second, func(frame) bool { return true })
	if first.Schema != SchemaStatus {
		t.Fatalf("first frame %s", first.Schema)
	}
	next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == SchemaSnapshot })
	p := product(t, next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == traffic.SchemaProduct }))
	ids := map[string]bool{}
	for _, tr := range p.Tracks {
		ids[tr.TrackID] = tr.Own || ids[tr.TrackID]
	}
	if _, ok := ids[flightB]; !ok || !ids[flightA] {
		t.Fatalf("tracks %v", ids)
	}
	if _, ok := ids[far]; ok {
		t.Fatal("a track 10 km away is in a 2 km product")
	}
	if len(p.Alerts) != 1 || p.For.IntentID == nil || *p.For.IntentID != intentA {
		t.Fatalf("alerts %d for %+v", len(p.Alerts), p.For)
	}
}

// Another operator's intent and an unknown one are 404 before any
// upgrade; an unread projection is 503 with Retry-After; no credential
// is a 4401 close.
func TestSubscriptionRefusals(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		path, token string
		status      int
	}{
		{"/v1/traffic?intent_id=" + intentA, "op-b", 404},
		{"/v1/traffic?intent_id=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "op-a", 404},
		{"/v1/alerts?intent_id=" + intentA, "op-b", 404},
		{"/v1/traffic?bbox=44,41,45,42", "op-a", 400},
	} {
		_, resp, err := r.dial(c.path, c.token, "")
		if err == nil || resp == nil || resp.StatusCode != c.status {
			t.Fatalf("%s with %s: %v %v", c.path, c.token, resp, err)
		}
	}
	r.hub.Intents = &bus.Mirror[intent.StateBody]{}
	_, resp, err := r.dial("/v1/traffic?intent_id="+intentA, "op-a", "")
	if err == nil || resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("unread projection: %v %v", resp, err)
	}
	c, _, err := r.dial("/v1/traffic?intent_id="+intentA, "nope", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Read(context.Background())
	if websocket.CloseStatus(err) != auth.CloseRelogin {
		t.Fatalf("close %v", err)
	}
}

// A staff session subscribes by console/subscribe/v1: a snapshot of the
// new bbox within 1 s; a broadcast track is labelled broadcast (R-05).
func TestStaffSubscribeAnswersWithASnapshot(t *testing.T) {
	ss := schemas(t)
	r := newRig(t)
	now := time.Now()
	r.manned("4ca7b6", core.TrustBroadcast, traffic.SourceAdsbRx, origin, now)
	c, _, err := r.dial("/v1/traffic", "staff", "https://console.test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == SchemaSnapshot })
	sub := `{"schema":"console/subscribe/v1","body":{"bbox":[44.7,41.6,44.9,41.8],"layers":["tracks","manned","alerts"]}}`
	sent := time.Now()
	if err := c.Write(context.Background(), websocket.MessageText, []byte(sub)); err != nil {
		t.Fatal(err)
	}
	snap := next(t, ss, c, time.Second, func(f frame) bool { return f.Schema == SchemaSnapshot })
	if time.Since(sent) > time.Second || !strings.Contains(string(snap.Body), "4ca7b6") {
		t.Fatalf("snapshot after %v: %s", time.Since(sent), snap.Body)
	}
	p := product(t, next(t, ss, c, time.Second, func(f frame) bool {
		return f.Schema == traffic.SchemaProduct && len(product(t, f).Tracks) > 0
	}))
	if p.Tracks[0].Trust != core.TrustBroadcast || p.Tracks[0].Source != traffic.SourceAdsbRx {
		t.Fatalf("track %+v", p.Tracks[0])
	}
	// A browser from another origin is refused (M22).
	c2, _, err := r.dial("/v1/traffic", "staff", "https://evil.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c2.Read(context.Background()); websocket.CloseStatus(err) != auth.CloseRelogin {
		t.Fatalf("other origin: %v", err)
	}
}

// Degraded both ways: no manned input says manned in every product; a
// manned sample takes it away; silence brings it back. A client that
// reads only status frames sees why (E-02).
func TestMannedDegradedBothWays(t *testing.T) {
	ss := schemas(t)
	r := newRig(t)
	c, _, err := r.dial("/v1/traffic?intent_id="+intentA, "op-a", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	has := func(ds []traffic.Degraded, in string) bool {
		for _, d := range ds {
			if d.Input == in {
				return true
			}
		}
		return false
	}
	p := product(t, next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == traffic.SchemaProduct }))
	if !has(p.Degraded, "manned") {
		t.Fatalf("degraded %+v", p.Degraded)
	}
	st := statusOf(t, next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == SchemaStatus }))
	if !strings.Contains(strings.Join(st.Degraded, ","), "manned") || !has(st.DegradedInputs, "manned") {
		t.Fatalf("status %+v", st)
	}
	r.manned("4ca7b5", core.TrustSurveillance, traffic.SourceANSPFeed, origin, time.Now())
	next(t, ss, c, 2*time.Second, func(f frame) bool {
		return f.Schema == traffic.SchemaProduct && !has(product(t, f).Degraded, "manned")
	})
}

// A source switched off: its tracks are source_disabled in the next
// product; back on, live again.
func TestSourceDisabledTracks(t *testing.T) {
	ss := schemas(t)
	r := newRig(t)
	r.track(flightB, core.TrustAuthenticated, telemetry.SourceOperatorWS, clientB, geodesy.Destination(origin, 0, 300), time.Now(), true)
	c, _, err := r.dial("/v1/traffic?intent_id="+intentA, "op-a", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	stateOf := func(f frame) string {
		for _, tr := range product(t, f).Tracks {
			if tr.TrackID == flightB {
				return tr.State
			}
		}
		return ""
	}
	next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == traffic.SchemaProduct && stateOf(f) == traffic.StateLive })
	r.gate.set(telemetry.SourceOperatorWS+"/"+clientB, true)
	next(t, ss, c, 2*time.Second, func(f frame) bool {
		return f.Schema == traffic.SchemaProduct && stateOf(f) == traffic.StateSourceDisabled
	})
	r.gate.set(telemetry.SourceOperatorWS+"/"+clientB, false)
	r.track(flightB, core.TrustAuthenticated, telemetry.SourceOperatorWS, clientB, geodesy.Destination(origin, 0, 300), time.Now(), true)
	next(t, ss, c, time.Second, func(f frame) bool { return f.Schema == traffic.SchemaProduct && stateOf(f) == traffic.StateLive })
}

// Throttle (05 §5): 300 tracks are sent each every other product with
// dropped_frames counting what was held back; 100 tracks every product,
// nothing dropped (the twin).
func TestThrottleAbove200Tracks(t *testing.T) {
	for _, n := range []int{300, 100} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ss := schemas(t)
			r := newRig(t)
			now := time.Now()
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
				r.track(id, core.TrustAuthenticated, telemetry.SourceOperatorWS, "c", geodesy.Destination(origin, float64(i), 100+float64(i)), now, true)
			}
			c, _, err := r.dial("/v1/traffic?intent_id="+intentA, "op-a", "")
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			seen := map[string]int{}
			var last traffic.ProductBody
			for k := 0; k < 4; k++ {
				last = product(t, next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == traffic.SchemaProduct }))
				for _, tr := range last.Tracks {
					seen[tr.TrackID]++
				}
			}
			if n > 200 {
				for id, k := range seen {
					if k > 2 {
						t.Fatalf("%s in %d of 4 products", id, k)
					}
				}
				if last.DroppedFrames == 0 || !last.Throttled {
					t.Fatalf("dropped %d throttled %v tracks %d", last.DroppedFrames, last.Throttled, len(last.Tracks))
				}
				st := statusOf(t, next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == SchemaStatus }))
				if st.DroppedFrames == 0 {
					t.Fatal("status says nothing was dropped")
				}
				return
			}
			for id, k := range seen {
				if k != 4 {
					t.Fatalf("%s in %d of 4 products", id, k)
				}
			}
			if last.DroppedFrames != 0 || last.Throttled {
				t.Fatalf("dropped %d", last.DroppedFrames)
			}
		})
	}
}

// The alert stream: the active alert on connect, its delivery recorded
// once, repeated while unacknowledged and critical; acknowledged, it is
// sent once more (the acknowledgement) and repeated no longer; cleared,
// the clear is sent.
func TestAlertStreamRepeatsUntilAcknowledged(t *testing.T) {
	ss := schemas(t)
	r := newRig(t)
	now := time.Now()
	r.track(flightA, core.TrustAuthenticated, telemetry.SourceOperatorWS, clientA, origin, now, true)
	r.alert(traffic.AlertRaised, false, now)
	c, _, err := r.dial("/v1/alerts?intent_id="+intentA, "op-a", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	isAlert := func(f frame) bool { return f.Schema == alerts.SchemaAlert }
	next(t, ss, c, 2*time.Second, isAlert)
	next(t, ss, c, 2*time.Second, isAlert) // the repeat
	waitFor(t, func() bool { return r.pub.count("alrt/delivery") == 1 })
	r.alert(traffic.AlertUpdated, true, now.Add(time.Second))
	f := next(t, ss, c, 2*time.Second, isAlert)
	if !strings.Contains(string(f.Body), "acked_at") {
		t.Fatalf("not the acknowledgement: %s", f.Body)
	}
	// No repeat after the acknowledgement: five status frames (a
	// second, over three repeat periods) and no alert between them.
	statuses := 0
	next(t, ss, c, 3*time.Second, func(f frame) bool {
		if f.Schema == alerts.SchemaAlert {
			t.Fatalf("repeated after the acknowledgement: %s", f.Body)
		}
		if f.Schema == SchemaStatus {
			statuses++
		}
		return statuses == 5
	})
	r.alert(traffic.AlertCleared, true, now.Add(2*time.Second))
	f = next(t, ss, c, 2*time.Second, isAlert)
	if !strings.Contains(string(f.Body), `"state":"cleared"`) || r.pub.count("alrt/delivery") != 1 {
		t.Fatalf("%s, deliveries %d", f.Body, r.pub.count("alrt/delivery"))
	}
}

// A snapshot over HTTP is one product; the record sample is published
// with the client.
func TestSnapshotAndRecord(t *testing.T) {
	ss := schemas(t)
	r := newRig(t)
	r.track(flightA, core.TrustAuthenticated, telemetry.SourceOperatorWS, clientA, origin, time.Now(), true)
	req, _ := http.NewRequest(http.MethodGet, r.http.URL+"/v1/traffic/snapshot?intent_id="+intentA, nil)
	req.Header.Set("Authorization", "Bearer op-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	var f frame
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	f.raw = buf.Bytes()
	_ = json.Unmarshal(f.raw, &f)
	validateFrame(t, ss, f)
	req2, _ := http.NewRequest(http.MethodGet, r.http.URL+"/v1/traffic/snapshot?intent_id="+intentA, nil)
	req2.Header.Set("Authorization", "Bearer op-b")
	if resp, err := http.DefaultClient.Do(req2); err != nil || resp.StatusCode != 404 {
		t.Fatalf("another operator: %v %v", resp, err)
	}
	c, _, err := r.dial("/v1/traffic?intent_id="+intentA, "op-a", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	go func() {
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	waitFor(t, func() bool { return r.pub.count("traffic.product/") > 0 })
	r.pub.mu.Lock()
	raw := r.pub.msgs["traffic.product/"][0]
	r.pub.mu.Unlock()
	var rec frame
	rec.raw = []byte(raw)
	_ = json.Unmarshal(rec.raw, &rec)
	validateFrame(t, ss, rec)
	if !strings.Contains(raw, `"client_id":"client-a"`) {
		t.Fatalf("record %s", raw)
	}
}

// A socket closes with 4401 when its credential expires.
func TestSocketClosesAtCredentialExpiry(t *testing.T) {
	r := newRig(t)
	c, _, err := r.dial("/v1/traffic?intent_id="+intentA, "short", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != auth.CloseRelogin {
				t.Fatalf("closed with %v", err)
			}
			return
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
