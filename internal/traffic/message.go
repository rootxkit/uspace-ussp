package traffic

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"strconv"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// Schemas and names of what the CPA path publishes.
const (
	// SchemaAlert is alert/v1 (schemas/alert/v1) on
	// alrt.v1.proximity.<cell5>.<alert_id>.
	SchemaAlert = "alert/v1"
	// KindProximity is the alert kind of a CPA conflict (04 §3.3).
	KindProximity = "proximity"
	// Producer is the envelope producer of the monitor.
	Producer = "ussp/monitor"
)

// Alert states (04 §3.3).
const (
	AlertRaised  = "raised"
	AlertUpdated = "updated"
	AlertCleared = "cleared"
)

// ClearNotReconfirmed is the clear of an alert carried across a restart,
// a handover or a policy change that core did not raise again although
// both aircraft were heard for longer than the hysteresis. Its
// clearing_detail says so; core raises the pair again the moment it
// holds.
const ClearNotReconfirmed = "not_reconfirmed"

// Peer is the other aircraft of a proximity alert (04 §3.3: peer
// {track_id, trust}; the source says where it was heard).
type Peer struct {
	TrackID string     `json:"track_id"`
	Trust   core.Trust `json:"trust"`
	Source  string     `json:"source"`
}

// AlertBody is alert/v1 as the CPA path writes it (schemas/alert/v1).
type AlertBody struct {
	AlertID             string         `json:"alert_id"`
	Kind                string         `json:"kind"`
	Severity            core.Severity  `json:"severity"`
	State               string         `json:"state"`
	ClearReason         *string        `json:"clear_reason"`
	FlightID            string         `json:"flight_id"`
	IntentID            *string        `json:"intent_id"`
	AuthorisationNumber *string        `json:"authorisation_number"`
	CapturedAt          bus.Stamp      `json:"captured_at"`
	RaisedAt            bus.Stamp      `json:"raised_at"`
	UpdatedAt           bus.Stamp      `json:"updated_at"`
	PolicyVersion       int64          `json:"policy_version"`
	Detail              map[string]any `json:"detail"`
	ClearingDetail      map[string]any `json:"clearing_detail,omitempty"`
}

// AlertMessage is one alert/v1 message.
type AlertMessage struct {
	bus.Envelope
	Body AlertBody `json:"body"`
}

// AlertID is the alert id of one flight's side of a pair raised at
// raisedAt: a version 4 UUID derived from both, so a republish, a
// restart and a handover name the same alert and the record stays one
// row (api records by it).
func AlertID(pairKey, flightID string, raisedAt time.Time) string {
	sum := sha256.Sum256([]byte(KindProximity + "|" + pairKey + "|" + flightID + "|" + strconv.FormatInt(raisedAt.UnixNano(), 10)))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// PairID is an opaque id of the pair, the same on both flights' alerts
// (C-11: one conflict, described identically from both sides).
func PairID(pairKey string) string {
	sum := sha256.Sum256([]byte(pairKey))
	return hex.EncodeToString(sum[:12])
}

// Detail is the proximity detail of a conflict under 04 §3.3's names
// from core's detail (alerting conflictRaise): t_cpa_s, d_cpa_h_m
// (d_cpa_horizontal_m), d_alt_m (d_alt_at_cpa_m, null when the vertical
// is not known, R-09), the peer, and beside them the current horizontal
// distance, whether the vertical separation was known, the time to the
// loss of separation (los_start_s, core's ranking), the pair id and the
// evaluation period. Core's numbers are carried at full precision.
func Detail(c map[string]any, peer Peer, pairID string, evaluationPeriodS float64) map[string]any {
	return map[string]any{
		"t_cpa_s": c["t_cpa_s"], "d_cpa_h_m": c["d_cpa_horizontal_m"], "d_alt_m": c["d_alt_at_cpa_m"],
		"d_horizontal_now_m": c["d_horizontal_now_m"], "vertical_separation_known": c["vertical_separation_known"],
		"los_start_s": c["los_start_s"], "peer": peer, "pair_id": pairID, "evaluation_period_s": evaluationPeriodS,
	}
}

// ClearingDetail is the clearing detail of a resolved conflict (C-14:
// the separation that cleared it) with its placed time as a timestamp.
func ClearingDetail(coreClearing map[string]any) map[string]any {
	if coreClearing == nil {
		return nil
	}
	d := maps.Clone(coreClearing)
	if s, ok := d["clearing_at_s"].(float64); ok {
		delete(d, "clearing_at_s")
		d["clearing_at"] = bus.Stamp{Time: timeOfS(s)}
	}
	return d
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
