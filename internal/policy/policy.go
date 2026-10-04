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
	"github.com/rootxkit/uspace-core/f3411"
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
	// nonconformance_nearby fan-out. ProximityRadiusM is the plan's D10
	// name for a proximity radius with no figure (§15 Q6); nothing reads
	// it: the CPA search radius is cpa_neighbour_radius_m and the traffic
	// information radius traffic_radius_m (WP-11).
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

	// Traffic information and the CPA proximity alert (WP-11).
	// CPAClearAfterS is the proximity alert's hysteresis (C-06: resolved
	// only once the pair has been shown clear for longer than this since
	// it was last in conflict); CPAStaleAfterS the silence after which an
	// aircraft is no longer tracked and its alerts clear stale (T-10).
	// CPAPairBudget bounds the pair checks one CPA worker makes per
	// second: above it the worker evaluates every 2 s and says so
	// (evaluation_period_s), never skipping (spec 05 §5). It is a count,
	// so its name carries no unit.
	CPAClearAfterS float64 `json:"cpa_clear_after_s"`
	CPAStaleAfterS float64 `json:"cpa_stale_after_s"`
	CPAPairBudget  int     `json:"cpa_pair_budget_count"`
	// TrafficRadiusM is the radius of the traffic information around an
	// intent's envelope (02 F5: 2 km). A track is shown live while its
	// age is at most TrafficLiveMaxAgeS, stale with its age after
	// TrafficStaleAfterS, and leaves the product TrafficDropAfterS after
	// its last sample, having been shown stale all that time (B-11).
	// TrafficThrottleTracks is the number of tracks above which one
	// subscriber's product sends each track every other second (05 §5,
	// dropped_frames counts what was held back); TrafficRecordEveryS the
	// period of the product sampled for the record (03 §3: 0.1 Hz).
	TrafficRadiusM        float64 `json:"traffic_radius_m"`
	TrafficLiveMaxAgeS    float64 `json:"traffic_live_max_age_s"`
	TrafficStaleAfterS    float64 `json:"traffic_stale_after_s"`
	TrafficDropAfterS     float64 `json:"traffic_drop_after_s"`
	TrafficThrottleTracks int     `json:"traffic_throttle_track_count"`
	TrafficRecordEveryS   float64 `json:"traffic_record_every_s"`
	// EscalationAfterS is how long a critical alert may stay
	// unacknowledged before it is escalated to the supervisor console
	// (02 F5); EscalationRepeatS repeats it to the operator meanwhile.
	EscalationAfterS float64 `json:"escalation_after_s"`

	// Geo-awareness and zone alerts (WP-12). ZoneClearAfterS is the zone
	// alert's hysteresis (C-06: resolved only once the aircraft's samples
	// have shown it outside the zone for longer than this since it was
	// last inside); ZoneStaleAfterS the silence after which an aircraft
	// is no longer judged against the zones and its zone alerts clear
	// stale (T-10). ZoneConditionalSeverity is what a CONDITIONAL zone
	// raises (info or warning, uspace-core zones.Policy, Z-10: never
	// above the zone's restriction). The 120 m height limit is not here:
	// it is the authority's to judge (spec 09 §2).
	ZoneClearAfterS         float64 `json:"zone_clear_after_s"`
	ZoneStaleAfterS         float64 `json:"zone_stale_after_s"`
	ZoneConditionalSeverity string  `json:"zone_conditional_severity"`

	// F3548 strategic coordination through the DSS (WP-13).
	// PeerSubscriptionMarginM widens each U-space airspace's box by this
	// much for the DSS subscription that tells this USSP of the peers'
	// intents and the constraints there. DSSExchangeRetentionDays is how
	// long the exchange log of GET /uss/v1/log_sets keeps a DSS or peer
	// exchange (and the reports peers sent).
	PeerSubscriptionMarginM  float64 `json:"peer_subscription_margin_m"`
	DSSExchangeRetentionDays int     `json:"dss_exchange_retention_days"`

	// Coordination with the ANSP, records and occurrences (WP-15).
	// ATSAckPollS is how often a notice that needs a person's
	// acknowledgement is read back from the ANSP and ATSAckEscalateS how
	// long it may stay unacknowledged before the console escalates it
	// (cross-plan M2: every 10 s for 5 min). RecordGapS is the silence
	// that cuts a flight's track into a hole in its service record (B-13:
	// more than 3 s). OperatorPositionRetentionDays is how long the
	// remote pilot's position stays on the telemetry of a flight no
	// occurrence report holds (spec 05 §4: 90 days). AirproxReportM and
	// AirproxReportVM are the closest approach, horizontal and vertical,
	// below which a proximity alert is reported to the authority as an
	// airprox occurrence (defaults: the CPA minima).
	ATSAckPollS                   float64 `json:"ats_ack_poll_s"`
	ATSAckEscalateS               float64 `json:"ats_ack_escalate_s"`
	RecordGapS                    float64 `json:"record_gap_s"`
	OperatorPositionRetentionDays int     `json:"operator_position_retention_days"`
	AirproxReportM                float64 `json:"airprox_report_m"`
	AirproxReportVM               float64 `json:"airprox_report_v_m"`

	// Peer flights and manned traffic inputs (WP-14). MannedMarginM pads
	// the union of the U-space airspaces for the bbox of the ANSP's
	// manned-traffic stream (02 F4); MannedUnavailableS is the silence
	// of that stream after which manned traffic is unavailable since the
	// last frame (the ANSP sends a status every 2 s). PeerUnavailableS is
	// how long a peer Service Provider's flights stay in the product,
	// marked peer_unavailable, after the peer stopped answering; then
	// they age out. PeerFlightsMax bounds the flights one GET
	// /uss/flights answer may carry: a larger answer is refused whole
	// and counted, never shown in part as if complete (06 T9). It is a
	// count, so its name carries no unit.
	MannedMarginM      float64 `json:"manned_margin_m"`
	MannedUnavailableS float64 `json:"manned_unavailable_s"`
	PeerUnavailableS   float64 `json:"peer_unavailable_s"`
	PeerFlightsMax     int     `json:"peer_flights_max_count"`
	// EchoColocationM and EchoColocationS bound the echo guard of the
	// manned inputs (PLAN §15 Q23, Q25): a manned or e-conspicuity
	// record whose callsign or registration is the UA registration of an
	// active own flight is that flight's echo only within
	// EchoColocationM of where the flight's live trk.v1 track places it,
	// that sample received at most EchoColocationS ago (and captured at
	// most that long before). Away from it, or with the track quiet, the
	// record is shown as a second aircraft.
	EchoColocationM float64 `json:"echo_colocation_m"`
	EchoColocationS float64 `json:"echo_colocation_s"`
}

