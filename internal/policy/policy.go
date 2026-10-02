// Package policy is the versioned policy row (docs/PLAN.md D10; LESSONS
// INV-03): every threshold a judgement uses, with its unit in its name,
// stored as a new version on every change, projected to the KV bucket
// `policy` inside the writing transaction (B-09, G-08), and carried as
// policy_version on every alert, decision and record (CLAUDE.md rule 5).
//
// The package sits at the bottom of the import order (PLAN §4) so the
// hot path can import Values without the store. The database side is
// the Store interface, which internal/store implements; the KV side is
// the Projector interface, which WP-6 implements on internal/bus.
//
// The 24 h limits on peer data and the standards' constants are not
// policy: they come from uspace-core f3411 and f3548.
package policy

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/rootxkit/uspace-core/alerting"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/cpa"
	"github.com/rootxkit/uspace-core/rid"
	"github.com/rootxkit/uspace-core/zones"
)

// Values are the thresholds of one policy version. Every field carries
// its unit; the JSON names are the wire and storage names.
type Values struct {
	// Deviation thresholds an intent gets when it brings none (Art.
	// 10(2)(d)): horizontal, vertical (AMSL) and time.
	DeviationHM float64 `json:"deviation_h_m"`
	DeviationVM float64 `json:"deviation_v_m"`
	DeviationTS float64 `json:"deviation_t_s"`

	// TelemetryLostS is the silence after which a flight is
	// telemetry_lost; LostLinkS the silence after which it is lost_link.
	TelemetryLostS float64 `json:"telemetry_lost_s"`
	LostLinkS      float64 `json:"lost_link_s"`

	// NonconformanceNearbyRadiusM is the radius of the
	// nonconformance_nearby fan-out; ProximityRadiusM the radius of the
	// traffic information around a flight.
	NonconformanceNearbyRadiusM float64 `json:"nonconformance_nearby_radius_m"`
	ProximityRadiusM            float64 `json:"proximity_radius_m"`

	// CISStaleS is the age after which the CIS cache is stale.
	CISStaleS float64 `json:"cis_stale_s"`
	// EscalationRepeatS is the repeat of an unacknowledged alert.
	EscalationRepeatS float64 `json:"escalation_repeat_s"`

	// CPA minima and pair selection (uspace-core cpa.Policy).
	CPATCPAMaxS         float64 `json:"cpa_tcpa_max_s"`
	CPAHorizontalMinM   float64 `json:"cpa_horizontal_min_m"`
	CPAVerticalMinM     float64 `json:"cpa_vertical_min_m"`
	CPANeighbourRadiusM float64 `json:"cpa_neighbour_radius_m"`
	CPANeighbourMaxAgeS float64 `json:"cpa_neighbour_max_age_s"`

	// Retention: telemetry online (floor 30 days, Art. 15(1)(g)),
	// alerts, intents and conformance records, and the audit log.
	TelemetryRetentionDays int `json:"telemetry_retention_days"`
	RecordRetentionDays    int `json:"record_retention_days"`
	AuditRetentionDays     int `json:"audit_retention_days"`

	// OperatorTokenTTLS is the lifetime of an operator machine token
	// this USSP issues (at most MaxOperatorTokenTTLS, cross-plan
	// Appendix A). ClientSecretOverlapS is how long the previous secret
	// of a rotated client keeps working (0: not at all).
	OperatorTokenTTLS    int `json:"operator_token_ttl_s"`
	ClientSecretOverlapS int `json:"client_secret_overlap_s"`

	// RegistryPositiveTTLS is how long an F8 answer the registry holds
	// (valid, suspended, revoked) is served from the cache;
	// RegistryNegativeTTLS how long an unknown is (spec 02 F8: 24 h and
	// 5 min). The change feed invalidates either sooner.
	RegistryPositiveTTLS float64 `json:"registry_positive_ttl_s"`
	RegistryNegativeTTLS float64 `json:"registry_negative_ttl_s"`

	// SpecialOperationPriority is the F3548 priority of a special
	// operation (SERA Art. 4, Art. 10(8)); a normal flight has 0. It is
	// dimensionless (an ordinal), so its name carries no unit.
	SpecialOperationPriority int `json:"special_operation_priority"`
	// DeconflictBufferM and DeconflictVerticalBufferM widen the
	// strategic deconfliction of intents horizontally and vertically
	// (AMSL); 0 is no buffer, a negative or non-finite value refuses
	// the check (E-15).
	DeconflictBufferM         float64 `json:"deconflict_buffer_m"`
	DeconflictVerticalBufferM float64 `json:"deconflict_vertical_buffer_m"`
	// ActivationLeadS is how long before its time_start an accepted
	// intent may be activated.
	ActivationLeadS float64 `json:"activation_lead_s"`
	// IntentOpenMaxCount bounds the intents one operator may hold that
	// are not ended, rejected or withdrawn (every write is bounded).
	IntentOpenMaxCount int `json:"intent_open_max_count"`

	// Operator telemetry ingest (WP-8). BacklogAfterS: a sample placed
	// further than this behind its receipt is history (backlog, T-04,
	// T-11) even when the client did not say so. FlightEndAfterS: the
	// silence after which a flight ends (telemetry_lost comes first, at
	// TelemetryLostS). IngestQueueS: how long a placed sample may wait in
	// the publisher's memory before it goes to the ingest.v1 work queue
	// instead (spec 05 §5). IngestBacklogMaxS: how old a sample in the
	// work queue may be before it is shed with a gap record (never the
	// newest; the queue itself holds ten minutes).
	BacklogAfterS     float64 `json:"backlog_after_s"`
	FlightEndAfterS   float64 `json:"flight_end_after_s"`
	IngestQueueS      float64 `json:"ingest_queue_s"`
	IngestBacklogMaxS float64 `json:"ingest_backlog_max_s"`
	// TelemetryRateHz is the live sample rate one aircraft of one client
	// may send (spec 05 §5: 2 Hz; over-rate samples are dropped and
	// counted); TelemetryBacklogRateHz the rate of its backlog samples, so
	// a drain clearly exceeds intake (B-01) and is still bounded.
	TelemetryRateHz        float64 `json:"telemetry_rate_hz"`
	TelemetryBacklogRateHz float64 `json:"telemetry_backlog_rate_hz"`
	// TelemetryDedupeS is how long a (serial, seq) is remembered so that a
	// replayed sample publishes nothing twice (B-05): as long as a client
	// keeps what it has not seen acknowledged (its ten-minute queue, 02
	// F5), since it sends that again after an outage. It is kept in the
	// telemetry_seen bucket, shared by every instance and surviving a
	// restart; the bucket's TTL (1 h) caps it.
	TelemetryDedupeS float64 `json:"telemetry_dedupe_s"`
	// TelemetryAheadToleranceS is how far ahead of its receipt a sample's
	// own time may be before it is clamped and counted (T-13: 1 s).
	TelemetryAheadToleranceS float64 `json:"telemetry_ahead_tolerance_s"`
	// TelemetryAnchorMaxAgeS is how long the clock relation learnt from an
	// aircraft's live samples places its later samples by their own time
	// (T-11) before it is learnt again.
	TelemetryAnchorMaxAgeS float64 `json:"telemetry_anchor_max_age_s"`
	// TelemetryBatchSpanS bounds the source-time span of one
	// POST /v1/telemetry/batch (02 F5: at most 1 s of samples).
	TelemetryBatchSpanS float64 `json:"telemetry_batch_span_s"`
	// PressureFallbackAccuracyCode is the lowest vertical accuracy code
	// (MAV_ODID_VER_ACC, as F3411's VerticalAccuracy orders it) at which a
	// geodetic altitude is used; below it the pressure altitude stands in
	// and is held for PressureHoldS (uspace-core rid.AltPolicy, R-08).
	PressureFallbackAccuracyCode int     `json:"pressure_fallback_accuracy_code"`
	PressureHoldS                float64 `json:"pressure_hold_s"`
	// TeleportSpeedMS is the speed between two samples of one aircraft
	// above which the later one is flagged anomaly teleport and counted,
	// never dropped (spec 06 T3: 100 m/s).
	TeleportSpeedMS float64 `json:"teleport_speed_ms"`

	// F3411 network identification Service Provider (WP-9).
	// RIDRecentPositionsMaxCount bounds the samples rid-sp keeps per
	// flight for the standard's 60 s window and recent_positions (E-10):
	// beyond it the oldest go first. SessionISARadiusM is the radius of
	// the Identification Service Area of a flight without an intent,
	// around its first position; SessionISAHorizonS how far ahead that
	// ISA reaches (renewed while the flight goes on).
	RIDRecentPositionsMaxCount int     `json:"rid_recent_positions_max_count"`
	SessionISARadiusM          float64 `json:"session_isa_radius_m"`
	SessionISAHorizonS         float64 `json:"session_isa_horizon_s"`

	// Conformance monitoring (WP-10). ConformanceClearAfterS is the
	// hysteresis: a nonconforming flight returns to conforming only once
	// its samples have shown it inside for longer than this since it was
	// last outside (C-06). PressureUncertaintyM widens the authorised band,
	// each way, for a sample whose AMSL altitude comes from pressure
	// (R-09; spec 04 §3.1 within_band). MonitorLiveMaxAgeS bounds the
	// ingest-to-monitor leg, wall - rx_ts (T-05): an older sample is
	// rejected as late and judges nothing.
	ConformanceClearAfterS float64 `json:"conformance_clear_after_s"`
	PressureUncertaintyM   float64 `json:"pressure_uncertainty_m"`
	MonitorLiveMaxAgeS     float64 `json:"monitor_live_max_age_s"`
}

