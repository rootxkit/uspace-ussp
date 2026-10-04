package policy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/cpa"
	"github.com/rootxkit/uspace-core/rid"
)

// memStore is the Store contract in memory: a row is kept only when
// beforeCommit succeeds.
type memStore struct {
	rows []Record
	next int64
}

func (m *memStore) NewestPolicy(context.Context) (Record, error) {
	if len(m.rows) == 0 {
		return Record{}, ErrNoPolicy
	}
	return m.rows[len(m.rows)-1], nil
}

func (m *memStore) InsertPolicy(ctx context.Context, actor, reason string, v Values, beforeCommit func(context.Context, Record) error) (Record, error) {
	m.next++ // a sequence: taken even when the transaction rolls back
	r := Record{Version: m.next, CreatedAt: time.Unix(0, 0).UTC(), Actor: actor, Reason: reason, Values: v}
	if err := beforeCommit(ctx, r); err != nil {
		return Record{}, err
	}
	m.rows = append(m.rows, r)
	return r, nil
}

type projector struct {
	err  error
	seen []Record
}

func (p *projector) ProjectPolicy(_ context.Context, r Record) error {
	p.seen = append(p.seen, r)
	return p.err
}

func TestDefaultsValidateAndCarryCoreCPA(t *testing.T) {
	d := Defaults()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.CPA() != cpa.DefaultPolicy {
		t.Errorf("CPA %+v, core %+v", d.CPA(), cpa.DefaultPolicy)
	}
	if d.DeviationHM != 50 || d.DeviationVM != 15 || d.DeviationTS != 60 || d.TelemetryLostS != 5 || d.LostLinkS != 15 ||
		d.NonconformanceNearbyRadiusM != 2000 || d.TelemetryRetentionDays != 90 {
		t.Errorf("defaults differ from PLAN §15 Q6/Q18: %+v", d)
	}
}

