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
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("drain: %v", err)
		}
		if t.Failed() {
			t.Logf("process log:\n%s", logs.String())
		}
	})
	select {
	case a := <-addr:
		c, err := client.NewClientWithResponses("http://" + a)
		if err != nil {
			t.Fatal(err)
		}
		return c
	case err := <-done:
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no listener within 15 s")
	}
	return nil
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
		if state == client.DependencyStateUp && (d.AgeS == nil || *d.AgeS != 0 || d.Detail != nil) {
			t.Errorf("%s up with age %v detail %v", name, d.AgeS, d.Detail)
		}
		if state == client.DependencyStateDown && (d.Detail == nil || *d.Detail == "") {
			t.Errorf("%s down without a reason", name)
		}
	}
}

// With every dependency running, api reports postgres, timescaledb and
// nats up and is ready (the success path, E-02).
func TestIntegrationAPIReadyWithEveryDependencyUp(t *testing.T) {
	ensureSchemas(t)
	c := run(t, api.Spec, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
	})
	code, body := readyz(t, c, client.ReadinessStatusReady)
	if code != 200 || body.Status != client.ReadinessStatusReady || len(body.Degraded) != 0 {
		t.Fatalf("readyz %d %+v", code, body)
	}
	wantStates(t, body, map[string]client.DependencyState{
		"postgres": client.DependencyStateUp, "timescaledb": client.DependencyStateUp, "nats": client.DependencyStateUp,
	})
}

// NATS taken away (a closed port): the databases stay up, nats is down
// with its reason, and /readyz answers 503 (E-02).
func TestIntegrationAPINotReadyWithoutNATS(t *testing.T) {
	ensureSchemas(t)
	c := run(t, api.Spec, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": "nats://" + closedAddr(t),
	})
	code, body := readyz(t, c, client.ReadinessStatusNotReady)
	if code != 503 || body.Status != client.ReadinessStatusNotReady || strings.Join(body.Degraded, ",") != "nats" {
		t.Fatalf("readyz %d %+v", code, body)
	}
	wantStates(t, body, map[string]client.DependencyState{
		"postgres": client.DependencyStateUp, "timescaledb": client.DependencyStateUp, "nats": client.DependencyStateDown,
	})
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
	c := run(t, api.Spec, map[string]string{
		"USSP_API_ADDR": "127.0.0.1:0",
		"USSP_PG_URL":   mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":   mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL": natsURL.String(),
	})
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
