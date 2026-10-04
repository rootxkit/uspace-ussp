package occurrence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/regnum"
)

// Schema is the name of the report the authority owns (spec 04 §3.3).
const Schema = "occurrence/v1"

// Kinds of occurrence (spec 04 §3.3, 376/2014 Art. 4(1)).
const (
	KindAirprox                = "airprox"
	KindNonconformanceInProhib = "nonconformance_in_prohibited"
	KindLostLinkInUSpace       = "lost_link_in_uspace"
	KindEmergency              = "emergency"
	KindOther                  = "other"
)

// Channels: a report the regulation requires, and one a person chose to
// make.
const (
	ChannelMandatory = "mandatory"
	ChannelVoluntary = "voluntary"
)

// Deadline is how long after the USSP became aware a report is due
// (376/2014 Art. 4(8): 72 hours; a regulatory figure, not policy).
const Deadline = 72 * time.Hour

// ReporterSystem is the person_ref of a report nobody flagged.
const ReporterSystem = "system"

// Bounds of a report.
const (
	MaxNarrative = 2000
	MaxAircraft  = 10
	MaxBodyBytes = 64 << 10
)

// Aircraft is one aircraft a report names.
type Aircraft struct {
	Serial              string  `json:"serial"`
	OperatorReg         *string `json:"operator_reg"`
	FlightID            string  `json:"flight_id"`
	AuthorisationNumber *string `json:"authorisation_number"`
}

// Manned is a manned aircraft a report names.
type Manned struct {
	ICAO24   string  `json:"icao24"`
	Callsign *string `json:"callsign"`
}

// Reporter is who reports: this USSP's code and an opaque reference of
// the person (M13), never a name.
type Reporter struct {
	Org       string `json:"org"`
	PersonRef string `json:"person_ref"`
}

// Separation is the closest approach of an airprox; VM is nil when the
// vertical separation was not judged.
type Separation struct {
	HM *float64  `json:"h_m"`
	VM *float64  `json:"v_m"`
	At time.Time `json:"at"`
}

// Payload is occurrence/v1 as spec 04 §3.3 names its fields.
type Payload struct {
	Schema        string      `json:"schema"`
	ReportRef     string      `json:"report_ref"`
	Kind          string      `json:"kind"`
	Channel       string      `json:"channel"`
	OccurredAt    time.Time   `json:"occurred_at"`
	BecameAwareAt time.Time   `json:"became_aware_at"`
	Reporter      Reporter    `json:"reporter"`
	Aircraft      []Aircraft  `json:"aircraft"`
	Manned        []Manned    `json:"manned"`
	IntentRefs    []string    `json:"intent_refs"`
	MinSeparation *Separation `json:"min_separation"`
	Narrative     string      `json:"narrative"`
	EvidenceURLs  []string    `json:"evidence_urls"`
	ReportedAt    time.Time   `json:"reported_at"`
}

// Report is one report to queue.
type Report struct {
	Ref        string
	Kind       string
	Channel    string
	FlaggedBy  string // system or supervisor
	SourceKind string // alert or flight
	SourceRef  string
	Reporter   string
	OccurredAt time.Time
	AwareAt    time.Time
	FlightIDs  []string
	IntentIDs  []string
	Payload    Payload
	Body       []byte
}

// RefOf is the report_ref of a report about source: stable, so a report
// queued twice is one.
func RefOf(systemID, sourceKind, sourceRef string) string {
	return systemID + ":occurrence:" + sourceKind + ":" + sourceRef
}

// BuildError is a report that cannot be built.
type BuildError struct{ Reason string }

func (e *BuildError) Error() string { return "occurrence report not built: " + e.Reason }

var kinds = map[string]bool{KindAirprox: true, KindNonconformanceInProhib: true, KindLostLinkInUSpace: true, KindEmergency: true, KindOther: true}

// publicReg is the public part of a registration number (G-04).
func publicReg(v *string) *string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	p := regnum.PublicPart(*v)
	return &p
}

// Build fills r's payload and body: the aircraft by their public
// registration part, the evidence links, the narrative clipped. It
// refuses an unknown kind, no aircraft, an awareness before the event
// and a number that is not finite.
func Build(r *Report, systemID string, aircraft []Aircraft, manned []Manned, sep *Separation, narrative string, evidence []string) error {
	switch {
	case !kinds[r.Kind]:
		return &BuildError{"unknown kind " + r.Kind}
	case r.Channel != ChannelMandatory && r.Channel != ChannelVoluntary:
		return &BuildError{"unknown channel " + r.Channel}
	case len(aircraft) == 0:
		return &BuildError{"no aircraft"}
	case r.OccurredAt.IsZero() || r.AwareAt.Before(r.OccurredAt.Add(-time.Second)):
		return &BuildError{"became_aware_at before occurred_at"}
	case r.Reporter == "" || len(r.Reporter) > 128:
		return &BuildError{"reporter reference empty or longer than 128"}
	}
	if sep != nil {
		for _, v := range []*float64{sep.HM, sep.VM} {
			if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0) {
				return &BuildError{"a separation that is not a finite number of at least 0"}
			}
		}
	}
	if len(aircraft) > MaxAircraft {
		aircraft = aircraft[:MaxAircraft]
	}
	out := make([]Aircraft, 0, len(aircraft))
	for _, a := range aircraft {
		a.OperatorReg = publicReg(a.OperatorReg)
		out = append(out, a)
	}
	if len(narrative) > MaxNarrative {
		narrative = narrative[:MaxNarrative]
	}
	if manned == nil {
		manned = []Manned{}
	}
	if evidence == nil {
		evidence = []string{}
	}
	intents := append([]string{}, r.IntentIDs...)
	r.Ref = RefOf(systemID, r.SourceKind, r.SourceRef)
	r.Payload = Payload{Schema: Schema, ReportRef: r.Ref, Kind: r.Kind, Channel: r.Channel, OccurredAt: r.OccurredAt.UTC(),
		BecameAwareAt: r.AwareAt.UTC(), Reporter: Reporter{Org: systemID, PersonRef: r.Reporter}, Aircraft: out, Manned: manned,
		IntentRefs: intents, MinSeparation: sep, Narrative: narrative, EvidenceURLs: evidence, ReportedAt: r.AwareAt.UTC()}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r.Payload); err != nil {
		return &BuildError{"the body does not encode: " + err.Error()}
	}
	r.Body = bytes.TrimRight(buf.Bytes(), "\n")
	if len(r.Body) > MaxBodyBytes {
		return &BuildError{fmt.Sprintf("the body is %d bytes, more than %d", len(r.Body), MaxBodyBytes)}
	}
	return nil
}

// errNoDeliverer is the reason a report is not sent.
var errNoDeliverer = errors.New("the authority's published contract (api/clients/authority.yaml) has no POST /v1/occurrences: the report is queued, not sent (spec gap)")
