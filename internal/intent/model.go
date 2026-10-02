package intent

import (
	"encoding/json"
	"time"

	"github.com/rootxkit/uspace-core/f3548"
)

// Schema names of the messages this package owns (docs/PLAN.md D8,
// schemas/intent/*).
const (
	SchemaRequest  = "intent/request/v1"
	SchemaDecision = "intent/decision/v1"
	SchemaState    = "intent/state/v1"
)

// Point is a WGS84 position as F3548 writes one (lat, lng in degrees).
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Contingency is the Art. 6(8) contingency measures of a request.
type Contingency struct {
	Procedure    string  `json:"procedure"`
	LandingSites []Point `json:"landing_sites,omitempty"`
}

// Request is intent/request/v1 (spec 04 §3.5): the ten Annex IV items,
// numbered in the comments as in the Annex, and what the USSP needs
// beside them. Every field the operator sends is here; an unknown one is
// refused by the decoder.
type Request struct {
	// ClientRef makes the request idempotent per client.
	ClientRef string `json:"client_ref"`
	// (1) the serial of the UA or of its add-on.
	UASSerial string `json:"uas_serial"`
	// (2) VLOS or BVLOS.
	Mode string `json:"mode"`
	// (3) normal or special_operation (SERA Art. 4); Priority follows
	// it (0, or the policy's special_operation_priority).
	FlightType string `json:"flight_type"`
	Priority   *int   `json:"priority,omitempty"`
	// (4) the category, with the open subcategory, the class label or
	// the type certificate, and the privately built declaration with its
	// MTOM (Art. 1(3)).
	Category        string   `json:"category"`
	Subcategory     string   `json:"subcategory,omitempty"`
	ClassLabel      string   `json:"class_label,omitempty"`
	TypeCertificate string   `json:"type_certificate,omitempty"`
	PrivatelyBuilt  bool     `json:"privately_built,omitempty"`
	MTOMKg          *float64 `json:"mtom_kg,omitempty"`
	// (5) the 4D trajectory as F3548 Volume4D, altitudes W84 in metres.
	Volumes []f3548.Volume4D `json:"volumes"`
	// (6) network, direct or both.
	IdentificationTechnology string `json:"identification_technology"`
	// (7) the C2 and data link methods.
	ConnectivityMethods []string `json:"connectivity_methods"`
	// (8) the endurance in seconds.
	EnduranceS int `json:"endurance_s"`
	// (9) the loss-of-C2 procedure.
	LossOfC2Procedure string `json:"loss_of_c2_procedure"`
	// (10) the operator registration number (public part) and the UA
	// registration when applicable (certified category).
	OperatorReg    string `json:"operator_reg"`
	UARegistration string `json:"ua_registration,omitempty"`

	PilotRef            string      `json:"pilot_ref,omitempty"`
	Takeoff             *Point      `json:"takeoff,omitempty"`
	Landing             *Point      `json:"landing,omitempty"`
	Contingency         Contingency `json:"contingency"`
	EmergencyContactRef string      `json:"emergency_contact_ref"`
	// AuthorisationRef is a specific-category authorisation from the
	// authority; it lifts nothing here, it turns pending_authority into
	// a condition for a REQ_AUTHORIZATION zone (spec Q12).
	AuthorisationRef string `json:"authorisation_ref,omitempty"`
}

// The enumerations of a request.
const (
	ModeVLOS  = "VLOS"
	ModeBVLOS = "BVLOS"

	FlightNormal  = "normal"
	FlightSpecial = "special_operation"

	CategoryOpen      = "open"
	CategorySpecific  = "specific"
	CategoryCertified = "certified"

	IdentNetwork = "network"
	IdentDirect  = "direct"
	IdentBoth    = "both"
)

// Decisions (intent/decision/v1 `decision`).
const (
	DecisionAuthorised        = "authorised"
	DecisionAcceptedVoluntary = "accepted_voluntary"
	DecisionRejected          = "rejected"
	DecisionPendingValidation = "pending_validation"
	DecisionPendingAuthority  = "pending_authority"
	DecisionPendingDSS        = "pending_dss"
)

// Local states (operational_intents.local_state).
const (
	StatePendingValidation = "pending_validation"
	StatePendingDSS        = "pending_dss"
	StatePendingAuthority  = "pending_authority"
	StateAccepted          = "accepted"
	StateActivated         = "activated"
	StateNonconforming     = "nonconforming"
	StateContingent        = "contingent"
	StateEnded             = "ended"
	StateRejected          = "rejected"
	StateWithdrawn         = "withdrawn"
)

// ActiveStates are the states whose volumes other intents are
// deconflicted against and that the hot path reads from intent_active.
var ActiveStates = []string{StateAccepted, StateActivated, StateNonconforming, StateContingent}

// OpenStates are the states that count against an operator's bound.
var OpenStates = []string{StatePendingValidation, StatePendingDSS, StatePendingAuthority, StateAccepted, StateActivated, StateNonconforming, StateContingent}

// Conflict kinds.
const (
	KindAnnexIV     = "annex_iv"
	KindRegistry    = "registry"
	KindCIS         = "cis"
	KindAirspace    = "airspace"
	KindZone        = "zone"
	KindRestriction = "restriction"
	KindIntent      = "intent"
	KindPolicy      = "policy"
	KindDSS         = "dss"
)

// Effects of a conflict on the decision.
const (
	EffectRejects    = "rejects"
	EffectHolds      = "holds"
	EffectFlagsOther = "flags_other"
)

