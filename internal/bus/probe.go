package bus

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// ReconnectWait is the pause between reconnection attempts.
const ReconnectWait = 2 * time.Second

// Conn is a NATS connection that is never given up on (B-08), with its
// link state tracked from the client's own events, so reading it never
// waits on the connection's lock (which a reconnection attempt holds
// while it dials).
type Conn struct {
	*nats.Conn

	js   jetstream.JetStream
	topo *Maintainer

	mu        sync.Mutex
	connected bool
	since     time.Time
	lastErr   string
}

// Connect returns a connection that retries the first connection and
// every reconnection forever, so the process starts while NATS is down
// and says so on /readyz rather than exiting or hanging. Only an option
// error is returned.
func Connect(url, credsFile, name string, logger *slog.Logger) (*Conn, error) {
	c := &Conn{since: time.Now(), lastErr: "never connected"}
	opts := []nats.Option{
		nats.Name(name),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(ReconnectWait),
		nats.Timeout(2 * time.Second),
		nats.ConnectHandler(func(*nats.Conn) {
			c.set(true, "")
			logger.LogAttrs(context.Background(), slog.LevelInfo, "nats connected", obs.Dependency("nats"))
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			reason := "disconnected"
			if err != nil {
				reason = err.Error()
			}
			c.set(false, reason)
			logger.LogAttrs(context.Background(), slog.LevelWarn, "nats disconnected", obs.Dependency("nats"), slog.String("error", reason))
		}),
		nats.ReconnectHandler(func(*nats.Conn) {
			c.set(true, "")
			logger.LogAttrs(context.Background(), slog.LevelInfo, "nats reconnected", obs.Dependency("nats"))
		}),
		nats.ReconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				c.set(false, err.Error())
			}
		}),
		nats.ClosedHandler(func(*nats.Conn) { c.set(false, "connection closed") }),
	}
	if credsFile != "" {
		opts = append(opts, nats.UserCredentials(credsFile))
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, err
	}
	c.Conn = nc
	if c.js, err = jetstream.New(nc); err != nil {
		nc.Close()
		return nil, err
	}
	// ConnectHandler is not called for a connection that succeeded
	// within Connect itself.
	if nc.IsConnected() {
		c.set(true, "")
	}
	return c, nil
}

func (c *Conn) set(connected bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if connected != c.connected {
		c.since = time.Now()
	}
	c.connected = connected
	if reason != "" {
		c.lastErr = reason
	}
}

// Link reports whether the connection is up, since when it is in that
// state, and the last reason it was not.
func (c *Conn) Link() (bool, time.Time, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected, c.since, c.lastErr
}

// JetStream is the connection's JetStream context (no I/O).
func (c *Conn) JetStream() jetstream.JetStream { return c.js }

// Maintain makes c keep the topology t in place: m.Run in the
// background, and the readiness probe reports what it found. It returns
// the maintainer; call it once.
func (c *Conn) Maintain(t Topology, logger *slog.Logger) *Maintainer {
	c.topo = &Maintainer{JS: c.js, Topology: t, Logger: logger}
	return c.topo
}

// topologyStale is how old the last topology check may be before the
// probe repeats it inline.
const topologyStale = time.Minute

// Probe returns the readiness check of c: down while it is not
// connected, with the last reason; degraded when it is connected but
// JetStream does not answer (a server started without -js, or an
// account without JetStream), and, with Maintain, when a stream or
// bucket is missing or differs from this build's topology (each named);
// up otherwise.
func (c *Conn) Probe() obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		connected, since, reason := c.Link()
		if !connected {
			return obs.StateDown, "not connected since " + since.UTC().Format(time.RFC3339) + ": " + reason
		}
		if _, err := c.js.AccountInfo(ctx); err != nil {
			return obs.StateDegraded, "connected; JetStream does not answer: " + err.Error()
		}
		if c.topo == nil {
			return obs.StateUp, ""
		}
		st, err := c.topo.Fresh(ctx, topologyStale)
		if err != nil && !st.Checked {
			return obs.StateDegraded, "connected; streams and buckets not checked: " + err.Error()
		}
		if len(st.Drift) > 0 {
			return obs.StateDegraded, "connected; streams and buckets differ from this build: " + strings.Join(st.Drift, "; ")
		}
		return obs.StateUp, ""
	}
}
