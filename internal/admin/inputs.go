package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/sources"
)

// SubjectSources is every source status (source/status/v1).
const SubjectSources = "src.v1.>"

// KnownSourceTypes are the adapter types of envelope/v1 this USSP runs
// (sitl is the lab's and never a production input): each is listed on
// the inputs page, never_heard until a status of it arrives.
var KnownSourceTypes = []string{"operator_ws", "network_rid", "direct_rid", "ansp_feed", "adsb_rx"}

// Bounds and periods of the inputs tracker (E-10).
const (
	// MaxSources bounds the source instances held; past it a new one is
	// counted and not held (an operator client per instance: 4096 is
	// well above the demo's).
	MaxSources = 4096
	// MaxMonitorInstances bounds the monitor instances read.
	MaxMonitorInstances = 64
	// SourceSilentAfter is how long a source may go without a status (it
	// publishes one every 2 s) before the console says it is not heard.
	SourceSilentAfter = 10 * time.Second
	// MaxStatusBytes bounds one source/status/v1 message read.
	MaxStatusBytes = 64 << 10
	// MonitorReadTimeout bounds the read of monitor_status.
	MonitorReadTimeout = 2 * time.Second
)

// Counters of the tracker.
const (
	CounterSourceUnreadable = "admin_source_status_unreadable"
	CounterSourcesOverBound = "admin_source_status_over_bound"
	CounterMonitorUnread    = "admin_monitor_status_unread"
	CounterSwitchesUnread   = "admin_source_switches_unread"
)

// States of the source switches on the inputs page.
const (
	SwitchesRead        = "read"
	SwitchesUnavailable = "unavailable"
)

// Console states of an input (the kit's source states, and unknown while
// the bus is not connected).
const (
	InputHealthy     = "healthy"
	InputStale       = "stale"
	InputLagging     = "lagging"
	InputUnreachable = "unreachable"
	InputNeverHeard  = "never_heard"
	InputDisabled    = "disabled"
	InputUnknown     = "unknown"
)

// heard is the last status of one source instance.
type heard struct {
	body     sources.StatusBody
	counters map[string]uint64
	lagS     *float64
	lagging  bool
	at       time.Time
}

// Link reports the bus connection (bus.Conn.Link).
type Link func() (connected bool, since time.Time, reason string)

// Inputs holds every source status heard on src.v1 (core NATS) and reads
// the monitor's from the KV bucket monitor_status. Take is the
// subscription's handler; it never blocks.
type Inputs struct {
	Link Link
	// Monitors reads monitor_status (bus.MonitorStatuses); nil says the
	// monitor's status is not read on this process.
	Monitors func(ctx context.Context) ([]bus.MonitorEntry, error)
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time

	mu    sync.Mutex
	srcs  map[string]heard
	over  bool
	start time.Time
}

func (in *Inputs) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

func (in *Inputs) count(name string) {
	if in.Counters != nil {
		in.Counters.Inc(name)
	}
}

// Take records one src.v1 message: refused unread when it is over
// MaxStatusBytes, not source/status/v1, or has no source.
func (in *Inputs) Take(_ string, data []byte) {
	var m struct {
		Schema string `json:"schema"`
		Body   struct {
			sources.StatusBody
			LagS    *float64 `json:"lag_s"`
			Lagging bool     `json:"lagging"`
		} `json:"body"`
	}
	if len(data) > MaxStatusBytes || json.Unmarshal(data, &m) != nil || m.Schema != sources.SchemaStatus || m.Body.Source == "" {
		in.count(CounterSourceUnreadable)
		return
	}
	key := m.Body.Source + "/"
	if m.Body.SourceInstance != nil {
		key += *m.Body.SourceInstance
	}
	now := in.now()
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.srcs == nil {
		in.srcs = map[string]heard{}
	}
	if _, ok := in.srcs[key]; !ok && len(in.srcs) >= MaxSources {
		in.over = true
		in.count(CounterSourcesOverBound)
		return
	}
	in.srcs[key] = heard{body: m.Body.StatusBody, counters: maps.Clone(m.Body.Counters), lagS: m.Body.LagS, lagging: m.Body.Lagging, at: now}
}