// Every JSON name carries a unit (CLAUDE.md rule 11) and round-trips.
func TestValuesJSONNamesCarryUnits(t *testing.T) {
	raw, err := json.Marshal(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != reflect.TypeFor[Values]().NumField() {
		t.Fatalf("%d JSON fields for %d struct fields", len(m), reflect.TypeFor[Values]().NumField())
	}
	for k := range m {
		// _priority, _count, _code and _severity are dimensionless (an
		// ordinal, a number of things, an enumeration code, a severity);
		// every other name carries its unit.
		if !strings.HasSuffix(k, "_m") && !strings.HasSuffix(k, "_s") && !strings.HasSuffix(k, "_days") &&
			!strings.HasSuffix(k, "_priority") && !strings.HasSuffix(k, "_count") && !strings.HasSuffix(k, "_hz") &&
			!strings.HasSuffix(k, "_code") && !strings.HasSuffix(k, "_ms") && !strings.HasSuffix(k, "_severity") {
			t.Errorf("%s has no unit", k)
		}
	}
	var back Values
	if err := json.Unmarshal(raw, &back); err != nil || back != Defaults() {
		t.Errorf("round trip %+v %v", back, err)
	}
}

func TestValidateNamesEveryRefusedField(t *testing.T) {
	v := Defaults()
	v.CPAHorizontalMinM = 0
	v.CPAVerticalMinM = math.NaN()
	v.DeviationHM = math.Inf(1)
	v.CPATCPAMaxS = -1
	v.TelemetryRetentionDays = TelemetryRetentionFloorDays - 1
	v.AuditRetentionDays = 0
	v.RecordRetentionDays = 0
	v.OperatorTokenTTLS = MaxOperatorTokenTTLS + 1
	v.ClientSecretOverlapS = -1
	v.SpecialOperationPriority = 0
	v.IntentOpenMaxCount = 0
	v.DeconflictBufferM = -1
	v.DeconflictVerticalBufferM = math.NaN()
	v.ActivationLeadS = 0
	v.TelemetryRateHz = 0
	v.PressureFallbackAccuracyCode = 0
	err := v.Validate()
	var fields []string
	var j interface{ Unwrap() []error }
	if !errors.As(err, &j) {
		t.Fatalf("not joined: %v", err)
	}
	for _, e := range j.Unwrap() {
		var fe *core.FieldError
		if !errors.As(e, &fe) {
			t.Fatalf("not a field error: %v", e)
		}
		fields = append(fields, fe.Field)
	}
	want := "deviation_h_m,cpa_horizontal_min_m,cpa_vertical_min_m,activation_lead_s,telemetry_rate_hz,cpa_tcpa_max_s,deconflict_buffer_m," +
		"deconflict_vertical_buffer_m,telemetry_retention_days,record_retention_days,audit_retention_days,operator_token_ttl_s," +
		"special_operation_priority,intent_open_max_count,pressure_fallback_accuracy_code,client_secret_overlap_s"
	if strings.Join(fields, ",") != want {
		t.Errorf("fields %v, want %s", fields, want)
	}
	// The zero window and zero age core allows are accepted; the floor
	// itself is accepted.
	v = Defaults()
	v.CPATCPAMaxS, v.CPANeighbourMaxAgeS, v.TelemetryRetentionDays = 0, 0, TelemetryRetentionFloorDays
	v.OperatorTokenTTLS, v.ClientSecretOverlapS = MaxOperatorTokenTTLS, 0
	v.DeconflictBufferM, v.DeconflictVerticalBufferM, v.SpecialOperationPriority = 0, 0, 1
	v.PressureFallbackAccuracyCode = MaxPressureFallbackAccuracyCode
	if err := v.Validate(); err != nil {
		t.Errorf("boundary values refused: %v", err)
	}
}

// The telemetry defaults are uspace-core's altitude selection, and a
// flight never ends before it is telemetry_lost (E-01 pair).
func TestTelemetryDefaultsAndFlightEndAfterTelemetryLost(t *testing.T) {
	d := Defaults()
	if d.AltPolicy() != rid.DefaultAltPolicy() {
		t.Errorf("altitude policy %+v, core %+v", d.AltPolicy(), rid.DefaultAltPolicy())
	}
	if d.TelemetryRateHz != 2 || d.BacklogAfterS != 10 || d.FlightEndAfterS != 120 || d.IngestQueueS != 10 || d.TelemetryDedupeS != 600 {
		t.Errorf("telemetry defaults differ from the WP-8 brief: %+v", d)
	}
	d.FlightEndAfterS = d.TelemetryLostS
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "flight_end_after_s") {
		t.Errorf("a flight ending at telemetry_lost accepted: %v", err)
	}
	d.FlightEndAfterS = d.TelemetryLostS + 1
	if err := d.Validate(); err != nil {
		t.Errorf("a flight ending after telemetry_lost refused: %v", err)
	}
}

// The Service Provider bounds (WP-9): a recent-positions bound outside 1
// to its maximum, and a session ISA without a positive radius or
// horizon, are refused; the bounds themselves are accepted (E-01 pair).
func TestRIDServiceProviderBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*Values)
		field string
	}{
		{"no samples", func(v *Values) { v.RIDRecentPositionsMaxCount = 0 }, "rid_recent_positions_max_count"},
		{"too many samples", func(v *Values) { v.RIDRecentPositionsMaxCount = MaxRIDRecentPositionsMaxCount + 1 }, "rid_recent_positions_max_count"},
		{"no radius", func(v *Values) { v.SessionISARadiusM = 0 }, "session_isa_radius_m"},
		{"no horizon", func(v *Values) { v.SessionISAHorizonS = math.Inf(1) }, "session_isa_horizon_s"},
	} {
		v := Defaults()
		tc.set(&v)
		if err := v.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: accepted or not named: %v", tc.name, err)
		}
	}
	v := Defaults()
	if v.RIDRecentPositionsMaxCount != 120 || v.SessionISARadiusM != 2000 || v.SessionISAHorizonS != 3600 {
		t.Errorf("Service Provider defaults %d %v %v", v.RIDRecentPositionsMaxCount, v.SessionISARadiusM, v.SessionISAHorizonS)
	}
	for _, n := range []int{1, MaxRIDRecentPositionsMaxCount} {
		v.RIDRecentPositionsMaxCount = n
		if err := v.Validate(); err != nil {
			t.Errorf("bound %d refused: %v", n, err)
		}
	}
}

