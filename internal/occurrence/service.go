package occurrence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Deliverer submits one report's bytes to the authority and returns its
// reference. Nil while the authority publishes no POST /v1/occurrences
// (errNoDeliverer).
type Deliverer interface {
	Submit(ctx context.Context, body []byte) (authorityRef string, err error)
}

// PermanentError is a refusal the authority will repeat (409, a 4xx on
// the request itself): the report fails for good.
type PermanentError struct{ Detail string }

func (e *PermanentError) Error() string { return "the authority refused the report: " + e.Detail }

// HoldProjector writes a flight held by a report to record_holds (after
// the report's commit).
type HoldProjector interface {
	Hold(ctx context.Context, flightID string, reasons []string, since time.Time) error
}

// Counters of the Service.
const (
	CounterQueued      = "occurrence_reports_queued"
	CounterNotBuilt    = "occurrence_reports_not_built"
	CounterDelivered   = "occurrence_reports_delivered"
	CounterRetried     = "occurrence_reports_retried"
	CounterFailed      = "occurrence_reports_failed"
	CounterGaveUp      = "occurrence_reports_gave_up"
	CounterStoreFailed = "occurrence_store_failed"
	CounterHoldFailed  = "occurrence_holds_not_projected"
	CounterSweepFailed = "occurrence_sweep_failed"
)

// Defaults of the Service.
const (
	DefaultEvery       = 5 * time.Second
	DefaultBatch       = 100
	DefaultMaxAttempts = 50
	DefaultBackoffMin  = 5 * time.Second
	DefaultBackoffMax  = 10 * time.Minute
	DefaultLease       = 60 * time.Second
	// HoldsEvery is how often every held flight is written to
	// record_holds again (a projection that failed after its commit).
	HoldsEvery = 10 * time.Minute
	// MaxListed bounds the reports the console lists at once.
	MaxListed = 500
)

// DepOccurrences is the readiness dependency of the reports.
const DepOccurrences = "occurrences"

