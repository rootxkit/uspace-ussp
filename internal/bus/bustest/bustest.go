// Package bustest gives tests streams of their own on the NATS of the
// integration suite, so a test never reads another test's messages.
// Imported by _test.go files only.
package bustest

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// TrackStream creates a stream configured like TRK, named name, over
// subject + ".>", capped at maxMsgs messages when maxMsgs > 0 (the
// oldest removed first), deletes it when the test ends and returns the
// topology holding it.
func TrackStream(t testing.TB, c *bus.Conn, name, subject string, maxMsgs int64) bus.Topology {
	t.Helper()
	top := bus.DefaultTopology()
	cfg, _ := top.Stream(bus.StreamTRK)
	cfg.Name, cfg.Subjects, cfg.Description = name, []string{subject + ".>"}, "test stream"
	if maxMsgs > 0 {
		cfg.MaxMsgs = maxMsgs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.JetStream().CreateStream(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.JetStream().DeleteStream(context.Background(), name) })
	return bus.Topology{Streams: []jetstream.StreamConfig{cfg}}
}

// Subscribe calls fn with the body of every core NATS message on
// subject until the test ends.
func Subscribe(t testing.TB, c *bus.Conn, subject string, fn func(data []byte)) {
	t.Helper()
	sub, err := c.Subscribe(subject, func(m *nats.Msg) { fn(m.Data) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}
