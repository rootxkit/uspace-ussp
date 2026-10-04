package manned

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/timeplace"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/ansp"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

func fastPolicy() policy.Values {
	v := policy.Defaults()
	v.MannedUnavailableS = 0.5
	return v
}

func newANSP(t *testing.T) *ansp.Fake {
	t.Helper()
	f, err := ansp.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	f.Periods(30*time.Millisecond, 60*time.Millisecond)
	return f
}

func newStream(f *ansp.Fake, s *sink, g *gate) *ANSPStream {
	return &ANSPStream{URL: f.StreamURL(), Tokens: &tokens{tok: "tok"}, Sink: s, Gate: g, Policy: fastPolicy,
		Counters: &core.Counters{}, RetryMin: 10 * time.Millisecond, StatusEvery: 20 * time.Millisecond, GateEvery: 10 * time.Millisecond}
}

func run(t *testing.T, fn func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fn(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func pf(v float64) *float64 { return &v }

var place = core.LatLon{LatDeg: 41.75, LonDeg: 44.85}

// The stream read end to end (E-02): the snapshot and every frame's
// aircraft republished on man.v1 with trust surveillance, source
// ansp_feed and the ANSP's adapter, placed on our clock, each a valid
// track/manned/v1 the CPA path and traffic-ws read; the bbox asked; a
// token of scope ansp.traffic for the ANSP's origin; the feed live on
// src.v1 and on /readyz.
func TestANSPStreamPublishesTheANSPsAircraft(t *testing.T) {
	f := newANSP(t)
	cs := "DLH4AB"
	f.SetAircraft(ansp.Aircraft{ICAO24: "4ca123", Callsign: &cs, Position: place, AltPressureM: pf(650), AltWGS84M: pf(700), GSMS: 60, TrackDeg: 90})
	s := &sink{}
	st := newStream(f, s, &gate{})
	tok := st.Tokens.(*tokens)
	st.BBox = func() (geodesy.BBox, bool) {
		return geodesy.BBox{MinLat: 41.6, MinLon: 44.7, MaxLat: 41.9, MaxLon: 45.0}, true
	}
	run(t, st.Run)
	within(t, 5*time.Second, "tracks published", func() bool { return len(s.tracks()) >= 3 })
	ss := schemas(t)
	for _, raw := range s.raw("man.v1.") {
		validate(t, ss, "track/manned/v1", raw)
		m, err := traffic.DecodeManned(raw)
		if err != nil {
			t.Fatalf("the CPA path refuses it: %v", err)
		}
		in := traffic.MannedInputOf(m, nil)
		if in.Trust != core.TrustSurveillance || in.Source != SourceANSPFeed || in.Instance != "fake-adsb-1" || in.AltSource != core.AltNone {
			t.Fatalf("input %+v", in)
		}
	}
	tr := s.tracks()[0]
	if tr.Producer != ProducerANSP || tr.Body.Callsign == nil || *tr.Body.Callsign != cs || tr.Body.State != StateLive {
		t.Fatalf("track %+v", tr)
	}
	if age := time.Since(tr.CapturedAt.Time); age < 0 || age > 5*time.Second || tr.TS == nil {
		t.Fatalf("placed %v (ts %v)", tr.CapturedAt, tr.TS)
	}
	auth, bboxes := f.StreamAuth()
	if len(auth) == 0 || auth[0] != "Bearer tok" || bboxes[0] != "44.700000,41.600000,45.000000,41.900000" {
		t.Fatalf("auth %q bbox %q", auth, bboxes)
	}
	tok.mu.Lock()
	got := tok.got[0]
	tok.mu.Unlock()
	if !strings.HasPrefix(got, "http://127.0.0.1:") || !strings.HasSuffix(got, "|ansp.traffic") {
		t.Fatalf("token asked for %q", got)
	}
	if st.Counters.Get(CounterANSPSnapshotTaken) == 0 || st.Counters.Get(CounterANSPSnapshotFrames) == 0 {
		t.Fatalf("snapshots %v", st.Counters.Snapshot())
	}
	within(t, 2*time.Second, "live status", func() bool {
		ss := s.statuses()
		return len(ss) > 0 && ss[len(ss)-1].Body.State == sources.StateLive
	})
	if state, d := st.Probe(context.Background()); state != obs.StateUp || !strings.Contains(d, "live since") {
		t.Fatalf("probe %s %s", state, d)
	}
}

// Stream cut (E-01 both ways): unavailable since the last frame once
// manned_unavailable_s passes, on src.v1 and /readyz, never an empty sky;
// back once the ANSP serves again, and the outage counted.
func TestANSPStreamCutIsUnavailableSinceTheLastFrameThenBack(t *testing.T) {
	f := newANSP(t)
	f.SetAircraft(ansp.Aircraft{ICAO24: "4ca123", Position: place, AltPressureM: pf(650)})
	s := &sink{}
	st := newStream(f, s, &gate{})
	run(t, st.Run)
	within(t, 5*time.Second, "live", func() bool { return st.State().State == sources.StateLive })
	cut := time.Now()
	f.CutStream()
	within(t, 3*time.Second, "unavailable", func() bool { return st.State().State == sources.StateDown })
	a := st.State()
	// Since the last frame: one in flight when the stream was cut may
	// still be read a moment after.
	if a.Since.After(cut.Add(200*time.Millisecond)) || cut.Sub(a.Since) > time.Second || !strings.Contains(a.Detail, "unavailable") {
		t.Fatalf("down %+v (cut at %v)", a, cut)
	}
	if state, d := st.Probe(context.Background()); state != obs.StateDown || !strings.Contains(d, "unavailable since") {
		t.Fatalf("probe %s %s", state, d)
	}
	within(t, 2*time.Second, "down on src.v1", func() bool {
		ss := s.statuses()
		for i := len(ss) - 1; i >= 0; i-- {
			if ss[i].Body.SourceInstance == nil {
				return ss[i].Body.State == sources.StateDown
			}
		}
		return false
	})
	f.RestoreStream()
	within(t, 5*time.Second, "back", func() bool { return st.State().State == sources.StateLive })
	if st.Counters.Get(CounterANSPConnects) < 2 || st.Counters.Get(CounterANSPDisconnects) < 1 {
		t.Fatalf("counters %v", st.Counters.Snapshot())
	}
}

// The ANSP sends only console/status/v1 with an adapter stale: the feed
// is stale since the ANSP's own time, not unavailable (02 F4); the
// adapter's own status says it with that time.
func TestANSPStreamStaleAdapterIsStaleSinceTheANSPsTime(t *testing.T) {
	f := newANSP(t)
	since := time.Now().Add(-42 * time.Second).UTC().Truncate(time.Second)
	f.TracksOff(true)
	f.SetAdapter(ansp.AdapterState{ID: "fake-adsb-1", State: "stale", Since: since})
	s := &sink{}
	st := newStream(f, s, &gate{})
	run(t, st.Run)
	within(t, 5*time.Second, "stale", func() bool { return st.State().State == sources.StateStale })
	if a := st.State(); !a.Since.Equal(since) || !strings.Contains(a.Detail, "fake-adsb-1 stale") {
		t.Fatalf("aggregate %+v, want stale since %v", a, since)
	}
	var adapter *sources.StatusBody
	for _, b := range st.Statuses() {
		if b.SourceInstance != nil && *b.SourceInstance == "fake-adsb-1" {
			adapter = &b
		}
	}
	if adapter == nil || adapter.State != sources.StateStale || !adapter.Since.Equal(since) {
		t.Fatalf("adapter status %+v", adapter)
	}
	if state, d := st.Probe(context.Background()); state != obs.StateDegraded || !strings.Contains(d, "stale since") {
		t.Fatalf("probe %s %s", state, d)
	}
	if len(s.tracks()) != 0 {
		t.Fatalf("tracks without any sent: %d", len(s.tracks()))
	}
	// The twin: the adapter live again, the feed live.
	f.SetAdapter(ansp.AdapterState{ID: "fake-adsb-1", State: "live", Since: time.Now()})
	within(t, 5*time.Second, "live", func() bool { return st.State().State == sources.StateLive })
	// An adapter switched off at the ANSP: stale for us, its own status
	// disabled by instance.
	f.SetAdapter(ansp.AdapterState{ID: "fake-adsb-1", State: "disabled", Since: since})
	within(t, 5*time.Second, "disabled adapter", func() bool {
		for _, b := range st.Statuses() {
			if b.SourceInstance != nil && b.State == sources.StateDisabled && b.DisabledBy != nil {
				return st.State().State == sources.StateStale
			}
		}
		return false
	})
}

// The switches: ansp_feed off closes the stream at once and nothing is
// dialled while it is off (disabled on src.v1 by type); on again, the
// stream comes back. An adapter switched off here: its tracks are left
// out, counted, and its status says so (E-01 pair).
func TestANSPStreamFollowsTheSwitches(t *testing.T) {
	f := newANSP(t)
	f.SetAircraft(ansp.Aircraft{ICAO24: "4ca123", Position: place})
	s := &sink{}
	g := &gate{}
	st := newStream(f, s, g)
	run(t, st.Run)
	within(t, 5*time.Second, "tracks", func() bool { return len(s.tracks()) > 0 })
	g.set(SourceANSPFeed, true)
	within(t, 2*time.Second, "closed", func() bool { return st.Counters.Get(CounterANSPSwitchedOff) == 1 })
	connects := f.StreamConnects()
	a := st.State()
	if a.State != sources.StateDisabled || a.DisabledBy == nil || *a.DisabledBy != "type" {
		t.Fatalf("aggregate %+v", a)
	}
	if state, _ := st.Probe(context.Background()); state != obs.StateDegraded {
		t.Fatalf("probe %s", state)
	}
	n := len(s.tracks())
	// While off: no dial at all.
	within(t, 2*time.Second, "an off status", func() bool {
		ss := s.statuses()
		return len(ss) > 0 && ss[len(ss)-1].Body.State != sources.StateLive
	})
	if f.StreamConnects() != connects {
		t.Fatalf("dialled while off: %d -> %d", connects, f.StreamConnects())
	}
	g.set(SourceANSPFeed, false)
	within(t, 5*time.Second, "back", func() bool { return len(s.tracks()) > n && f.StreamConnects() > connects })
	g.set(SourceANSPFeed+"/fake-adsb-1", true)
	within(t, 2*time.Second, "instance off", func() bool { return st.Counters.Get(CounterANSPInstanceOff) > 0 })
	found := false
	for _, b := range st.Statuses() {
		if b.SourceInstance != nil && *b.SourceInstance == "fake-adsb-1" {
			found = b.State == sources.StateDisabled && b.DisabledBy != nil && *b.DisabledBy == "instance"
		}
	}
	f.SetAdapter(ansp.AdapterState{ID: "fake-adsb-1", State: "live", Since: time.Now()})
	within(t, 2*time.Second, "adapter status off", func() bool {
		for _, b := range st.Statuses() {
			if b.SourceInstance != nil && *b.SourceInstance == "fake-adsb-1" {
				return b.State == sources.StateDisabled
			}
		}
		return found
	})
}

// The echo guard (Q23): an aircraft whose callsign is the UA
// registration of one of our active flights is left out and counted;
// another callsign at the same place is published (E-01 pair).
func TestANSPStreamLeavesOutAnEchoOfAnOwnFlight(t *testing.T) {
	f := newANSP(t)
	echo, other := "4L-ABC", "OTHER1"
	f.SetAircraft(ansp.Aircraft{ICAO24: "4ca001", Callsign: &echo, Position: place})
	f.SetAircraft(ansp.Aircraft{ICAO24: "4ca002", Callsign: &other, Position: place})
	s := &sink{}
	st := newStream(f, s, &gate{})
	st.Own = own{reg: "4LABC", flight: "flight-1"}
	run(t, st.Run)
	within(t, 5*time.Second, "tracks and echoes", func() bool {
		return len(s.tracks()) >= 2 && st.Counters.Get(CounterEchoOwnFlight) >= 2
	})
	for _, tr := range s.tracks() {
		if tr.Body.ICAO24 == "4ca001" {
			t.Fatalf("the echo of an own flight was published: %+v", tr.Body)
		}
	}
	if !strings.Contains(func() string { _, d := st.Probe(context.Background()); return d }(), "echoes of own flights left out") {
		t.Fatal("probe does not count the echoes")
	}
}

func frame(t *testing.T, schema string, ts, captured time.Time, body map[string]any) []byte {
	t.Helper()
	e := bus.NewEnvelope(schema, "ansp/manned-feed", core.Times{TS: &ts, RxTS: captured, CapturedAt: captured, Source: core.TimeSourceClock})
	b, err := json.Marshal(map[string]any{"schema": e.Schema, "msg_id": e.MsgID, "producer": e.Producer, "ts": e.TS, "rx_ts": e.RxTS,
		"captured_at": e.CapturedAt, "time_source": e.TimeSource, "backlog": false, "body": body})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mannedBody() map[string]any {
	return map[string]any{"icao24": "4ca123", "callsign": nil, "position": map[string]any{"lat": 41.75, "lng": 44.85},
		"alt_pressure_m": 650.0, "alt_wgs84_m": nil, "gs_ms": 60.0, "track_deg": 90.0, "vrate_ms": 0.0, "source_class": "ads_b",
		"quality": nil, "trust": "surveillance", "source": "ansp_feed", "source_instance": "fake-adsb-1", "state": "live", "spi": false}
}

// Frames one by one: a track placed by PlaceNetwork (a state over 60 s
// old not shown, one 7 s old placed at receipt, a stale one republished
// stale), every refusal counted and nothing published for it; status,
// snapshot, unknown and unreadable frames counted.
func TestANSPStreamTakesFramesOneByOne(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s := &sink{}
	st := &ANSPStream{Sink: s, Counters: &core.Counters{}, Now: func() time.Time { return now }}
	ctx := context.Background()
	st.Take(ctx, frame(t, SchemaManned, now.Add(-500*time.Millisecond), now, mannedBody()))
	if len(s.tracks()) != 1 || s.tracks()[0].TimeSource != core.TimeBroadcast {
		t.Fatalf("fresh track %+v", s.tracks())
	}
	st.Take(ctx, frame(t, SchemaManned, now.Add(-7*time.Second), now, mannedBody()))
	if st.Counters.Get(CounterANSPAtReceipt) != 1 || s.tracks()[1].TimeSource != core.TimeReceiver {
		t.Fatalf("7 s old: %v", st.Counters.Snapshot())
	}
	st.Take(ctx, frame(t, SchemaManned, now.Add(-2*time.Minute), now.Add(-2*time.Minute), mannedBody()))
	if st.Counters.Get(CounterANSPTooOld) != 1 || len(s.tracks()) != 2 {
		t.Fatalf("2 min old: %v", st.Counters.Snapshot())
	}
	stale := mannedBody()
	stale["state"] = "stale"
	st.Take(ctx, frame(t, SchemaManned, now.Add(-3*time.Second), now, stale))
	if got := s.tracks(); len(got) != 3 || got[2].Body.State != StateStale {
		t.Fatalf("stale %+v", got)
	}
	for name, mutate := range map[string]func(map[string]any){
		"icao24":    func(b map[string]any) { b["icao24"] = "4CA123" },
		"trust":     func(b map[string]any) { b["trust"] = "broadcast" },
		"source":    func(b map[string]any) { b["source"] = "adsb_rx" },
		"position":  func(b map[string]any) { b["position"] = map[string]any{"lat": 91.0, "lng": 0.0} },
		"class":     func(b map[string]any) { b["source_class"] = "radar" },
		"state":     func(b map[string]any) { b["state"] = "" },
		"instance":  func(b map[string]any) { b["source_instance"] = "Not A Slug" },
		"track":     func(b map[string]any) { b["track_deg"] = 360.0 },
		"speed":     func(b map[string]any) { b["gs_ms"] = -1.0 },
		"squawk":    func(b map[string]any) { b["squawk"] = "7800" },
		"callsign":  func(b map[string]any) { b["callsign"] = "TOOLONGCALL" },
		"quality":   func(b map[string]any) { b["quality"] = []int{1} },
		"body type": func(b map[string]any) { b["gs_ms"] = "fast" },
	} {
		b := mannedBody()
		mutate(b)
		before := st.Counters.Get(CounterANSPTrackRefused)
		st.Take(ctx, frame(t, SchemaManned, now, now, b))
		if st.Counters.Get(CounterANSPTrackRefused) != before+1 {
			t.Errorf("%s: not refused", name)
		}
	}
	// An envelope that does not validate.
	st.Take(ctx, []byte(`{"schema":"track/manned/v1","msg_id":"x","body":{}}`))
	if len(s.tracks()) != 3 {
		t.Fatalf("a refused track was published: %d", len(s.tracks()))
	}
	st.Take(ctx, frame(t, SchemaConsoleStatus, now, now, map[string]any{"degraded": []string{"ads_b_north"},
		"sources": []any{map[string]any{"source": "ansp_feed", "source_instance": "fake-adsb-1", "state": "live", "since": now}},
	}))
	st.Take(ctx, frame(t, SchemaConsoleStatus, now, now, map[string]any{"degraded": "not a list"}))
	st.Take(ctx, frame(t, SchemaConsoleSnapshot, now, now, map[string]any{"manned": []any{}}))
	st.Take(ctx, frame(t, SchemaConsoleSnapshot, now, now, map[string]any{"tracks": []any{}}))
	st.Take(ctx, frame(t, "weather/v1", now, now, map[string]any{}))
	st.Take(ctx, []byte(`not json`))
	for name, want := range map[string]uint64{CounterANSPStatusFrames: 2, CounterANSPSnapshotFrames: 2, CounterANSPUnknownSchema: 1,
		CounterANSPUnreadable: 3} {
		if got := st.Counters.Get(name); got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	// A sink that refuses: counted, the feed says refused.
	s.err = errSink
	st.Take(ctx, frame(t, SchemaManned, now, now, mannedBody()))
	if st.Counters.Get(CounterPublishFailed) != 1 {
		t.Fatalf("publish failure not counted: %v", st.Counters.Snapshot())
	}
}

// Refused (E-01 pair with the connection above): no token, a bearer the
// ANSP refuses; never connected, unavailable since the start with why.
func TestANSPStreamRefused(t *testing.T) {
	f := newANSP(t)
	no := &ANSPStream{URL: f.StreamURL(), Tokens: &tokens{err: errors.New("token service down")}, Policy: fastPolicy, RetryMin: 10 * time.Millisecond}
	run(t, no.Run)
	within(t, 3*time.Second, "no token", func() bool {
		s, d := no.Probe(context.Background())
		return s == obs.StateDown && strings.Contains(d, "no token for the ANSP")
	})
	f.CutStream()
	refused := &ANSPStream{URL: f.StreamURL(), Tokens: &tokens{tok: "tok"}, Policy: fastPolicy, RetryMin: 10 * time.Millisecond}
	run(t, refused.Run)
	within(t, 3*time.Second, "dial refused", func() bool {
		s, d := refused.Probe(context.Background())
		return s == obs.StateDown && strings.Contains(d, "dial")
	})
	if (&ANSPStream{URL: "wss://ansp.example/v1/manned-traffic/stream"}).baseURL() != "https://ansp.example" {
		t.Error("baseURL")
	}
	if (&ANSPStream{URL: "ws://ansp:8080/v1/manned-traffic/stream"}).baseURL() != "http://ansp:8080" {
		t.Error("baseURL ws")
	}
	if SnapshotURL("wss://ansp.example/v1/manned-traffic/stream?bbox=1") != "https://ansp.example/v1/manned-traffic/snapshot?bbox=1" ||
		SnapshotURL("ws://a/x") != "" {
		t.Error("SnapshotURL")
	}
	if got := withBBox("ws://a/s?x=1&bbox=old", "1,2,3,4", true); got != "ws://a/s?bbox=1%2C2%2C3%2C4&x=1" {
		t.Errorf("withBBox %q", got)
	}
}

// A changed bbox reconnects with the new one (no stale box kept).
func TestANSPStreamReconnectsOnANewBBox(t *testing.T) {
	f := newANSP(t)
	var mu sync.Mutex
	box := geodesy.BBox{MinLat: 41, MinLon: 44, MaxLat: 42, MaxLon: 45}
	st := newStream(f, &sink{}, &gate{})
	st.BBox = func() (geodesy.BBox, bool) { mu.Lock(); defer mu.Unlock(); return box, true }
	run(t, st.Run)
	within(t, 3*time.Second, "connected", func() bool { return f.StreamConnects() == 1 })
	mu.Lock()
	box.MaxLon = 46
	mu.Unlock()
	within(t, 3*time.Second, "reconnected", func() bool {
		_, b := f.StreamAuth()
		return len(b) >= 2 && b[len(b)-1] == "44.000000,41.000000,46.000000,42.000000"
	})
}

// USSP_MTLS_MODE=off is logged at error level in every status period.
func TestANSPStreamLogsMTLSOffEveryPeriod(t *testing.T) {
	f := newANSP(t)
	var buf syncBuffer
	st := newStream(f, &sink{}, &gate{})
	st.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	st.MTLSOff, st.MTLSOffEvery = true, 30*time.Millisecond
	run(t, st.Run)
	within(t, 3*time.Second, "two error lines", func() bool {
		return strings.Count(buf.String(), `"level":"ERROR","msg":"USSP_MTLS_MODE=off`) >= 2
	})
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The rid_time.json network cases owned by ussp through this adapter's
// placement bound (the same PlaceNetwork with the policy's tolerance):
// a check that the adapter's policy is uspace-core's network defaults.
func TestANSPPlacementPolicyIsCoresNetworkDefaults(t *testing.T) {
	p := (&ANSPStream{}).placementPolicy()
	d := timeplace.DefaultNetworkPolicy()
	if p.MaxAgeS != d.MaxAgeS || p.MaxLatencyS != d.MaxLatencyS || p.ToleranceS != policy.Defaults().TelemetryAheadToleranceS {
		t.Fatalf("placement policy %+v, core %+v", p, d)
	}
}

// clip cuts on a rune boundary.
func TestClip(t *testing.T) {
	s := strings.Repeat("ა", 150)
	c := clip(s)
	if len(c) > MaxDetailBytes || !strings.HasPrefix(s, c) || strings.ContainsRune(c, '�') {
		t.Fatalf("clip %q", c)
	}
	if clipErr(nil) != "closed" {
		t.Fatal("clipErr nil")
	}
}
