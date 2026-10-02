package obs

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// State is the state of one dependency.
type State string

// The four states (CLAUDE.md rule 7: a failed input is "down since T",
// never absent; a check that has not run is "unknown", never "up").
const (
	StateUp       State = "up"
	StateDegraded State = "degraded"
	StateDown     State = "down"
	StateUnknown  State = "unknown"
)

// The overall readiness of a process.
const (
	StatusReady    = "ready"
	StatusDegraded = "degraded"
	StatusNotReady = "not_ready"
)

// DefaultCheckTimeout bounds one dependency check.
const DefaultCheckTimeout = 2 * time.Second

// Probe checks one dependency and returns its state and, when it is not
// up, why. It must honour ctx.
type Probe func(ctx context.Context) (State, string)

// DependencyStatus is one dependency as /readyz shows it.
type DependencyStatus struct {
	State    State     `json:"state"`
	Required bool      `json:"required"`
	Since    time.Time `json:"since"`
	// AgeS is the time since the dependency was last seen up, in
	// seconds: 0 while it is up, absent when it was never up.
	AgeS   *float64 `json:"age_s,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// Report is the readiness of a process: not_ready while a required
// dependency is down or unknown, degraded while any dependency is not
// up, ready otherwise. Degraded names every dependency that is not up.
type Report struct {
	Status       string                      `json:"status"`
	CheckedAt    time.Time                   `json:"checked_at"`
	Dependencies map[string]DependencyStatus `json:"dependencies"`
	Degraded     []string                    `json:"degraded"`
}

// Ready reports whether the process takes traffic (200 on /readyz).
func (r Report) Ready() bool { return r.Status != StatusNotReady }

type dependency struct {
	required bool
	probe    Probe
	state    State
	since    time.Time
	lastUp   time.Time
	detail   string
}

// Health is the registry of the dependencies of one process. It is a
// prometheus.Collector: ussp_dependency_up{dep} and
// ussp_dependency_age_s{dep} are read from it at scrape time, so the
// age is current even when nobody polls /readyz. Safe for concurrent
// use.
type Health struct {
	logger  *slog.Logger
	timeout time.Duration
	now     func() time.Time

	mu   sync.Mutex
	deps map[string]*dependency

	upDesc, ageDesc *prometheus.Desc
}

// NewHealth returns an empty registry; a zero timeout is
// DefaultCheckTimeout. State changes are logged on logger.
func NewHealth(logger *slog.Logger, timeout time.Duration) *Health {
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	return &Health{
		logger:  logger,
		timeout: timeout,
		now:     time.Now,
		deps:    map[string]*dependency{},
		upDesc: prometheus.NewDesc(Namespace+"_dependency_up",
			"1 while the dependency is up, 0 while it is degraded, down or unknown.", []string{"dep"}, nil),
		ageDesc: prometheus.NewDesc(Namespace+"_dependency_age_s",
			"Seconds since the dependency was last seen up; +Inf when it never was.", []string{"dep"}, nil),
	}
}

// SetClock replaces the clock (tests).
func (h *Health) SetClock(now func() time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = now
}

// Register adds a dependency in state unknown. probe may be nil for a
// dependency whose state is pushed with Set.
func (h *Health) Register(name string, required bool, probe Probe) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deps[name] = &dependency{required: required, probe: probe, state: StateUnknown, since: h.now(), detail: "not checked yet"}
}

// Set records the state of a registered dependency; an unregistered
// name is registered as optional first, so a report is never lost.
func (h *Health) Set(name string, state State, detail string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.deps[name]
	if !ok {
		d = &dependency{state: StateUnknown, since: h.now()}
		h.deps[name] = d
	}
	h.setLocked(name, d, state, detail)
}

func (h *Health) setLocked(name string, d *dependency, state State, detail string) {
	now := h.now()
	if state == StateUp {
		// An up dependency may say what it is up with (the CIS cache:
		// its versions and age); the probes that say nothing give "".
		d.lastUp = now
	}
	if state != d.state {
		h.logger.LogAttrs(context.Background(), levelOf(state), "dependency state changed",
			Dependency(name), slog.String("from", string(d.state)), slog.String("to", string(state)),
			slog.Bool("required", d.required), slog.String("detail", detail))
		d.state, d.since = state, now
	}
	d.detail = detail
}

func levelOf(s State) slog.Level {
	switch s {
	case StateUp:
		return slog.LevelInfo
	case StateDegraded, StateUnknown:
		return slog.LevelWarn
	case StateDown:
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// Check runs every probe concurrently, each bounded by the check
// timeout, records the results and returns the report. A probe that
// overruns its bound is down, with the bound named.
func (h *Health) Check(ctx context.Context) Report {
	h.mu.Lock()
	probes := make(map[string]Probe, len(h.deps))
	for name, d := range h.deps {
		if d.probe != nil {
			probes[name] = d.probe
		}
	}
	h.mu.Unlock()

	type result struct {
		name   string
		state  State
		detail string
	}
	results := make(chan result, len(probes))
	for name, probe := range probes {
		go func() {
			pctx, cancel := context.WithTimeout(ctx, h.timeout)
			defer cancel()
			done := make(chan result, 1)
			go func() {
				s, d := probe(pctx)
				done <- result{name, s, d}
			}()
			select {
			case r := <-done:
				results <- r
			case <-pctx.Done():
				results <- result{name, StateDown, "check did not answer within " + h.timeout.String()}
			}
		}()
	}
	for range probes {
		r := <-results
		h.mu.Lock()
		if d, ok := h.deps[r.name]; ok {
			h.setLocked(r.name, d, r.state, r.detail)
		}
		h.mu.Unlock()
	}
	return h.Snapshot()
}

// Snapshot returns the report without running any probe.
func (h *Health) Snapshot() Report {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	rep := Report{Status: StatusReady, CheckedAt: now.UTC(), Dependencies: make(map[string]DependencyStatus, len(h.deps)), Degraded: []string{}}
	for name, d := range h.deps {
		st := DependencyStatus{State: d.state, Required: d.required, Since: d.since.UTC(), Detail: d.detail}
		switch {
		case d.state == StateUp:
			// Up as of its last check: the age is 0 by definition, not
			// the few milliseconds between that check and this report.
			age := 0.0
			st.AgeS = &age
		case !d.lastUp.IsZero():
			age := math.Round(now.Sub(d.lastUp).Seconds()*1000) / 1000
			st.AgeS = &age
		}
		rep.Dependencies[name] = st
		if d.state == StateUp {
			continue
		}
		rep.Degraded = append(rep.Degraded, name)
		if d.required && (d.state == StateDown || d.state == StateUnknown) {
			rep.Status = StatusNotReady
		} else if rep.Status == StatusReady {
			rep.Status = StatusDegraded
		}
	}
	slices.Sort(rep.Degraded)
	return rep
}

// Describe sends the two dependency metrics.
func (h *Health) Describe(ch chan<- *prometheus.Desc) {
	ch <- h.upDesc
	ch <- h.ageDesc
}

// Collect sends ussp_dependency_up and ussp_dependency_age_s per
// dependency.
func (h *Health) Collect(ch chan<- prometheus.Metric) {
	rep := h.Snapshot()
	for name, d := range rep.Dependencies {
		up := 0.0
		if d.State == StateUp {
			up = 1
		}
		age := math.Inf(1)
		if d.AgeS != nil {
			age = *d.AgeS
		}
		ch <- prometheus.MustNewConstMetric(h.upDesc, prometheus.GaugeValue, up, name)
		ch <- prometheus.MustNewConstMetric(h.ageDesc, prometheus.GaugeValue, age, name)
	}
}
