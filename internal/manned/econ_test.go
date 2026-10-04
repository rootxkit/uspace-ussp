package manned

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

func TestParseFeed(t *testing.T) {
	for in, want := range map[string]string{
		"http://rx/data/aircraft.json": FeedHTTP, "https://rx/data/aircraft.json": FeedHTTP,
		"sbs://rx:30003": FeedSBS, "file:///data/replay.jsonl": FeedFile,
	} {
		if k, _, err := ParseFeed(in); err != nil || k != want {
			t.Errorf("%s: %s %v", in, k, err)
		}
	}
	for _, bad := range []string{"", "ftp://rx", "sbs://rx", "file://", "http://"} {
		if _, _, err := ParseFeed(bad); err == nil || !strings.Contains(err.Error(), "USSP_ADSB_SOURCE") {
			t.Errorf("%q accepted or not named: %v", bad, err)
		}
	}
}

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// One aircraft.json entry onto the schema's body: feet, knots and feet
// per minute converted, the callsign trimmed, the emergency from the
// squawk or the flag, multilateration as Mode S, the quality, the age.
func TestRecordBodyConvertsTheFeedsUnits(t *testing.T) {
	var a AircraftRecord
	if err := json.Unmarshal([]byte(`{"hex":"4CA7B5","flight":"RYR1AB  ","alt_baro":1000,"alt_geom":1100,"gs":100,"track":360,
		"baro_rate":-600,"squawk":"7700","emergency":"none","lat":41.7,"lon":44.8,"seen_pos":1.5,"seen":0.2,"mlat":["lat","lon"],
		"nic":8,"nac_p":9,"r":"EI-ABC"}`), &a); err != nil {
		t.Fatal(err)
	}
	b, age, err := RecordBody(&a, "rx-1")
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "alt_pressure_m", b.AltPressureM, 304.8)
	approx(t, "alt_wgs84_m", b.AltWGS84M, 335.28)
	approx(t, "gs_ms", b.GSMS, 100*1852.0/3600)
	approx(t, "vrate_ms", b.VRateMS, -600*0.3048/60)
	approx(t, "track_deg", b.TrackDeg, 0)
	if b.ICAO24 != "4ca7b5" || b.Callsign == nil || *b.Callsign != "RYR1AB" || b.Emergency == nil || !*b.Emergency ||
		b.SourceClass != "mode_s" || b.Squawk == nil || age != 1.5 || b.Trust != core.TrustBroadcast || b.Source != SourceAdsbRx ||
		b.SourceInstance != "rx-1" || string(b.Quality) != `{"nic":8,"nac_p":9}` {
		t.Fatalf("body %+v age %v", b, age)
	}
	// On the ground, no geometric altitude, no flag and a normal squawk:
	// no pressure altitude, not an emergency; geom_rate stands in.
	if err := json.Unmarshal([]byte(`{"hex":"4ca7b6","alt_baro":"ground","geom_rate":120,"squawk":"1200","lat":41.7,"lon":44.8,"seen":3}`), &a); err != nil {
		t.Fatal(err)
	}
	a.AltGeom, a.BaroRate, a.Emergency, a.MLAT, a.SeenPos, a.NIC, a.NACp = nil, nil, nil, nil, nil, nil, nil
	b, age, err = RecordBody(&a, "rx-1")
	if err != nil || b.AltPressureM != nil || b.AltWGS84M != nil || b.Emergency == nil || *b.Emergency || age != 3 || b.SourceClass != "ads_b" {
		t.Fatalf("ground %+v %v %v", b, age, err)
	}
	approx(t, "vrate_ms from geom_rate", b.VRateMS, 120*0.3048/60)
	if emergencyOf(nil, nil) != nil || !*emergencyOf(nil, ptr("general")) || *emergencyOf(nil, ptr("none")) {
		t.Fatal("emergencyOf")
	}
	for name, raw := range map[string]string{
		"non-ICAO":    `{"hex":"~abcdef","lat":1,"lon":1}`,
		"no position": `{"hex":"abcdef"}`,
		"bad hex":     `{"hex":"xyz","lat":1,"lon":1}`,
		"bad lat":     `{"hex":"abcdef","lat":91,"lon":1}`,
		"bad track":   `{"hex":"abcdef","lat":1,"lon":1,"track":400}`,
	} {
		var r AircraftRecord
		_ = json.Unmarshal([]byte(raw), &r)
		if _, _, err := RecordBody(&r, "rx-1"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func doc(now float64, aircraft string) []byte {
	return []byte(fmt.Sprintf(`{"now":%f,"messages":10,"aircraft":[%s]}`, now, aircraft))
}

// aircraft.json read end to end: the receiver's tracks published as
// broadcast from adsb_rx, placed at arrival less their own age (T-12),
// each a track/manned/v1 the CPA path reads: with a geometric altitude
// and the geoid AMSL geodetic, without one the pressure altitude shown
// as pressure (judged on the horizontal alone, counted) (E-01 pair).
func TestEconspicuityPollsAircraftJSON(t *testing.T) {
	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		now := float64(time.Now().UnixNano()) / 1e9
		_, _ = w.Write(doc(now, `{"hex":"4ca001","flight":"GEO1","alt_baro":2000,"alt_geom":2100,"gs":120,"track":45,"lat":41.75,"lon":44.85,"seen_pos":0.5},
			{"hex":"4ca002","alt_baro":2500,"gs":110,"track":90,"lat":41.76,"lon":44.86,"seen_pos":2},
			{"hex":"~c0ffee","lat":41.7,"lon":44.8},{"hex":"4ca003","seen":0.1},{"hex":"4ca004","lat":41.7,"lon":44.8,"seen_pos":120}`))
	}))
	t.Cleanup(srv.Close)
	s := &sink{}
	g := &gate{}
	e := &Econspicuity{Source: srv.URL + "/data/aircraft.json", ReceiverID: "rx-1", Sink: s, Gate: g, Policy: fastPolicy,
		Every: 20 * time.Millisecond, StatusEvery: 20 * time.Millisecond, GateEvery: 10 * time.Millisecond}
	run(t, e.Run)
	within(t, 3*time.Second, "tracks", func() bool { return len(s.tracks()) >= 4 })
	ss := schemas(t)
	und := flatGeoid{}
	sawGeo, sawPressure := false, false
	for _, raw := range s.raw("man.v1.") {
		m, err := traffic.DecodeManned(raw)
		if err != nil {
			t.Fatalf("the CPA path refuses it: %v", err)
		}
		// The pinned schema holds the ANSP's feed only (PLAN §15 Q25):
		// everything but trust and source validates.
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		body := v["body"].(map[string]any)
		body["trust"], body["source"] = "surveillance", "ansp_feed"
		fixed, _ := json.Marshal(v)
		validate(t, ss, "track/manned/v1", fixed)
		in := traffic.MannedInputOf(m, und, policy.Defaults().AltPolicy())
		if in.Trust != core.TrustBroadcast || in.Source != SourceAdsbRx {
			t.Fatalf("not broadcast: %+v", in)
		}
		switch m.Body.ICAO24 {
		case "4ca001":
			sawGeo = in.AltSource == core.AltGeodetic && in.AltAMSLM != nil && math.Abs(*in.AltAMSLM-(2100*0.3048-20)) < 1e-6
			if age := time.Since(m.CapturedAt.Time); age < 400*time.Millisecond {
				t.Fatalf("seen_pos not subtracted: captured %v ago", age)
			}
		case "4ca002":
			sawPressure = in.AltSource == core.AltPressure
		default:
			t.Fatalf("a refused record was published: %s", m.Body.ICAO24)
		}
	}
	if !sawGeo || !sawPressure {
		t.Fatalf("geodetic %v pressure %v", sawGeo, sawPressure)
	}
	for _, name := range []string{CounterAdsbNonICAO, CounterAdsbNoPosition, CounterAdsbTooOld, CounterAdsbPressureOnly} {
		if e.Counters.Get(name) == 0 {
			t.Errorf("%s not counted", name)
		}
	}
	within(t, 2*time.Second, "live", func() bool { return e.Status().State == sources.StateLive })
	if st, d := e.Probe(context.Background()); st != obs.StateUp || !strings.Contains(d, "broadcast") {
		t.Fatalf("probe %s %s", st, d)
	}
	// Switched off: the polls stop at once (SC-16) and resume when on.
	g.set(SourceAdsbRx+"/rx-1", true)
	within(t, time.Second, "disabled", func() bool { return e.Status().State == sources.StateDisabled })
	n := polls.Load()
	within(t, 2*time.Second, "a disabled status published", func() bool {
		ss := s.statuses()
		return len(ss) > 2 && ss[len(ss)-1].Body.State == sources.StateDisabled
	})
	if got := polls.Load(); got > n+1 {
		t.Fatalf("polled %d times while off", got-n)
	}
	g.set(SourceAdsbRx+"/rx-1", false)
	within(t, 2*time.Second, "polls again", func() bool { return polls.Load() > n+2 })
}

