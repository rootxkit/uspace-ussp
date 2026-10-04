// Package admin is the USSP console's side of api (docs/PLAN.md §4,
// §6.1; brief WP-18; spec 01 §3 S11, 02 F5, 05 §6, 07 S-M5): the reads
// of the console pages (active flights, alerts and escalations, the DSS
// state, every input with its state and time, the policy and its
// history, the source switches, the emergency cases, the record days,
// the audit rows of a console entity) and the console's own writes (a
// supervisor's escalation and close of an alert, the emergency
// workflow, a policy version, a source switch).
//
// The console informs people; nothing here has a path towards an
// aircraft (CLAUDE.md rule 1), and api never re-judges what the monitor
// decided (§3.2): closing an alert is the console's handling of it, never
// its clearing. Every write is validated before anything is written, is
// one transaction with its events row (actor and reason), and anything it
// publishes on the bus goes after the commit; the policy and the source
// switches keep their KV projection inside the transaction, refused with
// 503 when the KV cannot take it (B-09). Every time written is the
// database clock. Every list is bounded and says when it was cut.
package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Bounds of the console's lists (E-10).
const (
	MaxFlights      = 500
	MaxAlerts       = 500
	MaxOutboxErrors = 20
	MaxSubs         = 200
	MaxPolicies     = 50
	MaxCases        = 200
	MaxNotes        = 5000
	MaxEvents       = 500
	DefaultEvents   = 100
	MaxReason       = 500
	MaxNote         = 2000
	// RecentAlerts is how far back the recent view of the alerts reaches.
	RecentAlerts = 24 * time.Hour
	// ClosedCases is how long a closed emergency case stays listed.
	ClosedCases = 7 * 24 * time.Hour
	// RecordWindowDays is how many UTC days the record page covers.
	RecordWindowDays = 31
)

// Problem slugs of the console's refusals.
const (
	SlugNotFound      = "not_found"
	SlugAlreadyClosed = "already_closed"
	SlugCaseOpen      = "case_open"
	SlugNoOpenCase    = "no_open_case"
	SlugPolicyChanged = "policy_changed"
	SlugUnavailable   = "admin_unavailable"
)

// Error is a refusal with its status, slug and a detail a person reads
// (httpx.StatusError): 404, 409 or 503.
type Error struct {
	Status int
	Slug   string
	Detail string
}

func (e *Error) Error() string { return e.Slug + ": " + e.Detail }

// HTTPStatus is the answer's status.
func (e *Error) HTTPStatus() int { return e.Status }

// ProblemSlug is the problem type.
func (e *Error) ProblemSlug() string { return e.Slug }

// ProblemDetail is the sentence the console shows.
func (e *Error) ProblemDetail() string { return e.Detail }

func notFound(what string) error {
	return &Error{Status: http.StatusNotFound, Slug: SlugNotFound, Detail: what + " not found"}
}

func conflict(slug, detail string) error {
	return &Error{Status: http.StatusConflict, Slug: slug, Detail: detail}
}

// Service is the console's handle on the record. Store is the relational
// database (api is its only writer); the other fields are optional and
// their absence is said in the answers, never hidden.
type Service struct {
	Store *store.Store
	// Current is the policy in force (its defaults while none is stored).
	Current func() policy.Record
	// Samples reads the newest sample of flights from the telemetry
	// record; nil says the record is not configured.
	Samples SampleReader
	// Republisher republishes an alert the console escalated, after its
	// commit (alerts.Service).
	Republisher Republisher
	// Policies stores a policy version (policy.Service).
	Policies PolicyPutter
	// Switches writes and lists the source switches (sources.Writer).
	Switches SourceSwitcher
	// Inputs tracks the source statuses and the monitor's (Inputs).
	Inputs *Inputs
	// Health is the readiness of api's dependencies (rt.Health.Check).
	Health func(ctx context.Context) obs.Report
	// RecordLink is the record path of a flight.
	RecordLink func(flightID string) string
	Counters   *core.Counters
	Logger     *slog.Logger
	Now        func() time.Time

	mu sync.Mutex
	// samplesSince is when the telemetry record began to fail (zero
	// while it answers).
	samplesSince time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

func (s *Service) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func (s *Service) policy() policy.Record {
	if s.Current != nil {
		return s.Current()
	}
	return policy.Record{Values: policy.Defaults()}
}

// reason is a required, bounded free text: refused empty or longer than
// limit bytes.
func reason(field, v string, limit int) error {
	switch {
	case v == "":
		return core.Fieldf(field, "required")
	case len(v) > limit:
		return core.Fieldf(field, "longer than %d bytes", limit)
	}
	return nil
}

// object is raw JSON as an object; anything else (absent, null, not an
// object) is {}.
func object(raw []byte) map[string]any {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func ageS(now, at time.Time) float64 { return max(0, now.Sub(at).Seconds()) }
