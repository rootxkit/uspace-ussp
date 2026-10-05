package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Counters of the monitor's input outages (PLAN §15.2 Q35).
const (
	// CounterInputOutage counts the outages of the monitor's own input
	// (its bus link) it observed: each a monitoring outage, during which
	// no lost_link is judged.
	CounterInputOutage = "monitor_input_outages"
)

// InputFunc reports the monitor's own input: whether it is up, and since
// when it is in that state (bus.Conn.Link).
type InputFunc func() (up bool, since time.Time)

// Outage is the monitor's view of its own input as the status line and
// the console say it: DownSince while it is down (a monitoring outage);
// BackAt when it last came back; SuspendedUntil, while it is later
// than now, the end of the grace after the return during which a flight
// still silent is not yet judged lost (BackAt + lost_link_s).
type Outage struct {
	DownSince      *time.Time
	BackAt         *time.Time
	SuspendedUntil *time.Time
}

// inputWatch turns the input's state into the trackers' Feed and says
// each monitoring outage once, when it starts and when it ends.
type inputWatch struct {
	input   InputFunc
	started time.Time

	mu        sync.Mutex
	downSince time.Time
	backAt    time.Time
}

// feed reads the input at now. A return between two reads (an outage
// shorter than a tick) is seen by its since being later than the last
// return and the watch's start.
func (iw *inputWatch) feed(e *Engine, now time.Time, lostLinkS float64) conformance.Feed {
	if iw == nil || iw.input == nil {
		return conformance.Feed{}
	}
	up, since := iw.input()
	iw.mu.Lock()
	defer iw.mu.Unlock()
	if !up {
		if iw.downSince.IsZero() {
			iw.downSince = since
			if iw.downSince.IsZero() || iw.downSince.After(now) {
				iw.downSince = now
			}
			e.Counters.Inc(CounterInputOutage)
			e.logger().LogAttrs(context.Background(), slog.LevelError,
				"monitoring outage: the monitor's input is down; no lost_link is judged until it is back",
				obs.Dependency("nats"), slog.Time("down_since", iw.downSince.UTC()))
		}
		return conformance.Feed{Down: true, BackAt: iw.backAt}
	}
	returned := !iw.downSince.IsZero() || (since.After(iw.started) && since.After(iw.backAt))
	if returned && since.After(iw.backAt) {
		from := iw.downSince
		if from.IsZero() {
			// Down and back between two reads: counted, from unknown.
			e.Counters.Inc(CounterInputOutage)
		}
		iw.backAt, iw.downSince = since, time.Time{}
		e.logger().LogAttrs(context.Background(), slog.LevelWarn,
			"monitoring outage ended: lost_link is judged again from the input's return plus lost_link_s",
			obs.Dependency("nats"), slog.Time("down_since", from.UTC()), slog.Time("back_at", since.UTC()),
			slog.Time("lost_link_suspended_until", since.Add(secs(lostLinkS)).UTC()))
	} else if returned {
		iw.downSince = time.Time{}
	}
	return conformance.Feed{BackAt: iw.backAt}
}

// outage is the watch's state at now for the status line.
func (iw *inputWatch) outage(now time.Time, lostLinkS float64) Outage {
	var o Outage
	if iw == nil {
		return o
	}
	iw.mu.Lock()
	defer iw.mu.Unlock()
	if !iw.downSince.IsZero() {
		d := iw.downSince.UTC()
		o.DownSince = &d
	}
	if !iw.backAt.IsZero() {
		b := iw.backAt.UTC()
		o.BackAt = &b
		if until := iw.backAt.Add(secs(lostLinkS)); now.Before(until) && o.DownSince == nil {
			u := until.UTC()
			o.SuspendedUntil = &u
		}
	}
	return o
}
