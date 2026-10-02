package ridsp

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// DefaultFeedRetry is the wait before a feed that could not open its
// consumer tries again.
const DefaultFeedRetry = 2 * time.Second

// TrackSource delivers trk.v1 messages from a moment on, then as they
// come, to handle, until stop is called.
type TrackSource interface {
	Open(ctx context.Context, from time.Time, handle func(data []byte)) (stop func(), err error)
}

// JSTracks is TrackSource on the TRK stream: an ordered consumer from
// the moment asked, which follows NATS through reconnects on its own.
type JSTracks struct{ JS jetstream.JetStream }

// Open implements TrackSource.
func (j JSTracks) Open(ctx context.Context, from time.Time, handle func([]byte)) (func(), error) {
	cons, err := j.JS.OrderedConsumer(ctx, bus.StreamTRK, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{bus.SubjectTrkAll}, DeliverPolicy: jetstream.DeliverByStartTimePolicy, OptStartTime: &from,
	})
	if err != nil {
		return nil, err
	}
	cc, err := cons.Consume(func(m jetstream.Msg) { handle(m.Data()) })
	if err != nil {
		return nil, err
	}
	return cc.Stop, nil
}

// TrackFeed fills the window from trk.v1 starting Horizon back, so a
// restarted rid-sp serves the last minute at once (B-05), then every new
// track as it is captured. A source that cannot be opened (NATS down,
// TRK missing) is retried every Retry and says so on /readyz.
type TrackFeed struct {
	Source TrackSource
	Window *Window
	Retry  time.Duration
	Logger *slog.Logger
	Now    func() time.Time

	mu     sync.Mutex
	up     bool
	since  time.Time
	reason string
}

func (f *TrackFeed) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *TrackFeed) set(up bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.since.IsZero() || f.up != up {
		f.since = f.now().UTC()
	}
	f.up, f.reason = up, reason
}

// Probe is the readiness entry trk: up while the consumer runs, unknown
// before it opened, down since T with the reason while it cannot open.
func (f *TrackFeed) Probe() obs.Probe {
	return func(context.Context) (obs.State, string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.since.IsZero():
			return obs.StateUnknown, "the track consumer has not opened yet"
		case !f.up:
			return obs.StateDown, "down since " + f.since.Format(time.RFC3339) + ": " + f.reason +
				"; /uss/flights serves only what the window already holds"
		}
		return obs.StateUp, ""
	}
}

// Run feeds the window until ctx ends.
func (f *TrackFeed) Run(ctx context.Context) {
	retry := f.Retry
	if retry <= 0 {
		retry = DefaultFeedRetry
	}
	logger := f.Logger
	if logger == nil {
		logger = obs.Discard()
	}
	for ctx.Err() == nil {
		stop, err := f.Source.Open(ctx, f.now().Add(-Horizon), f.Window.Take)
		if err != nil {
			f.set(false, "the TRK consumer does not open: "+err.Error())
			logger.LogAttrs(ctx, slog.LevelWarn, "track feed not open; retried", slog.Duration("retry_in", retry), obs.Err(err))
			t := time.NewTimer(retry)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
			continue
		}
		f.set(true, "")
		<-ctx.Done()
		stop()
	}
}
