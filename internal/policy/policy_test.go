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
		// _priority and _count are dimensionless (an ordinal, a number of
		// things); every other name carries its unit.
		if !strings.HasSuffix(k, "_m") && !strings.HasSuffix(k, "_s") && !strings.HasSuffix(k, "_days") &&
			!strings.HasSuffix(k, "_priority") && !strings.HasSuffix(k, "_count") {
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
	want := "deviation_h_m,cpa_horizontal_min_m,cpa_vertical_min_m,activation_lead_s,cpa_tcpa_max_s,deconflict_buffer_m,deconflict_vertical_buffer_m," +
		"telemetry_retention_days,record_retention_days,audit_retention_days,operator_token_ttl_s,special_operation_priority,intent_open_max_count,client_secret_overlap_s"
	if strings.Join(fields, ",") != want {
		t.Errorf("fields %v, want %s", fields, want)
	}
	// The zero window and zero age core allows are accepted; the floor
	// itself is accepted.
	v = Defaults()
	v.CPATCPAMaxS, v.CPANeighbourMaxAgeS, v.TelemetryRetentionDays = 0, 0, TelemetryRetentionFloorDays
	v.OperatorTokenTTLS, v.ClientSecretOverlapS = MaxOperatorTokenTTLS, 0
	v.DeconflictBufferM, v.DeconflictVerticalBufferM, v.SpecialOperationPriority = 0, 0, 1
	if err := v.Validate(); err != nil {
		t.Errorf("boundary values refused: %v", err)
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