// The receiver silent: unavailable since its last answer (or the start),
// with why; a frozen clock is stale; a refusing receiver counted.
func TestEconspicuityUnavailableAndStale(t *testing.T) {
	var mode atomic.Int32
	frozen := float64(time.Now().Unix())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode.Load() {
		case 0:
			_, _ = w.Write(doc(float64(time.Now().UnixNano())/1e9, ""))
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			_, _ = w.Write(doc(frozen, ""))
		case 3:
			_, _ = w.Write([]byte(`{"now":"x"}`))
		}
	}))
	t.Cleanup(srv.Close)
	e := &Econspicuity{Source: srv.URL, ReceiverID: "rx-1", Policy: fastPolicy, Every: 20 * time.Millisecond}
	if b := e.Status(); b.State != sources.StateDown || !strings.Contains(b.Detail, "not answered yet") {
		t.Fatalf("before %+v", b)
	}
	run(t, e.Run)
	within(t, 2*time.Second, "live", func() bool { return e.Status().State == sources.StateLive })
	mode.Store(1)
	within(t, 3*time.Second, "down", func() bool { return e.Status().State == sources.StateDown })
	if st, d := e.Probe(context.Background()); st != obs.StateDown || !strings.Contains(d, "500") {
		t.Fatalf("probe %s %s", st, d)
	}
	mode.Store(2)
	within(t, 3*time.Second, "stale", func() bool { return e.Status().State == sources.StateStale })
	mode.Store(3)
	within(t, 2*time.Second, "unreadable counted", func() bool { return e.Counters.Get(CounterAdsbUnreadable) > 0 })
	bad := &Econspicuity{Source: "ftp://x", ReceiverID: "rx-1", StatusEvery: 10 * time.Millisecond}
	run(t, bad.Run)
	within(t, time.Second, "refused source", func() bool { return strings.Contains(bad.Status().Detail, "USSP_ADSB_SOURCE") })
}