// MaxSpecialOperationPriority bounds SpecialOperationPriority (an
// int32 column and the F3548 integer).
const MaxSpecialOperationPriority = 1 << 30

// MaxIntentOpenMaxCount bounds IntentOpenMaxCount.
const MaxIntentOpenMaxCount = 100_000

// MaxOperatorTokenTTLS bounds OperatorTokenTTLS: an operator token
// lives at most one hour. MaxClientSecretOverlapS bounds the overlap of
// a rotation at seven days.
const (
	MaxOperatorTokenTTLS    = 3600
	MaxClientSecretOverlapS = 7 * 24 * 3600
)

// MaxPressureFallbackAccuracyCode is the highest vertical accuracy code
// (MAV_ODID_VER_ACC 6: under 1 m).
const MaxPressureFallbackAccuracyCode = 6

// MaxRIDRecentPositionsMaxCount bounds RIDRecentPositionsMaxCount: a
// flight at the ingest's 2 Hz cap holds 120 samples in 60 s.
const MaxRIDRecentPositionsMaxCount = 10_000

// TelemetryRetentionFloorDays is the shortest telemetry retention a
// policy may set (Art. 15(1)(g)); a lower value is refused.
const TelemetryRetentionFloorDays = 30

// Defaults are the demo defaults of docs/PLAN.md §15 Q6 and Q18, owner
// questions still open: deviation 50 m / 15 m / 60 s, telemetry_lost
// 5 s, lost_link 15 s, nearby radius 2000 m, uspace-core's CPA defaults
// (cpa.DefaultPolicy: 60 s, 60 m, 20 m, 800 m, 10 s), CIS stale after
// 300 s, escalation every 10 s, telemetry 90 days, records 5 years,
// audit 10 years, operator tokens for one hour and a rotated client
// secret overlapping its successor for one day, registry answers cached
// for 24 h and an unknown for 5 min (spec 02 F8), special operations at
// priority 100, no deconfliction buffer (WP-7 brief), activation from 10
// minutes before time_start, and 1000 open intents per operator. ProximityRadiusM has no figure in the plan; it takes
// the CPA neighbour radius until GCAA answers Q6. They are shown with
// their policy_version, never presented as the policy answer.
//
// The telemetry ingest defaults are spec 05 §5 and the WP-8 brief: backlog
// after 10 s, flights end after 120 s of silence, 10 s in the publisher's
// memory, shed from the work queue at 9 minutes (inside its 10), 2 Hz
// live and 20 Hz backlog per aircraft, replays remembered 600 s (the
// client queue; the brief's 30 s let a sample re-sent after a longer
// outage be published twice), 1 s
// ahead (T-13), the clock relation relearnt every 60 s, batches of 1 s,
// uspace-core's altitude selection (rid.DefaultAltPolicy), and a teleport
// above 100 m/s (spec 06 T3).
//
// The Service Provider defaults (WP-9, no figure in the plan): 120
// samples per flight (60 s at the ingest's 2 Hz cap), a 2000 m session
// ISA reaching one hour ahead.
//
// The conformance defaults (WP-10) are uspace-core's alert lifecycle
// figures (alerting.DefaultConfig: 3 s hysteresis, 10 s live age) and
// its pressure margin (zones.DefaultPolicy: 250 m), so a conformance
// judgement and a zone judgement treat one sample alike.
func Defaults() Values {
	c := cpa.DefaultPolicy
	alt := rid.DefaultAltPolicy()
	lc := alerting.DefaultConfig()
	return Values{
		DeviationHM:                 50,
		DeviationVM:                 15,
		DeviationTS:                 60,
		TelemetryLostS:              5,
		LostLinkS:                   15,
		NonconformanceNearbyRadiusM: 2000,
		ProximityRadiusM:            c.NeighbourRadiusM,
		CISStaleS:                   300,
		EscalationRepeatS:           10,
		CPATCPAMaxS:                 c.TCPAMaxS,
		CPAHorizontalMinM:           c.DHorizontalMinM,
		CPAVerticalMinM:             c.DVerticalMinM,
		CPANeighbourRadiusM:         c.NeighbourRadiusM,
		CPANeighbourMaxAgeS:         c.NeighbourMaxAgeS,
		TelemetryRetentionDays:      90,
		RecordRetentionDays:         5 * 365,
		AuditRetentionDays:          10 * 365,
		OperatorTokenTTLS:           3600,
		ClientSecretOverlapS:        24 * 3600,
		RegistryPositiveTTLS:        24 * 3600,
		RegistryNegativeTTLS:        300,
		SpecialOperationPriority:    100,
		DeconflictBufferM:           0,
		DeconflictVerticalBufferM:   0,
		ActivationLeadS:             600,
		IntentOpenMaxCount:          1000,

		BacklogAfterS:                10,
		FlightEndAfterS:              120,
		IngestQueueS:                 10,
		IngestBacklogMaxS:            540,
		TelemetryRateHz:              2,
		TelemetryBacklogRateHz:       20,
		TelemetryDedupeS:             600,
		TelemetryAheadToleranceS:     1,
		TelemetryAnchorMaxAgeS:       60,
		TelemetryBatchSpanS:          1,
		PressureFallbackAccuracyCode: int(alt.MinVerticalAccuracy),
		PressureHoldS:                alt.PressureHoldS,
		TeleportSpeedMS:              100,

		RIDRecentPositionsMaxCount: 120,
		SessionISARadiusM:          2000,
		SessionISAHorizonS:         3600,

		ConformanceClearAfterS: lc.ClearAfterS,
		PressureUncertaintyM:   zones.DefaultPolicy().PressureUncertaintyM,
		MonitorLiveMaxAgeS:     lc.LiveMaxAgeS,
	}
}