// MaxPeerFlightsMax bounds PeerFlightsMax.
const MaxPeerFlightsMax = 100_000

// MaxOperatorPositionRetentionDays bounds OperatorPositionRetentionDays.
const MaxOperatorPositionRetentionDays = 3660

// MaxCPAPairBudget bounds CPAPairBudget.
const MaxCPAPairBudget = 10_000_000

// MaxDSSExchangeRetentionDays bounds DSSExchangeRetentionDays.
const MaxDSSExchangeRetentionDays = 366

// MaxTrafficThrottleTracks bounds TrafficThrottleTracks.
const MaxTrafficThrottleTracks = 100_000

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
// The traffic defaults (WP-11) are uspace-core's alert lifecycle figures
// for the proximity alert (alerting.DefaultConfig: 3 s hysteresis, 15 s
// stale), spec 05 §9's budget of 50 000 pair checks per second per
// worker, 02 F5's 2 km traffic radius and 30 s escalation, a track shown
// live up to 2 s old, stale from 5 s (telemetry_lost_s) and dropped after
// 60 s, the 200-track throttle of 05 §5 and the 0.1 Hz record sample of
// 03 §3.
//
// The zone defaults (WP-12) are uspace-core's alert lifecycle figures
// (alerting.DefaultConfig: 3 s hysteresis, 15 s stale) and its
// CONDITIONAL severity (zones.DefaultPolicy: warning).
//
// The conformance defaults (WP-10) are uspace-core's alert lifecycle
// figures (alerting.DefaultConfig: 3 s hysteresis, 10 s live age) and
// its pressure margin (zones.DefaultPolicy: 250 m), so a conformance
// judgement and a zone judgement treat one sample alike.
//
// The coordination and record defaults (WP-15) are cross-plan M2's poll
// of an ANSP notice every 10 s for 5 min, B-13's 3 s silence, spec 05
// §4's 90 days for the remote pilot's position and, for an airprox
// report, the CPA minima.
//
// The peer and manned defaults (WP-14) are the brief's 5 km margin and
// 10 s silence (five of the ANSP's 2 s status periods), F3411's 60 s
// near-real-time window for a peer's last report
// (NetMaxNearRealTimeDataPeriodSeconds: older is never current) and 1000
// flights per answer (no figure in the plan). The echo guard's
// co-location is spec 04 §3.2's serial_conflict figures, pending GCAA:
// within 300 m (spoof_distance_m) of a track received at most 5 s ago.
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

		CPAClearAfterS:        lc.ClearAfterS,
		CPAStaleAfterS:        lc.StaleAfterS,
		CPAPairBudget:         50_000,
		TrafficRadiusM:        2000,
		TrafficLiveMaxAgeS:    2,
		TrafficStaleAfterS:    5,
		TrafficDropAfterS:     60,
		TrafficThrottleTracks: 200,
		TrafficRecordEveryS:   10,
		EscalationAfterS:      30,

		ZoneClearAfterS:         lc.ClearAfterS,
		ZoneStaleAfterS:         lc.StaleAfterS,
		ZoneConditionalSeverity: string(zones.DefaultPolicy().ConditionalSeverity),

		PeerSubscriptionMarginM:  2000,
		DSSExchangeRetentionDays: 7,

		ATSAckPollS:                   10,
		ATSAckEscalateS:               300,
		RecordGapS:                    3,
		OperatorPositionRetentionDays: 90,
		AirproxReportM:                c.DHorizontalMinM,
		AirproxReportVM:               c.DVerticalMinM,

		MannedMarginM:      5000,
		MannedUnavailableS: 10,
		PeerUnavailableS:   f3411.NetMaxNearRealTimeDataPeriodSeconds,
		PeerFlightsMax:     1000,
		// Pending GCAA (spec 04 §3.2 defaults).
		EchoColocationM: 300,
		EchoColocationS: 5,
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

