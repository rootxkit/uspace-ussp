package proc

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// closedAddr is a local address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func env(kv map[string]string) config.LookupFunc {
	return func(name string) (string, bool) {
		v, ok := kv[name]
		return v, ok
	}
}

// syncBuffer is a log sink safe for the process's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var apiSpec = Spec{Process: config.ProcessAPI, Postgres: Required, TimescaleDB: Optional, NATS: Required, Migrate: true}

// start runs spec in the background and returns its base URL; the
// returned function cancels it and returns Run's result.
func start(t *testing.T, cfg config.Config, spec Spec, out *syncBuffer) (string, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, spec, Options{Out: out, Listening: func(a string) { addr <- a }})
	}()
	select {
	case a := <-addr:
		return "http://" + a, func() error { cancel(); return <-done }
	case err := <-done:
		cancel()
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Run did not listen within 10 s")
	}
	return "", nil
}

// E-02: with every dependency taken away the process still starts,
// /readyz answers 503 naming each one down, and cancelling it drains
// cleanly. Read through the generated client, so the body is the
// contract's.
func TestRunStartsDegradedAndReadyzNamesWhatIsDown(t *testing.T) {
	pg, nats := closedAddr(t), closedAddr(t)
	cfg, err := config.LoadFrom(env(map[string]string{
		"USSP_API_ADDR":                   "127.0.0.1:0",
		"USSP_PG_URL":                     "postgres://ussp:pw@" + pg + "/ussp_relational?connect_timeout=1",
		"USSP_TS_URL":                     "postgres://ussp:pw@" + pg + "/ussp_timeseries?connect_timeout=1",
		"USSP_NATS_URL":                   "nats://" + nats,
		"USSP_STATUS_INTERVAL_S":          "3600",
		"USSP_READINESS_CHECK_TIMEOUT_MS": "1500",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	base, stop := start(t, cfg, apiSpec, &logs)

	c, err := client.NewClientWithResponses(base)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.GetReadyzWithResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body := resp.JSON503
	if resp.StatusCode() != http.StatusServiceUnavailable || body == nil {
		t.Fatalf("readyz %d: %s", resp.StatusCode(), resp.Body)
	}
	if body.Status != client.ReadinessStatusNotReady || strings.Join(body.Degraded, ",") != "nats,postgres,timescaledb" {
		t.Fatalf("readyz body %s", resp.Body)
	}
	for name, required := range map[string]bool{"nats": true, "postgres": true, "timescaledb": false} {
		d, ok := body.Dependencies[name]
		if !ok || d.State != client.DependencyStateDown || d.Required != required || d.Detail == nil || *d.Detail == "" || d.AgeS != nil {
			t.Errorf("%s: %+v", name, d)
		}
	}
	if !strings.HasPrefix(*body.Dependencies["nats"].Detail, "not connected since ") {
		t.Errorf("nats detail %q", *body.Dependencies["nats"].Detail)
	}

	hz, err := c.GetHealthzWithResponse(context.Background())
	if err != nil || hz.StatusCode() != 200 || hz.JSON200 == nil || hz.JSON200.Status != client.Ok {
		t.Fatalf("healthz while not ready: %v %s", err, hz.Body)
	}
	if code := Healthcheck(context.Background(), strings.TrimPrefix(base, "http://"), "/readyz", &bytes.Buffer{}); code != ExitFailed {
		t.Errorf("healthcheck /readyz while not ready: %d", code)
	}
	if code := Healthcheck(context.Background(), strings.TrimPrefix(base, "http://"), "/healthz", &bytes.Buffer{}); code != ExitOK {
		t.Errorf("healthcheck /healthz: %d", code)
	}

	m, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	var metrics bytes.Buffer
	_, _ = metrics.ReadFrom(m.Body)
	_ = m.Body.Close()
	for _, want := range []string{`ussp_build_info{commit="unknown",go_version=`, `ussp_dependency_up{dep="nats"} 0`, `ussp_dependency_age_s{dep="postgres"} +Inf`} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("%s missing from /metrics", want)
		}
	}

	if err := stop(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	out := logs.String()
	for _, want := range []string{`"msg":"started"`, `"dependencies":{"client_address":"optional","nats":"required","postgres":"required","timescaledb":"optional"}`, `"msg":"stopped"`, `USSP_PG_URL=\"postgres://ussp:xxxxx@`} {
		if !strings.Contains(out, want) {
			t.Errorf("%s missing from the log", want)
		}
	}
	if strings.Contains(out, ":pw@") {
		t.Error("a password reached the log")
	}
}

// The other side of the same endpoint: every dependency up is 200
// ready, an optional one down is 200 degraded (E-01).
func TestReadyzAnswers200WhenReadyOrDegraded(t *testing.T) {
	h := obs.NewHealth(obs.Discard(), 0)
	state := obs.StateUp
	var mu sync.Mutex
	h.Register("nats", true, func(context.Context) (obs.State, string) { return obs.StateUp, "" })
	h.Register("timescaledb", false, func(context.Context) (obs.State, string) {
		mu.Lock()
		defer mu.Unlock()
		if state == obs.StateUp {
			return state, ""
		}
		return state, "refused"
	})
	mux := http.NewServeMux()
	HealthRoutes(mux, h, obs.NewRegistry("test"))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.GetReadyzWithResponse(context.Background())
	if err != nil || resp.StatusCode() != 200 || resp.JSON200 == nil || resp.JSON200.Status != client.ReadinessStatusReady || len(resp.JSON200.Degraded) != 0 {
		t.Fatalf("all up: %v %s", err, resp.Body)
	}
	if age := resp.JSON200.Dependencies["nats"].AgeS; age == nil || *age != 0 {
		t.Fatalf("age of an up dependency: %v", age)
	}
	mu.Lock()
	state = obs.StateDown
	mu.Unlock()
	resp, err = c.GetReadyzWithResponse(context.Background())
	if err != nil || resp.StatusCode() != 200 || resp.JSON200 == nil || resp.JSON200.Status != client.ReadinessStatusDegraded ||
		strings.Join(resp.JSON200.Degraded, ",") != "timescaledb" {
		t.Fatalf("optional down: %v %s", err, resp.Body)
	}
}

func TestMainExitCodes(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), apiSpec, []string{"--help"}, &out, &errOut, env(nil)); code != ExitOK ||
		!strings.Contains(out.String(), "usage: ussp-api [--help | healthcheck [path] | migrate [up | down [version] | status]]") || !strings.Contains(out.String(), "USSP_PG_URL (required)") {
		t.Fatalf("help %d: %s", code, out.String())
	}
	errOut.Reset()
	if code := Main(context.Background(), apiSpec, nil, &out, &errOut, env(nil)); code != ExitConfig ||
		!strings.Contains(errOut.String(), `"variables":["USSP_PG_URL","USSP_TS_URL","USSP_NATS_URL"]`) {
		t.Fatalf("missing config %d: %s", code, errOut.String())
	}
	errOut.Reset()
	if code := Main(context.Background(), apiSpec, nil, &out, &errOut, env(map[string]string{"USSP_LOG_LEVEL": "loud"})); code != ExitConfig ||
		!strings.Contains(errOut.String(), "USSP_LOG_LEVEL") {
		t.Fatalf("invalid config %d: %s", code, errOut.String())
	}
	errOut.Reset()
	monitor := Spec{Process: config.ProcessMonitor, NATS: Required}
	if code := Main(context.Background(), monitor, []string{"migrate"}, &out, &errOut, env(nil)); code != ExitConfig ||
		!strings.Contains(errOut.String(), "unknown argument") {
		t.Fatalf("migrate on monitor %d: %s", code, errOut.String())
	}
	errOut.Reset()
	if code := Main(context.Background(), apiSpec, []string{"migrate"}, &out, &errOut, env(nil)); code != ExitConfig ||
		!strings.Contains(errOut.String(), `"variables":["USSP_PG_URL"]`) {
		t.Fatalf("migrate without a database %d: %s", code, errOut.String())
	}
	errOut.Reset()
	tsdb := Spec{Process: config.ProcessTSDBWriter, TimescaleDB: Optional, NATS: Required, Migrate: true}
	if code := Main(context.Background(), tsdb, []string{"migrate", "status"}, &out, &errOut, env(nil)); code != ExitConfig ||
		!strings.Contains(errOut.String(), `"variables":["USSP_TS_URL"]`) {
		t.Fatalf("tsdb-writer migrate without a database %d: %s", code, errOut.String())
	}
	for _, args := range [][]string{{"migrate", "sideways"}, {"migrate", "down", "-1"}, {"migrate", "down", "x"}, {"migrate", "up", "2"}} {
		errOut.Reset()
		if code := Main(context.Background(), apiSpec, args, &out, &errOut, env(map[string]string{"USSP_PG_URL": "postgres://u@" + closedAddr(t) + "/db"})); code != ExitConfig ||
			!strings.Contains(errOut.String(), "unknown argument") {
			t.Fatalf("%v %d: %s", args, code, errOut.String())
		}
	}
	// A database that does not answer: the subcommand fails (exit 1) and
	// says so; it never reports a version it did not reach.
	out.Reset()
	if code := Main(context.Background(), apiSpec, []string{"migrate"}, &out, &errOut, env(map[string]string{"USSP_PG_URL": "postgres://u@" + closedAddr(t) + "/db?connect_timeout=1"})); code != ExitFailed ||
		!strings.Contains(out.String(), `"msg":"migrate failed"`) || strings.Contains(out.String(), "migrate done") {
		t.Fatalf("migrate against a closed port %d: %s", code, out.String())
	}
	errOut.Reset()
	if code := Main(context.Background(), apiSpec, []string{"healthcheck"}, &out, &errOut, env(map[string]string{"USSP_API_ADDR": closedAddr(t)})); code != ExitFailed ||
		!strings.Contains(errOut.String(), "healthcheck failed") {
		t.Fatalf("healthcheck on a closed port %d: %s", code, errOut.String())
	}
}

