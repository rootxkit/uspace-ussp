//go:build integration

// Package integration runs the processes against real PostgreSQL +
// PostGIS, TimescaleDB and NATS JetStream (make compose-deps locally,
// the service containers in CI). A missing address fails the test: a
// suite that skips proves nothing, and make integration fails when
// zero tests ran.
package integration

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/app/tsdbwriter"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
)

func mustEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set; see the Makefile's integration target", name)
	}
	return v
}

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

// run starts spec with the given variables on a free port and returns
// a client of its national API.
func run(t *testing.T, spec proc.Spec, vars map[string]string) *client.ClientWithResponses {
	t.Helper()
	c, err := client.NewClientWithResponses("http://" + runAt(t, spec, vars))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runAt is run returning the process's listen address.
func runAt(t *testing.T, spec proc.Spec, vars map[string]string) string {
	t.Helper()
	addr, _ := runStoppable(t, spec, vars)
	return addr
}

// runStoppable is runAt with the stop of the process: it drains the
// process and waits for it, once (the cleanup stops it otherwise).
func runStoppable(t *testing.T, spec proc.Spec, vars map[string]string) (string, func()) {
	t.Helper()
	vars["USSP_STATUS_INTERVAL_S"] = "3600"
	cfg, err := config.LoadFrom(func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Require(spec.Process); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	logs := &logBuffer{}
	go func() {
		done <- proc.Run(ctx, cfg, spec, proc.Options{Out: logs, Listening: func(a string) { addr <- a }})
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("drain: %v", err)
			}
		})
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			t.Logf("process log:\n%s", logs.String())
		}
	})
	select {
	case a := <-addr:
		return a, stop
	case err := <-done:
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no listener within 15 s")
	}
	return "", stop
}