// A recorded file replayed at its own spacing, every document re-based
// onto our clock: the tracks' times are now, not the recording's; the
// replay's end is unavailable since then.
func TestEconspicuityReplaysARecordedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "replay.jsonl")
	rec := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)
	var lines []string
	for i := 0; i < 3; i++ {
		lines = append(lines, string(doc(float64(rec.Unix())+float64(i)*0.05,
			fmt.Sprintf(`{"hex":"4ca0%02d","alt_baro":3000,"lat":41.7,"lon":44.8,"seen_pos":0}`, i))))
	}
	lines = append(lines, "", "not json")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	start := time.Now()
	e := &Econspicuity{Source: "file://" + p, ReceiverID: "rx-1", Sink: s, Policy: fastPolicy}
	run(t, e.Run)
	within(t, 3*time.Second, "replayed", func() bool { return e.Status().State == sources.StateDown && len(s.tracks()) == 3 })
	for _, tr := range s.tracks() {
		if tr.CapturedAt.Before(start.Add(-time.Second)) || tr.TS == nil || tr.TS.Before(start.Add(-time.Second)) {
			t.Fatalf("not re-based: %+v", tr.Envelope)
		}
	}
	if b := e.Status(); !strings.Contains(b.Detail, "replay file ended") || e.Counters.Get(CounterAdsbUnreadable) != 1 {
		t.Fatalf("end %+v %v", b, e.Counters.Snapshot())
	}
	missing := &Econspicuity{Source: "file://" + filepath.Join(dir, "none.jsonl"), ReceiverID: "rx-1"}
	run(t, missing.Run)
	within(t, time.Second, "missing file", func() bool { return strings.Contains(missing.Status().Detail, "replay file") })
}