// TestDSSBounds: a negative or non-finite subscription margin and an
// exchange retention outside 1 to its maximum are refused, each naming
// its field; the bounds themselves (a zero margin, one day, the
// maximum) are accepted (E-01 pair).
func TestDSSBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*Values)
		field string
	}{
		{"negative margin", func(v *Values) { v.PeerSubscriptionMarginM = -1 }, "peer_subscription_margin_m"},
		{"infinite margin", func(v *Values) { v.PeerSubscriptionMarginM = math.Inf(1) }, "peer_subscription_margin_m"},
		{"no retention", func(v *Values) { v.DSSExchangeRetentionDays = 0 }, "dss_exchange_retention_days"},
		{"too long a retention", func(v *Values) { v.DSSExchangeRetentionDays = MaxDSSExchangeRetentionDays + 1 }, "dss_exchange_retention_days"},
	} {
		v := Defaults()
		tc.set(&v)
		if err := v.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: accepted or not named: %v", tc.name, err)
		}
	}
	v := Defaults()
	if v.PeerSubscriptionMarginM != 2000 || v.DSSExchangeRetentionDays != 7 {
		t.Errorf("DSS defaults %v %d", v.PeerSubscriptionMarginM, v.DSSExchangeRetentionDays)
	}
	for _, c := range []struct {
		m float64
		d int
	}{{0, 1}, {0, MaxDSSExchangeRetentionDays}} {
		v.PeerSubscriptionMarginM, v.DSSExchangeRetentionDays = c.m, c.d
		if err := v.Validate(); err != nil {
			t.Errorf("bounds %v %d refused: %v", c.m, c.d, err)
		}
	}
}

// TestPeerAndMannedBounds: a negative or non-finite manned margin, a
// zero or non-finite silence or peer window and a flight cap outside 1
// to its maximum are refused, each naming its field; the defaults and
// the bounds themselves are accepted (E-01 pair).
func TestPeerAndMannedBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*Values)
		field string
	}{
		{"negative margin", func(v *Values) { v.MannedMarginM = -1 }, "manned_margin_m"},
		{"NaN margin", func(v *Values) { v.MannedMarginM = math.NaN() }, "manned_margin_m"},
		{"no silence", func(v *Values) { v.MannedUnavailableS = 0 }, "manned_unavailable_s"},
		{"infinite silence", func(v *Values) { v.MannedUnavailableS = math.Inf(1) }, "manned_unavailable_s"},
		{"no peer window", func(v *Values) { v.PeerUnavailableS = 0 }, "peer_unavailable_s"},
		{"no flight cap", func(v *Values) { v.PeerFlightsMax = 0 }, "peer_flights_max_count"},
		{"too large a flight cap", func(v *Values) { v.PeerFlightsMax = MaxPeerFlightsMax + 1 }, "peer_flights_max_count"},
	} {
		v := Defaults()
		tc.set(&v)
		if err := v.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: accepted or not named: %v", tc.name, err)
		}
	}
	v := Defaults()
	if v.MannedMarginM != 5000 || v.MannedUnavailableS != 10 || v.PeerUnavailableS != 60 || v.PeerFlightsMax != 1000 {
		t.Errorf("peer and manned defaults %v %v %v %d", v.MannedMarginM, v.MannedUnavailableS, v.PeerUnavailableS, v.PeerFlightsMax)
	}
	for _, n := range []int{1, MaxPeerFlightsMax} {
		v.MannedMarginM, v.PeerFlightsMax = 0, n
		if err := v.Validate(); err != nil {
			t.Errorf("bounds 0 m, %d flights refused: %v", n, err)
		}
	}
}