func TestMainAllNeedsTheUnionOfTheProcesses(t *testing.T) {
	var out, errOut bytes.Buffer
	specs := []Spec{{Process: config.ProcessMonitor, NATS: Required}, {Process: config.ProcessTSDBWriter, TimescaleDB: Optional, NATS: Required}}
	if code := MainAll(context.Background(), specs, nil, &out, &errOut, env(nil)); code != ExitConfig ||
		!strings.Contains(errOut.String(), `"variables":["USSP_TS_URL","USSP_NATS_URL"]`) {
		t.Fatalf("%d: %s", code, errOut.String())
	}
	if code := MainAll(context.Background(), specs, []string{"--help"}, &out, &errOut, env(nil)); code != ExitOK ||
		!strings.Contains(out.String(), "usage: ussp-monitor") || !strings.Contains(out.String(), "usage: ussp-tsdb-writer") {
		t.Fatalf("help %d", code)
	}
}

// Two processes run side by side on their own listeners and stop
// together when the context ends; a listener that cannot bind fails
// the run with exit 1.
func TestMainAllRunsEveryProcessAndFailsOnABindError(t *testing.T) {
	nats := closedAddr(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	specs := []Spec{{Process: config.ProcessMonitor, NATS: Required}, {Process: config.ProcessDSSSync, NATS: Required}}
	var out syncBuffer
	code := MainAll(context.Background(), specs, nil, &out, &bytes.Buffer{}, env(map[string]string{
		"USSP_NATS_URL": "nats://" + nats, "USSP_MONITOR_ADDR": "127.0.0.1:0", "USSP_DSS_SYNC_ADDR": busy.Addr().String(),
		"USSP_STATUS_INTERVAL_S": "3600",
	}))
	if code != ExitFailed || !strings.Contains(out.String(), `"msg":"process failed","process":"ussp-dev","error":"dss-sync listener`) {
		t.Fatalf("%d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), `"process":"monitor"`) {
		t.Fatalf("monitor did not start: %s", out.String())
	}
}

// The CONF bounds the configuration names are the ones every process
// ensures (E-10).
func TestTopologyOfConfiguresCONF(t *testing.T) {
	cfg, err := config.LoadFrom(func(k string) (string, bool) {
		v, ok := map[string]string{"USSP_CONF_STREAM_MAX_AGE_S": "7200", "USSP_CONF_STREAM_MAX_BYTES": "1073741824"}[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	conf, _ := TopologyOf(cfg).Stream(bus.StreamCONF)
	if conf.MaxAge != 2*time.Hour || conf.MaxBytes != 1<<30 {
		t.Fatalf("CONF %v %d", conf.MaxAge, conf.MaxBytes)
	}
	def, err := config.LoadFrom(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	conf, _ = TopologyOf(def).Stream(bus.StreamCONF)
	if conf.MaxAge != bus.DefaultConfMaxAge || conf.MaxBytes != bus.DefaultConfMaxBytes {
		t.Fatalf("CONF defaults %v %d", conf.MaxAge, conf.MaxBytes)
	}
}

// client_address both ways (audit S7): up while no untrusted peer sent
// X-Forwarded-For; degraded naming the peer and the variable once one
// did; up again ten minutes later.
func TestClientAddressProbe(t *testing.T) {
	watch := &httpx.ProxyWatch{}
	now := time.Now()
	probe := ClientAddressProbe(watch, func() time.Time { return now })
	if st, d := probe(context.Background()); st != obs.StateUp || d != "" {
		t.Fatalf("nothing seen: %s %q", st, d)
	}
	h := httpx.RealIP(nil, watch)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr, now = "172.18.0.5:443", time.Now()
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if st, d := probe(context.Background()); st != obs.StateDegraded || !strings.Contains(d, "172.18.0.5") || !strings.Contains(d, "USSP_TRUSTED_PROXIES") {
		t.Fatalf("seen: %s %q", st, d)
	}
	now = now.Add(11 * time.Minute)
	if st, _ := probe(context.Background()); st != obs.StateUp {
		t.Fatalf("ten minutes later: %s", st)
	}
}
