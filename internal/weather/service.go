package weather

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Area is a product's area, which is also its QNH's area: the circle of
// RadiusM around the station's position.
type Area struct {
	Station string  `json:"station"`
	LatDeg  float64 `json:"lat_deg"`
	LonDeg  float64 `json:"lon_deg"`
	RadiusM float64 `json:"radius_m"`
}

// Content is what weather_products.product holds (the Art. 12(2)
// fields, the change groups and the raw report).
type Content struct {
	Area    Area     `json:"area"`
	QNHArea string   `json:"qnh_area"`
	Fields  Fields   `json:"fields"`
	Changes []Change `json:"changes"`
	Raw     string   `json:"raw"`
}

// NewProduct is a product to store.
type NewProduct struct {
	Station    string
	Kind       Kind
	ObservedAt time.Time
	ValidFrom  time.Time
	ValidTo    time.Time
	Content    Content
}

// Product is one stored product as GET /v1/weather answers it.
type Product struct {
	ID      string `json:"id"`
	Station string `json:"station"`
	Kind    Kind   `json:"kind"`
	Source  string `json:"source"`
	// ObservedAt is the observation of a METAR or SPECI and the issue of
	// a TAF.
	ObservedAt time.Time `json:"observed_at"`
	ValidFrom  time.Time `json:"valid_from"`
	ValidTo    time.Time `json:"valid_to"`
	FetchedAt  time.Time `json:"fetched_at"`
	// AgeS is the time since ObservedAt on the database clock.
	AgeS float64 `json:"age_s"`
	// InForce says the product's validity holds the answer's instant.
	InForce bool `json:"in_force"`
	// Stale says the source has not delivered within the policy's
	// weather_stale_s (or its last fetch failed): this may not be the
	// newest product.
	Stale bool `json:"stale"`
	Content
}

// SourceStatus is the state of the configured source as stored in
// weather_source_status (on the database clock, so it survives a
// restart).
type SourceStatus struct {
	LastAttemptAt *time.Time
	LastSuccessAt *time.Time
	LastFailureAt *time.Time
	// LastError is the failure of the last attempt; "" when it succeeded.
	LastError string
}

// Store is the database side (pgstore implements it).
type Store interface {
	// Now is the database clock.
	Now(ctx context.Context) (time.Time, error)
	// Save stores the products of source, a report already held (source,
	// station, kind, observed_at) left as it is; it returns how many were
	// new.
	Save(ctx context.Context, source string, ps []NewProduct, radiusM float64) (int, error)
	// RecordFetch records the outcome of a fetch of source at the
	// database clock: failure "" is a success.
	RecordFetch(ctx context.Context, source, failure string) error
	// Status is the source's stored state (zero when never fetched).
	Status(ctx context.Context, source string) (SourceStatus, error)
	// Newest is the newest product of each station and kind of source
	// whose area meets box and that was issued at or before at, at most
	// limit.
	Newest(ctx context.Context, source string, box geodesy.BBox, at time.Time, limit int) ([]Product, error)
	// InForce are the products of source whose area meets one of boxes
	// and whose validity overlaps [from, to], newest first, at most limit.
	InForce(ctx context.Context, source string, boxes []geodesy.BBox, from, to time.Time, limit int) ([]Product, error)
	// Prune deletes at most limit products whose validity ended before
	// before; it returns how many.
	Prune(ctx context.Context, before time.Time, limit int) (int64, error)
}

// Bounds of the service (E-10).
const (
	// MaxAnswer bounds the products of one answer: a METAR, a SPECI and a
	// TAF for every station.
	MaxAnswer = 3 * policy.MaxWeatherStationIDs
	// MaxChecked bounds the products one decision records.
	MaxChecked = 16
	// PruneBatch bounds the products one poll deletes.
	PruneBatch = 1000
	// RetryAfter is the wait after a failed fetch (at most the period).
	RetryAfter = time.Minute
	// MaxFailureBytes bounds the stored failure text.
	MaxFailureBytes = 512
)

// Reasons of a weather_unavailable answer (errors[].reason, field
// weather_source).
const (
	ReasonNotConfigured = "not_configured"
	ReasonNoStations    = "no_stations"
	ReasonDatabase      = "database"
)

// States of the source (SourceState.State).
const (
	StateUp           = "up"
	StateFailing      = "failing"
	StateNeverFetched = "never_fetched"
)

// UnavailableError is the 503 weather_unavailable with its reason.
type UnavailableError struct {
	Reason string
	Detail string
}

func (e *UnavailableError) Error() string {
	return "weather unavailable: " + e.Reason + ": " + e.Detail
}

