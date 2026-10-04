package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/regnum"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Schema is the name of a flight record (the body of GET
// /v1/records/flights/{id}, FlightRecord in api/openapi.yaml).
const Schema = "record/flight/v1"

// Section states.
const (
	StateIncluded    = "included"
	StateUnavailable = "unavailable"
)

// Bounds of a record's sections (E-10): a section that holds fewer
// items than there were says truncated and how many there were.
const (
	MaxVersions    = 200
	MaxAlerts      = 1000
	MaxConformance = 2000
	MaxNotices     = 200
	MaxHoles       = 500
	MaxIngestGaps  = 500
	MaxWriterGaps  = 100
	MaxProducts    = 360
)

// CauseNone labels a hole nothing explains (B-13).
const CauseNone = "no recorded cause"

// ErrNotFound is a flight this USSP holds no record of.
var ErrNotFound = errors.New("no such flight")

// Section is how a section reads: included, or unavailable with why.
type Section struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

func included() Section { return Section{State: StateIncluded} }

func unavailable(what string, err error) Section {
	return Section{State: StateUnavailable, Reason: what + " cannot be read: " + clip(err.Error())}
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// Record is one flight's service record.
type Record struct {
	Schema      string    `json:"schema"`
	FlightID    string    `json:"flight_id"`
	USSPID      string    `json:"ussp_id"`
	GeneratedAt time.Time `json:"generated_at"`

	Flight          FlightPart       `json:"flight"`
	Intent          IntentPart       `json:"intent"`
	Telemetry       TelemetryPart    `json:"telemetry"`
	Alerts          AlertsPart       `json:"alerts"`
	Conformance     ConformancePart  `json:"conformance"`
	Coordination    CoordinationPart `json:"coordination"`
	TrafficProducts ProductsPart     `json:"traffic_products"`
	PolicyVersions  PoliciesPart     `json:"policy_versions"`
}

// FlightPart is the flight as the flights table records it, the
// operator by the public part of its registration number.
type FlightPart struct {
	StartedAt           time.Time  `json:"started_at"`
	EndedAt             *time.Time `json:"ended_at"`
	EndReason           *string    `json:"end_reason"`
	UASSerial           string     `json:"uas_serial"`
	OperatorRegPublic   *string    `json:"operator_reg_public"`
	AuthorisationNumber *string    `json:"authorisation_number"`
	IntentID            *string    `json:"intent_id"`
	RIDFlightID         *string    `json:"rid_flight_id"`
	Emergency           bool       `json:"emergency"`
	LastState           *string    `json:"last_state"`
}

// IntentPart is the authorisation: the intent as it stands and every
// version's decision. Absent (state included, Intent nil) for a flight
// without an intent.
type IntentPart struct {
	Section
	Intent   *IntentView `json:"intent"`
	Versions []Version   `json:"versions"`
	// VersionsTruncated is set when the intent has more versions than
	// MaxVersions.
	VersionsTruncated bool `json:"versions_truncated,omitempty"`
}

// IntentView is the intent row a record holds: the Annex IV items that
// are not free text, the decision and the inputs it was taken on.
type IntentView struct {
	IntentID                 string          `json:"intent_id"`
	Version                  int             `json:"version"`
	LocalState               string          `json:"local_state"`
	DSSState                 *string         `json:"dss_state"`
	Decision                 *string         `json:"decision"`
	AuthorisationNumber      *string         `json:"authorisation_number"`
	Priority                 int             `json:"priority"`
	TimeStart                time.Time       `json:"time_start"`
	TimeEnd                  time.Time       `json:"time_end"`
	Volumes                  json.RawMessage `json:"volumes"`
	DeviationThresholds      json.RawMessage `json:"deviation_thresholds"`
	Conflicts                json.RawMessage `json:"conflicts"`
	Conditions               []string        `json:"conditions"`
	CISVersionChecked        *string         `json:"cis_version_checked"`
	RegistryCheckedAt        *time.Time      `json:"registry_checked_at"`
	PolicyVersion            *int64          `json:"policy_version"`
	InUSpaceAirspace         *bool           `json:"in_uspace_airspace"`
	USpaceAirspaceIDs        []string        `json:"uspace_airspace_ids"`
	ExemptArt13              bool            `json:"exempt_art_1_3"`
	Mode                     *string         `json:"mode"`
	FlightType               *string         `json:"flight_type"`
	Category                 *string         `json:"category"`
	ClassLabel               *string         `json:"class_label"`
	IdentificationTechnology *string         `json:"identification_technology"`
	ConnectivityMethods      []string        `json:"connectivity_methods"`
	EnduranceS               *int            `json:"endurance_s"`
	UASSerial                string          `json:"uas_serial"`
	OperatorRegPublic        *string         `json:"operator_reg_public"`
	UARegistration           *string         `json:"ua_registration"`
	CreatedAt                time.Time       `json:"created_at"`
	UpdatedAt                time.Time       `json:"updated_at"`
}

// Version is one version of an intent: when, by whom (a client id or
// "system"), why, and the decision of that version as it was taken.
type Version struct {
	Version      int             `json:"version"`
	At           time.Time       `json:"at"`
	Actor        string          `json:"actor"`
	ChangeReason string          `json:"change_reason"`
	Decision     json.RawMessage `json:"decision"`
}

// TelemetryPart is the telemetry summary with its holes.
type TelemetryPart struct {
	Section
	Samples     int64       `json:"samples"`
	FirstAt     *time.Time  `json:"first_at"`
	LastAt      *time.Time  `json:"last_at"`
	BBox        *[4]float64 `json:"bbox"`
	MaxAltAMSLM *float64    `json:"max_alt_amsl_m"`
	// GapS is the silence that makes a hole (policy record_gap_s).
	GapS           float64 `json:"gap_s"`
	PolicyVersion  int64   `json:"policy_version"`
	Holes          []Hole  `json:"holes"`
	HolesTruncated bool    `json:"holes_truncated,omitempty"`
	// Causes says whether the causes could be read: unavailable when the
	// gap records cannot be, and then every hole says so.
	Causes Section `json:"causes"`
}

// Hole is a stretch of a flight's track with no sample: a silence
// between After and Before, or a gap telemetry-ingest declared.
type Hole struct {
	After  time.Time `json:"after"`
	Before time.Time `json:"before"`
	GapS   float64   `json:"gap_s"`
	Cause  string    `json:"cause"`
	Detail string    `json:"detail,omitempty"`
}

// AlertsPart is every alert of the flight with its lifecycle.
type AlertsPart struct {
	Section
	Items     []Alert `json:"items,omitzero"`
	Truncated bool    `json:"truncated,omitempty"`
}

// Alert is one alert as recorded: raised, last updated, cleared with its
// reason, acknowledged, escalated.
type Alert struct {
	AlertID       string          `json:"alert_id"`
	Kind          string          `json:"kind"`
	Severity      string          `json:"severity"`
	State         string          `json:"state"`
	RaisedAt      time.Time       `json:"raised_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	ClearedAt     *time.Time      `json:"cleared_at"`
	ClearReason   *string         `json:"clear_reason"`
	CapturedAt    *time.Time      `json:"captured_at"`
	AckedAt       *time.Time      `json:"acked_at"`
	EscalatedAt   *time.Time      `json:"escalated_at"`
	PeerRef       *string         `json:"peer_ref"`
	PolicyVersion int64           `json:"policy_version"`
	Detail        json.RawMessage `json:"detail"`
}

// ConformancePart is the conformance timeline.
type ConformancePart struct {
	Section
	Items     []ConformanceState `json:"items,omitzero"`
	Truncated bool               `json:"truncated,omitempty"`
}

// ConformanceState is one state of the timeline, with when the ANSP was
// told and its acknowledgement.
type ConformanceState struct {
	At               time.Time  `json:"at"`
	State            string     `json:"state"`
	Reason           *string    `json:"reason"`
	DistanceOutsideM *float64   `json:"distance_outside_m"`
	HeightOverM      *float64   `json:"height_over_m"`
	TimeOutsideS     *float64   `json:"time_outside_s"`
	PolicyVersion    int64      `json:"policy_version"`
	ATSNotifiedAt    *time.Time `json:"ats_notified_at"`
	ATSAckRef        *string    `json:"ats_ack_ref"`
}

// CoordinationPart is every Annex V notice about the flight or its intent.
type CoordinationPart struct {
	Section
	Items     []Notice `json:"items,omitzero"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Notice is one notice to the ANSP; AcknowledgedBy is a role.
type Notice struct {
	NoticeRef      string     `json:"notice_ref"`
	Kind           string     `json:"kind"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"created_at"`
	ReceivedAt     *time.Time `json:"received_at"`
	AckID          *string    `json:"ack_id"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
	AcknowledgedBy *string    `json:"acknowledged_by"`
	EscalatedAt    *time.Time `json:"escalated_at"`
	FailedAt       *time.Time `json:"failed_at"`
}

// ProductsPart is what the flight's operator client was shown while it
// flew (the 0.1 Hz sample of traffic-ws), with the degraded inputs.
type ProductsPart struct {
	Section
	Items     []Product `json:"items,omitzero"`
	Total     int64     `json:"total"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Product is one sampled traffic product.
type Product struct {
	At            time.Time       `json:"at"`
	IntentID      *string         `json:"intent_id"`
	TracksShown   json.RawMessage `json:"tracks_shown"`
	Degraded      []string        `json:"degraded"`
	PolicyVersion int64           `json:"policy_version"`
}

// PoliciesPart holds every policy version the record names, with its
// values, so a number reads with the threshold it was judged against.
type PoliciesPart struct {
	Section
	Items []PolicyVersion `json:"items,omitzero"`
	// Missing are versions named that the policy table does not hold.
	Missing []int64 `json:"missing,omitempty"`
}

// PolicyVersion is one stored policy version.
type PolicyVersion struct {
	PolicyVersion int64           `json:"policy_version"`
	CreatedAt     time.Time       `json:"created_at"`
	Values        json.RawMessage `json:"values"`
}

// Source rows: what the stores hand the Builder.

// FlightRow is a flights row.
type FlightRow struct {
	ID                  string
	IntentID            *string
	AuthorisationNumber *string
	UASSerial           string
	OperatorReg         *string
	ClientID            *string
	StartedAt           time.Time
	EndedAt             *time.Time
	EndReason           *string
	RIDFlightID         *string
	Emergency           bool
	LastState           *string
}

// IntentRow is an operational_intents row as a record reads it (the
// operator registration number in full: the Builder keeps its public
// part).
type IntentRow struct {
	IntentView
	OperatorReg *string
}

// IngestGap is a gap record of telemetry-ingest.
type IngestGap struct {
	Cause      string
	Started    time.Time
	Ended      time.Time
	Dropped    int
	RecordedAt time.Time
}

// WriterGap is a hole tsdb-writer recorded in the TRK stream.
type WriterGap struct {
	Cause     string
	Count     int64
	CountUnit string
	AfterAt   *time.Time
	BeforeAt  *time.Time
	Detail    string
	At        time.Time
}

// Summary is a flight's telemetry summary (Samples 0: none).
type Summary struct {
	Samples     int64
	FirstAt     *time.Time
	LastAt      *time.Time
	BBox        *[4]float64
	MaxAltAMSLM *float64
}

// Silence is a stretch between two samples longer than the gap.
type Silence struct{ After, Before time.Time }

// Reader is the relational side of a record.
type Reader interface {
	Flight(ctx context.Context, id string) (FlightRow, error)
	Intent(ctx context.Context, id string) (IntentRow, error)
	Versions(ctx context.Context, intentID string, n int) ([]Version, error)
	Alerts(ctx context.Context, flightID string, n int) ([]Alert, error)
	Conformance(ctx context.Context, flightID string, n int) ([]ConformanceState, error)
	Notices(ctx context.Context, flightID, intentID string, n int) ([]Notice, error)
	IngestGaps(ctx context.Context, clientID string, from, to time.Time, n int) ([]IngestGap, error)
	Policies(ctx context.Context, versions []int64) ([]PolicyVersion, error)
	Now(ctx context.Context) (time.Time, error)
}

// Series is the time-series side of a record.
type Series interface {
	Summary(ctx context.Context, flightID string) (Summary, error)
	Silences(ctx context.Context, flightID string, gapS float64, n int) ([]Silence, error)
	WriterGaps(ctx context.Context, from, to time.Time, n int) ([]WriterGap, error)
	Products(ctx context.Context, clientID string, from, to time.Time, n int) ([]Product, int64, error)
}

// Builder builds records.
type Builder struct {
	Reader Reader
	// Series is nil when TimescaleDB is not configured: the telemetry and
	// the traffic products are then unavailable, and say so.
	Series Series
	Policy func() policy.Record
	// USSPID is this USSP's code (USSP_SYSTEM_ID).
	USSPID string
}

func (b *Builder) policy() policy.Record {
	if b.Policy == nil {
		return policy.Record{Values: policy.Defaults()}
	}
	return b.Policy()
}

// errNoSeries is the reason without TimescaleDB.
var errNoSeries = errors.New("TimescaleDB is not configured on this process (USSP_TS_URL)")

// publicReg is the public part of a registration number (G-04: the
// secret part never leaves the USSP).
func publicReg(v *string) *string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	p, _ := regnum.Public(strings.TrimSpace(*v))
	return &p
}

// Flight builds the record of flightID. ErrNotFound when the flights
// table does not hold it; any other error is the flights table itself
// failing (the caller answers 503). Every other store's failure is its
// section's, never the record's.
func (b *Builder) Flight(ctx context.Context, flightID string) (Record, error) {
	f, err := b.Reader.Flight(ctx, flightID)
	if err != nil {
		return Record{}, err
	}
	now, err := b.Reader.Now(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("database clock: %w", err)
	}
	r := Record{Schema: Schema, FlightID: f.ID, USSPID: b.USSPID, GeneratedAt: now.UTC(),
		Flight: FlightPart{StartedAt: f.StartedAt.UTC(), EndedAt: f.EndedAt, EndReason: f.EndReason, UASSerial: f.UASSerial,
			OperatorRegPublic: publicReg(f.OperatorReg), AuthorisationNumber: f.AuthorisationNumber, IntentID: f.IntentID,
			RIDFlightID: f.RIDFlightID, Emergency: f.Emergency, LastState: f.LastState}}
	end := now
	if f.EndedAt != nil {
		end = *f.EndedAt
	}
	versions := map[int64]bool{}
	r.Intent = b.intent(ctx, f, versions)
	r.Alerts = b.alerts(ctx, f.ID, versions)
	r.Conformance = b.conformance(ctx, f.ID, versions)
	r.Coordination = b.coordination(ctx, f)
	r.Telemetry = b.telemetry(ctx, f, end, versions)
	r.TrafficProducts = b.products(ctx, f, end, versions)
	r.PolicyVersions = b.policies(ctx, versions)
	return r, nil
}

func (b *Builder) intent(ctx context.Context, f FlightRow, versions map[int64]bool) IntentPart {
	out := IntentPart{Section: included(), Versions: []Version{}}
	if f.IntentID == nil {
		return out
	}
	in, err := b.Reader.Intent(ctx, *f.IntentID)
	if err != nil {
		return IntentPart{Section: unavailable("the intent", err), Versions: []Version{}}
	}
	v := in.IntentView
	v.OperatorRegPublic = publicReg(in.OperatorReg)
	if v.PolicyVersion != nil {
		versions[*v.PolicyVersion] = true
	}
	out.Intent = &v
	vs, err := b.Reader.Versions(ctx, *f.IntentID, MaxVersions+1)
	if err != nil {
		out.Section = unavailable("the intent's versions", err)
		return out
	}
	if len(vs) > MaxVersions {
		vs, out.VersionsTruncated = vs[:MaxVersions], true
	}
	for i := range vs {
		var d struct {
			PolicyVersion *int64 `json:"policy_version"`
		}
		if json.Unmarshal(vs[i].Decision, &d) == nil && d.PolicyVersion != nil {
			versions[*d.PolicyVersion] = true
		}
	}
	out.Versions = vs
	return out
}

func (b *Builder) alerts(ctx context.Context, flightID string, versions map[int64]bool) AlertsPart {
	as, err := b.Reader.Alerts(ctx, flightID, MaxAlerts+1)
	if err != nil {
		return AlertsPart{Section: unavailable("the alerts", err)}
	}
	out := AlertsPart{Section: included(), Items: as}
	if len(as) > MaxAlerts {
		out.Items, out.Truncated = as[:MaxAlerts], true
	}
	if out.Items == nil {
		out.Items = []Alert{}
	}
	for i := range out.Items {
		versions[out.Items[i].PolicyVersion] = true
	}
	return out
}

func (b *Builder) conformance(ctx context.Context, flightID string, versions map[int64]bool) ConformancePart {
	cs, err := b.Reader.Conformance(ctx, flightID, MaxConformance+1)
	if err != nil {
		return ConformancePart{Section: unavailable("the conformance timeline", err)}
	}
	out := ConformancePart{Section: included(), Items: cs}
	if len(cs) > MaxConformance {
		out.Items, out.Truncated = cs[:MaxConformance], true
	}
	if out.Items == nil {
		out.Items = []ConformanceState{}
	}
	for i := range out.Items {
		versions[out.Items[i].PolicyVersion] = true
	}
	return out
}

func (b *Builder) coordination(ctx context.Context, f FlightRow) CoordinationPart {
	intentID := ""
	if f.IntentID != nil {
		intentID = *f.IntentID
	}
	ns, err := b.Reader.Notices(ctx, f.ID, intentID, MaxNotices+1)
	if err != nil {
		return CoordinationPart{Section: unavailable("the coordination notices", err)}
	}
	out := CoordinationPart{Section: included(), Items: ns}
	if len(ns) > MaxNotices {
		out.Items, out.Truncated = ns[:MaxNotices], true
	}
	if out.Items == nil {
		out.Items = []Notice{}
	}
	return out
}

// telemetry is the summary and the holes: every silence longer than
// record_gap_s and every declared gap of the flight's client in its
// window, each with the recorded cause that overlaps it, or CauseNone.
func (b *Builder) telemetry(ctx context.Context, f FlightRow, end time.Time, versions map[int64]bool) TelemetryPart {
	pol := b.policy()
	out := TelemetryPart{Section: included(), GapS: pol.Values.RecordGapS, PolicyVersion: pol.Version, Holes: []Hole{}, Causes: included()}
	if b.Series == nil {
		return TelemetryPart{Section: unavailable("the telemetry", errNoSeries), GapS: out.GapS, PolicyVersion: out.PolicyVersion, Holes: []Hole{}, Causes: out.Causes}
	}
	versions[pol.Version] = true
	sum, err := b.Series.Summary(ctx, f.ID)
	if err != nil {
		out.Section = unavailable("the telemetry", err)
		return out
	}
	out.Samples, out.FirstAt, out.LastAt, out.BBox, out.MaxAltAMSLM = sum.Samples, sum.FirstAt, sum.LastAt, sum.BBox, sum.MaxAltAMSLM
	sil, err := b.Series.Silences(ctx, f.ID, pol.Values.RecordGapS, MaxHoles+1)
	if err != nil {
		out.Section = unavailable("the telemetry's holes", err)
		return out
	}
	var ig []IngestGap
	var causesErr error
	if f.ClientID != nil {
		ig, causesErr = b.Reader.IngestGaps(ctx, *f.ClientID, f.StartedAt, end, MaxIngestGaps)
	}
	wg, werr := b.Series.WriterGaps(ctx, f.StartedAt, end, MaxWriterGaps)
	if err := errors.Join(causesErr, werr); err != nil {
		out.Causes = unavailable("the gap records", err)
	}
	holes := make([]Hole, 0, len(sil)+len(ig))
	for _, s := range sil {
		holes = append(holes, Hole{After: s.After.UTC(), Before: s.Before.UTC(), GapS: s.Before.Sub(s.After).Seconds()})
	}
	// A declared gap is a hole however short (B-13); one inside a silence
	// already listed is that silence's cause, not a second hole.
	for _, g := range ig {
		if !slices.ContainsFunc(holes, func(h Hole) bool { return overlaps(h.After, h.Before, g.Started, g.Ended) }) {
			holes = append(holes, Hole{After: g.Started.UTC(), Before: g.Ended.UTC(), GapS: g.Ended.Sub(g.Started).Seconds()})
		}
	}
	slices.SortFunc(holes, func(a, b Hole) int { return a.After.Compare(b.After) })
	for i := range holes {
		holes[i].Cause, holes[i].Detail = causeOf(holes[i], ig, wg, out.Causes)
	}
	if len(holes) > MaxHoles {
		holes, out.HolesTruncated = holes[:MaxHoles], true
	}
	out.Holes = holes
	return out
}

func overlaps(a0, a1, b0, b1 time.Time) bool { return !a1.Before(b0) && !b1.Before(a0) }

// causeOf names what was recorded for a hole: the declared gaps of
// telemetry-ingest that overlap it first, else a tsdb-writer gap of the
// TRK stream around it, else CauseNone (or, when the gap records could
// not be read, that they could not).
func causeOf(h Hole, ig []IngestGap, wg []WriterGap, causes Section) (cause, detail string) {
	var parts []string
	for _, g := range ig {
		if overlaps(h.After, h.Before, g.Started, g.Ended) {
			parts = append(parts, fmt.Sprintf("telemetry-ingest dropped %d samples captured %s to %s (%s)",
				g.Dropped, g.Started.UTC().Format(time.RFC3339Nano), g.Ended.UTC().Format(time.RFC3339Nano), g.Cause))
			if cause == "" {
				cause = g.Cause
			}
		}
	}
	if cause != "" {
		return cause, strings.Join(parts, "; ")
	}
	for _, g := range wg {
		from, to := g.At, g.At
		if g.AfterAt != nil {
			from = *g.AfterAt
		}
		if g.BeforeAt != nil {
			to = *g.BeforeAt
		}
		if overlaps(h.After, h.Before, from, to) {
			return "tsdb_writer_" + g.Cause, fmt.Sprintf("tsdb-writer recorded %d %s missing from the TRK stream (%s); the gap is of the stream, not of this flight alone",
				g.Count, g.CountUnit, clip(g.Detail))
		}
	}
	if causes.State == StateUnavailable {
		return CauseNone, "the gap records could not be read: " + causes.Reason
	}
	return CauseNone, ""
}

func (b *Builder) products(ctx context.Context, f FlightRow, end time.Time, versions map[int64]bool) ProductsPart {
	if b.Series == nil {
		return ProductsPart{Section: unavailable("the traffic products", errNoSeries)}
	}
	if f.ClientID == nil {
		return ProductsPart{Section: included(), Items: []Product{}}
	}
	ps, total, err := b.Series.Products(ctx, *f.ClientID, f.StartedAt, end, MaxProducts)
	if err != nil {
		return ProductsPart{Section: unavailable("the traffic products", err)}
	}
	if ps == nil {
		ps = []Product{}
	}
	for i := range ps {
		versions[ps[i].PolicyVersion] = true
	}
	return ProductsPart{Section: included(), Items: ps, Total: total, Truncated: total > int64(len(ps))}
}

func (b *Builder) policies(ctx context.Context, versions map[int64]bool) PoliciesPart {
	want := make([]int64, 0, len(versions))
	for v := range versions {
		if v > 0 {
			want = append(want, v)
		}
	}
	slices.Sort(want)
	if len(want) == 0 {
		return PoliciesPart{Section: included(), Items: []PolicyVersion{}}
	}
	ps, err := b.Reader.Policies(ctx, want)
	if err != nil {
		return PoliciesPart{Section: unavailable("the policy versions", err), Missing: want}
	}
	out := PoliciesPart{Section: included(), Items: ps}
	if out.Items == nil {
		out.Items = []PolicyVersion{}
	}
	for _, v := range want {
		if !slices.ContainsFunc(ps, func(p PolicyVersion) bool { return p.PolicyVersion == v }) {
			out.Missing = append(out.Missing, v)
		}
	}
	return out
}
