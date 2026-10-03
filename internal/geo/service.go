package geo

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/cis"
)

// Bounds of GET /v1/geo (E-10).
const (
	// MaxBBoxSpanDeg bounds a query box's side: the cache answers a
	// region, never the whole country of every zone at once.
	MaxBBoxSpanDeg = 5.0
	// DefaultMaxItems bounds the features one answer lists; more is
	// said (truncated), never cut silently.
	DefaultMaxItems = 2000
	// MaxWindow bounds a window query.
	MaxWindow = 31 * 24 * time.Hour
)

// Counters of the service.
const (
	CounterQueries         = "geo_queries"
	CounterQueriesStale    = "geo_queries_stale"
	CounterQueryTruncated  = "geo_queries_truncated"
	CounterQueryNotAnswerd = "geo_queries_not_answered"
)

// Evaluator is what the service reads of the CIS cache (cis.Evaluator):
// no CISP call is in the request path.
type Evaluator interface {
	ZonesFor(envelope geodesy.BBox, from, to time.Time) cis.ZonesResult
	ApplicabilityAt(en *cis.Entry, part int, at time.Time) (cis.Applicability, error)
	Snapshot() *cis.Snapshot
}

// Limit is one vertical limit as published, in metres with its
// reference (never converted: AGL and AMSL never meet, D-01).
type Limit struct {
	ValueM float64 `json:"value_m"`
	Ref    string  `json:"ref"`
}

// Part is one part of a feature (a layer of a GeometryCollection) with
// its limits.
type Part struct {
	ID    string `json:"id"`
	Lower *Limit `json:"lower"`
	Upper *Limit `json:"upper"`
}

// Applicability is how a feature applies at the instant or over the
// window asked: kind always, during or scheduled (cis.WindowKind) and
// from/to; at an instant also whether it applies then (applies,
// unknown with why; a feature that does not apply is not listed).
type Applicability struct {
	Kind    string     `json:"kind"`
	From    *time.Time `json:"from"`
	To      *time.Time `json:"to"`
	AtState string     `json:"at_state,omitempty"`
	Why     string     `json:"why,omitempty"`
}

// Item is one feature of an answer: its identity and the versions it
// rests on (updated_at is its dataSource.updateDateTime, else the
// dataset version's cis_updated_at; version the dataset version), its
// validity bounds, how it applies, its parts with their limits and the
// ED-318 feature verbatim.
type Item struct {
	Identifier    string          `json:"identifier"`
	Type          string          `json:"type"`
	Dataset       string          `json:"dataset"`
	Version       string          `json:"version"`
	UpdatedAt     *time.Time      `json:"updated_at"`
	ValidFrom     *time.Time      `json:"valid_from"`
	ValidTo       *time.Time      `json:"valid_to"`
	Applicability Applicability   `json:"applicability"`
	Parts         []Part          `json:"parts"`
	Feature       json.RawMessage `json:"feature"`
}

// Airspace is a U-space airspace of an answer with its Art. 3(4)
// requirements as published (cis/uspace_requirements/v1), the services
// it requires and its adjacent airspaces, or why they cannot be read.
type Airspace struct {
	Item
	Requirements        json.RawMessage `json:"requirements"`
	RequirementsProblem *string         `json:"requirements_problem"`
	ServicesRequired    []string        `json:"services_required"`
	Adjacent            []string        `json:"adjacent"`
}

// Restriction is an ANSP restriction of an answer with its state and
// window (the CISP's cis_restriction; null members when it carries
// none).
type Restriction struct {
	Item
	RestrictionID *string    `json:"restriction_id"`
	State         *string    `json:"state"`
	StartsAt      *time.Time `json:"starts_at"`
	EndsAt        *time.Time `json:"ends_at"`
	ANSPRef       *string    `json:"ansp_ref"`
}

// Answer is GET /v1/geo's body (and GET /v1/geo/intents/{id}'s): what
// applies in the box at the instant or over the window, with the CIS
// basis (cis_version, cis_age_s, stale beyond the policy's bound) and
// whether the list was cut at MaxItems.
type Answer struct {
	CISVersion      string        `json:"cis_version"`
	CISAgeS         float64       `json:"cis_age_s"`
	Stale           bool          `json:"stale"`
	At              *time.Time    `json:"at"`
	From            *time.Time    `json:"from"`
	To              *time.Time    `json:"to"`
	IntentID        *string       `json:"intent_id,omitempty"`
	USpaceAirspaces []Airspace    `json:"uspace_airspaces"`
	Zones           []Item        `json:"zones"`
	Restrictions    []Restriction `json:"restrictions"`
	Truncated       bool          `json:"truncated"`
}

// Query is one question: boxes (one per intent volume, or the bbox) and
// for each its window, or one instant.
type Query struct {
	Boxes []Window
	// At is the instant of a bbox query; zero for a window query.
	At time.Time
}