// Service polls the source, answers GET /v1/weather and the decision's
// check.
type Service struct {
	// Source is the configured source; nil when USSP_WEATHER_SOURCE is
	// unset.
	Source   Source
	Store    Store
	Policy   func() policy.Record
	Counters *core.Counters
	Logger   *slog.Logger
}

func (s *Service) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc("weather_" + name)
	}
}

func (s *Service) add(name string, n int) {
	if s.Counters != nil && n > 0 {
		s.Counters.Add("weather_"+name, uint64(n))
	}
}

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

func (s *Service) policy() policy.Record {
	if s.Policy == nil {
		return policy.Record{Values: policy.Defaults()}
	}
	return s.Policy()
}

// unavailable is why nothing can be answered; nil when something can.
func (s *Service) unavailable(pol policy.Values) *UnavailableError {
	switch {
	case s.Source == nil:
		return &UnavailableError{Reason: ReasonNotConfigured, Detail: "no weather source is configured (USSP_WEATHER_SOURCE)"}
	case s.Store == nil:
		return &UnavailableError{Reason: ReasonDatabase, Detail: "no database is configured on this process"}
	case len(pol.WeatherStationIDs) == 0:
		return &UnavailableError{Reason: ReasonNoStations, Detail: "the policy lists no weather station (weather_station_ids)"}
	}
	return nil
}

// Run polls until ctx ends: at once, then every weather_refresh_s, and
// RetryAfter (at most the period) after a failure. One fetch at a time;
// a fetch is bounded by the source's deadline.
func (s *Service) Run(ctx context.Context) {
	if s.Source == nil || s.Store == nil {
		return
	}
	for {
		wait := time.Duration(s.policy().Values.WeatherRefreshS * float64(time.Second))
		if err := s.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = min(wait, RetryAfter)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Poll fetches the policy's stations once, stores what parsed, records
// the outcome and prunes the products past retention. A failed fetch
// stores nothing and is recorded with its time.
func (s *Service) Poll(ctx context.Context) error {
	rec := s.policy()
	pol := rec.Values
	if u := s.unavailable(pol); u != nil {
		return u
	}
	b, err := s.Source.Fetch(ctx, Stations(pol.WeatherStationIDs))
	if err != nil {
		s.count("fetch_failed")
		s.logger().LogAttrs(ctx, slog.LevelWarn, "weather fetch failed", slog.String("source", s.Source.Name()), obs.Err(err))
		failure := err.Error()
		if failure == "" {
			failure = "the fetch failed"
		}
		if len(failure) > MaxFailureBytes {
			failure = strings.ToValidUTF8(failure[:MaxFailureBytes], "")
		}
		if rerr := s.Store.RecordFetch(ctx, s.Source.Name(), failure); rerr != nil {
			s.count("status_not_recorded")
			obs.Error(ctx, s.logger(), "weather fetch failure not recorded", rerr)
		}
		return err
	}
	if b.Refused > 0 {
		s.add("report_refused", b.Refused)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "weather reports refused", slog.String("source", s.Source.Name()),
			slog.Int("refused", b.Refused), slog.String("reasons", strings.Join(b.Reasons, "; ")))
	}
	ps := make([]NewProduct, 0, len(b.Observed))
	for i := range b.Observed {
		ps = append(ps, productOf(&b.Observed[i], pol))
	}
	n, err := s.Store.Save(ctx, s.Source.Name(), ps, pol.WeatherAreaRadiusM)
	if err != nil {
		s.count("store_failed")
		obs.Error(ctx, s.logger(), "weather products not stored", err)
		_ = s.Store.RecordFetch(ctx, s.Source.Name(), "the products could not be stored")
		return err
	}
	if err := s.Store.RecordFetch(ctx, s.Source.Name(), ""); err != nil {
		s.count("status_not_recorded")
		obs.Error(ctx, s.logger(), "weather fetch not recorded", err)
		return err
	}
	s.add("product_stored", n)
	s.count("fetched")
	if now, err := s.Store.Now(ctx); err == nil {
		before := now.Add(-time.Duration(rec.Values.RecordRetentionDays) * 24 * time.Hour)
		if k, err := s.Store.Prune(ctx, before, PruneBatch); err != nil {
			obs.Error(ctx, s.logger(), "weather products not pruned", err)
		} else {
			s.add("product_pruned", int(k))
		}
	}
	return nil
}

// productOf is the product of one report: a METAR or SPECI in force
// from its observation for weather_observation_valid_s, a TAF over its
// validity.
func productOf(o *Observed, pol policy.Values) NewProduct {
	r := o.Report
	p := NewProduct{Station: r.Station, Kind: r.Kind, ObservedAt: r.IssuedAt, ValidFrom: r.ValidFrom, ValidTo: r.ValidTo,
		Content: Content{Area: Area{Station: r.Station, LatDeg: o.LatDeg, LonDeg: o.LonDeg, RadiusM: pol.WeatherAreaRadiusM},
			QNHArea: r.Station, Fields: r.Fields, Changes: r.Changes, Raw: r.Raw}}
	if r.Kind != KindTAF {
		p.ValidFrom = r.IssuedAt
		p.ValidTo = r.IssuedAt.Add(time.Duration(pol.WeatherObservationValidS * float64(time.Second)))
	}
	if p.Content.Changes == nil {
		p.Content.Changes = []Change{}
	}
	return p
}

// SourceState is the source's state as an answer shows it.
type SourceState struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// LastSuccessAt is the last fetch that delivered; AgeS the time
	// since, on the database clock (nil when never).
	LastSuccessAt *time.Time `json:"last_success_at"`
	AgeS          *float64   `json:"age_s"`
	// LastFailureAt and Failure are the last failed fetch, while it is
	// newer than the last success.
	LastFailureAt *time.Time `json:"last_failure_at"`
	Failure       *string    `json:"failure"`
}