// Service decides, queues and delivers the reports.
type Service struct {
	Store     Store
	Deliverer Deliverer
	Holds     HoldProjector
	Policy    func() policy.Values
	// SystemID is this USSP's code; RecordsURL, when set, is the base of
	// the evidence links (GET {RecordsURL}/v1/records/flights/{id}).
	SystemID    string
	RecordsURL  string
	MaxAttempts int
	Counters    *core.Counters
	Logger      *slog.Logger

	holdMu sync.Mutex
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

func (s *Service) policy() policy.Values {
	if s.Policy == nil {
		return policy.Defaults()
	}
	return s.Policy()
}

// Run detects, delivers and keeps the holds until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultEvery
	}
	go s.projectHoldsLoop(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.Detect(ctx); err != nil && ctx.Err() == nil {
			s.count(CounterSweepFailed)
			s.logger().LogAttrs(ctx, slog.LevelWarn, "occurrence detection failed; the next sweep retries", obs.Err(err))
		}
		s.DeliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func f64(m map[string]any, k string) (float64, bool) {
	v, ok := m[k].(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

var flightTrackRe = regexp.MustCompile(`^trk:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var mannedTrackRe = regexp.MustCompile(`^man:([0-9a-f]{6})$`)

// kindOf is the occurrence kind of an alert the store found.
func kindOf(alertKind string) string {
	switch alertKind {
	case "proximity":
		return KindAirprox
	case "zone_incursion":
		return KindNonconformanceInProhib
	case "lost_link":
		return KindLostLinkInUSpace
	}
	return KindOther
}

func (s *Service) aircraftOf(f FlightRef) Aircraft {
	return Aircraft{Serial: f.Serial, OperatorReg: f.OperatorReg, FlightID: f.FlightID, AuthorisationNumber: f.AuthorisationNumber}
}

func (s *Service) evidence(ids ...string) []string {
	if s.RecordsURL == "" {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strings.TrimRight(s.RecordsURL, "/")+"/v1/records/flights/"+id)
	}
	return out
}

// reportOf builds the report of alert event e, as kind (by a reporter).
func (s *Service) reportOf(ctx context.Context, e AlertEvent, kind, channel, flaggedBy, reporter, narrative string, awareAt time.Time) (Report, error) {
	r := Report{Kind: kind, Channel: channel, FlaggedBy: flaggedBy, SourceKind: "alert", SourceRef: e.SourceRef, Reporter: reporter,
		OccurredAt: e.RaisedAt, AwareAt: awareAt, FlightIDs: []string{e.Flight.FlightID}}
	if e.Flight.IntentID != nil {
		r.IntentIDs = append(r.IntentIDs, *e.Flight.IntentID)
	}
	aircraft := []Aircraft{s.aircraftOf(e.Flight)}
	var manned []Manned
	var sep *Separation
	var d map[string]any
	_ = json.Unmarshal(e.Detail, &d)
	var auto string
	if e.Kind == "proximity" {
		sep = &Separation{At: e.RaisedAt.UTC()}
		h, hok := f64(d, "d_cpa_h_m")
		v, vok := f64(d, "d_alt_m")
		if hok {
			sep.HM = &h
		}
		if vok {
			sep.VM = &v
		}
		peer := ""
		if p, ok := d["peer"].(map[string]any); ok {
			peer, _ = p["track_id"].(string)
		}
		if m := flightTrackRe.FindStringSubmatch(peer); m != nil {
			if fs, err := s.Store.Flights(ctx, []string{m[1]}); err == nil && len(fs) == 1 {
				aircraft = append(aircraft, s.aircraftOf(fs[0]))
				r.FlightIDs = append(r.FlightIDs, fs[0].FlightID)
				if fs[0].IntentID != nil {
					r.IntentIDs = append(r.IntentIDs, *fs[0].IntentID)
				}
			}
		}
		if m := mannedTrackRe.FindStringSubmatch(peer); m != nil {
			manned = append(manned, Manned{ICAO24: m[1]})
		}
		vert := "vertical separation not judged"
		if vok {
			vert = fmt.Sprintf("%.1f m vertically", v)
		}
		auto = fmt.Sprintf("Proximity alert %s: closest approach %s horizontally, %s, with %s.", e.AlertID, metres(sep.HM), vert, peerOf(peer))
	} else {
		auto = fmt.Sprintf("%s alert %s raised %s on flight %s.", e.Kind, e.AlertID, e.RaisedAt.UTC().Format(time.RFC3339), e.Flight.FlightID)
		if zt, ok := d["zone_type"].(string); ok {
			zid, _ := d["zone_id"].(string)
			auto += fmt.Sprintf(" Zone %s (%s).", zid, zt)
		}
	}
	text := auto
	if narrative != "" {
		text = narrative + " (" + auto + ")"
	}
	if err := Build(&r, s.SystemID, aircraft, manned, sep, text, s.evidence(r.FlightIDs...)); err != nil {
		return r, err
	}
	return r, nil
}

func metres(v *float64) string {
	if v == nil {
		return "an unknown distance"
	}
	return fmt.Sprintf("%.1f m", *v)
}

func peerOf(track string) string {
	if track == "" {
		return "an aircraft the alert does not name"
	}
	return "track " + track
}

// Detect queues every occurrence the alerts and flights show (see the
// package documentation). The error is the first failure.
func (s *Service) Detect(ctx context.Context) error {
	pol := s.policy()
	now, err := s.Store.Now(ctx)
	if err != nil {
		return err
	}
	events, err := s.Store.AlertEvents(ctx, Deadline, pol.AirproxReportM, pol.AirproxReportVM, DefaultBatch)
	if err != nil {
		return err
	}
	var errs []error
	for i := range events {
		e := &events[i]
		r, err := s.reportOf(ctx, *e, kindOf(e.Kind), ChannelMandatory, "system", ReporterSystem, "", now)
		if err != nil {
			s.count(CounterNotBuilt)
			s.logger().LogAttrs(ctx, slog.LevelError, "occurrence report not built", slog.String("alert_id", e.AlertID), obs.Err(err))
			continue
		}
		if _, _, err := s.enqueue(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	flights, err := s.Store.EmergencyFlights(ctx, Deadline, DefaultBatch)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for i := range flights {
		f := &flights[i]
		r := Report{Kind: KindEmergency, Channel: ChannelMandatory, FlaggedBy: "system", SourceKind: "flight", SourceRef: f.FlightID,
			Reporter: ReporterSystem, OccurredAt: now, AwareAt: now, FlightIDs: []string{f.FlightID}}
		if f.IntentID != nil {
			r.IntentIDs = []string{*f.IntentID}
		}
		text := fmt.Sprintf("Flight %s declared an emergency (first seen by this USSP at %s).", f.FlightID, now.UTC().Format(time.RFC3339))
		if err := Build(&r, s.SystemID, []Aircraft{s.aircraftOf(*f)}, nil, nil, text, s.evidence(f.FlightID)); err != nil {
			s.count(CounterNotBuilt)
			continue
		}
		if _, _, err := s.enqueue(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// FlagError is a supervisor's flag that cannot be taken.
type FlagError struct {
	NotFound bool
	Reason   string
}

func (e *FlagError) Error() string { return e.Reason }

// Flag is a supervisor's report of an alert (POST /v1/admin/occurrences):
// kind "" takes the alert's own kind; the report is mandatory unless it
// is "other". created is false when the event was reported before (the
// existing report is returned).
func (s *Service) Flag(ctx context.Context, staffID, alertID, kind, narrative string) (Item, bool, error) {
	e, err := s.Store.Alert(ctx, alertID)
	if errors.Is(err, ErrNotFound) {
		return Item{}, false, &FlagError{NotFound: true, Reason: "no alert of a flight of this USSP has this id"}
	}
	if err != nil {
		return Item{}, false, err
	}
	if kind == "" {
		kind = kindOf(e.Kind)
	}
	if !kinds[kind] {
		return Item{}, false, &FlagError{Reason: "unknown kind " + kind}
	}
	channel := ChannelMandatory
	if kind == KindOther {
		channel = ChannelVoluntary
	}
	now, err := s.Store.Now(ctx)
	if err != nil {
		return Item{}, false, err
	}
	r, err := s.reportOf(ctx, e, kind, channel, "supervisor", staffID, narrative, now)
	if err != nil {
		return Item{}, false, &FlagError{Reason: err.Error()}
	}
	return s.enqueue(ctx, r)
}

// enqueue stores r and, after its commit, holds its flights.
func (s *Service) enqueue(ctx context.Context, r Report) (Item, bool, error) {
	it, created, err := s.Store.Enqueue(ctx, r)
	if err != nil {
		return Item{}, false, err
	}
	if created {
		s.count(CounterQueued)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "occurrence report queued for the authority", slog.String("report_ref", r.Ref),
			slog.String("kind", r.Kind), slog.Time("deadline_at", it.DeadlineAt), slog.Any("flight_ids", r.FlightIDs))
		s.hold(ctx, r.FlightIDs, r.Kind, it.BecameAwareAt)
	}
	return it, created, nil
}

func (s *Service) hold(ctx context.Context, ids []string, reason string, since time.Time) {
	if s.Holds == nil {
		return
	}
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	for _, id := range ids {
		if err := s.Holds.Hold(ctx, id, []string{reason}, since); err != nil {
			s.count(CounterHoldFailed)
			s.logger().LogAttrs(ctx, slog.LevelWarn, "record hold not written yet; written again within the period", obs.FlightID(id), obs.Err(err))
		}
	}
}

// ProjectHolds writes every held flight to record_holds again (since:
// the database clock of this write).
func (s *Service) ProjectHolds(ctx context.Context) error {
	ids, err := s.Store.Held(ctx)
	if err != nil {
		return err
	}
	now, err := s.Store.Now(ctx)
	if err != nil {
		return err
	}
	s.hold(ctx, ids, "occurrence", now)
	return nil
}

func (s *Service) projectHoldsLoop(ctx context.Context) {
	t := time.NewTicker(HoldsEvery)
	defer t.Stop()
	for {
		if err := s.ProjectHolds(ctx); err != nil && ctx.Err() == nil {
			s.count(CounterHoldFailed)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Backoff is the wait before try attempts+1.
func Backoff(attempts int) time.Duration {
	d := DefaultBackoffMin
	for i := 1; i < attempts && d < DefaultBackoffMax; i++ {
		d *= 2
	}
	return min(d, DefaultBackoffMax)
}

// DeliverDue submits the due reports and returns how many the authority
// took. Without a Deliverer nothing is claimed: the reports stay
// pending, listed and on /readyz.
func (s *Service) DeliverDue(ctx context.Context) int {
	if s.Deliverer == nil {
		return 0
	}
	qs, err := s.Store.Claim(ctx, 8, DefaultLease)
	if err != nil {
		if ctx.Err() == nil {
			s.count(CounterStoreFailed)
		}
		return 0
	}
	maxTries := s.MaxAttempts
	if maxTries <= 0 {
		maxTries = DefaultMaxAttempts
	}
	n := 0
	for _, q := range qs {
		ref, err := s.Deliverer.Submit(ctx, q.Body)
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		var perm *PermanentError
		switch {
		case err == nil:
			if err := s.Store.Delivered(sctx, q.ID, ref); err != nil {
				s.count(CounterStoreFailed)
			} else {
				n++
				s.count(CounterDelivered)
				s.logger().LogAttrs(ctx, slog.LevelInfo, "occurrence report delivered to the authority", slog.String("report_ref", q.Ref), slog.String("authority_ref", ref))
			}
		case errors.As(err, &perm):
			s.count(CounterFailed)
			_ = s.Store.Fail(sctx, q.ID, err.Error())
			s.logger().LogAttrs(ctx, slog.LevelError, "occurrence report refused for good; it is on the console", slog.String("report_ref", q.Ref), obs.Err(err))
		case q.Attempts >= maxTries:
			s.count(CounterGaveUp)
			_ = s.Store.Fail(sctx, q.ID, fmt.Sprintf("gave up after %d tries: %v", q.Attempts, err))
		default:
			s.count(CounterRetried)
			if serr := s.Store.Retry(sctx, q.ID, err.Error(), Backoff(q.Attempts)); serr != nil {
				s.count(CounterStoreFailed)
			}
			s.logger().LogAttrs(ctx, slog.LevelWarn, "occurrence report not delivered; tried again later", slog.String("report_ref", q.Ref), obs.Err(err))
		}
		cancel()
	}
	return n
}

// Probe is the readiness of the reports: up when every report is
// delivered (or none was made); degraded with the counts and the nearest
// deadline while one is pending or failed; down when one is past its
// deadline undelivered (a critical item); unknown when the reports
// cannot be read. Why nothing can deliver is said on every answer.
func Probe(st Store, delivering bool) obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		s, err := st.Summarise(ctx)
		if err != nil {
			return obs.StateUnknown, "the occurrence reports cannot be read: " + err.Error()
		}
		detail := fmt.Sprintf("%d pending, %d failed, %d past their 72 h deadline", s.Pending, s.Failed, s.Critical)
		if s.Pending+s.Failed > 0 {
			detail += fmt.Sprintf(" (nearest deadline in %.0f s)", s.NearestDeadlineS)
		}
		if !delivering {
			detail = errNoDeliverer.Error() + "; " + detail
		}
		switch {
		case s.Critical > 0:
			return obs.StateDown, detail
		case s.Pending+s.Failed > 0:
			return obs.StateDegraded, detail
		}
		return obs.StateUp, detail
	}
}

// NotDelivering is why reports are not sent when no Deliverer is set.
func NotDelivering() string { return errNoDeliverer.Error() }