// logBuffer collects the process log; it is printed when the test
// fails. Nothing writes to t from the process's goroutines, since a
// NATS handler may still write after the test has ended.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// readyz polls /readyz until it reports want or the deadline passes,
// and returns the last answer.
func readyz(t *testing.T, c *client.ClientWithResponses, want client.ReadinessStatus) (int, *client.Readiness) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := c.GetReadyzWithResponse(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		body := resp.JSON200
		if body == nil {
			body = resp.JSON503
		}
		if body == nil {
			t.Fatalf("readyz %d: %s", resp.StatusCode(), resp.Body)
		}
		if body.Status == want || time.Now().After(deadline) {
			t.Logf("readyz %d: %s", resp.StatusCode(), resp.Body)
			return resp.StatusCode(), body
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func wantStates(t *testing.T, body *client.Readiness, want map[string]client.DependencyState) {
	t.Helper()
	if len(body.Dependencies) != len(want) {
		t.Errorf("dependencies %v, want %v", body.Dependencies, want)
	}
	for name, state := range want {
		d, ok := body.Dependencies[name]
		if !ok || d.State != state {
			t.Errorf("%s: %+v, want %s", name, d, state)
			continue
		}
		// cis says what it is up with (its versions and age), registry
		// its last success and the feed's cursor.
		if state == client.DependencyStateUp && (d.AgeS == nil || *d.AgeS != 0 || (d.Detail != nil) != (name == "cis" || name == "registry")) {
			t.Errorf("%s up with age %v detail %v", name, d.AgeS, d.Detail)
		}
		if state == client.DependencyStateDown && (d.Detail == nil || *d.Detail == "") {
			t.Errorf("%s down without a reason", name)
		}
	}
}

// With every dependency running, api reports postgres, timescaledb,
// nats, its issuer key, the ecosystem JWKS and the DSS up and is ready
// (the success path, E-02).
func TestIntegrationAPIReadyWithEveryDependencyUp(t *testing.T) {
	ensureSchemas(t)
	c := run(t, api.Spec, withAuth(t, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
	}, newFakeAuthority(t)))
	code, body := readyz(t, c, client.ReadinessStatusReady)
	if code != 200 || body.Status != client.ReadinessStatusReady || len(body.Degraded) != 0 {
		t.Fatalf("readyz %d %+v", code, body)
	}
	wantStates(t, body, map[string]client.DependencyState{
		"postgres": client.DependencyStateUp, "timescaledb": client.DependencyStateUp, "nats": client.DependencyStateUp,
		"issuer": client.DependencyStateUp, "jwks": client.DependencyStateUp,
		"cis": client.DependencyStateUp, "cis_notify_keys": client.DependencyStateUp, "cis_publisher_keys": client.DependencyStateUp,
		"registry": client.DependencyStateUp, "geoid": client.DependencyStateUp, "dss": client.DependencyStateUp,
		"client_address": client.DependencyStateUp,
	})
}

// Without an issuer key, an audience, a CISP, an authority, a geoid and
// a DSS, api still starts (B-08) and says what it cannot do: issuer,
// jwks, registry, geoid and dss down with their reasons, cis unknown,
// ready but degraded.
func TestIntegrationAPIDegradedWithoutAuthConfiguration(t *testing.T) {
	ensureSchemas(t)
	c := run(t, api.Spec, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
	})
	code, body := readyz(t, c, client.ReadinessStatusDegraded)
	if code != 200 || strings.Join(body.Degraded, ",") != "cis,dss,geoid,issuer,jwks,registry" {
		t.Fatalf("readyz %d %+v", code, body)
	}
	wantStates(t, body, map[string]client.DependencyState{
		"postgres": client.DependencyStateUp, "timescaledb": client.DependencyStateUp, "nats": client.DependencyStateUp,
		"issuer": client.DependencyStateDown, "jwks": client.DependencyStateDown, "cis": client.DependencyStateUnknown,
		"registry": client.DependencyStateDown, "geoid": client.DependencyStateDown, "dss": client.DependencyStateDown,
		"client_address": client.DependencyStateUp,
	})
	if d := body.Dependencies["geoid"].Detail; d == nil || !strings.Contains(*d, "USSP_GEOID_FILE is not set") {
		t.Fatalf("geoid detail %v", d)
	}
	if d := body.Dependencies["dss"].Detail; d == nil || !strings.Contains(*d, "USSP_DSS_BASE_URL is not set") {
		t.Fatalf("dss detail %v", d)
	}
}

// NATS taken away (a closed port): the databases stay up, nats is down
// with its reason, cis is degraded because its zones cannot reach the
// hot path (the cis_current projection fails within its bound), and
// /readyz answers 503 (E-02).
func TestIntegrationAPINotReadyWithoutNATS(t *testing.T) {
	ensureSchemas(t)
	c := run(t, api.Spec, withAuth(t, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": "nats://" + closedAddr(t),
	}, newFakeAuthority(t)))
	code, body := readyz(t, c, client.ReadinessStatusNotReady)
	// The CIS cache and the registry feed come up on their own (they
	// need no NATS); wait for them.
	settled := func() bool {
		return strings.Join(body.Degraded, ",") == "cis,nats" && body.Dependencies["cis"].State == client.DependencyStateDegraded
	}
	for deadline := time.Now().Add(15 * time.Second); !settled() && time.Now().Before(deadline); {
		time.Sleep(250 * time.Millisecond)
		code, body = readyz(t, c, client.ReadinessStatusNotReady)
	}
	if code != 503 || body.Status != client.ReadinessStatusNotReady || strings.Join(body.Degraded, ",") != "cis,nats" {
		t.Fatalf("readyz %d %+v", code, body)
	}
	wantStates(t, body, map[string]client.DependencyState{
		"postgres": client.DependencyStateUp, "timescaledb": client.DependencyStateUp, "nats": client.DependencyStateDown,
		"issuer": client.DependencyStateUp, "jwks": client.DependencyStateUp,
		"cis": client.DependencyStateDegraded, "cis_notify_keys": client.DependencyStateUp, "cis_publisher_keys": client.DependencyStateUp,
		"registry": client.DependencyStateUp, "geoid": client.DependencyStateUp, "dss": client.DependencyStateUp,
		"client_address": client.DependencyStateUp,
	})
	if d := body.Dependencies["cis"].Detail; d == nil || !strings.Contains(*d, "projection: ") || !strings.Contains(*d, "cis_current") {
		t.Fatalf("cis detail %v", d)
	}
}