// Answer is GET /v1/weather.
type Answer struct {
	At            time.Time   `json:"at"`
	Stale         bool        `json:"stale"`
	Source        SourceState `json:"source"`
	PolicyVersion int64       `json:"policy_version"`
	Products      []Product   `json:"products"`
}

// state is the source's state at now and whether its products are
// stale: never fetched, failing (the last attempt failed: LastError is
// set, and a success clears it) or older than stale.
func state(name string, st SourceStatus, now time.Time, staleS float64) (SourceState, bool) {
	out := SourceState{Name: name, State: StateNeverFetched, LastSuccessAt: st.LastSuccessAt}
	if st.LastSuccessAt != nil {
		out.State = StateUp
		out.AgeS = ptr(math.Max(0, now.Sub(*st.LastSuccessAt).Seconds()))
	}
	if st.LastError != "" && st.LastFailureAt != nil {
		out.State = StateFailing
		out.LastFailureAt = st.LastFailureAt
		out.Failure = ptr(st.LastError)
	}
	stale := out.State != StateUp || *out.AgeS > staleS
	return out, stale
}

// Answer is the products whose area meets box: the newest of each
// station and kind issued at or before at (now when zero), with its
// age, whether it is in force at at, and whether the source is stale.
func (s *Service) Answer(ctx context.Context, box geodesy.BBox, at time.Time) (Answer, error) {
	rec := s.policy()
	if u := s.unavailable(rec.Values); u != nil {
		return Answer{}, u
	}
	now, err := s.Store.Now(ctx)
	if err != nil {
		return Answer{}, &UnavailableError{Reason: ReasonDatabase, Detail: "the database clock could not be read"}
	}
	if at.IsZero() {
		at = now
	}
	at = at.UTC()
	st, err := s.Store.Status(ctx, s.Source.Name())
	if err != nil {
		return Answer{}, &UnavailableError{Reason: ReasonDatabase, Detail: "the source's state could not be read"}
	}
	src, stale := state(s.Source.Name(), st, now, rec.Values.WeatherStaleS)
	ps, err := s.Store.Newest(ctx, s.Source.Name(), box, at, MaxAnswer)
	if err != nil {
		return Answer{}, &UnavailableError{Reason: ReasonDatabase, Detail: "the products could not be read"}
	}
	if ps == nil {
		ps = []Product{}
	}
	for i := range ps {
		ps[i].AgeS = math.Max(0, now.Sub(ps[i].ObservedAt).Seconds())
		ps[i].InForce = !at.Before(ps[i].ValidFrom) && !at.After(ps[i].ValidTo)
		ps[i].Stale = stale
	}
	slices.SortStableFunc(ps, func(a, b Product) int { return b.ObservedAt.Compare(a.ObservedAt) })
	if stale {
		s.count("answer_stale")
	}
	return Answer{At: at, Stale: stale, Source: src, PolicyVersion: rec.Version, Products: ps}, nil
}

// Check is what a decision consulted (Art. 10(3)): the products in force
// over its window, whether none could be, whether they are stale, and
// the policy's wind advisory. It never judges: nothing here refuses.
type Check struct {
	// ProductIDs are the products consulted, newest first (at most
	// MaxChecked).
	ProductIDs []string
	// Unavailable says why none was consulted ("" when some were).
	Unavailable string
	// Stale says why those consulted may not be the newest ("" when
	// fresh).
	Stale string
	// Advisories name each product whose wind or gust reaches the
	// policy's weather_advisory_wind_ms.
	Advisories []string
}