// Window is one box and the time span it is asked for.
type Window struct {
	Box      geodesy.BBox
	From, To time.Time
}

// Service answers geo-awareness from the CIS cache (brief WP-12).
type Service struct {
	CIS      Evaluator
	Counters *core.Counters
	Now      func() time.Time
	// MaxItems bounds an answer (DefaultMaxItems).
	MaxItems int
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

// ParseBBox reads west,south,east,north in WGS84 degrees, at most
// MaxBBoxSpanDeg a side; an antimeridian box is refused (ED-318 refuses
// shapes across it).
func ParseBBox(raw string) (geodesy.BBox, error) {
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return geodesy.BBox{}, core.Fieldf("bbox", "four numbers west,south,east,north")
	}
	var v [4]float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || !core.IsFinite(f) {
			return geodesy.BBox{}, core.Fieldf("bbox", "not a number: %q", clip(p))
		}
		v[i] = f
	}
	b := geodesy.BBox{MinLon: v[0], MinLat: v[1], MaxLon: v[2], MaxLat: v[3]}
	switch {
	case b.MinLat < -90 || b.MaxLat > 90 || b.MinLon < -180 || b.MaxLon > 180:
		return geodesy.BBox{}, core.Fieldf("bbox", "outside WGS84 bounds")
	case b.MinLat > b.MaxLat || b.MinLon > b.MaxLon:
		return geodesy.BBox{}, core.Fieldf("bbox", "west must not exceed east, south must not exceed north")
	case b.MaxLat-b.MinLat > MaxBBoxSpanDeg || b.MaxLon-b.MinLon > MaxBBoxSpanDeg:
		return geodesy.BBox{}, core.Fieldf("bbox", "a side is longer than %.0f degrees", MaxBBoxSpanDeg)
	}
	return b, nil
}

func clip(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}

// Box answers a bbox at an instant (at zero: now).
func (s *Service) Box(box geodesy.BBox, at time.Time) (Answer, error) {
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	a, err := s.answer(Query{Boxes: []Window{{Box: box, From: at, To: at}}, At: at})
	a.At = &at
	return a, err
}

// Windows answers each box over its window (an intent's volumes).
func (s *Service) Windows(ws []Window) (Answer, error) {
	if len(ws) == 0 {
		return Answer{}, core.Fieldf("volumes", "none")
	}
	a, err := s.answer(Query{Boxes: ws})
	from, to := ws[0].From.UTC(), ws[0].To.UTC()
	for _, w := range ws[1:] {
		if w.From.Before(from) {
			from = w.From.UTC()
		}
		if w.To.After(to) {
			to = w.To.UTC()
		}
	}
	a.From, a.To = &from, &to
	return a, err
}

// NotAnsweredError is a query the cache could not answer (an invalid
// envelope or window): never an empty sky.
type NotAnsweredError struct{ Detail string }

func (e *NotAnsweredError) Error() string { return "the CIS cache did not answer: " + e.Detail }

// HTTPStatus is 400: the query itself is not one the cache answers.
func (e *NotAnsweredError) HTTPStatus() int { return 400 }

// ProblemSlug implements httpx.StatusError.
func (e *NotAnsweredError) ProblemSlug() string { return "geo_query_invalid" }

// ProblemDetail implements httpx.StatusError.
func (e *NotAnsweredError) ProblemDetail() string { return e.Detail }

func (s *Service) answer(q Query) (Answer, error) {
	s.count(CounterQueries)
	out := Answer{USpaceAirspaces: []Airspace{}, Zones: []Item{}, Restrictions: []Restriction{}}
	seen := map[string]bool{}
	n := 0
	maxItems := s.MaxItems
	if maxItems <= 0 {
		maxItems = DefaultMaxItems
	}
	for _, w := range q.Boxes {
		if w.To.Sub(w.From) > MaxWindow {
			s.count(CounterQueryNotAnswerd)
			return out, &NotAnsweredError{Detail: "the window is longer than " + MaxWindow.String()}
		}
		r := s.CIS.ZonesFor(w.Box, w.From, w.To)
		out.CISVersion, out.CISAgeS, out.Stale = r.CISVersion, r.CISAgeS, r.Stale
		if r.Error != "" {
			s.count(CounterQueryNotAnswerd)
			return out, &NotAnsweredError{Detail: r.Error}
		}
		for _, c := range r.Zones {
			e := c.Entry
			key := string(e.Dataset) + "/" + e.Identifier
			if seen[key] {
				continue
			}
			appl := Applicability{Kind: string(c.Kind), From: c.From, To: c.To}
			if !q.At.IsZero() {
				st, err := s.CIS.ApplicabilityAt(e, c.Part, q.At)
				if st == cis.NotApplicable {
					continue
				}
				appl.AtState = string(st)
				if err != nil {
					appl.Why = err.Error()
				}
			}
			seen[key] = true
			if n >= maxItems {
				out.Truncated = true
				continue
			}
			n++
			it := s.item(e, appl)
			switch e.Dataset {
			case cis.USpaceAirspace:
				out.USpaceAirspaces = append(out.USpaceAirspaces, airspaceOf(it, e))
			case cis.Restrictions:
				out.Restrictions = append(out.Restrictions, restrictionOf(it, e))
			case cis.Zones, cis.USSPList:
				out.Zones = append(out.Zones, it)
			}
		}
	}
	if out.Stale {
		s.count(CounterQueriesStale)
	}
	if out.Truncated {
		s.count(CounterQueryTruncated)
	}
	byID := strings.Compare
	slices.SortFunc(out.Zones, func(a, b Item) int { return byID(a.Identifier, b.Identifier) })
	slices.SortFunc(out.USpaceAirspaces, func(a, b Airspace) int { return byID(a.Identifier, b.Identifier) })
	slices.SortFunc(out.Restrictions, func(a, b Restriction) int { return byID(a.Identifier, b.Identifier) })
	return out, nil
}