// Run keeps the src.v1 subscription open until ctx ends, retrying while
// NATS is down (B-08).
func (in *Inputs) Run(ctx context.Context, listen func(subject string, handle func(string, []byte)) (func(), error)) {
	in.mu.Lock()
	in.start = in.now()
	in.mu.Unlock()
	for ctx.Err() == nil {
		stop, err := listen(SubjectSources, in.Take)
		if err != nil {
			if in.Logger != nil {
				in.Logger.LogAttrs(ctx, slog.LevelWarn, "source status subscription not open; retried", obs.Err(err))
			}
			t := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
			continue
		}
		<-ctx.Done()
		stop()
	}
}

// Disabled says who switched an input off.
type Disabled struct {
	By     string    `json:"by"`
	ByWho  string    `json:"by_who"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// Input is one source type or instance on the inputs page.
type Input struct {
	Source         string            `json:"source"`
	SourceInstance *string           `json:"source_instance,omitempty"`
	State          string            `json:"state"`
	Since          *time.Time        `json:"since,omitempty"`
	LastHeardAt    *time.Time        `json:"last_heard_at,omitempty"`
	AgeS           *float64          `json:"age_s,omitempty"`
	LagS           *float64          `json:"lag_s,omitempty"`
	Disabled       *Disabled         `json:"disabled,omitempty"`
	Detail         *string           `json:"detail,omitempty"`
	Counters       map[string]uint64 `json:"counters,omitempty"`
}

// Bus is api's link to NATS as the inputs page shows it.
type Bus struct {
	State  string    `json:"state"`
	Since  time.Time `json:"since"`
	Detail *string   `json:"detail,omitempty"`
}

// MonitorInstance is one monitor's status as the console shows it.
type MonitorInstance struct {
	bus.MonitorStatus
	AgeS float64 `json:"age_s"`
}

// Monitor is the monitor's state on the inputs page.
type Monitor struct {
	State       string            `json:"state"`
	LastHeardAt *time.Time        `json:"last_heard_at,omitempty"`
	Detail      string            `json:"detail"`
	Instances   []MonitorInstance `json:"instances"`
}

// SwitchesState says whether the stored source switches were read for the
// answer: while they are not, no input is shown disabled by a switch,
// and whether one is switched off is not known (rule 7).
type SwitchesState struct {
	State  string  `json:"state"`
	Detail *string `json:"detail,omitempty"`
}

// InputsView is GET /v1/admin/inputs.
type InputsView struct {
	CheckedAt        time.Time                       `json:"checked_at"`
	Bus              Bus                             `json:"bus"`
	Switches         SwitchesState                   `json:"switches"`
	Sources          []Input                         `json:"sources"`
	SourcesTruncated bool                            `json:"sources_truncated"`
	Monitor          Monitor                         `json:"monitor"`
	Dependencies     map[string]obs.DependencyStatus `json:"dependencies"`
}

// SwitchRow is one stored switch as the inputs page applies it.
type SwitchRow struct {
	SourceType string
	InstanceID *string
	Enabled    bool
	Actor      string
	Reason     string
	ChangedAt  time.Time
}

// stateOf maps a source/status/v1 state onto the console's: a live
// source that says it is lagging (its own judgement, the B-03 extra of
// operator_ws) is lagging.
func stateOf(s string, lagging bool) string {
	switch s {
	case sources.StateLive:
		if lagging {
			return InputLagging
		}
		return InputHealthy
	case sources.StateStale:
		return InputStale
	case sources.StateDown:
		return InputUnreachable
	case sources.StateDisabled:
		return InputDisabled
	}
	return InputUnknown
}

// view is every input: each instance heard and each switch, every known
// type, with its state and time. A switch row that disables a type or
// an instance overrides what the source said (the database is the
// truth of who switched it off); a status older than SourceSilentAfter
// is unreachable since it was last heard; while the bus is not
// connected every input is unknown since then, with its last status.
func (in *Inputs) view(now time.Time, switches []SwitchRow, staff func(string) string) (Bus, []Input, bool) {
	connected, since, why := true, in.startTime(now), ""
	if in.Link != nil {
		connected, since, why = in.Link()
		if since.IsZero() {
			since = in.startTime(now)
		}
	}
	b := Bus{State: "connected", Since: since.UTC()}
	if !connected {
		b.State = "disconnected"
		d := "api's subscription to the source statuses (src.v1) is not connected: every input is unknown since then"
		if why != "" {
			d += " (" + why + ")"
		}
		b.Detail = &d
	}
	in.mu.Lock()
	srcs := maps.Clone(in.srcs)
	over := in.over
	in.mu.Unlock()
	type key struct{ src, inst string }
	all := map[key]*Input{}
	get := func(src string, inst *string) *Input {
		k := key{src: src}
		if inst != nil {
			k.inst = *inst
		}
		if i, ok := all[k]; ok {
			return i
		}
		i := &Input{Source: src, SourceInstance: inst, State: InputNeverHeard}
		all[k] = i
		return i
	}
	for _, t := range KnownSourceTypes {
		get(t, nil)
	}
	for k := range srcs {
		h := srcs[k]
		var inst *string
		if h.body.SourceInstance != nil {
			v := *h.body.SourceInstance
			inst = &v
		}
		i := get(h.body.Source, inst)
		heardAt, st := h.at.UTC(), h.body.Since.UTC()
		i.LastHeardAt, i.AgeS, i.LagS, i.Counters = &heardAt, h.body.AgeS, h.lagS, h.counters
		i.State, i.Since = stateOf(h.body.State, h.lagging), &st
		if h.body.Detail != "" {
			d := h.body.Detail
			i.Detail = &d
		}
		if now.Sub(h.at) > SourceSilentAfter {
			i.State, i.Since = InputUnreachable, &heardAt
			d := fmt.Sprintf("no status from this source since %s", heardAt.Format(time.RFC3339))
			i.Detail = &d
		}
	}
	// The switches, decided by core's source-control model (the one
	// judgement of which source is enabled, CLAUDE.md rule 3): a type
	// switched off disables every instance of it, even one whose own row
	// is on; an instance row decides its instance. A disabled input is
	// labelled with the row that disables it: who, when and why.
	st := coresources.State{Controls: make([]coresources.Control, 0, len(switches))}
	typeRow, instRow := map[string]SwitchRow{}, map[key]SwitchRow{}
	for _, w := range switches {
		get(w.SourceType, w.InstanceID)
		st.Controls = append(st.Controls, coresources.Control{SourceType: w.SourceType, InstanceID: w.InstanceID, Enabled: w.Enabled})
		if w.InstanceID == nil {
			typeRow[w.SourceType] = w
		} else {
			instRow[key{w.SourceType, *w.InstanceID}] = w
		}
	}
	for k, i := range all {
		var inst *string
		if k.inst != "" {
			inst = &k.inst
		}
		d := st.Query(k.src, inst)
		if d.Enabled || d.WhyDisabled == nil {
			continue
		}
		w, by := typeRow[k.src], string(coresources.WhyType)
		if *d.WhyDisabled == coresources.WhyInstance {
			w, by = instRow[k], string(coresources.WhyInstance)
		}
		at := w.ChangedAt.UTC()
		i.State, i.Since = InputDisabled, &at
		i.Disabled = &Disabled{By: by, ByWho: staff(w.Actor), At: at, Reason: w.Reason}
	}
	if !connected {
		s := since.UTC()
		for _, i := range all {
			if i.State != InputDisabled {
				i.State, i.Since = InputUnknown, &s
			}
		}
	}
	out := make([]Input, 0, len(all))
	for _, i := range all {
		out = append(out, *i)
	}
	slices.SortFunc(out, func(a, b Input) int {
		if a.Source != b.Source {
			if a.Source < b.Source {
				return -1
			}
			return 1
		}
		ai, bi := "", ""
		if a.SourceInstance != nil {
			ai = *a.SourceInstance
		}
		if b.SourceInstance != nil {
			bi = *b.SourceInstance
		}
		switch {
		case ai < bi:
			return -1
		case ai > bi:
			return 1
		}
		return 0
	})
	return b, out, over
}

// startTime is when Run started, or now (the caller's reading of the
// tracker's clock) before it has.
func (in *Inputs) startTime(now time.Time) time.Time {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.start.IsZero() {
		return now
	}
	return in.start
}

// monitor reads every instance's status from monitor_status: up when one
// was stored within missingS, down otherwise ("conformance and traffic
// alerts stopped since" the newest), unknown when the bucket cannot be
// read.
func (in *Inputs) monitor(ctx context.Context, now time.Time, missingS float64) Monitor {
	out := Monitor{Instances: []MonitorInstance{}}
	if in.Monitors == nil {
		out.State, out.Detail = "unknown", "the monitor's status is not read on this process"
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, MonitorReadTimeout)
	defer cancel()
	entries, err := in.Monitors(ctx)
	if err != nil {
		in.count(CounterMonitorUnread)
		out.State = "unknown"
		out.Detail = "the monitor's status (monitor_status) cannot be read: whether conformance and traffic alerts run is unknown (" + err.Error() + ")"
		return out
	}
	for i := range entries {
		e := &entries[i]
		out.Instances = append(out.Instances, MonitorInstance{MonitorStatus: e.Status, AgeS: ageS(now, e.Stored)})
		if c := e.Stored.UTC(); out.LastHeardAt == nil || c.After(*out.LastHeardAt) {
			out.LastHeardAt = &c
		}
	}
	slices.SortFunc(out.Instances, func(a, b MonitorInstance) int {
		switch {
		case a.Instance < b.Instance:
			return -1
		case a.Instance > b.Instance:
			return 1
		}
		return 0
	})
	switch {
	case len(out.Instances) == 0:
		out.State = "down"
		out.Detail = "no monitor has written its status: conformance and traffic alerts are not running"
	case out.LastHeardAt != nil && now.Sub(*out.LastHeardAt).Seconds() > missingS:
		out.State = "down"
		out.Detail = "conformance and traffic alerts stopped since " + out.LastHeardAt.Format(time.RFC3339) +
			fmt.Sprintf(": no monitor status for %.0f s (monitor_status_missing_s %.0f)", now.Sub(*out.LastHeardAt).Seconds(), missingS)
	default:
		out.State = "up"
		out.Detail = fmt.Sprintf("%d monitor instance(s) writing their status", len(out.Instances))
	}
	return out
}

// InputsView is GET /v1/admin/inputs. It reads one clock once, the
// tracker's (Inputs.Now), after the switches are read: the clock that
// stamped when each source status was heard and when the tracker
// started, so a source's silence is measured on the clock that stamped
// it and the Service's clock is never compared with those stamps. The
// same reading is checked_at and the "now" of the monitor's ages.
func (s *Service) InputsView(ctx context.Context) (InputsView, error) {
	out := InputsView{Dependencies: map[string]obs.DependencyStatus{}, Sources: []Input{}}
	var switches []SwitchRow
	out.Switches = SwitchesState{State: SwitchesRead}
	if s.Switches == nil {
		d := "the source switches are not read on this process: whether an input is switched off is not known"
		out.Switches = SwitchesState{State: SwitchesUnavailable, Detail: &d}
	} else {
		rows, err := s.Switches.Rows(ctx)
		if err != nil {
			// The cause is logged and counted, never sent.
			s.count(CounterSwitchesUnread)
			s.logger().WarnContext(ctx, "source switches not read; inputs shown without them", "error", err.Error())
			d := "the source switches cannot be read: no input is shown switched off, and whether one is is not known"
			out.Switches = SwitchesState{State: SwitchesUnavailable, Detail: &d}
			rows = nil
		}
		switches = rows
	}
	staff := s.staffNames(ctx, actorsOf(switches))
	in := s.Inputs
	if in == nil {
		in = &Inputs{Now: s.Now}
	}
	now := in.now()
	out.CheckedAt = now.UTC()
	out.Bus, out.Sources, out.SourcesTruncated = in.view(now, switches, staff)
	out.Monitor = in.monitor(ctx, now, s.policy().Values.MonitorStatusMissingS)
	if s.Health != nil {
		out.Dependencies = s.Health(ctx).Dependencies
	}
	return out, nil
}

func actorsOf(rows []SwitchRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Actor)
	}
	return out
}
