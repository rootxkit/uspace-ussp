package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rootxkit/uspace-core/core"
	"go.opentelemetry.io/otel"
)

// switchable is a probe whose answer the test sets.
type switchable struct {
	mu     sync.Mutex
	state  State
	detail string
}

func (s *switchable) set(st State, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state, s.detail = st, detail
}

func (s *switchable) probe(context.Context) (State, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.detail
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestHealth(t *testing.T) (*Health, *clock, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	h := NewHealth(NewLogger(&logs, "info", "test"), 0)
	clk := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	h.SetClock(clk.now)
	return h, clk, &logs
}

// E-02: take each dependency away and read the report; bring it back
// and read it again.
func TestReadinessFollowsTheDependencies(t *testing.T) {
	h, clk, logs := newTestHealth(t)
	pg, nats, ts := &switchable{state: StateUp}, &switchable{state: StateUp}, &switchable{state: StateUp}
	h.Register("postgres", true, pg.probe)
	h.Register("nats", true, nats.probe)
	h.Register("timescaledb", false, ts.probe)

	before := h.Snapshot()
	if before.Status != StatusNotReady || before.Dependencies["nats"].State != StateUnknown || before.Dependencies["nats"].AgeS != nil {
		t.Fatalf("an unchecked dependency must read unknown and not ready: %+v", before)
	}

	rep := h.Check(context.Background())
	if rep.Status != StatusReady || !rep.Ready() || len(rep.Degraded) != 0 {
		t.Fatalf("all up: %+v", rep)
	}
	if age := rep.Dependencies["postgres"].AgeS; age == nil || *age != 0 {
		t.Fatalf("an up dependency has age 0: %v", age)
	}

	clk.advance(10 * time.Second)
	nats.set(StateDown, "not connected (RECONNECTING)")
	rep = h.Check(context.Background())
	n := rep.Dependencies["nats"]
	if rep.Status != StatusNotReady || rep.Ready() || n.State != StateDown || n.Detail != "not connected (RECONNECTING)" ||
		!n.Since.Equal(clk.now()) || !slices.Equal(rep.Degraded, []string{"nats"}) {
		t.Fatalf("required down: %+v", rep)
	}
	clk.advance(5 * time.Second)
	if age := h.Snapshot().Dependencies["nats"].AgeS; age == nil || *age != 15 {
		t.Fatalf("age since last up: %v", age)
	}
	if !strings.Contains(logs.String(), `"dep":"nats","from":"up","to":"down"`) {
		t.Fatalf("the transition was not logged: %s", logs.String())
	}

	nats.set(StateUp, "")
	ts.set(StateDown, "connection refused")
	rep = h.Check(context.Background())
	if rep.Status != StatusDegraded || !rep.Ready() || !slices.Equal(rep.Degraded, []string{"timescaledb"}) {
		t.Fatalf("optional down: %+v", rep)
	}

	ts.set(StateUp, "")
	pg.set(StateDegraded, "connected; extension postgis is not installed")
	rep = h.Check(context.Background())
	if rep.Status != StatusDegraded || !rep.Ready() || rep.Dependencies["nats"].Detail != "" {
		t.Fatalf("required degraded: %+v", rep)
	}

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status":"degraded"`, `"degraded":["postgres"]`, `"required":true`, `"since":"2026-10-02T12:00:15Z"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s missing from %s", want, b)
		}
	}
}

func TestAProbeThatOverrunsItsBoundIsDown(t *testing.T) {
	h := NewHealth(Discard(), 20*time.Millisecond)
	release := make(chan struct{})
	defer close(release)
	h.Register("nats", true, func(context.Context) (State, string) {
		<-release // ignores its context
		return StateUp, ""
	})
	rep := h.Check(context.Background())
	if d := rep.Dependencies["nats"]; d.State != StateDown || !strings.Contains(d.Detail, "within 20ms") || rep.Ready() {
		t.Fatalf("got %+v", rep)
	}
}

func TestSetReportsAPushedState(t *testing.T) {
	h, _, _ := newTestHealth(t)
	h.Set("cisp", StateDegraded, "last pull 900 s ago")
	if d := h.Snapshot().Dependencies["cisp"]; d.State != StateDegraded || d.Required || d.Detail != "last pull 900 s ago" {
		t.Fatalf("got %+v", d)
	}
}

func TestDependencyMetrics(t *testing.T) {
	h, clk, _ := newTestHealth(t)
	nats := &switchable{state: StateUp}
	h.Register("nats", true, nats.probe)
	h.Register("postgres", true, func(context.Context) (State, string) { return StateDown, "refused" })
	reg := prometheus.NewRegistry()
	reg.MustRegister(h)
	h.Check(context.Background())
	clk.advance(3 * time.Second)
	nats.set(StateDown, "gone")
	h.Check(context.Background())
	clk.advance(2 * time.Second)

	got := gather(t, reg)
	if got["ussp_dependency_up/nats"] != 0 || got["ussp_dependency_age_s/nats"] != 5 ||
		got["ussp_dependency_up/postgres"] != 0 || !math.IsInf(got["ussp_dependency_age_s/postgres"], 1) {
		t.Fatalf("metrics %v", got)
	}
	nats.set(StateUp, "")
	h.Check(context.Background())
	if got := gather(t, reg); got["ussp_dependency_up/nats"] != 1 || got["ussp_dependency_age_s/nats"] != 0 {
		t.Fatalf("after recovery: %v", got)
	}
}