// BaseStation lines: identity, velocity and squawk joined to the
// positions that follow; a position published placed at arrival; an
// emergency squawk an emergency; bad lines counted. A switched-off
// receiver closes the connection (E-01 pair).
func TestEconspicuityReadsBaseStationLines(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func(c net.Conn) {
				for _, l := range []string{
					"MSG,1,1,1,4CA7B5,1,2026/10/04,12:00:00.000,2026/10/04,12:00:00.000,GEO123  ,,,,,,,,,,,",
					"MSG,4,1,1,4CA7B5,1,2026/10/04,12:00:00.000,2026/10/04,12:00:00.000,,,120,90,,,-640,,,,,0",
					"MSG,6,1,1,4CA7B5,1,2026/10/04,12:00:00.000,2026/10/04,12:00:00.000,,,,,,,,7700,0,1,0,0",
					"MSG,3,1,1,4CA7B5,1,2026/10/04,12:00:00.000,2026/10/04,12:00:00.000,,3000,,,41.75,44.85,,,0,0,0,0",
					"MSG,3,1,1,4CA7B6,1,2026/10/04,12:00:00.000,2026/10/04,12:00:00.000,,3000,,,,,,,0,0,0,0",
					"MSG,3,1,1,ZZZZZZ,1,,,,,,,,,,,,,,,,",
					"STA,1,1,1,4CA7B5",
					strings.Repeat("x", 2*MaxSBSLineBytes),
				} {
					if _, err := c.Write([]byte(l + "\r\n")); err != nil {
						return
					}
				}
				buf := make([]byte, 1)
				_, _ = c.Read(buf)
			}(c)
		}
	}()
	s := &sink{}
	g := &gate{}
	e := &Econspicuity{Source: "sbs://" + ln.Addr().String(), ReceiverID: "rx-1", Sink: s, Gate: g, Policy: fastPolicy,
		GateEvery: 10 * time.Millisecond, RetryMin: 10 * time.Millisecond}
	run(t, e.Run)
	within(t, 3*time.Second, "a position", func() bool { return len(s.tracks()) >= 1 })
	b := s.tracks()[0].Body
	if b.ICAO24 != "4ca7b5" || b.Callsign == nil || *b.Callsign != "GEO123" || b.Emergency == nil || !*b.Emergency ||
		b.Squawk == nil || *b.Squawk != "7700" || b.TrackDeg == nil || *b.TrackDeg != 90 {
		t.Fatalf("body %+v", b)
	}
	approx(t, "alt_pressure_m", b.AltPressureM, 914.4)
	approx(t, "gs_ms", b.GSMS, 120*1852.0/3600)
	approx(t, "vrate_ms", b.VRateMS, -640*0.3048/60)
	if s.tracks()[0].TimeSource != core.TimeSystem {
		t.Fatalf("not placed at arrival: %s", s.tracks()[0].TimeSource)
	}
	within(t, 2*time.Second, "bad lines counted", func() bool {
		return e.Counters.Get(CounterAdsbUnreadable) >= 2 && e.Counters.Get(CounterAdsbNonICAO) >= 1 && e.Counters.Get(CounterAdsbNoPosition) >= 1
	})
	g.set(SourceAdsbRx, true)
	within(t, 2*time.Second, "closed", func() bool { return e.Counters.Get(CounterAdsbSwitchedOff) == 1 })
	mu.Lock()
	n := len(conns)
	mu.Unlock()
	g.set(SourceAdsbRx, false)
	within(t, 3*time.Second, "reconnected", func() bool { mu.Lock(); defer mu.Unlock(); return len(conns) > n })
}