// E-01 pair, B-09: a failing projection refuses the write with the
// 503-shaped error and leaves nothing; a succeeding one leaves one row
// that Current and Load return.
func TestPutRefusesWhenTheProjectionFailsAndKeepsWhenItSucceeds(t *testing.T) {
	ctx := context.Background()
	st := &memStore{}
	p := &projector{err: errors.New("nats: no responders")}
	s := New(st, p, nil)

	_, err := s.Put(ctx, "staff-1", "tighten", Defaults())
	var pe *ProjectionError
	if !errors.As(err, &pe) || pe.Bucket != BucketPolicy || pe.HTTPStatus() != 503 || pe.ProblemSlug() != "projection_unavailable" ||
		strings.Contains(pe.ProblemDetail(), "nats") || !errors.Is(err, p.err) {
		t.Fatalf("failing projector: %v", err)
	}
	if len(st.rows) != 0 || len(p.seen) != 1 || s.Counters().Get(CounterProjectionFailed) != 1 || s.Counters().Get(CounterPut) != 0 {
		t.Fatalf("rows %d seen %d counters %v", len(st.rows), len(p.seen), s.Counters().Snapshot())
	}
	if _, ok := s.Current(); ok {
		t.Fatal("a refused write became current")
	}
	if _, err := s.Load(ctx); !errors.Is(err, ErrNoPolicy) {
		t.Fatalf("load with no row: %v", err)
	}

	p.err = nil
	r, err := s.Put(ctx, "staff-1", "tighten", Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 || r.Version != 2 || s.Counters().Get(CounterPut) != 1 || p.seen[1] != r {
		t.Fatalf("rows %v record %+v", st.rows, r)
	}
	if cur, ok := s.Current(); !ok || cur != r {
		t.Fatalf("current %+v %v", cur, ok)
	}
	fresh := New(st, p, nil)
	if got, err := fresh.Load(ctx); err != nil || got != r {
		t.Fatalf("load %+v %v", got, err)
	}
}

func TestPutRefusesInvalidInputBeforeTheStore(t *testing.T) {
	st := &memStore{}
	p := &projector{}
	s := New(st, p, nil)
	bad := Defaults()
	bad.LostLinkS = 0
	_, err := s.Put(context.Background(), "", "", bad)
	for _, f := range []string{"lost_link_s", "actor", "reason"} {
		if err == nil || !strings.Contains(err.Error(), f) {
			t.Errorf("%s not named: %v", f, err)
		}
	}
	if st.next != 0 || len(p.seen) != 0 {
		t.Error("an invalid policy reached the store or the projection")
	}
}

func TestLoadRefusesAStoredPolicyThatDoesNotValidate(t *testing.T) {
	bad := Defaults()
	bad.CPAHorizontalMinM = 0
	st := &memStore{rows: []Record{{Version: 3, Values: bad}}}
	s := New(st, &projector{}, nil)
	if _, err := s.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "version 3") {
		t.Fatalf("load: %v", err)
	}
	if _, ok := s.Current(); ok {
		t.Fatal("an invalid stored policy became current")
	}
}

// The conformance defaults are core's lifecycle and pressure figures; a
// hysteresis no longer than twice the ahead tolerance is refused naming
// conformance_clear_after_s, and the first value above it is accepted
// (E-01 pair, E-15).
func TestConformanceDefaultsAndHysteresisBound(t *testing.T) {
	d := Defaults()
	if d.ConformanceClearAfterS != 3 || d.PressureUncertaintyM != 250 || d.MonitorLiveMaxAgeS != 10 {
		t.Fatalf("conformance defaults %v %v %v", d.ConformanceClearAfterS, d.PressureUncertaintyM, d.MonitorLiveMaxAgeS)
	}
	v := Defaults()
	v.ConformanceClearAfterS = 2 * v.TelemetryAheadToleranceS
	err := v.Validate()
	var fe *core.FieldError
	if !errors.As(err, &fe) || fe.Field != "conformance_clear_after_s" {
		t.Fatalf("hysteresis at twice the tolerance: %v", err)
	}
	v.ConformanceClearAfterS = 2*v.TelemetryAheadToleranceS + 0.001
	if err := v.Validate(); err != nil {
		t.Fatalf("hysteresis above twice the tolerance refused: %v", err)
	}
	for _, bad := range []func(*Values){
		func(v *Values) { v.MonitorLiveMaxAgeS = 0 },
		func(v *Values) { v.PressureUncertaintyM = -1 },
		func(v *Values) { v.ConformanceClearAfterS = math.NaN() },
	} {
		v := Defaults()
		bad(&v)
		if v.Validate() == nil {
			t.Errorf("%+v accepted", v)
		}
	}
	v = Defaults()
	v.PressureUncertaintyM = 0
	if err := v.Validate(); err != nil {
		t.Errorf("no pressure margin refused: %v", err)
	}
}