func (s *Service) item(e *cis.Entry, appl Applicability) Item {
	it := Item{
		Identifier: e.Identifier, Type: string(e.Type), Dataset: string(e.Dataset), Version: string(e.Dataset) + ":" + strconv.FormatInt(e.Version, 10),
		ValidFrom: e.ValidFrom, ValidTo: e.ValidTo, Applicability: appl, Feature: e.Raw, Parts: []Part{},
	}
	if f := e.Feature; f != nil && f.Properties.DataSource != nil && f.Properties.DataSource.UpdateDateTime != nil {
		t := f.Properties.DataSource.UpdateDateTime.Time.UTC()
		it.UpdatedAt = &t
	} else if v := s.CIS.Snapshot().Version(e.Dataset); v != nil && v.Meta.CISUpdatedAt != nil {
		t := v.Meta.CISUpdatedAt.UTC()
		it.UpdatedAt = &t
	}
	for _, p := range e.Parts {
		part := Part{ID: p.Identifier}
		if p.Lower != nil {
			part.Lower = &Limit{ValueM: round(p.Lower.ValueM), Ref: string(p.Lower.Ref)}
		}
		if p.Upper != nil {
			part.Upper = &Limit{ValueM: round(p.Upper.ValueM), Ref: string(p.Upper.Ref)}
		}
		it.Parts = append(it.Parts, part)
	}
	return it
}

// round keeps a limit at millimetres (feet converted to metres carry
// float noise).
func round(v float64) float64 { return math.Round(v*1000) / 1000 }

func airspaceOf(it Item, e *cis.Entry) Airspace {
	a := Airspace{Item: it, ServicesRequired: []string{}, Adjacent: []string{}}
	if r := e.Requirements; r != nil {
		a.Requirements = r.Raw
		for _, sv := range r.ServicesRequired {
			a.ServicesRequired = append(a.ServicesRequired, string(sv))
		}
		a.Adjacent = append(a.Adjacent, r.Adjacent...)
	} else if e.RequirementsProblem != "" {
		p := e.RequirementsProblem
		a.RequirementsProblem = &p
	}
	if a.Requirements == nil {
		a.Requirements = json.RawMessage("null")
	}
	return a
}

func restrictionOf(it Item, e *cis.Entry) Restriction {
	r := Restriction{Item: it}
	if c := e.Restriction; c != nil {
		id, st, ref := c.Id, string(c.State), c.AnspRef
		s, en := c.StartsAt.UTC(), c.EndsAt.UTC()
		r.RestrictionID, r.State, r.ANSPRef, r.StartsAt, r.EndsAt = &id, &st, &ref, &s, &en
	}
	return r
}

// IntentWindows are the boxes and windows of an intent's volumes
// (intent.Service.Volumes for the caller's own intent).
type IntentWindows interface {
	Windows(ctx context.Context, clientID, intentID string) ([]geodesy.BBox, []time.Time, []time.Time, error)
}

// Intent answers GET /v1/geo/intents/{id}: the operator's own intent's
// volumes and windows as the query.
func (s *Service) Intent(ctx context.Context, src IntentWindows, clientID, intentID string) (Answer, error) {
	boxes, from, to, err := src.Windows(ctx, clientID, intentID)
	if err != nil {
		return Answer{}, err
	}
	ws := make([]Window, len(boxes))
	for i := range boxes {
		ws[i] = Window{Box: boxes[i], From: from[i], To: to[i]}
	}
	a, err := s.Windows(ws)
	a.IntentID = &intentID
	return a, err
}
