package pgstore

import (
	"errors"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Without a relational pool the counts are an error naming it, never
// zeros; an operator id that is no UUID is refused before any query.
// The counts on the database are test/integration's (the portal and
// console summaries).
func TestStateCountsWithoutADatabase(t *testing.T) {
	for _, p := range []Store{{}, {S: &store.Store{}}} {
		if _, err := p.StateCounts(t.Context(), nil, 10); !errors.Is(err, store.ErrNoPool) {
			t.Fatalf("%+v: %v", p, err)
		}
	}
	bad := "not-a-uuid"
	if _, err := (Store{S: &store.Store{}}).StateCounts(t.Context(), &bad, 10); err == nil {
		t.Fatal("an operator that is no UUID was counted")
	}
}
