package testinterfaces

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/testif"
)

// logSink collects the JSON log and tells when a line with msg arrives.
type logSink struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines []map[string]any
	wait  map[string]chan map[string]any
}

func newSink(msgs ...string) *logSink {
	s := &logSink{wait: map[string]chan map[string]any{}}
	for _, m := range msgs {
		s.wait[m] = make(chan map[string]any, 1)
	}
	return s
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadBytes('\n')
		if err != nil {
			s.buf.Write(line) // an incomplete line waits for the rest
			break
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		s.lines = append(s.lines, m)
		if msg, _ := m["msg"].(string); s.wait[msg] != nil {
			select {
			case s.wait[msg] <- m:
			default:
			}
		}
	}
	return len(p), nil
}

func (s *logSink) await(t *testing.T, msg string) map[string]any {
	t.Helper()
	select {
	case m := <-s.wait[msg]:
		return m
	case <-time.After(20 * time.Second):
		s.mu.Lock()
		defer s.mu.Unlock()
		var msgs []string
		for _, l := range s.lines {
			m, _ := l["msg"].(string)
			e, _ := l["error"].(string)
			msgs = append(msgs, m+" "+e)
		}
		t.Fatalf("no %q line; the log: %q", msg, msgs)
		return nil
	}
}

func lookup(m map[string]string) config.LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// stackEnv is a complete configuration against a stub USSP whose api is
// ready.
func stackEnv(t *testing.T) map[string]string {
	t.Helper()
	ussp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ready","checked_at":"2026-10-13T00:00:00Z","dependencies":{},"degraded":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ussp.Close)
	dir := t.TempDir()
	secret := filepath.Join(dir, "operator-secret")
	if err := os.WriteFile(secret, []byte("not-a-real-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		testif.VarSwitch: "on", testif.VarSystemID: "DEV01",
		"USSP_TEST_INTERFACES_ADDR":                 "127.0.0.1:0",
		"USSP_TEST_INTERFACES_USSP_BASE_URL":        ussp.URL,
		"USSP_TEST_INTERFACES_OPERATOR_CLIENT_ID":   "op-test",
		"USSP_TEST_INTERFACES_OPERATOR_SECRET_FILE": secret,
		"USSP_TEST_INTERFACES_OPERATOR_FILE":        filepath.Join("..", "..", "..", "deploy", "conformance", "test-operator.yaml"),
		"USSP_STATUS_INTERVAL_S":                    "1",
		"USSP_SHUTDOWN_TIMEOUT_S":                   "2",
	}
}

// Without the switch, or with a system id that is not a test or
// development one, the process exits 2 naming both variables, before
// it reads the rest of its configuration or opens a listener (E-01:
// TestMainStartsWithTheSwitchAndATestSystemID is the twin).
func TestMainRefusesUnlessEnabled(t *testing.T) {
	for name, change := range map[string]map[string]string{
		"switch off":       {testif.VarSwitch: "off"},
		"switch unset":     {testif.VarSwitch: ""},
		"production id":    {testif.VarSystemID: "GEOUSSP1"},
		"system id unset":  {testif.VarSystemID: ""},
		"neither, broken":  {testif.VarSwitch: "", testif.VarSystemID: "", "USSP_TEST_INTERFACES_USSP_BASE_URL": "::"},
		"production, addr": {testif.VarSystemID: "PROD", "USSP_TEST_INTERFACES_ADDR": "127.0.0.1:1"},
	} {
		env := stackEnv(t)
		for k, v := range change {
			env[k] = v
		}
		var stdout, stderr bytes.Buffer
		code := Main(context.Background(), nil, &stdout, &stderr, lookup(env))
		if code != proc.ExitConfig {
			t.Errorf("%s: exit %d, want %d", name, code, proc.ExitConfig)
		}
		if !strings.Contains(stderr.String(), testif.VarSwitch) || !strings.Contains(stderr.String(), testif.VarSystemID) ||
			!strings.Contains(stderr.String(), "refusing to start") {
			t.Errorf("%s: %q does not name both variables", name, stderr.String())
		}
		if strings.Contains(stdout.String(), `"started"`) {
			t.Errorf("%s: started", name)
		}
	}
}

// With both, the process starts on its own listener, serves /healthz,
// says its readiness from the USSP's api, writes the switch at error
// level every status period, and stops cleanly.
func TestMainStartsWithTheSwitchAndATestSystemID(t *testing.T) {
	env := stackEnv(t)
	out := newSink("started", testif.SwitchOnMessage, "stopped")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stderr bytes.Buffer
	go func() { done <- Main(ctx, nil, out, &stderr, lookup(env)) }()
	started := out.await(t, "started")
	addr, _ := started["addr"].(string)
	// No idle connection is left for the drain to wait on.
	hc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := hc.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz %d", resp.StatusCode)
	}
	resp, err = hc.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	var rd struct {
		Dependencies map[string]struct {
			State    string `json:"state"`
			Required bool   `json:"required"`
		} `json:"dependencies"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rd)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if d := rd.Dependencies[testif.DepUSSP]; d.State != "up" || !d.Required {
		t.Errorf("readiness of the USSP's api %+v", rd.Dependencies)
	}
	for range 2 { // at start and at least one status period later
		line := out.await(t, testif.SwitchOnMessage)
		if line["level"] != "ERROR" {
			t.Errorf("the switch line is at %v, want ERROR", line["level"])
		}
	}
	cancel()
	if code := <-done; code != proc.ExitOK {
		t.Errorf("exit %d; stderr %s", code, stderr.String())
	}
	out.await(t, "stopped")
}

// The production compose file defines no test-interfaces service and
// names no testif image: the interfaces never run in production (T11).
func TestProductionComposeHasNoTestInterfaces(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "compose", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"test-interfaces", "uspace-ussp-testif", "USSP_TEST_INTERFACES", Binary} {
		if strings.Contains(string(b), s) {
			t.Errorf("deploy/compose/docker-compose.yml names %s", s)
		}
	}
	if !strings.Contains(string(b), "ussp-api") {
		t.Error("deploy/compose/docker-compose.yml is not the production compose (no ussp-api): the check reads the wrong file")
	}
}
