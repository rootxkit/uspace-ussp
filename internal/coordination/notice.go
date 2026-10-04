package coordination

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/coordination/anspclient"
)

// Kind is the kind of an Annex V notice.
type Kind = anspclient.AnnexVNoticeKind

// The four kinds of coordination/annex_v/v1.
const (
	KindIntentNotice   = anspclient.AnnexVNoticeKindIntentNotice
	KindNonconformance = anspclient.AnnexVNoticeKindNonconformance
	KindContingent     = anspclient.AnnexVNoticeKindContingent
	KindEnded          = anspclient.AnnexVNoticeKindEnded
)

// AckRequired reports whether a person at the ANSP must acknowledge a
// notice of kind k (Art. 13(2)): a nonconformance or contingent notice;
// an intent_notice or ended notice is informational.
func AckRequired(k Kind) bool { return k == KindNonconformance || k == KindContingent }

// Bounds of a notice (the schema's maxItems, the ANSP's 1000 volumes per
// notice) and of its body.
const (
	MaxVolumes   = 100
	MaxVertices  = 1000
	MaxBodyBytes = 1 << 20
	maxRemarks   = 1000
)

// Intent is what a notice says of one intent.
type Intent struct {
	ID                  string
	AuthorisationNumber string
	// LocalState is this USSP's lifecycle state and DSSState the F3548
	// state as decided ("" when none).
	LocalState string
	DSSState   string
	TimeStart  time.Time
	TimeEnd    time.Time
	// Volumes are the intent's F3548 Volume4D list as stored (W84).
	Volumes json.RawMessage
}

// Deviation is the conformance state a nonconformance or contingent
// notice reports.
type Deviation struct {
	// StateID is the conformance_states row.
	StateID  int64
	FlightID string
	// State is the conformance state (nonconforming, lost_link,
	// contingent) and Reason the judgement's reason, as recorded.
	State            string
	Reason           string
	At               time.Time
	DistanceOutsideM *float64
	HeightOverM      *float64
	LastPosition     *core.LatLon
}

// Ref is the notice_ref of a notice: the system's code, the intent and
// the kind, and the conformance state for a deviation. It is stable, so
// a notice queued twice is one notice (the ANSP answers a repeat with
// the first receipt).
func Ref(systemID string, k Kind, intentID string, stateID int64) string {
	r := systemID + ":" + intentID + ":" + string(k)
	if stateID > 0 {
		r += fmt.Sprintf(":%d", stateID)
	}
	return r
}

// stateOf is the F3548 state a notice of kind k gives the intent: the
// state the deviation put it in, else the local state's, else the state
// as decided, else Activated (an ended intent keeps its last state).
func stateOf(k Kind, in Intent) anspclient.AnnexVNoticeIntentsState {
	switch k {
	case KindNonconformance:
		return anspclient.AnnexVNoticeIntentsStateNonconforming
	case KindContingent:
		return anspclient.AnnexVNoticeIntentsStateContingent
	case KindIntentNotice, KindEnded:
	}
	switch in.LocalState {
	case "activated":
		return anspclient.AnnexVNoticeIntentsStateActivated
	case "nonconforming":
		return anspclient.AnnexVNoticeIntentsStateNonconforming
	case "contingent":
		return anspclient.AnnexVNoticeIntentsStateContingent
	}
	if s := anspclient.AnnexVNoticeIntentsState(in.DSSState); s.Valid() {
		return s
	}
	return anspclient.AnnexVNoticeIntentsStateActivated
}

// ReasonOf maps a conformance state and reason to the reason of the
// notice (as alert/v1 names it): a lost link or silence is lost_link;
// threshold_exceeded, and an excursion that outlasted t_s (an outline,
// a limit or the window: the time threshold), are threshold_exceeded;
// anything else is other. The recorded reason travels in the remarks.
func ReasonOf(state, reason string) anspclient.AnnexVNoticeNonconformanceReason {
	switch {
	case state == "lost_link" || reason == "telemetry_lost":
		return anspclient.LostLink
	case reason == "threshold_exceeded", reason == "outside_volume_h", reason == "above_upper",
		reason == "below_lower", reason == "before_start", reason == "after_end":
		return anspclient.ThresholdExceeded
	default:
		return anspclient.Other
	}
}