// Reasons: the stable snake_case code of every conflict and condition
// (E-09: each is also a counter, intent_<reason>).
const (
	ReasonOperatorSuspended    = "operator_suspended"
	ReasonOperatorRevoked      = "operator_revoked"
	ReasonUASSuspended         = "uas_suspended"
	ReasonUASRevoked           = "uas_revoked"
	ReasonPilotSuspended       = "pilot_suspended"
	ReasonPilotRevoked         = "pilot_revoked"
	ReasonRegistryUnknown      = "registry_unknown"
	ReasonRegistryUnavailable  = "registry_unavailable"
	ReasonRegistryRefused      = "registry_answer_refused"
	ReasonCISStale             = "cis_stale"
	ReasonCISOutdated          = "cis_outdated"
	ReasonCISUnavailable       = "cis_unavailable"
	ReasonCeilingExceeded      = "airspace_ceiling_exceeded"
	ReasonCeilingNotJudged     = "airspace_ceiling_not_judged"
	ReasonRequirementsUnread   = "airspace_requirements_unreadable"
	ReasonZoneProhibited       = "zone_prohibited"
	ReasonZoneUnknownType      = "zone_type_unknown"
	ReasonZoneReqAuthorisation = "zone_requires_authorisation"
	ReasonRestrictionActive    = "restriction_active"
	ReasonIntentPriority       = "intent_higher_priority"
	ReasonIntentFirstCome      = "intent_filed_first"
	ReasonIntentFlagged        = "intent_flagged_for_update"
	ReasonThresholdInvalid     = "threshold_invalid"
	ReasonDeconflictNotJudged  = "deconfliction_not_judged"
	ReasonDSSUnavailable       = "dss_unavailable"

	CondRegistryUnverified   = "registry_unverified"
	CondAuthorisationRef     = "authorisation_ref_required_zone"
	CondConditionalZone      = "zone_conditional"
	CondLimitNotJudged       = "zone_limit_not_judged"
	CondAirspaceRequirements = "uspace_airspace_requirements"
	CondLocalDeconfliction   = "local_deconfliction_only"
)

// Overlap is how a volume meets a zone, an airspace or another intent:
// horizontal separation, vertical overlap and common time; a member
// that could not be judged is null.
type Overlap struct {
	HM *float64 `json:"h_m"`
	VM *float64 `json:"v_m"`
	TS *float64 `json:"t_s"`
}

// Conflict is one entry of conflicts[]: what refused, held or was
// flagged, naming the Annex IV item when there is one, the zone, the
// airspace, the restriction, the intent or the registry key in Ref, our
// volume by index, and the overlap.
type Conflict struct {
	Kind    string   `json:"kind"`
	Reason  string   `json:"reason"`
	Effect  string   `json:"effect"`
	Ref     string   `json:"ref,omitempty"`
	Item    *int     `json:"item"`
	Volume  *int     `json:"volume"`
	Overlap *Overlap `json:"overlap"`
	Detail  string   `json:"detail"`
}

// Condition is one entry of conditions[]: a condition of an accepted
// intent (a CONDITIONAL zone's message, an authorisation reference the
// authority must hold, the airspace's Art. 3(4) requirements).
type Condition struct {
	Code   string `json:"code"`
	Ref    string `json:"ref,omitempty"`
	Detail string `json:"detail"`
}

// Thresholds are the deviation thresholds of Art. 10(2)(d).
type Thresholds struct {
	HM float64 `json:"h_m"`
	VM float64 `json:"v_m"`
	TS float64 `json:"t_s"`
}

// VolumeAMSL is one volume's band in AMSL derived through the geoid at
// its outline's centroid, beside the W84 values as given (D-01).
type VolumeAMSL struct {
	LowerAMSLM  float64 `json:"lower_amsl_m"`
	UpperAMSLM  float64 `json:"upper_amsl_m"`
	UndulationM float64 `json:"undulation_m"`
	LowerW84M   float64 `json:"lower_w84_m"`
	UpperW84M   float64 `json:"upper_w84_m"`
}

// Decision is intent/decision/v1: the answer to a request and the state
// of the intent.
type Decision struct {
	IntentID            string          `json:"intent_id"`
	Version             int             `json:"version"`
	ClientRef           string          `json:"client_ref"`
	Decision            string          `json:"decision"`
	State               string          `json:"state"`
	DSSState            *string         `json:"dss_state"`
	AuthorisationNumber *string         `json:"authorisation_number"`
	ExemptArt13         bool            `json:"exempt_art_1_3"`
	InUSpaceAirspace    bool            `json:"in_uspace_airspace"`
	USpaceAirspaceIDs   []string        `json:"uspace_airspace_ids"`
	Priority            int             `json:"priority"`
	DeviationThresholds *Thresholds     `json:"deviation_thresholds"`
	Alternative         json.RawMessage `json:"alternative"`
	Conflicts           []Conflict      `json:"conflicts"`
	Conditions          []Condition     `json:"conditions"`
	VolumesAMSL         []VolumeAMSL    `json:"volumes_amsl"`
	ValidFrom           time.Time       `json:"valid_from"`
	ValidTo             time.Time       `json:"valid_to"`
	CISVersionChecked   *string         `json:"cis_version_checked"`
	CISAgeS             *float64        `json:"cis_age_s"`
	RegistryCheckedAt   *time.Time      `json:"registry_checked_at"`
	PolicyVersion       int64           `json:"policy_version"`
	WeatherCheckedRef   *string         `json:"weather_checked_ref"`
	ChangeReason        *string         `json:"change_reason"`
	DecidedAt           time.Time       `json:"decided_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// nullJSON is the alternative of every decision: no proposal yet
// (Art. 10(4) is optional; brief WP-7: present and null).
var nullJSON = json.RawMessage("null")
