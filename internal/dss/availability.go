package dss

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Counter names of the availability poll.
const (
	CounterAvailabilityPolled  = "dss_availability_polled"
	CounterAvailabilityFailed  = "dss_availability_poll_failed"
	CounterAvailabilityChanged = "dss_availability_changed"
)

// DefaultAvailabilityEvery is the period of the availability poll.
const DefaultAvailabilityEvery = time.Minute

// Availability is this USSP's availability as the DSS holds it (F3548
// UssAvailabilityState, set by the authority): GET
// /dss/v1/uss_availability/{our id} every Every, recorded in dss_state
// and kept here. Down stops every new DSS write (intents wait pending_dss
// with uss_availability_down); Normal resumes them. Safe for concurrent
// use.
type Availability struct {
	Client *Client
	Store  Store
	// USSID is the configured client id; the id polled is the manager
	// the DSS records for us (Client.Manager: the sub of our tokens),
	// USSID only when no DSS token can be had.
	USSID    string
	Every    time.Duration
	Counters *core.Counters
	Logger   *slog.Logger
	// OnChange, when set, is called after a change is recorded (api
	// publishes ctl.dss_state).
	OnChange func(state f3548.UssAvailabilityState)

	mu      sync.Mutex
	loaded  bool
	known   bool
	state   f3548.UssAvailabilityState
	version string
	at      time.Time
	err     string
}

func (a *Availability) count(name string) {
	if a.Counters != nil {
		a.Counters.Inc(name)
	}
}

func (a *Availability) logger() *slog.Logger {
	if a.Logger == nil {
		return obs.Discard()
	}
	return a.Logger
}

// Down reports whether the authority holds this USSP Down.
func (a *Availability) Down() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.known && a.state == f3548.Down
}

// State is the availability last known, whether it is known, when it
// was read and the last poll's error.
func (a *Availability) State() (state f3548.UssAvailabilityState, known bool, at time.Time, lastErr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, a.known, a.at, a.err
}

// Loaded reports whether the availability recorded in dss_state has
// been loaded, or read from the DSS: before that a Down the authority set
// is not known, and no new DSS write may be made. A nil *Availability is
// always loaded (it never stops a write).
func (a *Availability) Loaded() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.loaded
}

// WaitLoaded loads the recorded availability, again every retry until
// it succeeds or ctx ends (the writer starts only after it).
func (a *Availability) WaitLoaded(ctx context.Context, retry time.Duration) error {
	for {
		err := a.Load(ctx)
		if err == nil {
			return nil
		}
		a.logger().LogAttrs(ctx, slog.LevelWarn, "recorded USS availability not read; no DSS write is made until it is", obs.Err(err))
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Load takes the availability recorded in dss_state (a restart keeps a
// Down the authority set until the next poll says otherwise).
func (a *Availability) Load(ctx context.Context) error {
	st, err := a.Store.State(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loaded = true
	if st.Availability == "" {
		return nil
	}
	if !a.known {
		a.known, a.state = true, f3548.UssAvailabilityState(st.Availability)
		if st.SetAt != nil {
			a.at = *st.SetAt
		}
	}
	return nil
}

// Poll reads the availability once and records it.
func (a *Availability) Poll(ctx context.Context) error {
	id := a.USSID
	if m, merr := a.Client.Manager(ctx); merr == nil {
		id = m
	}
	res, err := a.Client.Availability(ctx, id)
	if err != nil {
		a.count(CounterAvailabilityFailed)
		a.mu.Lock()
		a.err = clipErr(err)
		a.mu.Unlock()
		_ = a.Store.SetReachable(ctx, !errors.Is(err, ErrDSSDown))
		return err
	}
	a.count(CounterAvailabilityPolled)
	_ = a.Store.SetReachable(ctx, true)
	state := res.Status.Availability
	changed, err := a.Store.SetAvailability(ctx, string(state), "dss")
	if err != nil {
		return err
	}
	// The read time is the database clock, as dss_state's set_at.
	at, err := a.Store.Now(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	prev, wasKnown := a.state, a.known
	a.loaded, a.known, a.state, a.version, a.at, a.err = true, true, state, res.Version, at.UTC(), ""
	a.mu.Unlock()
	if changed || !wasKnown || prev != state {
		a.count(CounterAvailabilityChanged)
		a.logger().LogAttrs(ctx, slog.LevelWarn, "this USSP's availability in the DSS", slog.String("availability", string(state)),
			slog.String("previous", string(prev)))
		if a.OnChange != nil {
			a.OnChange(state)
		}
	}
	return nil
}

func (a *Availability) every() time.Duration {
	if a.Every <= 0 {
		return DefaultAvailabilityEvery
	}
	return a.Every
}

// Run polls every Every until ctx ends.
func (a *Availability) Run(ctx context.Context) {
	if err := a.Load(ctx); err != nil {
		a.logger().LogAttrs(ctx, slog.LevelWarn, "recorded USS availability not read", obs.Err(err))
	}
	t := time.NewTicker(a.every())
	defer t.Stop()
	for {
		if err := a.Poll(ctx); err != nil && ctx.Err() == nil {
			a.logger().LogAttrs(ctx, slog.LevelWarn, "USS availability not read from the DSS; polled again", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Gate is the DSS as a decision sees it (intent.DSS): whether the
// strategic coordination write can be made now.
type Gate struct {
	Client       *Client
	Availability *Availability
	// Unconfigured, when set, is why there is no DSS (no base URL, no
	// token client): every decision that needs it waits pending_dss.
	Unconfigured string
}

var _ intent.DSS = (*Gate)(nil)

// Available implements intent.DSS: false with uss_availability_down while
// the authority holds us Down, false while the DSS does not answer, true
// otherwise (and before the first call: the write itself finds out).
func (g *Gate) Available(context.Context) (bool, string) {
	switch {
	case g == nil || g.Unconfigured != "":
		why := "no DSS is configured"
		if g != nil {
			why = g.Unconfigured
		}
		return false, why
	case g.Availability.Down():
		return false, intent.ReasonUSSAvailabilityDown + ": the authority set this USSP's availability Down in the DSS"
	}
	if g.Client != nil {
		if r := g.Client.Reach(); r.Known && !r.Up {
			return false, "the DSS does not answer since " + r.Since.Format(time.RFC3339) + ": " + r.Reason
		}
	}
	return true, ""
}