// KindOfState is the notice kind of a conformance state: nonconformance
// for nonconforming and lost_link, contingent for contingent, none ("")
// otherwise.
func KindOfState(state string) Kind {
	switch state {
	case "nonconforming", "lost_link":
		return KindNonconformance
	case "contingent":
		return KindContingent
	}
	return ""
}

// BuildError is a notice that cannot be built: it is queued failed with
// this reason, never dropped.
type BuildError struct{ Reason string }

func (e *BuildError) Error() string { return "notice not built: " + e.Reason }

// Build is the coordination/annex_v/v1 body of one notice and its bytes:
// kind k about in (and dev for a nonconformance or contingent notice),
// sent by systemID at sentAt. The bytes are what every try posts.
func Build(k Kind, ref, systemID string, in Intent, dev *Deviation, remarks string, sentAt time.Time) (anspclient.AnnexVNotice, []byte, error) {
	var n anspclient.AnnexVNotice
	switch {
	case !k.Valid():
		return n, nil, &BuildError{"unknown kind " + string(k)}
	case ref == "" || len(ref) > 128:
		return n, nil, &BuildError{"notice_ref empty or longer than 128"}
	case systemID == "" || len(systemID) > 64:
		return n, nil, &BuildError{"ussp_id empty or longer than 64"}
	case in.AuthorisationNumber == "" || len(in.AuthorisationNumber) > 64:
		return n, nil, &BuildError{"the intent has no authorisation number of 1 to 64 characters"}
	case !in.TimeEnd.After(in.TimeStart):
		return n, nil, &BuildError{"the intent's window is empty"}
	case AckRequired(k) && dev == nil:
		return n, nil, &BuildError{string(k) + " without its conformance state"}
	}
	n.Schema = anspclient.CoordinationannexVv1
	n.NoticeRef, n.Kind, n.UsspId, n.SentAt = ref, k, systemID, sentAt.UTC().Truncate(time.Millisecond)
	// The stored volumes are F3548 Volume4D verbatim; the schema's volume
	// is that shape, so the intent is read into the generated type as it
	// is (members the schema does not name are left out).
	item, err := json.Marshal([]any{map[string]any{
		"intent_ref": in.ID, "authorisation_number": in.AuthorisationNumber, "state": stateOf(k, in),
		"time_start": in.TimeStart.UTC(), "time_end": in.TimeEnd.UTC(), "volumes": in.Volumes,
	}})
	if err != nil {
		return n, nil, &BuildError{"the intent's volumes are not JSON"}
	}
	if err := json.Unmarshal(item, &n.Intents); err != nil {
		return n, nil, &BuildError{"the intent does not read as the schema's intent: " + clip(err.Error())}
	}
	it := &n.Intents[0]
	if err := checkVolumes(it.Volumes); err != nil {
		return n, nil, err
	}
	if dev != nil {
		if err := deviation(&n, it.IntentRef, in.AuthorisationNumber, dev); err != nil {
			return n, nil, err
		}
		remarks = joinRemarks("conformance state "+dev.State+reasonRemark(dev.Reason), remarks)
	}
	if remarks != "" {
		if len(remarks) > maxRemarks {
			remarks = remarks[:maxRemarks]
		}
		n.Remarks = &remarks
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(n); err != nil {
		return n, nil, &BuildError{"the body does not encode: " + err.Error()}
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	if len(body) > MaxBodyBytes {
		return n, nil, &BuildError{fmt.Sprintf("the body is %d bytes, more than %d", len(body), MaxBodyBytes)}
	}
	return n, body, nil
}

// point is a WGS84 position as the schema writes one.
type point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func reasonRemark(r string) string {
	if r == "" {
		return ""
	}
	return " (" + r + ")"
}

func joinRemarks(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

// checkVolumes holds the volumes to the schema's bounds: 1 to MaxVolumes,
// exactly one outline each, 3 to MaxVertices vertices on a polygon, W84
// metres on both limits when given and a positive radius on a circle.
func checkVolumes[V any](vs []V) error {
	if len(vs) == 0 || len(vs) > MaxVolumes {
		return &BuildError{fmt.Sprintf("%d volumes, want 1 to %d", len(vs), MaxVolumes)}
	}
	raw, err := json.Marshal(vs)
	if err != nil {
		return &BuildError{"the volumes do not encode"}
	}
	var check []struct {
		Volume struct {
			OutlinePolygon *struct {
				Vertices []json.RawMessage `json:"vertices"`
			} `json:"outline_polygon"`
			OutlineCircle *struct {
				Radius struct {
					Value float64 `json:"value"`
				} `json:"radius"`
			} `json:"outline_circle"`
			AltitudeLower *struct {
				Reference string `json:"reference"`
				Units     string `json:"units"`
			} `json:"altitude_lower"`
			AltitudeUpper *struct {
				Reference string `json:"reference"`
				Units     string `json:"units"`
			} `json:"altitude_upper"`
		} `json:"volume"`
	}
	if err := json.Unmarshal(raw, &check); err != nil {
		return &BuildError{"the volumes do not read back"}
	}
	for i, v := range check {
		p, c := v.Volume.OutlinePolygon, v.Volume.OutlineCircle
		switch {
		case (p == nil) == (c == nil):
			return &BuildError{fmt.Sprintf("volumes[%d] has not exactly one outline", i)}
		case p != nil && (len(p.Vertices) < 3 || len(p.Vertices) > MaxVertices):
			return &BuildError{fmt.Sprintf("volumes[%d] has %d vertices, want 3 to %d", i, len(p.Vertices), MaxVertices)}
		case c != nil && !(c.Radius.Value > 0):
			return &BuildError{fmt.Sprintf("volumes[%d] has a radius that is not positive", i)}
		}
		for _, l := range []*struct {
			Reference string `json:"reference"`
			Units     string `json:"units"`
		}{v.Volume.AltitudeLower, v.Volume.AltitudeUpper} {
			if l != nil && (l.Reference != "W84" || l.Units != "M") {
				return &BuildError{fmt.Sprintf("volumes[%d] has a limit that is not W84 metres", i)}
			}
		}
	}
	return nil
}

// deviation fills the nonconformance member from dev: the numbers
// recorded with the conformance state (a number not judged stays out,
// never 0) and the last position.
func deviation(n *anspclient.AnnexVNotice, intentRef openapi_types.UUID, number string, dev *Deviation) error {
	nc := &struct {
		AltWgs84M           *float64                                    `json:"alt_wgs84_m,omitempty"`
		AuthorisationNumber string                                      `json:"authorisation_number"`
		DetectedAt          time.Time                                   `json:"detected_at"`
		DistanceOutsideM    *float64                                    `json:"distance_outside_m,omitempty"`
		HeightOverM         *float64                                    `json:"height_over_m,omitempty"`
		IntentRef           openapi_types.UUID                          `json:"intent_ref"`
		Position            *point                                      `json:"position,omitempty"`
		Reason              anspclient.AnnexVNoticeNonconformanceReason `json:"reason"`
	}{AuthorisationNumber: number, IntentRef: intentRef, DetectedAt: dev.At.UTC(), Reason: ReasonOf(dev.State, dev.Reason)}
	for _, f := range []struct {
		name string
		v    *float64
		dst  **float64
	}{{"distance_outside_m", dev.DistanceOutsideM, &nc.DistanceOutsideM}, {"height_over_m", dev.HeightOverM, &nc.HeightOverM}} {
		if f.v == nil {
			continue
		}
		if !core.IsFinite(*f.v) || *f.v < 0 {
			return &BuildError{f.name + " is not a finite number of at least 0"}
		}
		v := *f.v
		*f.dst = &v
	}
	if p := dev.LastPosition; p != nil {
		if !p.Valid() {
			return &BuildError{"the last position is not a WGS84 position"}
		}
		nc.Position = &point{Lat: p.LatDeg, Lng: p.LonDeg}
	}
	raw, err := json.Marshal(nc)
	if err != nil {
		return &BuildError{"the deviation does not encode"}
	}
	return json.Unmarshal(raw, &n.Nonconformance)
}
