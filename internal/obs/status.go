package obs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Status writes one status line per interval with every component's
// counters and every dependency's state, so that silence is told apart
// from health (E-09): a component with no counter moved still appears,
// empty, and every dependency appears with its state.
type Status struct {
	Logger   *slog.Logger
	Interval time.Duration
	Health   *Health

	mu         sync.Mutex
	components map[string]*core.Counters
	started    time.Time
}

// Add puts one component's counters on the line.
func (s *Status) Add(component string, c *core.Counters) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.components == nil {
		s.components = map[string]*core.Counters{}
	}
	s.components[component] = c
}

// Emit runs the dependency checks and writes one status line now.
func (s *Status) Emit(ctx context.Context) {
	s.mu.Lock()
	if s.started.IsZero() {
		s.started = time.Now()
	}
	counters := make(map[string]map[string]uint64, len(s.components))
	for name, c := range s.components {
		counters[name] = c.Snapshot()
	}
	uptime := time.Since(s.started)
	s.mu.Unlock()
	attrs := []slog.Attr{
		slog.Int64("uptime_s", int64(uptime.Seconds())),
		slog.Any("counters", counters),
	}
	if s.Health != nil {
		rep := s.Health.Check(ctx)
		deps := make(map[string]string, len(rep.Dependencies))
		for name, d := range rep.Dependencies {
			deps[name] = string(d.State)
		}
		attrs = append(attrs, slog.String("readiness", rep.Status), slog.Any("dependencies", deps))
	}
	s.Logger.LogAttrs(ctx, slog.LevelInfo, "status", attrs...)
}

// Run writes a status line at once, then every Interval, until ctx ends.
func (s *Status) Run(ctx context.Context) {
	s.mu.Lock()
	s.started = time.Now()
	s.mu.Unlock()
	s.Emit(ctx)
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Emit(ctx)
		}
	}
}
