package trafficws

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"
)

// busLink is a NATS link a test switches.
type busLink struct{ down atomic.Bool }

func (l *busLink) Link() (bool, time.Time, string) {
	if l.down.Load() {
		return false, time.Unix(1_700_000_000, 0), "disconnected"
	}
	return true, time.Unix(1_700_000_000, 0), ""
}

// closedWith reads until the socket closes and returns its close code
// and how many status frames came before it.
func closedWith(t *testing.T, c *websocket.Conn, within time.Duration) (websocket.StatusCode, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	statuses := 0
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("the socket stayed open %s (%d status frames)", within, statuses)
			}
			return websocket.CloseStatus(err), statuses
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatal(err)
		}
		if f.Schema == SchemaStatus {
			statuses++
		}
	}
}

// 05 §6: with its NATS down, a console freezes with its age shown.
// traffic-ws holds neither tracks nor alerts then, so it must not keep a
// socket that says it is live: every open stream closes with 1013 at the
// next status frame, and a stream opened while the bus is down closes so
// before any status frame. With the bus up the same streams are live
// and stay open (the presence pair), and they are again once it is back.
func TestBusDownClosesTheStreams(t *testing.T) {
	ss := schemas(t)
	r := newRig(t)
	link := &busLink{}
	r.srv.Link = link.Link
	counters := &core.Counters{}
	r.srv.Counters = counters
	type stream struct {
		name, path, token, origin string
	}
	streams := []stream{
		{"console traffic", "/v1/traffic?bbox=44.7,41.6,44.9,41.8", "staff", "https://console.test"},
		{"operator traffic", "/v1/traffic?intent_id=" + intentA, "op-a", ""},
		{"operator alerts", "/v1/alerts?intent_id=" + intentA, "op-a", ""},
	}
	open := map[string]*websocket.Conn{}
	for _, s := range streams {
		c, _, err := r.dial(s.path, s.token, s.origin)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		defer c.CloseNow()
		// Two status frames: live, and still live one period later.
		for range 2 {
			next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == SchemaStatus })
		}
		open[s.name] = c
	}

	link.down.Store(true)
	for _, s := range streams {
		if code, _ := closedWith(t, open[s.name], 2*time.Second); code != websocket.StatusTryAgainLater {
			t.Errorf("%s with the bus down: closed %d, want 1013", s.name, code)
		}
	}
	for _, s := range streams {
		c, _, err := r.dial(s.path, s.token, s.origin)
		if err != nil {
			t.Fatalf("%s opened with the bus down: %v", s.name, err)
		}
		code, statuses := closedWith(t, c, 2*time.Second)
		if code != websocket.StatusTryAgainLater || statuses != 0 {
			t.Errorf("%s opened with the bus down: closed %d after %d status frames, want 1013 after none", s.name, code, statuses)
		}
	}
	if n := counters.Get(CounterBusDown); n < uint64(2*len(streams)) {
		t.Errorf("%s = %d, want at least %d", CounterBusDown, n, 2*len(streams))
	}

	link.down.Store(false)
	for _, s := range streams {
		c, _, err := r.dial(s.path, s.token, s.origin)
		if err != nil {
			t.Fatalf("%s with the bus back: %v", s.name, err)
		}
		defer c.CloseNow()
		for range 2 {
			next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == SchemaStatus })
		}
	}
}
