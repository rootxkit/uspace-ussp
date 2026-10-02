//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// busConn connects to url (a closed address for "NATS down") and waits
// for the connection when want is true.
func busConn(t *testing.T, url string, want bool) *bus.Conn {
	t.Helper()
	c, err := bus.Connect(url, "", "integration", quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	for deadline := time.Now().Add(5 * time.Second); want && !c.IsConnected(); {
		if time.Now().After(deadline) {
			t.Fatal("NATS not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c
}

func within(t *testing.T, d time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > d {
			t.Fatalf("not within %v", d)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

// WP-1's policy write against the real projector (B-09): with NATS
// unreachable the write is refused with 503 within the projector's bound
// and leaves no row and no event; with NATS there it commits, and a
// follower on the bucket reads the new version within a second.
func TestIntegrationPolicyPutWithBusProjector(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	st := appStore(t)
	app := relApp(t)
	rows := func() int64 { return count(t, app, "SELECT count(*) FROM policy") }
	events := func() int64 { return count(t, app, "SELECT count(*) FROM events WHERE entity_type = 'policy'") }
	rows0, events0 := rows(), events()

	down := bus.NewProjector(busConn(t, "nats://"+closedAddr(t), false), nil)
	down.Timeout = 500 * time.Millisecond
	start := time.Now()
	_, err := policy.New(st, down, nil).Put(ctx, "staff-1", "integration: NATS down", policy.Defaults())
	var pe *policy.ProjectionError
	if !errors.As(err, &pe) || pe.Bucket != policy.BucketPolicy || httpx.ProblemFromError(err).Status != 503 {
		t.Fatalf("NATS down: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("refusal took %v", took)
	}
	if rows() != rows0 || events() != events0 {
		t.Fatalf("refused write left rows %d->%d events %d->%d", rows0, rows(), events0, events())
	}
	t.Logf("NATS down: refused with 503 after %v, no row", time.Since(start).Round(time.Millisecond))

	conn := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	f := &bus.Follower[policy.Record]{JS: conn.JetStream(), Bucket: bus.BucketPolicy, Key: bus.KeyPolicy, Core: conn.Conn, Push: bus.CtlPolicy}
	fctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.Run(fctx); close(done) }()
	defer func() { cancel(); <-done }()
	v := policy.Defaults()
	v.LostLinkS = 21
	r, err := policy.New(st, bus.NewProjector(conn, nil), nil).Put(ctx, "staff-1", "integration: NATS up", v)
	if err != nil {
		t.Fatal(err)
	}
	if rows() != rows0+1 || events() != events0+1 {
		t.Fatalf("accepted write: rows %d->%d events %d->%d", rows0, rows(), events0, events())
	}
	took := within(t, time.Second, func() bool { got, _, ok := f.Value(); return ok && got.Version == r.Version })
	got, age, _ := f.Value()
	if got.Values.LostLinkS != 21 || age > 1 {
		t.Fatalf("follower %+v age %v", got, age)
	}
	t.Logf("NATS up: policy_version %d committed and read by the follower after %v", r.Version, took.Round(time.Millisecond))
}

// The source switches against the real projector: refused with 503 and
// no row when NATS is down; with NATS up the follower of
// internal/sources applies the state and answers the switch.
func TestIntegrationSourceSwitchWithBusProjector(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	st := appStore(t)
	app := relApp(t)
	instance := fmt.Sprint("rx-", time.Now().UnixNano())
	c := coresources.Control{SourceType: "adsb_rx", InstanceID: &instance, Enabled: false}
	rowsFor := func() int64 {
		return count(t, app, "SELECT count(*) FROM source_controls WHERE source_type = 'adsb_rx' AND instance_id = $1", instance)
	}

	down := bus.NewProjector(busConn(t, "nats://"+closedAddr(t), false), nil)
	down.Timeout = 500 * time.Millisecond
	_, err := (&sources.Writer{Store: st, Projector: down}).Switch(ctx, "staff-1", "maintenance", c)
	if httpx.ProblemFromError(err).Status != 503 || rowsFor() != 0 {
		t.Fatalf("NATS down: %v, %d rows", err, rowsFor())
	}

	conn := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	fol := sources.Follow(conn, quiet())
	fctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { fol.Run(fctx); close(done) }()
	defer func() { cancel(); <-done }()
	// Before the switch the instance is enabled (B-09: never closed by omission).
	if d := fol.Query("adsb_rx", &instance); !d.Enabled {
		t.Fatalf("before: %+v", d)
	}
	s, err := (&sources.Writer{Store: st, Projector: bus.NewProjector(conn, nil)}).Switch(ctx, "staff-1", "maintenance", c)
	if err != nil || rowsFor() != 1 {
		t.Fatalf("NATS up: %v, %d rows", err, rowsFor())
	}
	within(t, time.Second, func() bool { got, ok := fol.State(); return ok && got.Version >= s.Version })
	if d := fol.Query("adsb_rx", &instance); d.Enabled {
		t.Fatalf("after the switch: %+v", d)
	}
	if age, ok := fol.AgeS(); !ok || age > 1 {
		t.Fatalf("age %v %v", age, ok)
	}
}