// tsdb-writer keeps running without TimescaleDB (it queues, then
// spills; PLAN §3.1): ready but degraded, timescaledb down.
func TestIntegrationTSDBWriterDegradedWithoutTimescaleDB(t *testing.T) {
	c := run(t, tsdbwriter.Spec, map[string]string{
		"USSP_TSDB_WRITER_ADDR": "127.0.0.1:0",
		"USSP_TS_URL":           "postgres://ussp:unused@" + closedAddr(t) + "/ussp_timeseries?connect_timeout=1",
		"USSP_NATS_URL":         mustEnv(t, "USSP_TEST_NATS_URL"),
	})
	code, body := readyz(t, c, client.ReadinessStatusDegraded)
	if code != 200 || body.Status != client.ReadinessStatusDegraded || strings.Join(body.Degraded, ",") != "timescaledb" {
		t.Fatalf("readyz %d %+v", code, body)
	}
	wantStates(t, body, map[string]client.DependencyState{
		"timescaledb": client.DependencyStateDown, "nats": client.DependencyStateUp,
	})
}

// proxy forwards TCP to target until stopped; it stands between a
// process and NATS so the test can take NATS away and bring it back
// on the same address.
type proxy struct {
	t      *testing.T
	addr   string
	target string
	mu     sync.Mutex
	ln     net.Listener
	conns  []net.Conn
}

func newProxy(t *testing.T, target string) *proxy {
	p := &proxy{t: t, target: target, addr: closedAddr(t)}
	p.start()
	t.Cleanup(p.stop)
	return p
}

func (p *proxy) start() {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", p.target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
}

// stop closes the listener and every connection through it.
func (p *proxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// NATS goes away while api runs and comes back on the same address:
// /readyz follows it down (503, nats down with its reason) and up
// again (200), without a restart (B-08, E-02).
func TestIntegrationAPIFollowsNATSAwayAndBack(t *testing.T) {
	ensureSchemas(t)
	natsURL, err := url.Parse(mustEnv(t, "USSP_TEST_NATS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	p := newProxy(t, natsURL.Host)
	natsURL.Host = p.addr
	c := run(t, api.Spec, withAuth(t, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": natsURL.String(),
	}, newFakeAuthority(t)))
	if code, body := readyz(t, c, client.ReadinessStatusReady); code != 200 {
		t.Fatalf("before: %d %+v", code, body)
	}

	p.stop()
	code, body := readyz(t, c, client.ReadinessStatusNotReady)
	if code != 503 || strings.Join(body.Degraded, ",") != "nats" {
		t.Fatalf("NATS away: %d %+v", code, body)
	}
	wantStates(t, body, map[string]client.DependencyState{
		"postgres": client.DependencyStateUp, "timescaledb": client.DependencyStateUp, "nats": client.DependencyStateDown,
		"issuer": client.DependencyStateUp, "jwks": client.DependencyStateUp,
		"cis": client.DependencyStateUp, "cis_notify_keys": client.DependencyStateUp, "cis_publisher_keys": client.DependencyStateUp,
		"registry": client.DependencyStateUp, "geoid": client.DependencyStateUp, "dss": client.DependencyStateUp,
		"client_address": client.DependencyStateUp,
	})
	if d := body.Dependencies["nats"]; d.AgeS == nil || *d.AgeS <= 0 {
		t.Errorf("nats down without the age of its last good state: %+v", d)
	}

	p.start()
	code, body = readyz(t, c, client.ReadinessStatusReady)
	if code != 200 || len(body.Degraded) != 0 {
		t.Fatalf("NATS back: %d %+v", code, body)
	}
}
