package testif

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/national/client"
	inj "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/ridinjection"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

func labInjection(t *testing.T) inj.CreateTestParameters {
	t.Helper()
	b, err := os.ReadFile("testdata/injection-tbilisi.json")
	if err != nil {
		t.Fatal(err)
	}
	var p inj.CreateTestParameters
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// The mapping of one injected state, member by member, on the lab's
// tbilisi_flights_data shape: F3411's special values (alt and height
// -1000, speed 255, track 361, vertical speed 63) are unknown (null), as
// uspace-core reads them; the others keep the sender's decimals.
func TestFrameOfMapsTheInjectedState(t *testing.T) {
	p := labInjection(t)
	op := &client.TelemetryOperatorPosition{Lat: 41.7151, Lng: 44.8271}
	f, err := FrameOf(p.RequestedFlights[0].Telemetry[0], "TEST-INJ-0001", "e", 7, op)
	if err != nil {
		t.Fatal(err)
	}
	ptr := func(v float64) *float64 { return &v }
	ep := "e"
	// The lab's states carry their height in the state's (v19) member,
	// not in the position: F3411-22a has none to send
	// (TestFrameOfTakesTheHeightFromThePosition).
	want := client.TelemetryFrame{
		Ts: time.Date(2026, 10, 13, 8, 0, 1, 500_000_000, time.UTC), Serial: "TEST-INJ-0001", Seq: 7, Epoch: &ep,
		Position: client.TelemetryPosition{Lat: 41.7161, Lng: 44.8281}, AltWgs84M: ptr(652.5), AltPressureM: ptr(640),
		SpeedMs: ptr(1.9), TrackDeg: ptr(90), VspeedMs: ptr(0.2),
		Status: client.TelemetryFrameStatusAirborne, AccuracyH: client.HA1m, AccuracyV: client.VA3m,
		TimestampAccuracyS: ptr(0.1), OperatorPosition: op,
	}
	if !reflect.DeepEqual(f, want) {
		got, _ := json.Marshal(f)
		exp, _ := json.Marshal(want)
		t.Fatalf("frame\n%s\nwant\n%s", got, exp)
	}
	special, err := FrameOf(p.RequestedFlights[0].Telemetry[1], "S", "e", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if special.AltWgs84M != nil || special.HeightM != nil || special.HeightRef != nil || special.SpeedMs != nil ||
		special.TrackDeg != nil || special.VspeedMs != nil {
		b, _ := json.Marshal(special)
		t.Errorf("special values are not unknown: %s", b)
	}
	ground, err := FrameOf(p.RequestedFlights[0].Telemetry[2], "S", "e", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ground.Status != client.TelemetryFrameStatusGround || ground.AccuracyH != client.HAUnknown || ground.AccuracyV != client.VAUnknown ||
		ground.Emergency || ground.TimestampAccuracyS != nil {
		t.Errorf("absent accuracies are unknown, Ground is no emergency: %+v", ground)
	}
	em, err := FrameOf(p.RequestedFlights[1].Telemetry[0], "S", "e", 0, nil)
	if err != nil || !em.Emergency || em.Status != client.TelemetryFrameStatusEmergency {
		t.Errorf("Emergency: %+v %v", em, err)
	}
	// Every frame, as the socket carries it, is a valid telemetry/v1
	// message as telemetry-ingest decodes it.
	for _, fr := range []client.TelemetryFrame{f, special, ground, em} {
		b, _ := json.Marshal(outgoing{Schema: schemaTelemetry, Body: fr})
		if _, err := telemetry.DecodeMessage(b); err != nil {
			t.Errorf("telemetry-ingest refuses the frame %s: %v", b, err)
		}
	}
}

// A state the file allows but that cannot be a sample is refused,
// naming it, before anything is sent; so is an unknown enumeration value.
func TestFrameOfRefusesWhatCannotBeASample(t *testing.T) {
	ts := time.Now()
	lat, lng := 41.7, 44.8
	bad := inj.RIDOperationalStatus("Hovering")
	for name, st := range map[string]inj.RIDAircraftState{
		"no timestamp": {Position: &inj.RIDAircraftPosition{Lat: &lat, Lng: &lng}},
		"no position":  {Timestamp: &ts},
		"no lng":       {Timestamp: &ts, Position: &inj.RIDAircraftPosition{Lat: &lat}},
		"status":       {Timestamp: &ts, Position: &inj.RIDAircraftPosition{Lat: &lat, Lng: &lng}, OperationalStatus: &bad},
	} {
		if _, err := FrameOf(st, "S", "e", 0, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The status members the injection reads are telemetry-ingest's own
// (its ConsoleStatusBody marshals them under these names).
func TestStatusMembersAreTheIngests(t *testing.T) {
	b, err := json.Marshal(telemetry.ConsoleStatusBody{Accepted: 3, Refused: 2, Dropped: 1, Outcomes: map[string]uint64{"accepted": 3, "refused_unbound": 2, "dropped_rate": 1}})
	if err != nil {
		t.Fatal(err)
	}
	var st ingestStatus
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Accepted != 3 || st.Refused != 2 || st.Dropped != 1 || st.Outcomes["refused_unbound"] != 2 {
		t.Fatalf("%+v from %s", st, b)
	}
	if got := st.refusals(ingestStatus{}); !slices.Equal(got, []string{"dropped_rate", "refused_unbound"}) {
		t.Errorf("refusals %v", got)
	}
}

func put(t *testing.T, ts *testServer, id string, body any) *httptestResult {
	t.Helper()
	w := ts.do(t, http.MethodPut, BaseInjection+"/tests/"+id, ScopeInjectTestData, body)
	return &httptestResult{code: w.Code, body: w.Body.Bytes()}
}

type httptestResult struct {
	code int
	body []byte
}

// An injection is bound, streamed and acknowledged on the real path:
// each flight's serial (its own, else the fixture's pool) bound through
// the portal route, one socket per flight with the operator's token, the
// frames in time order, past ones as backlog, then an end frame; the
// answer names every injection id and the version; the delete answers
// the flights.
func TestInjectionStreamsTheFlightsAndEnds(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	p := labInjection(t)
	res := put(t, ts, "test-1", p)
	if res.code != http.StatusOK {
		t.Fatalf("PUT %d %s", res.code, res.body)
	}
	var ch inj.ChangeTestResponse
	if err := json.Unmarshal(res.body, &ch); err != nil || ch.Version == "" || len(ch.InjectedFlights) != 2 ||
		ch.InjectedFlights[0].InjectionId != p.RequestedFlights[0].InjectionId {
		t.Fatalf("%s %v", res.body, err)
	}
	ts.inject.work.Wait() // every state is in the past: the streams end on their own
	f.mu.Lock()
	bindings := slices.Clone(f.bindings)
	sockets := f.sockets
	f.mu.Unlock()
	if !slices.Equal(bindings, []string{"TEST-INJ-0001", "TEST-WP21-0001"}) || sockets != 2 {
		t.Errorf("bindings %v (the injected serial, then the pool's first), sockets %d", bindings, sockets)
	}
	bySerial := map[string][]client.TelemetryFrame{}
	for _, fr := range f.received() {
		bySerial[fr.Serial] = append(bySerial[fr.Serial], fr)
	}
	one := bySerial["TEST-INJ-0001"]
	if len(one) != 4 {
		t.Fatalf("flight 1: %d frames, want 3 states and the end", len(one))
	}
	for i, fr := range one {
		if fr.Seq != int64(i) || (i > 0 && !fr.Ts.After(one[i-1].Ts)) {
			t.Errorf("frame %d: seq %d ts %s out of order", i, fr.Seq, fr.Ts)
		}
		if i < 3 && (fr.Backlog == nil || !*fr.Backlog) {
			t.Errorf("frame %d of a past state is not backlog", i)
		}
		if (fr.End != nil && *fr.End) != (i == 3) {
			t.Errorf("frame %d end %v", i, fr.End)
		}
	}
	if one[0].Status != client.TelemetryFrameStatusGround {
		t.Error("the earliest state is not sent first")
	}
	// The end frame is no new measurement: its sample is the last one's
	// a millisecond later, not the time it was sent (the states here are
	// a day old), so the SP's last timestamp stays the injected one.
	if !one[3].Ts.Equal(one[2].Ts.Add(time.Millisecond)) || one[3].Position != one[2].Position {
		t.Errorf("end frame at %s %+v, want the last state's %s + 1 ms at %+v", one[3].Ts, one[3].Position, one[2].Ts, one[2].Position)
	}
	if two := bySerial["TEST-WP21-0001"]; len(two) != 2 || two[1].End == nil || !*two[1].End {
		t.Errorf("flight 2 frames %+v", two)
	}
	del := ts.do(t, http.MethodDelete, BaseInjection+"/tests/test-1/"+ch.Version, ScopeInjectTestData, nil)
	var dr inj.DeleteTestResponse
	if del.Code != http.StatusOK || json.Unmarshal(del.Body.Bytes(), &dr) != nil || len(dr.InjectedFlights) != 2 {
		t.Fatalf("DELETE %d %s", del.Code, del.Body)
	}
	if n := len(f.received()); n != 6 {
		t.Errorf("the delete of ended flights sent more frames: %d", n)
	}
	if again := ts.do(t, http.MethodDelete, BaseInjection+"/tests/test-1/"+ch.Version, ScopeInjectTestData, nil); again.Code != http.StatusNotFound {
		t.Errorf("a second delete %d", again.Code)
	}
}

// A refusal on the real path is the injection's failure with the USSP's
// reason, and nothing is retried past it: an unbound serial (no portal
// session, so nothing binds) is refused_unbound; a serial bound to
// another client is the binding's 409; a switched-off operator_ws source
// refuses the socket. The accepted twin is TestInjectionStreamsTheFlightsAndEnds.
func TestInjectionFailsWithTheUSSPsReason(t *testing.T) {
	p := labInjection(t)
	p.RequestedFlights = p.RequestedFlights[:1]

	f := newFakeUSSP(t)
	ts := newTestServer(t, f, func(c *Config) { c.SessionFile = "" })
	ts.USSP.Session = ""
	res := put(t, ts, "unbound", p)
	if res.code != http.StatusBadRequest || !strings.Contains(string(res.body), "refused_unbound") {
		t.Fatalf("unbound: %d %s", res.code, res.body)
	}
	ts.inject.work.Wait()
	if n := len(f.received()); n != 1 {
		t.Errorf("the refused flight sent %d frames; nothing goes past the refusal", n)
	}
	if again := put(t, ts, "unbound", p); again.code != http.StatusBadRequest {
		t.Errorf("the failed test is still held: %d", again.code)
	}

	f2 := newFakeUSSP(t)
	f2.elsewhere["TEST-INJ-0001"] = true
	ts2 := newTestServer(t, f2, nil)
	res = put(t, ts2, "elsewhere", p)
	if res.code != http.StatusBadRequest || !strings.Contains(string(res.body), "serial_bound_elsewhere") || f2.sockets != 0 {
		t.Fatalf("bound elsewhere: %d %s sockets %d", res.code, res.body, f2.sockets)
	}

	f3 := newFakeUSSP(t)
	f3.sourceDisabled = true
	ts3 := newTestServer(t, f3, nil)
	res = put(t, ts3, "disabled", p)
	if res.code != http.StatusServiceUnavailable || !strings.Contains(string(res.body), "source_disabled") {
		t.Fatalf("source disabled: %d %s", res.code, res.body)
	}
	if n := len(f3.received()); n != 0 {
		t.Errorf("frames sent through a refused socket: %d", n)
	}
}

// The request is validated before any side effect: a state that cannot
// be a sample, too many flights or states, a repeated test id, more
// tests than held (E-10: one past each bound).
func TestInjectionValidatesBeforeAnySideEffect(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, func(c *Config) { c.MaxTests = 1; c.MaxFlightsPerTest = 2; c.MaxStatesFlight = 3 })
	p := labInjection(t)
	broken := labInjection(t)
	broken.RequestedFlights[1].Telemetry[0].Timestamp = nil
	res := put(t, ts, "broken", broken)
	if res.code != http.StatusBadRequest || !strings.Contains(string(res.body), "requested_flights[1].telemetry[0].timestamp") {
		t.Fatalf("%d %s", res.code, res.body)
	}
	many := labInjection(t)
	many.RequestedFlights = append(many.RequestedFlights, many.RequestedFlights[1])
	if res := put(t, ts, "many", many); res.code != http.StatusBadRequest {
		t.Errorf("three flights past a bound of two: %d", res.code)
	}
	long := labInjection(t)
	long.RequestedFlights[0].Telemetry = append(long.RequestedFlights[0].Telemetry, long.RequestedFlights[0].Telemetry[0])
	if res := put(t, ts, "long", long); res.code != http.StatusBadRequest {
		t.Errorf("four states past a bound of three: %d", res.code)
	}
	if res := put(t, ts, "garbage", "{"); res.code != http.StatusBadRequest {
		t.Errorf("a malformed body: %d", res.code)
	}
	f.mu.Lock()
	if len(f.bindings) != 0 || f.sockets != 0 || len(f.frames) != 0 {
		t.Errorf("side effects of refused requests: %v %d %d", f.bindings, f.sockets, len(f.frames))
	}
	f.mu.Unlock()
	if res := put(t, ts, "ok", p); res.code != http.StatusOK {
		t.Fatalf("the bound itself: %d %s", res.code, res.body)
	}
	if res := put(t, ts, "ok", p); res.code != http.StatusConflict {
		t.Errorf("the same test id again: %d", res.code)
	}
	if res := put(t, ts, "second", p); res.code != http.StatusTooManyRequests {
		t.Errorf("a second test past a bound of one: %d", res.code)
	}
	if w := ts.do(t, http.MethodDelete, BaseInjection+"/tests/ok/not-the-version", ScopeInjectTestData, nil); w.Code != http.StatusNotFound {
		t.Errorf("a delete with another version: %d", w.Code)
	}
}

// recordingClock is now t0 and records every wait without waiting.
type recordingClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Time
}

func (c *recordingClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *recordingClock) Until(ctx context.Context, t time.Time) error {
	c.mu.Lock()
	c.waits = append(c.waits, t)
	if t.After(c.now) {
		c.now = t // the wall clock reaches the frame's time
	}
	c.mu.Unlock()
	return ctx.Err()
}

// A live flight goes at its timestamps: each frame waits for its own
// time and is not backlog; the end frame comes after the last state.
func TestInjectionSendsLiveStatesAtTheirTimestamps(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	p := labInjection(t)
	p.RequestedFlights = p.RequestedFlights[:1]
	clock := &recordingClock{now: time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)}
	ts.inject.clock = clock
	if res := put(t, ts, "live", p); res.code != http.StatusOK {
		t.Fatalf("%d %s", res.code, res.body)
	}
	ts.inject.work.Wait()
	frames := f.received()
	if len(frames) != 4 {
		t.Fatalf("%d frames", len(frames))
	}
	clock.mu.Lock()
	waits := slices.Clone(clock.waits)
	clock.mu.Unlock()
	for i, fr := range frames[:3] {
		if fr.Backlog != nil {
			t.Errorf("live frame %d is backlog", i)
		}
		if i >= len(waits) || !waits[i].Equal(fr.Ts) {
			t.Errorf("frame %d at %s was not sent at its time (waits %v)", i, fr.Ts, waits)
		}
	}
	if end := frames[3]; end.End == nil || !end.Ts.After(frames[2].Ts) {
		t.Errorf("end frame %+v", end)
	}
}

// user_notifications answers the empty list for a valid window and
// refuses a missing or malformed one.
func TestUserNotifications(t *testing.T) {
	ts := newTestServer(t, newFakeUSSP(t), nil)
	base := BaseInjection + "/user_notifications"
	w := ts.do(t, http.MethodGet, base+"?after=2026-10-13T08:00:00Z&before=2026-10-13T09:00:00Z", ScopeInjectTestData, nil)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"user_notifications":[]}` {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	for _, q := range []string{"", "?after=yesterday", "?after=2026-10-13T08:00:00Z&before=2026-10-13T07:00:00Z"} {
		if w := ts.do(t, http.MethodGet, base+q, ScopeInjectTestData, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", q, w.Code)
		}
	}
	if w := ts.do(t, http.MethodGet, base+"?after=2026-10-13T08:00:00Z", ScopePlan, nil); w.Code != http.StatusForbidden {
		t.Errorf("another scope: %d", w.Code)
	}
}

// serialsOf is the serials the frames of a test's version flew.
func serialsOf(f *fakeUSSP, version string) []string {
	var out []string
	frames := f.received()
	for i := range frames {
		fr := &frames[i]
		if fr.Epoch != nil && strings.HasPrefix(*fr.Epoch, "t"+version+"-") && !slices.Contains(out, fr.Serial) {
			out = append(out, fr.Serial)
		}
	}
	slices.Sort(out)
	return out
}

// Each held test has its own serials: a second test without serials
// takes the next of the pool, not the first test's; a serial a held test
// flies is refused for another, before any side effect; once a test is
// deleted its serials are free again. The single-test twin is
// TestInjectionStreamsTheFlightsAndEnds.
func TestInjectionGivesEachTestItsOwnSerial(t *testing.T) {
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	version := func(res *httptestResult) string {
		t.Helper()
		var ch inj.ChangeTestResponse
		if res.code != http.StatusOK || json.Unmarshal(res.body, &ch) != nil {
			t.Fatalf("PUT %d %s", res.code, res.body)
		}
		return ch.Version
	}
	a := version(put(t, ts, "a", labInjection(t)))
	pooled := labInjection(t)
	pooled.RequestedFlights = pooled.RequestedFlights[1:] // names no serial
	b := version(put(t, ts, "b", pooled))
	ts.inject.work.Wait()
	if got := serialsOf(f, a); !slices.Equal(got, []string{"TEST-INJ-0001", "TEST-WP21-0001"}) {
		t.Errorf("test a flew %v", got)
	}
	if got := serialsOf(f, b); !slices.Equal(got, []string{"TEST-WP21-0002"}) {
		t.Errorf("test b flew %v, want the pool's next serial, not test a's", got)
	}
	own := labInjection(t)
	own.RequestedFlights = own.RequestedFlights[:1] // TEST-INJ-0001, which test a flies
	sockets := f.sockets
	if res := put(t, ts, "c", own); res.code != http.StatusBadRequest || !strings.Contains(string(res.body), "another held test") {
		t.Errorf("a serial test a flies: %d %s", res.code, res.body)
	}
	f.mu.Lock()
	if f.sockets != sockets {
		t.Errorf("the refused test opened %d sockets", f.sockets-sockets)
	}
	f.mu.Unlock()
	if w := ts.do(t, http.MethodDelete, BaseInjection+"/tests/a/"+a, ScopeInjectTestData, nil); w.Code != http.StatusOK {
		t.Fatalf("DELETE %d %s", w.Code, w.Body)
	}
	c := version(put(t, ts, "c", own))
	d := version(put(t, ts, "d", pooled))
	ts.inject.work.Wait()
	if got := serialsOf(f, c); !slices.Equal(got, []string{"TEST-INJ-0001"}) {
		t.Errorf("test c flew %v once test a was deleted", got)
	}
	if got := serialsOf(f, d); !slices.Equal(got, []string{"TEST-WP21-0001"}) {
		t.Errorf("test d flew %v, want the serial test a freed", got)
	}
}

// gatedClock lets the first frame go at once and holds every later one
// until open is closed: the injection is answered with the flight still
// in the air.
type gatedClock struct {
	mu    sync.Mutex
	now   time.Time
	first time.Time
	open  chan struct{}
}

func (c *gatedClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *gatedClock) Until(ctx context.Context, t time.Time) error {
	if t.After(c.first) {
		select {
		case <-c.open:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	if t.After(c.now) {
		c.now = t
	}
	c.mu.Unlock()
	return ctx.Err()
}

// A frame the ingest refuses after the injection was answered fails the
// test: nothing past the refusal is sent, the flight gets its end frame,
// the failure is counted, and the test stays held until its delete,
// which still answers. The accepted twin, every frame accepted and sent,
// is
// TestInjectionStreamsTheFlightsAndEnds.
func TestInjectionFailsOnARefusalInFlight(t *testing.T) {
	p := labInjection(t)
	p.RequestedFlights = p.RequestedFlights[:1] // three states, seq 0 to 2

	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	t0 := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	clock := &gatedClock{now: t0, first: t0.Add(500 * time.Millisecond), open: make(chan struct{})}
	ts.inject.clock = clock
	res := put(t, ts, "live", p)
	if res.code != http.StatusOK {
		t.Fatalf("PUT %d %s", res.code, res.body)
	}
	var ch inj.ChangeTestResponse
	if err := json.Unmarshal(res.body, &ch); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.refuseFrom, f.refuseFromSeq = true, 1
	f.mu.Unlock()
	close(clock.open)
	ts.inject.work.Wait()
	frames := f.received()
	var seqs []int64
	ended := false
	for _, fr := range frames {
		if fr.End != nil && *fr.End {
			ended = true
			// At the last accepted sample, numbered after the refused one.
			if fr.Seq != 2 || fr.Position != frames[0].Position {
				t.Errorf("end frame seq %d at %+v, want seq 2 at the accepted %+v", fr.Seq, fr.Position, frames[0].Position)
			}
			continue
		}
		seqs = append(seqs, fr.Seq)
	}
	if !slices.Equal(seqs, []int64{0, 1}) {
		t.Errorf("samples sent %v: want 0, then 1 (refused) and nothing past it", seqs)
	}
	if !ended {
		t.Error("the failed flight got no end frame")
	}
	if n := ts.counters.Snapshot()[CounterInjectionFailedLive]; n != 1 {
		t.Errorf("%s = %d, want 1", CounterInjectionFailedLive, n)
	}
	if again := put(t, ts, "live", p); again.code != http.StatusConflict {
		t.Errorf("the failed test is not held until its delete: %d", again.code)
	}
	n := len(frames)
	if w := ts.do(t, http.MethodDelete, BaseInjection+"/tests/live/"+ch.Version, ScopeInjectTestData, nil); w.Code != http.StatusOK {
		t.Fatalf("DELETE of the failed test %d %s", w.Code, w.Body)
	}
	if got := len(f.received()); got != n {
		t.Errorf("the delete of the ended flight sent %d more frames", got-n)
	}

}

// heldClock holds every wait for a time after now until open is closed:
// a flight scheduled later does not fly while it is held.
type heldClock struct {
	mu   sync.Mutex
	now  time.Time
	open chan struct{}
}

func (c *heldClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *heldClock) Until(ctx context.Context, t time.Time) error {
	if t.After(c.Now()) {
		select {
		case <-c.open:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	if t.After(c.now) {
		c.now = t
	}
	c.mu.Unlock()
	return ctx.Err()
}

// putWithin runs the PUT and fails the test when it does not answer
// within bound: the injection is answered before a later flight flies.
func putWithin(t *testing.T, ts *testServer, id string, body any, bound time.Duration) *httptestResult {
	t.Helper()
	got := make(chan *httptestResult, 1)
	go func() {
		w := ts.do(t, http.MethodPut, BaseInjection+"/tests/"+id, ScopeInjectTestData, body)
		got <- &httptestResult{code: w.Code, body: w.Body.Bytes()}
	}()
	select {
	case r := <-got:
		return r
	case <-time.After(bound):
		t.Fatalf("PUT %s not answered within %v", id, bound)
		return nil
	}
}

// A flight whose first state is later than now is scheduled, not
// awaited: the PUT answers at once (uss_qualifier's read timeout is
// shorter than the acknowledgement bound, and a PUT that waited for the
// first state was retried and refused out of order, WP-21 R7). Its
// frames then go at their times. The awaited twin, a first state in the
// past refused on the real path, is TestInjectionFailsWithTheUSSPsReason.
func TestInjectionAnswersBeforeALaterFlightFlies(t *testing.T) {
	p := labInjection(t)
	p.RequestedFlights = p.RequestedFlights[:1] // first state 08:00:00.5
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	clock := &heldClock{now: time.Date(2026, 10, 13, 7, 59, 58, 0, time.UTC), open: make(chan struct{})} // within the acknowledgement bound of the first state
	ts.inject.clock = clock
	res := putWithin(t, ts, "later", p, 5*time.Second)
	if res.code != http.StatusOK {
		t.Fatalf("PUT %d %s", res.code, res.body)
	}
	if n := len(f.received()); n != 0 {
		t.Fatalf("%d frames sent before the first state's time", n)
	}
	close(clock.open)
	ts.inject.work.Wait()
	frames := f.received()
	if len(frames) != 4 || frames[0].Backlog != nil || frames[3].End == nil || !*frames[3].End {
		t.Fatalf("the scheduled flight did not fly at its times: %+v", frames)
	}
}

// The first frame of a scheduled flight refused after the PUT answered
// fails the test in flight, as a later frame's refusal does: counted,
// the test held until its delete.
func TestInjectionFailsWhenAScheduledFirstFrameIsRefused(t *testing.T) {
	p := labInjection(t)
	p.RequestedFlights = p.RequestedFlights[:1]
	f := newFakeUSSP(t)
	ts := newTestServer(t, f, nil)
	clock := &heldClock{now: time.Date(2026, 10, 13, 7, 59, 58, 0, time.UTC), open: make(chan struct{})} // within the acknowledgement bound of the first state
	ts.inject.clock = clock
	res := putWithin(t, ts, "later", p, 5*time.Second)
	if res.code != http.StatusOK {
		t.Fatalf("PUT %d %s", res.code, res.body)
	}
	f.mu.Lock()
	f.refuseFrom, f.refuseFromSeq = true, 0
	f.mu.Unlock()
	close(clock.open)
	ts.inject.work.Wait()
	if n := ts.counters.Snapshot()[CounterInjectionFailedLive]; n != 1 {
		t.Errorf("%s = %d, want 1", CounterInjectionFailedLive, n)
	}
	for _, fr := range f.received() {
		if fr.Seq > 0 {
			t.Errorf("a frame past the refused first one was sent: seq %d", fr.Seq)
		}
	}
	if again := put(t, ts, "later", p); again.code != http.StatusConflict {
		t.Errorf("the failed test is not held until its delete: %d", again.code)
	}
}

// F3411-22a has height only in the position (RIDAircraftPosition.height;
// its RIDAircraftState has no height): an injected state's height member,
// the v19 place the interface file still carries, is not the 22a
// position's height and is not sent. The SP then serves the special
// height, as uss_qualifier expects for a value not injected. The twin: a
// position height is sent with its reference.
func TestFrameOfTakesTheHeightFromThePosition(t *testing.T) {
	ts := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	lat, lng := 41.7, 44.8
	stateOnly := inj.RIDAircraftState{Timestamp: &ts, Position: &inj.RIDAircraftPosition{Lat: &lat, Lng: &lng},
		Height: &inj.RIDHeight{Distance: 50, Reference: inj.RIDHeightReferenceTakeoffLocation}}
	f, err := FrameOf(stateOnly, "S", "e", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.HeightM != nil || f.HeightRef != nil {
		t.Errorf("the state's (v19) height was sent as the position's: %v %v", *f.HeightM, *f.HeightRef)
	}
	inPos := inj.RIDAircraftState{Timestamp: &ts, Position: &inj.RIDAircraftPosition{Lat: &lat, Lng: &lng,
		Height: &inj.RIDHeight{Distance: 30.5, Reference: inj.RIDHeightReferenceGroundLevel}},
		Height: &inj.RIDHeight{Distance: 50, Reference: inj.RIDHeightReferenceTakeoffLocation}}
	g, err := FrameOf(inPos, "S", "e", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.HeightM == nil || *g.HeightM != 30.5 || g.HeightRef == nil || *g.HeightRef != client.GroundLevel {
		t.Errorf("the position's height was not sent: %v %v", g.HeightM, g.HeightRef)
	}
	inPos.Position.Height.Distance = -1000 // F3411's special height: unknown
	if s, err := FrameOf(inPos, "S", "e", 0, nil); err != nil || s.HeightM != nil || s.HeightRef != nil {
		t.Errorf("the special position height is not unknown: %+v %v", s, err)
	}
}

// The ingest acknowledges an accepted sample only with its periodic
// status (every 2 s). A live flight's samples still go at their own
// times: a sample's decision is waited for only until the next one is
// due, never the whole acknowledgement bound (WP-21: waiting it put a
// 1 Hz flight's samples seconds behind their timestamps). The refused
// twin, a refusal read before the next sample, is
// TestInjectionFailsOnARefusalInFlight.
func TestInjectionDoesNotWaitForAcknowledgementsBetweenSamples(t *testing.T) {
	f := newFakeUSSP(t)
	f.quiet, f.seen = true, make(chan struct{}, 16)
	ts := newTestServer(t, f, nil) // acknowledgement bound 5 s
	p := labInjection(t)
	p.RequestedFlights = p.RequestedFlights[:1]
	t0 := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	for i := range p.RequestedFlights[0].Telemetry {
		at := t0.Add(time.Duration(i+1) * 200 * time.Millisecond)
		p.RequestedFlights[0].Telemetry[i].Timestamp = &at
	}
	ts.inject.clock = &recordingClock{now: t0}
	began := time.Now()
	if res := put(t, ts, "quiet", p); res.code != http.StatusOK {
		t.Fatalf("%d %s", res.code, res.body)
	}
	deadline := time.After(3 * time.Second) // under one 5 s bound for the three samples
	for range 3 {
		select {
		case <-f.seen:
		case <-deadline:
			t.Fatalf("the samples waited for acknowledgements the ingest gives only every 2 s: %d sent", len(f.received()))
		}
	}
	t.Logf("3 samples in %v", time.Since(began).Round(time.Millisecond))
	frames := f.received()
	for i, fr := range frames[:3] {
		if fr.Backlog != nil || fr.End != nil {
			t.Errorf("sample %d went late (backlog) or as the end", i)
		}
	}
	// The last sample's decision is still read before the end frame,
	// within the bound (the quiet fake never acknowledges it: counted).
	ts.inject.work.Wait()
	if fr := f.received(); len(fr) != 4 || fr[3].End == nil || !*fr[3].End {
		t.Fatalf("no end frame after the samples: %+v", fr)
	}
	if n := ts.counters.Snapshot()[CounterFramesNotAcked]; n != 1 {
		t.Errorf("%s = %d, want 1 (the last sample)", CounterFramesNotAcked, n)
	}
}