// The zone defaults are core's lifecycle figures and CONDITIONAL
// severity; the hysteresis outlasts twice the ahead tolerance, and a
// CONDITIONAL zone never raises critical (Z-10). The zones policy never
// carries a height limit (the 120 m rule is the authority's).
func TestZoneDefaultsAndBounds(t *testing.T) {
	d := Defaults()
	if d.ZoneClearAfterS != 3 || d.ZoneStaleAfterS != 15 || d.ZoneConditionalSeverity != "warning" {
		t.Fatalf("zone defaults %v %v %q", d.ZoneClearAfterS, d.ZoneStaleAfterS, d.ZoneConditionalSeverity)
	}
	if p := d.Zones(); p.MaxHeightAGLM != nil || p.ConditionalSeverity != core.SeverityWarning || p.PressureUncertaintyM != d.PressureUncertaintyM {
		t.Fatalf("zones policy %+v", p)
	}
	for _, sev := range []string{"info", "warning"} {
		v := Defaults()
		v.ZoneConditionalSeverity = sev
		if err := v.Validate(); err != nil {
			t.Errorf("%s refused: %v", sev, err)
		}
	}
	for name, bad := range map[string]func(*Values){
		"zone_conditional_severity": func(v *Values) { v.ZoneConditionalSeverity = "critical" },
		"zone_clear_after_s":        func(v *Values) { v.ZoneClearAfterS = 2 * v.TelemetryAheadToleranceS },
		"zone_stale_after_s":        func(v *Values) { v.ZoneStaleAfterS = 0 },
	} {
		v := Defaults()
		bad(&v)
		var fe *core.FieldError
		if err := v.Validate(); !errors.As(err, &fe) || fe.Field != name {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestRecordsBounds (WP-15): a non-positive or non-finite poll, escalation,
// record gap or airprox threshold, an escalation shorter than the poll and
// an operator-position retention outside 1 to its maximum are refused,
// each naming its field; the defaults and the bounds are accepted (E-01
// pair).
func TestRecordsBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*Values)
		field string
	}{
		{"no poll", func(v *Values) { v.ATSAckPollS = 0 }, "ats_ack_poll_s"},
		{"NaN escalation", func(v *Values) { v.ATSAckEscalateS = math.NaN() }, "ats_ack_escalate_s"},
		{"escalation before the first poll", func(v *Values) { v.ATSAckEscalateS = v.ATSAckPollS / 2 }, "ats_ack_escalate_s"},
		{"negative gap", func(v *Values) { v.RecordGapS = -1 }, "record_gap_s"},
		{"no airprox distance", func(v *Values) { v.AirproxReportM = 0 }, "airprox_report_m"},
		{"infinite airprox height", func(v *Values) { v.AirproxReportVM = math.Inf(1) }, "airprox_report_v_m"},
		{"no position retention", func(v *Values) { v.OperatorPositionRetentionDays = 0 }, "operator_position_retention_days"},
		{"too long a position retention", func(v *Values) { v.OperatorPositionRetentionDays = MaxOperatorPositionRetentionDays + 1 }, "operator_position_retention_days"},
	} {
		v := Defaults()
		tc.set(&v)
		if err := v.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: accepted or not named: %v", tc.name, err)
		}
	}
	v := Defaults()
	if v.ATSAckPollS != 10 || v.ATSAckEscalateS != 300 || v.RecordGapS != 3 || v.OperatorPositionRetentionDays != 90 ||
		v.AirproxReportM != v.CPAHorizontalMinM || v.AirproxReportVM != v.CPAVerticalMinM {
		t.Errorf("WP-15 defaults %+v", v)
	}
	v.ATSAckEscalateS, v.OperatorPositionRetentionDays = v.ATSAckPollS, MaxOperatorPositionRetentionDays
	if err := v.Validate(); err != nil {
		t.Errorf("bounds refused: %v", err)
	}
	v.OperatorPositionRetentionDays = 1
	if err := v.Validate(); err != nil {
		t.Errorf("one day refused: %v", err)
	}
}
