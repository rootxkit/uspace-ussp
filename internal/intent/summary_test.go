package intent

import (
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"
)

// seedStates puts one intent of the operator in each state given, the
// later ones newer.
func seedStates(st *memStore, operatorID string, from int, states ...string) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, state := range states {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", from+i)
		st.byID[id] = &Record{ID: id, OperatorID: operatorID, ClientID: testClient, LocalState: state,
			CreatedAt: base.Add(time.Duration(from+i) * time.Second), Decision: Decision{IntentID: id, State: state}}
	}
}

// The summary counts the operator's intents by state, every state
// present and zero when none, on the store's clock, and leaves another
// operator's out; every operator's counts both. With none at all every
// state is zero and the total is zero (E-01 pair).
func TestSummaryCountsByState(t *testing.T) {
	s, st, _ := newService(newRig())
	own := st.owners[testClient].OperatorID
	const other = "00000000-0000-4000-8000-000000000002"

	empty, err := s.SummaryOperator(t.Context(), own)
	if err != nil || empty.Total != 0 || empty.Truncated || len(empty.Counts) != len(States) || !empty.AsOf.Equal(testNow) {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	for _, state := range States {
		if n, ok := empty.Counts[state]; !ok || n != 0 {
			t.Fatalf("empty: %s is %d, present %v", state, n, ok)
		}
	}

	seedStates(st, own, 0, StateActivated, StateActivated, StateAccepted, StatePendingDSS, StateNonconforming, StateEnded, StateRejected, StateWithdrawn, StateWithdrawn)
	seedStates(st, other, 100, StateActivated, StateContingent, StateEnded)
	got, err := s.SummaryOperator(t.Context(), own)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{StatePendingValidation: 0, StatePendingDSS: 1, StatePendingAuthority: 0, StateAccepted: 1, StateActivated: 2,
		StateNonconforming: 1, StateContingent: 0, StateEnded: 1, StateRejected: 1, StateWithdrawn: 2}
	if !maps.Equal(got.Counts, want) || got.Total != 9 || got.Truncated {
		t.Fatalf("own: %+v", got)
	}
	all, err := s.SummaryAll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want[StateActivated], want[StateContingent], want[StateEnded] = 3, 1, 2
	if !maps.Equal(all.Counts, want) || all.Total != 12 || all.Truncated {
		t.Fatalf("all: %+v", all)
	}
	if o, err := s.SummaryOperator(t.Context(), other); err != nil || o.Total != 3 || o.Counts[StateContingent] != 1 {
		t.Fatalf("other: %+v %v", o, err)
	}
}

// Each count is bounded (E-10): one intent more than MaxSummaryRows
// truncates the summary and the counts stay at the bound read; at the
// bound exactly it is not truncated (E-01 pair).
func TestSummaryIsBounded(t *testing.T) {
	s, st, _ := newService(newRig())
	own := st.owners[testClient].OperatorID
	states := make([]string, MaxSummaryRows)
	for i := range states {
		states[i] = StateEnded
	}
	seedStates(st, own, 0, states...)
	at, err := s.SummaryOperator(t.Context(), own)
	if err != nil || at.Truncated || at.Counts[StateEnded] != MaxSummaryRows || at.Total != MaxSummaryRows {
		t.Fatalf("at the bound: truncated %v, ended %d, total %d, %v", at.Truncated, at.Counts[StateEnded], at.Total, err)
	}
	seedStates(st, own, MaxSummaryRows, StateEnded)
	over, err := s.SummaryOperator(t.Context(), own)
	if err != nil || !over.Truncated || over.Counts[StateEnded] != MaxSummaryRows+1 {
		t.Fatalf("over the bound: truncated %v, ended %d, %v", over.Truncated, over.Counts[StateEnded], err)
	}
}

// No operator is refused rather than answered with every operator's;
// a store that cannot count is 503 (E-02).
func TestSummaryRefusals(t *testing.T) {
	s, st, _ := newService(newRig())
	if _, err := s.SummaryOperator(t.Context(), ""); err == nil {
		t.Fatal("an empty operator was counted")
	}
	if _, err := s.SummaryOperator(t.Context(), st.owners[testClient].OperatorID); err != nil {
		t.Fatal(err)
	}
	st.failList = errors.New("database down")
	for _, f := range []func() (CountSummary, error){
		func() (CountSummary, error) { return s.SummaryOperator(t.Context(), st.owners[testClient].OperatorID) },
		func() (CountSummary, error) { return s.SummaryAll(t.Context()) },
	} {
		_, err := f()
		var ue *UnavailableError
		if !errors.As(err, &ue) || ue.HTTPStatus() != 503 {
			t.Fatalf("a failed count: %v", err)
		}
	}
}