// gather reads every gauge of reg as name/first-label-value.
func gather(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			got[mf.GetName()+"/"+m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	return got
}

func TestBuildInfoAndCounters(t *testing.T) {
	reg := NewRegistry("api")
	c := &core.Counters{}
	c.Inc("dropped frames")
	reg.MustRegister(NewCountersCollector("http", c))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	if !names["ussp_build_info"] || !names["ussp_dropped_frames"] {
		t.Fatalf("metrics %v", names)
	}
}

func TestLoggerLevelsAndAttributes(t *testing.T) {
	var b bytes.Buffer
	l := NewLogger(&b, "warn", "monitor")
	l.Info("hidden")
	Error(context.Background(), l, "failed", errors.New("boom"), FlightID("f-1"), IntentID("i-1"), ClientID("c-1"), DroneID("TEST1"), RequestID("r-1"))
	out := b.String()
	if strings.Contains(out, "hidden") {
		t.Fatal("info logged at warn")
	}
	for _, want := range []string{`"process":"monitor"`, `"error":"boom"`, `"flight_id":"f-1"`, `"intent_id":"i-1"`, `"client_id":"c-1"`, `"drone_id":"TEST1"`, `"request_id":"r-1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("%s missing from %s", want, out)
		}
	}
	for in, want := range map[string]string{"debug": "DEBUG", "ERROR": "ERROR", "": "INFO", "x": "INFO"} {
		if ParseLevel(in).String() != want {
			t.Errorf("%q: %s", in, ParseLevel(in))
		}
	}
}

// E-11: tracing setup restores the global provider it replaced.
func TestTracingSetupRestoresTheGlobalProvider(t *testing.T) {
	before := otel.GetTracerProvider()
	on, shutdown, err := SetupTracing(context.Background(), "", "ussp-test")
	if err != nil || on {
		t.Fatalf("no endpoint: on=%v err=%v", on, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != before {
		t.Fatal("global provider not restored")
	}
	if _, _, err := SetupTracing(context.Background(), "collector:4318", "ussp-test"); err == nil {
		t.Fatal("a relative endpoint was accepted")
	}
	on, shutdown, err = SetupTracing(context.Background(), "http://127.0.0.1:1/v1/traces", "ussp-test")
	if err != nil || !on {
		t.Fatalf("endpoint: on=%v err=%v", on, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(ctx) // the collector is absent; only the restore matters here
	if otel.GetTracerProvider() != before {
		t.Fatal("global provider not restored after an exporter")
	}
}

// lockedBuffer is a log sink the test may read while the status loop
// writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestStatusLineCarriesCountersAndDependencies(t *testing.T) {
	var b lockedBuffer
	h := NewHealth(Discard(), 0)
	h.Register("nats", true, func(context.Context) (State, string) { return StateDown, "refused" })
	s := &Status{Logger: NewLogger(&b, "info", "api"), Interval: time.Hour, Health: h}
	c := &core.Counters{}
	c.Inc("http_body_too_large")
	s.Add("http", c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(b.String(), `"msg":"status"`) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	out := b.String()
	for _, want := range []string{`"readiness":"not_ready"`, `"dependencies":{"nats":"down"}`, `"http_body_too_large":1`, `"uptime_s":0`} {
		if !strings.Contains(out, want) {
			t.Errorf("%s missing from %s", want, out)
		}
	}
}

// An up dependency keeps what its probe says it is up with (the CIS
// cache: its versions and age), and says nothing when the probe says
// nothing (E-01 pair).
func TestUpKeepsItsDetail(t *testing.T) {
	h, _, _ := newTestHealth(t)
	cis := &switchable{}
	cis.set(StateUp, "zones:3, age 12 s")
	quiet := &switchable{}
	quiet.set(StateUp, "")
	h.Register("cis", false, cis.probe)
	h.Register("nats", true, quiet.probe)
	rep := h.Check(context.Background())
	if d := rep.Dependencies["cis"]; d.State != StateUp || d.Detail != "zones:3, age 12 s" || d.AgeS == nil || *d.AgeS != 0 {
		t.Fatalf("cis: %+v", d)
	}
	if d := rep.Dependencies["nats"]; d.State != StateUp || d.Detail != "" {
		t.Fatalf("nats: %+v", d)
	}
	if rep.Status != StatusReady {
		t.Fatalf("status %s", rep.Status)
	}
}