// Ref is weather_checked_ref: the product ids joined by commas; nil
// when none was consulted.
func (c Check) Ref() *string {
	if len(c.ProductIDs) == 0 {
		return nil
	}
	return ptr(strings.Join(c.ProductIDs, ","))
}

// Check consults the products whose area meets one of boxes and whose
// validity overlaps [from, to].
func (s *Service) Check(ctx context.Context, boxes []geodesy.BBox, from, to time.Time) Check {
	rec := s.policy()
	pol := rec.Values
	if u := s.unavailable(pol); u != nil {
		return Check{Unavailable: u.Detail}
	}
	now, err := s.Store.Now(ctx)
	if err != nil {
		return Check{Unavailable: "the database clock could not be read"}
	}
	ps, err := s.Store.InForce(ctx, s.Source.Name(), boxes, from, to, MaxChecked)
	if err != nil {
		s.count("check_failed")
		return Check{Unavailable: "the weather products could not be read"}
	}
	if len(ps) == 0 {
		return Check{Unavailable: "no weather product of the source is in force over the volumes and their window"}
	}
	var c Check
	for i := range ps {
		c.ProductIDs = append(c.ProductIDs, ps[i].ID)
		if a := advisory(&ps[i], pol.WeatherAdvisoryWindMS); a != "" {
			c.Advisories = append(c.Advisories, a)
		}
	}
	st, err := s.Store.Status(ctx, s.Source.Name())
	if err != nil {
		c.Stale = "the source's state could not be read"
	} else if src, stale := state(s.Source.Name(), st, now, pol.WeatherStaleS); stale {
		c.Stale = staleDetail(src)
	}
	return c
}

func staleDetail(src SourceState) string {
	switch {
	case src.State == StateFailing:
		return fmt.Sprintf("the weather source has failed since %s: %s", src.LastFailureAt.Format(time.RFC3339), *src.Failure)
	case src.AgeS != nil:
		return fmt.Sprintf("the weather source last delivered %.0f s ago", *src.AgeS)
	}
	return "the weather source has never delivered"
}

// advisory names p when its wind or gust, in the report or a change
// group, reaches threshold (0: off).
func advisory(p *Product, thresholdMS float64) string {
	if thresholdMS <= 0 {
		return ""
	}
	peak := 0.0
	fs := append([]Fields{p.Fields}, fieldsOf(p.Changes)...)
	for i := range fs {
		for _, v := range []*float64{fs[i].WindSpeedMS, fs[i].GustMS} {
			if v != nil {
				peak = math.Max(peak, *v)
			}
		}
	}
	if peak < thresholdMS {
		return ""
	}
	return fmt.Sprintf("%s %s %s: wind or gust %.1f m/s at or above the policy's %.1f m/s", strings.ToUpper(string(p.Kind)), p.Station, p.ID, peak, thresholdMS)
}

func fieldsOf(cs []Change) []Fields {
	out := make([]Fields, len(cs))
	for i := range cs {
		out[i] = cs[i].Fields
	}
	return out
}

// Probe is the readiness of weather on /readyz. Weather is optional
// (Art. 3(3)): not configured is up with the reason as its detail, as
// every optional service left unset is; configured, it is up when the
// source delivered within weather_stale_s, degraded when stale, failing
// (with the failure and its time) or without a station, down when its
// state cannot be read. Never a required dependency.
func (s *Service) Probe() obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		pol := s.policy().Values
		if u := s.unavailable(pol); u != nil {
			switch u.Reason {
			case ReasonNotConfigured:
				return obs.StateUp, u.Reason + ": " + u.Detail + "; GET /v1/weather answers 503 and every decision says weather_unavailable"
			case ReasonNoStations:
				return obs.StateDegraded, u.Reason + ": " + u.Detail
			}
			return obs.StateDown, u.Reason + ": " + u.Detail
		}
		now, err := s.Store.Now(ctx)
		if err != nil {
			return obs.StateDown, "the database clock could not be read"
		}
		st, err := s.Store.Status(ctx, s.Source.Name())
		if err != nil {
			return obs.StateDown, "the source's state could not be read"
		}
		src, stale := state(s.Source.Name(), st, now, pol.WeatherStaleS)
		if stale {
			return obs.StateDegraded, staleDetail(src)
		}
		return obs.StateUp, ""
	}
}
