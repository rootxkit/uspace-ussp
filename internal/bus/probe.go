// Package bus is the NATS JetStream connection of every process. WP-0
// holds the connection and its readiness probe; streams, subjects, KV
// buckets and projections arrive with WP-6.
package bus

import (
	"context"
	"log/slog"
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

// Probe returns the readiness check of c: down while it is not
// connected, with the last reason; degraded when it is connected but
// JetStream does not answer (a server started without -js, or an
// account without JetStream); up otherwise.
func (c *Conn) Probe() obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		connected, since, reason := c.Link()
		if !connected {
			return obs.StateDown, "not connected since " + since.UTC().Format(time.RFC3339) + ": " + reason
		}
		js, err := jetstream.New(c.Conn)
		if err != nil {
			return obs.StateDegraded, "connected; JetStream: " + err.Error()
		}
		if _, err := js.AccountInfo(ctx); err != nil {
			return obs.StateDegraded, "connected; JetStream does not answer: " + err.Error()
		}
		return obs.StateUp, ""
	}
}
