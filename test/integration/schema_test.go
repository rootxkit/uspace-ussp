//go:build integration

package integration

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// lowered rolls the relational tree back two versions and restores it
// when the test ends.
func lowered(t *testing.T) (have, want int64) {
	t.Helper()
	ensureSchemas(t)
	want = latest(t, store.TreeRelational)
	have = want - 2
	if _, err := relOwner(t).MigrateDown(context.Background(), store.TreeRelational, have); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool(t, mustEnv(t, "USSP_TEST_PG_URL"), "").Migrate(context.Background(), store.TreeRelational); err != nil {
			t.Errorf("restore the schema: %v", err)
		}
	})
	return have, want
}

func apiVars(t *testing.T, waitS string) map[string]string {
	return map[string]string{
		"USSP_API_ADDR":          "127.0.0.1:0",
		"USSP_PG_URL":            mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":            mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL":          mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_SCHEMA_WAIT_S":     waitS,
		"USSP_STATUS_INTERVAL_S": "3600",
	}
}

// start runs api in the background and returns the channels of its
// listening address and its end.
func start(t *testing.T, vars map[string]string) (<-chan string, <-chan error, context.CancelFunc, *logBuffer) {
	t.Helper()
	cfg, err := config.LoadFrom(func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	logs := &logBuffer{}
	go func() {
		done <- proc.Run(ctx, cfg, api.Spec, proc.Options{Out: logs, Listening: func(a string) { addr <- a }})
	}()
	return addr, done, cancel, logs
}

// D5: a process started against an older schema waits and never
// migrates; the migrate subcommand, run here by the test and not by the
// process, lands the schema and the process then starts. The subcommand
// reports the versions it moved between (E-02: the success path read).
func TestIntegrationProcessWaitsForTheMigrateSubcommand(t *testing.T) {
	have, want := lowered(t)
	addr, done, cancel, logs := start(t, apiVars(t, "60"))
	defer cancel()

	select {
	case a := <-addr:
		t.Fatalf("api listened on %s with the schema at %d of %d", a, have, want)
	case err := <-done:
		t.Fatalf("api ended while waiting: %v\n%s", err, logs.String())
	case <-time.After(2 * time.Second):
	}
	if v, err := relOwner(t).SchemaVersion(context.Background(), store.TreeRelational); err != nil || v != have {
		t.Fatalf("the waiting process changed the schema: %d %v", v, err)
	}

	var out, errOut bytes.Buffer
	vars := apiVars(t, "60")
	code := proc.Main(context.Background(), api.Spec, []string{"migrate"}, &out, &errOut, func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if code != proc.ExitOK || !strings.Contains(out.String(), `"msg":"migrate done"`) ||
		!strings.Contains(out.String(), `"from":`+itoa(have)+`,"to":`+itoa(want)) {
		t.Fatalf("migrate %d: %s %s", code, out.String(), errOut.String())
	}

	select {
	case <-addr:
	case err := <-done:
		t.Fatalf("api ended instead of starting: %v\n%s", err, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("api did not start after the migration\n%s", logs.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !strings.Contains(logs.String(), `"msg":"schema","process":"api","tree":"relational","need":`+itoa(want)+`,"state":"ready"`) {
		t.Errorf("no schema line:\n%s", logs.String())
	}
}

// Twin: nobody migrates; after USSP_SCHEMA_WAIT_S the process refuses to
// start and names both versions and the command.
func TestIntegrationProcessRefusesAnOlderSchemaAfterTheWait(t *testing.T) {
	have, want := lowered(t)
	addr, done, cancel, logs := start(t, apiVars(t, "1"))
	defer cancel()
	select {
	case a := <-addr:
		t.Fatalf("api listened on %s with the schema at %d of %d", a, have, want)
	case err := <-done:
		msg := "refusing to start: relational schema is at version " + itoa(have) + ", this build needs " + itoa(want) + ": run `ussp-api migrate`"
		if err == nil || err.Error() != msg {
			t.Fatalf("got %v, want %q\n%s", err, msg, logs.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("api neither started nor refused")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// The readiness probe judges the schema too (CLAUDE.md rule 7): a
// schema lowered under a running api is reported degraded with both
// versions, and up again once the migrate subcommand restores it.
func TestIntegrationReadyzReportsAnOlderSchema(t *testing.T) {
	ensureSchemas(t)
	c := run(t, api.Spec, apiVars(t, "60"))
	if code, body := readyz(t, c, client.ReadinessStatusReady); code != 200 {
		t.Fatalf("before: %d %+v", code, body)
	}
	have, want := lowered(t)
	_, body := readyz(t, c, client.ReadinessStatusDegraded)
	d := body.Dependencies["postgres"]
	msg := "relational schema is at version " + itoa(have) + ", this build needs " + itoa(want) + ": run `ussp-api migrate`"
	if d.State != client.DependencyStateDegraded || d.Detail == nil || *d.Detail != msg {
		t.Fatalf("postgres with an older schema: %+v", d)
	}
	if _, err := relOwner(t).Migrate(context.Background(), store.TreeRelational); err != nil {
		t.Fatal(err)
	}
	if code, body := readyz(t, c, client.ReadinessStatusReady); code != 200 || body.Dependencies["postgres"].State != client.DependencyStateUp {
		t.Fatalf("after migrate: %d %+v", code, body)
	}
}
