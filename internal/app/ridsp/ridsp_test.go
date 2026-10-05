package ridsp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

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

// E-02: the rid-sp process itself serves the F3411 USS endpoints behind
// the guard (a request without a token is 401 problem+json, not 404 and
// not 501) beside its health operations, with NATS down. The scopes and
// the 501 behind them are internal/stdapi's tests.
func TestRIDSPServesTheF3411EndpointsGuarded(t *testing.T) {
	kv := map[string]string{
		"USSP_RID_SP_ADDR":       "127.0.0.1:0",
		"USSP_NATS_URL":          "nats://" + closedAddr(t),
		"USSP_AUDIENCES":         "ussp.test",
		"USSP_STATUS_INTERVAL_S": "3600",
	}
	cfg, err := config.LoadFrom(func(k string) (string, bool) { v, ok := kv[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- proc.Run(ctx, cfg, Spec, proc.Options{Out: io.Discard, Listening: func(a string) { addr <- a }})
	}()
	var base string
	select {
	case a := <-addr:
		base = "http://" + a
	case err := <-done:
		cancel()
		t.Fatalf("rid-sp did not start: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("rid-sp did not listen within 10 s")
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("rid-sp stopped with %v", err)
		}
	}()

	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res, b
	}
	if res, _ := get("/healthz"); res.StatusCode != http.StatusOK {
		t.Fatalf("healthz %d", res.StatusCode)
	}
	for _, path := range []string{"/uss/flights?view=34.1,-118.4,34.2,-118.3", "/uss/flights/x/details"} {
		res, body := get(path)
		var p httpx.ProblemBody
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("Content-Type") != httpx.ProblemContentType ||
			json.Unmarshal(body, &p) != nil || p.Type != httpx.ProblemTypeBase+httpx.SlugUnauthenticated {
			t.Errorf("%s: %d %s", path, res.StatusCode, body)
		}
	}
	// An operation F3411 does not define on the USS side is not served.
	if res, _ := get("/uss/identification_service_areas/x"); res.StatusCode != http.StatusNotFound && res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET of the ISA notification: %d", res.StatusCode)
	}
	res, body := get("/metrics")
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), obs.MetricName("auth_no_credential")) {
		t.Errorf("the guard's refusals are not on /metrics: %d", res.StatusCode)
	}
}

// startRIDSP runs rid-sp with the push switch and returns its base URL.
func startRIDSP(t *testing.T, push string) string {
	t.Helper()
	kv := map[string]string{
		"USSP_RID_SP_ADDR":       "127.0.0.1:0",
		"USSP_NATS_URL":          "nats://" + closedAddr(t),
		"USSP_AUDIENCES":         "ussp.test",
		"USSP_STATUS_INTERVAL_S": "3600",
		"USSP_AUTHORITY_PUSH":    push,
	}
	cfg, err := config.LoadFrom(func(k string) (string, bool) { v, ok := kv[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- proc.Run(ctx, cfg, Spec, proc.Options{Out: io.Discard, Listening: func(a string) { addr <- a }})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("rid-sp stopped with %v", err)
		}
	})
	select {
	case a := <-addr:
		return a
	case err := <-done:
		t.Fatalf("rid-sp did not start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("rid-sp did not listen within 10 s")
	}
	return ""
}

// D12 both ways: with USSP_AUTHORITY_PUSH off (the default) WS
// /v1/authority/flights does not exist (404) and the Service Provider
// path is served; with it on the upgrade is served (an unauthenticated
// one is refused 401 before any upgrade, C8) and the Service Provider
// path is served all the same.
func TestAuthorityPushOffAndOn(t *testing.T) {
	for _, push := range []string{"off", "on"} {
		addr := startRIDSP(t, push)
		res, err := http.Get("http://" + addr + "/v1/authority/flights")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if push == "off" && res.StatusCode != http.StatusNotFound {
			t.Errorf("push off: %d, want 404", res.StatusCode)
		}
		if push == "on" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn, resp, err := websocket.Dial(ctx, "ws://"+addr+"/v1/authority/flights", nil)
			cancel()
			if err == nil {
				_ = conn.CloseNow()
				t.Fatal("push on, no token: upgraded")
			}
			if resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("push on, no token: %v %v, want 401", resp, err)
			}
		}
		res, err = http.Get("http://" + addr + "/uss/flights?view=41.7,44.8,41.71,44.81")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("push %s: /uss/flights %d, want 401 (served, guarded)", push, res.StatusCode)
		}
	}
}