// Zones is the uspace-core zones policy of v: the pressure margin and
// the CONDITIONAL severity; no height limit (MaxHeightAGLM nil: the
// 120 m rule is the authority's, brief WP-12).
func (v Values) Zones() zones.Policy {
	p := zones.DefaultPolicy()
	p.PressureUncertaintyM = v.PressureUncertaintyM
	p.ConditionalSeverity = core.Severity(v.ZoneConditionalSeverity)
	p.MaxHeightAGLM = nil
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
// retentions at least one day and telemetry at least the floor (the DSS
// exchange log at most MaxDSSExchangeRetentionDays); the
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
		{"cpa_clear_after_s", v.CPAClearAfterS}, {"cpa_stale_after_s", v.CPAStaleAfterS},
		{"traffic_radius_m", v.TrafficRadiusM}, {"traffic_live_max_age_s", v.TrafficLiveMaxAgeS},
		{"traffic_stale_after_s", v.TrafficStaleAfterS}, {"traffic_drop_after_s", v.TrafficDropAfterS},
		{"traffic_record_every_s", v.TrafficRecordEveryS}, {"escalation_after_s", v.EscalationAfterS},
		{"zone_clear_after_s", v.ZoneClearAfterS}, {"zone_stale_after_s", v.ZoneStaleAfterS},
		{"ats_ack_poll_s", v.ATSAckPollS}, {"ats_ack_escalate_s", v.ATSAckEscalateS}, {"record_gap_s", v.RecordGapS},
		{"airprox_report_m", v.AirproxReportM}, {"airprox_report_v_m", v.AirproxReportVM},
		{"manned_unavailable_s", v.MannedUnavailableS}, {"peer_unavailable_s", v.PeerUnavailableS},
		{"echo_colocation_m", v.EchoColocationM}, {"echo_colocation_s", v.EchoColocationS},
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
		{"pressure_uncertainty_m", v.PressureUncertaintyM}, {"peer_subscription_margin_m", v.PeerSubscriptionMarginM},
		{"manned_margin_m", v.MannedMarginM}} {
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
	if v.DSSExchangeRetentionDays < 1 || v.DSSExchangeRetentionDays > MaxDSSExchangeRetentionDays {
		errs = append(errs, core.Fieldf("dss_exchange_retention_days", "must be from 1 to %d, got %d", MaxDSSExchangeRetentionDays, v.DSSExchangeRetentionDays))
	}
	if v.OperatorPositionRetentionDays < 1 || v.OperatorPositionRetentionDays > MaxOperatorPositionRetentionDays {
		errs = append(errs, core.Fieldf("operator_position_retention_days", "must be from 1 to %d, got %d", MaxOperatorPositionRetentionDays, v.OperatorPositionRetentionDays))
	}
	if finite(v.ATSAckEscalateS) && finite(v.ATSAckPollS) && v.ATSAckEscalateS < v.ATSAckPollS {
		errs = append(errs, core.Fieldf("ats_ack_escalate_s", "must be at least ats_ack_poll_s (%v), got %v", v.ATSAckPollS, v.ATSAckEscalateS))
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
	if v.CPAPairBudget < 1 || v.CPAPairBudget > MaxCPAPairBudget {
		errs = append(errs, core.Fieldf("cpa_pair_budget_count", "must be from 1 to %d, got %d", MaxCPAPairBudget, v.CPAPairBudget))
	}
	if v.TrafficThrottleTracks < 1 || v.TrafficThrottleTracks > MaxTrafficThrottleTracks {
		errs = append(errs, core.Fieldf("traffic_throttle_track_count", "must be from 1 to %d, got %d", MaxTrafficThrottleTracks, v.TrafficThrottleTracks))
	}
	// The proximity hysteresis must outlast the ahead tolerance twice
	// over (alerting.Config's rule), and a track goes stale only after it
	// stopped being live, and leaves the product only after it was stale.
	if finite(v.CPAClearAfterS) && finite(v.TelemetryAheadToleranceS) && v.CPAClearAfterS <= 2*v.TelemetryAheadToleranceS {
		errs = append(errs, core.Fieldf("cpa_clear_after_s", "must be longer than twice telemetry_ahead_tolerance_s (%v), got %v", v.TelemetryAheadToleranceS, v.CPAClearAfterS))
	}
	if finite(v.ZoneClearAfterS) && finite(v.TelemetryAheadToleranceS) && v.ZoneClearAfterS <= 2*v.TelemetryAheadToleranceS {
		errs = append(errs, core.Fieldf("zone_clear_after_s", "must be longer than twice telemetry_ahead_tolerance_s (%v), got %v", v.TelemetryAheadToleranceS, v.ZoneClearAfterS))
	}
	switch core.Severity(v.ZoneConditionalSeverity) {
	case core.SeverityInfo, core.SeverityWarning:
	case core.SeverityCritical:
		errs = append(errs, core.Fieldf("zone_conditional_severity", "must be info or warning (a CONDITIONAL zone never raises above its restriction, Z-10), got %q", v.ZoneConditionalSeverity))
	default:
		errs = append(errs, core.Fieldf("zone_conditional_severity", "must be info or warning (a CONDITIONAL zone never raises above its restriction, Z-10), got %q", v.ZoneConditionalSeverity))
	}
	if finite(v.TrafficStaleAfterS) && finite(v.TrafficLiveMaxAgeS) && v.TrafficStaleAfterS < v.TrafficLiveMaxAgeS {
		errs = append(errs, core.Fieldf("traffic_stale_after_s", "must be at least traffic_live_max_age_s (%v), got %v", v.TrafficLiveMaxAgeS, v.TrafficStaleAfterS))
	}
	if finite(v.TrafficDropAfterS) && finite(v.TrafficStaleAfterS) && v.TrafficDropAfterS <= v.TrafficStaleAfterS {
		errs = append(errs, core.Fieldf("traffic_drop_after_s", "must be longer than traffic_stale_after_s (%v), got %v", v.TrafficStaleAfterS, v.TrafficDropAfterS))
	}
	if v.PeerFlightsMax < 1 || v.PeerFlightsMax > MaxPeerFlightsMax {
		errs = append(errs, core.Fieldf("peer_flights_max_count", "must be from 1 to %d, got %d", MaxPeerFlightsMax, v.PeerFlightsMax))
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