// E-10: past MaxSBSAircraft a new aircraft is refused and counted until
// the silent ones are forgotten.
func TestEconspicuityBoundsBaseStationAircraft(t *testing.T) {
	now := time.Now()
	e := &Econspicuity{ReceiverID: "rx-1", Sink: &sink{}}
	for i := 0; i <= MaxSBSAircraft; i++ {
		e.TakeSBS(context.Background(), fmt.Sprintf("MSG,1,1,1,%06x,1,,,,,CS,,,,,,,,,,,", i), now)
	}
	if e.Counters.Get(CounterAdsbOverBound) != 1 {
		t.Fatalf("over bound %v", e.Counters.Snapshot())
	}
	e.TakeSBS(context.Background(), "MSG,1,1,1,fffffe,1,,,,,CS,,,,,,,,,,,", now.Add(time.Minute))
	if e.Counters.Get(CounterAdsbOverBound) != 1 {
		t.Fatal("the silent ones were not forgotten")
	}
}

// E-10: a document over MaxDocAircraft takes the first MaxDocAircraft
// and counts the rest.
func TestEconspicuityBoundsADocument(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxDocAircraft+3; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"hex":"%06x"}`, i)
	}
	e := &Econspicuity{ReceiverID: "rx-1", Sink: &sink{}}
	if err := e.TakeDocument(context.Background(), doc(1, b.String()), time.Now()); err != nil {
		t.Fatal(err)
	}
	if e.Counters.Get(CounterAdsbOverBound) != 3 || e.Counters.Get(CounterAdsbRecords) != MaxDocAircraft {
		t.Fatalf("%v", e.Counters.Snapshot())
	}
	for _, bad := range []string{`[]`, `{"now":1}`, `{"now":-1,"aircraft":[]}`} {
		if err := e.TakeDocument(context.Background(), []byte(bad), time.Now()); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// The echo guard on the receiver: a registration (r) or callsign of an
// own flight left out; another published.
func TestEconspicuityEchoGuard(t *testing.T) {
	s := &sink{}
	e := &Econspicuity{ReceiverID: "rx-1", Sink: s, Own: own{reg: "4L-UAV", flight: "f1"}}
	_ = e.TakeDocument(context.Background(), doc(1, `{"hex":"4ca001","r":"4L-UAV","lat":41.7,"lon":44.8},{"hex":"4ca002","flight":"4LUAV","lat":41.7,"lon":44.8},
		{"hex":"4ca003","flight":"OTHER","lat":41.7,"lon":44.8}`), time.Now())
	if len(s.tracks()) != 1 || s.tracks()[0].Body.ICAO24 != "4ca003" || e.Counters.Get(CounterEchoOwnFlight) != 2 {
		t.Fatalf("tracks %+v counters %v", s.tracks(), e.Counters.Snapshot())
	}
	s.err = errSink
	_ = e.TakeDocument(context.Background(), doc(1, `{"hex":"4ca004","lat":41.7,"lon":44.8}`), time.Now())
	if e.Status().Counters["refused"] != 1 {
		t.Fatalf("a refused publish not counted: %+v", e.Status())
	}
}

// flatGeoid is a geoid 20 m above the ellipsoid everywhere.
type flatGeoid struct{}

func (flatGeoid) UndulationM(core.LatLon) (float64, error) { return 20, nil }