// AltPolicy is the uspace-core altitude selection of v (the pressure
// fallback and hold, R-08).
func (v Values) AltPolicy() rid.AltPolicy {
	p := rid.DefaultAltPolicy()
	p.MinVerticalAccuracy = uint8(min(max(v.PressureFallbackAccuracyCode, 0), MaxPressureFallbackAccuracyCode))
	p.PressureHoldS = v.PressureHoldS
	return p
}

// CPA is the uspace-core CPA policy of v.
func (v Values) CPA() cpa.Policy {
	return cpa.Policy{
		TCPAMaxS:         v.CPATCPAMaxS,
		DHorizontalMinM:  v.CPAHorizontalMinM,
		DVerticalMinM:    v.CPAVerticalMinM,
		NeighbourRadiusM: v.CPANeighbourRadiusM,
		NeighbourMaxAgeS: v.CPANeighbourMaxAgeS,
	}
}

// Validate refuses a value no judgement can use: every threshold
// finite; every minimum, radius, timeout and repeat positive (a zero
// minimum would read every pair as clear); the CPA window and maximum
// age may be zero (cpa.Policy: "now only", "same instant only");
// the deconfliction buffers may be zero (no buffer); the special
// operation priority at least 1 (above a normal flight's 0) and the
// open-intent bound at least 1;
// retentions at least one day and telemetry at least the floor; the
// operator token TTL from 60 s to one hour and the secret overlap from 0
// to seven days. Every refusal names its field.
func (v Values) Validate() error {
	var errs []error
	positive := []struct {
		name string
		v    float64
	}{
		{"deviation_h_m", v.DeviationHM}, {"deviation_v_m", v.DeviationVM}, {"deviation_t_s", v.DeviationTS},
		{"telemetry_lost_s", v.TelemetryLostS}, {"lost_link_s", v.LostLinkS},
		{"nonconformance_nearby_radius_m", v.NonconformanceNearbyRadiusM}, {"proximity_radius_m", v.ProximityRadiusM},
		{"cis_stale_s", v.CISStaleS}, {"escalation_repeat_s", v.EscalationRepeatS},
		{"cpa_horizontal_min_m", v.CPAHorizontalMinM}, {"cpa_vertical_min_m", v.CPAVerticalMinM},
		{"cpa_neighbour_radius_m", v.CPANeighbourRadiusM},
		{"registry_positive_ttl_s", v.RegistryPositiveTTLS}, {"registry_negative_ttl_s", v.RegistryNegativeTTLS},
		{"activation_lead_s", v.ActivationLeadS},
		{"backlog_after_s", v.BacklogAfterS}, {"flight_end_after_s", v.FlightEndAfterS},
		{"ingest_queue_s", v.IngestQueueS}, {"ingest_backlog_max_s", v.IngestBacklogMaxS},
		{"telemetry_rate_hz", v.TelemetryRateHz}, {"telemetry_backlog_rate_hz", v.TelemetryBacklogRateHz},
		{"telemetry_dedupe_s", v.TelemetryDedupeS}, {"telemetry_ahead_tolerance_s", v.TelemetryAheadToleranceS},
		{"telemetry_anchor_max_age_s", v.TelemetryAnchorMaxAgeS}, {"telemetry_batch_span_s", v.TelemetryBatchSpanS},
		{"pressure_hold_s", v.PressureHoldS}, {"teleport_speed_ms", v.TeleportSpeedMS},
		{"session_isa_radius_m", v.SessionISARadiusM}, {"session_isa_horizon_s", v.SessionISAHorizonS},
		{"conformance_clear_after_s", v.ConformanceClearAfterS}, {"monitor_live_max_age_s", v.MonitorLiveMaxAgeS},
	}
	for _, f := range positive {
		if !finite(f.v) || f.v <= 0 {
			errs = append(errs, core.Fieldf(f.name, "must be a finite number greater than 0, got %v", f.v))
		}
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"cpa_tcpa_max_s", v.CPATCPAMaxS}, {"cpa_neighbour_max_age_s", v.CPANeighbourMaxAgeS},
		{"deconflict_buffer_m", v.DeconflictBufferM}, {"deconflict_vertical_buffer_m", v.DeconflictVerticalBufferM},
		{"pressure_uncertainty_m", v.PressureUncertaintyM}} {
		if !finite(f.v) || f.v < 0 {
			errs = append(errs, core.Fieldf(f.name, "must be a finite number of at least 0, got %v", f.v))
		}
	}
	if v.TelemetryRetentionDays < TelemetryRetentionFloorDays {
		errs = append(errs, core.Fieldf("telemetry_retention_days", "must be at least %d (Art. 15(1)(g)), got %d", TelemetryRetentionFloorDays, v.TelemetryRetentionDays))
	}
	if v.RecordRetentionDays < 1 {
		errs = append(errs, core.Fieldf("record_retention_days", "must be at least 1, got %d", v.RecordRetentionDays))
	}
	if v.AuditRetentionDays < 1 {
		errs = append(errs, core.Fieldf("audit_retention_days", "must be at least 1, got %d", v.AuditRetentionDays))
	}
	if v.OperatorTokenTTLS < 60 || v.OperatorTokenTTLS > MaxOperatorTokenTTLS {
		errs = append(errs, core.Fieldf("operator_token_ttl_s", "must be from 60 to %d, got %d", MaxOperatorTokenTTLS, v.OperatorTokenTTLS))
	}
	if v.SpecialOperationPriority < 1 || v.SpecialOperationPriority > MaxSpecialOperationPriority {
		errs = append(errs, core.Fieldf("special_operation_priority", "must be from 1 to %d, got %d", MaxSpecialOperationPriority, v.SpecialOperationPriority))
	}
	if v.IntentOpenMaxCount < 1 || v.IntentOpenMaxCount > MaxIntentOpenMaxCount {
		errs = append(errs, core.Fieldf("intent_open_max_count", "must be from 1 to %d, got %d", MaxIntentOpenMaxCount, v.IntentOpenMaxCount))
	}
	if v.PressureFallbackAccuracyCode < 1 || v.PressureFallbackAccuracyCode > MaxPressureFallbackAccuracyCode {
		errs = append(errs, core.Fieldf("pressure_fallback_accuracy_code", "must be from 1 to %d, got %d", MaxPressureFallbackAccuracyCode, v.PressureFallbackAccuracyCode))
	}
	if v.RIDRecentPositionsMaxCount < 1 || v.RIDRecentPositionsMaxCount > MaxRIDRecentPositionsMaxCount {
		errs = append(errs, core.Fieldf("rid_recent_positions_max_count", "must be from 1 to %d, got %d", MaxRIDRecentPositionsMaxCount, v.RIDRecentPositionsMaxCount))
	}
	if finite(v.FlightEndAfterS) && finite(v.TelemetryLostS) && v.FlightEndAfterS <= v.TelemetryLostS {
		errs = append(errs, core.Fieldf("flight_end_after_s", "must be longer than telemetry_lost_s (%v), got %v", v.TelemetryLostS, v.FlightEndAfterS))
	}
	// The hysteresis must outlast what a clock ahead within the tolerance
	// could shift (alerting.Config's rule: clear_after_s > 2 x the ahead
	// tolerance), or a placement ahead would buy a clear.
	if finite(v.ConformanceClearAfterS) && finite(v.TelemetryAheadToleranceS) && v.ConformanceClearAfterS <= 2*v.TelemetryAheadToleranceS {
		errs = append(errs, core.Fieldf("conformance_clear_after_s", "must be longer than twice telemetry_ahead_tolerance_s (%v), got %v", v.TelemetryAheadToleranceS, v.ConformanceClearAfterS))
	}
	if v.ClientSecretOverlapS < 0 || v.ClientSecretOverlapS > MaxClientSecretOverlapS {
		errs = append(errs, core.Fieldf("client_secret_overlap_s", "must be from 0 to %d, got %d", MaxClientSecretOverlapS, v.ClientSecretOverlapS))
	}
	return errors.Join(errs...)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// Record is one stored policy version.
type Record struct {
	Version   int64     `json:"policy_version"`
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	Values    Values    `json:"values"`
}

// ProjectionError is the refusal of a write whose KV projection could
// not be written (B-09): the transaction was rolled back and nothing
// changed. It is the 503-shaped error every writer of a KV projection
// returns (policy here, source switches in internal/sources); httpx maps
// it to a 503 problem of type projection_unavailable.
type ProjectionError struct {
	// Bucket is the KV bucket that refused the write.
	Bucket string
	Err    error
}

func (e *ProjectionError) Error() string {
	return fmt.Sprintf("KV bucket %s cannot take the write; nothing was changed: %v", e.Bucket, e.Err)
}

func (e *ProjectionError) Unwrap() error { return e.Err }

// HTTPStatus is 503: the dependency is down, the request may be retried.
func (e *ProjectionError) HTTPStatus() int { return 503 }

// ProblemSlug is the problem type of the refusal.
func (e *ProjectionError) ProblemSlug() string { return "projection_unavailable" }

// ProblemDetail names the bucket and says nothing changed; the cause is
// logged, never sent.
func (e *ProjectionError) ProblemDetail() string {
	return "the " + e.Bucket + " projection cannot take the write; nothing was changed"
}
