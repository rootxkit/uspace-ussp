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

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/cpa"
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
}

// MaxOperatorTokenTTLS bounds OperatorTokenTTLS: an operator token
// lives at most one hour. MaxClientSecretOverlapS bounds the overlap of
// a rotation at seven days.
const (
	MaxOperatorTokenTTLS    = 3600
	MaxClientSecretOverlapS = 7 * 24 * 3600
)

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
// for 24 h and an unknown for 5 min (spec 02 F8). ProximityRadiusM has no figure in the plan; it takes
// the CPA neighbour radius until GCAA answers Q6. They are shown with
// their policy_version, never presented as the policy answer.
func Defaults() Values {
	c := cpa.DefaultPolicy
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
	}
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
	}
	for _, f := range positive {
		if !finite(f.v) || f.v <= 0 {
			errs = append(errs, core.Fieldf(f.name, "must be a finite number greater than 0, got %v", f.v))
		}
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"cpa_tcpa_max_s", v.CPATCPAMaxS}, {"cpa_neighbour_max_age_s", v.CPANeighbourMaxAgeS}} {
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
