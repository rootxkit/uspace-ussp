package store

import (
	"testing"
	"time"
)

// The operator positions are removed one time window per statement: the
// windows tile [from, cutoff) without gap or overlap, none is longer
// than the window, and a span past many windows is cut into as many
// (E-10).
func TestOperatorPositionWindows(t *testing.T) {
	cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	from := cutoff.Add(-10*24*time.Hour - 90*time.Minute)
	ws := operatorPositionWindows(from, cutoff, 24*time.Hour)
	if len(ws) != 11 {
		t.Fatalf("%d windows, want 11", len(ws))
	}
	at := from
	for i, w := range ws {
		if !w.lo.Equal(at) || !w.hi.After(w.lo) || w.hi.Sub(w.lo) > 24*time.Hour {
			t.Fatalf("window %d is [%v, %v) after %v", i, w.lo, w.hi, at)
		}
		at = w.hi
	}
	if !at.Equal(cutoff) {
		t.Fatalf("the windows end at %v, not the cutoff %v", at, cutoff)
	}
	if ws := operatorPositionWindows(cutoff, cutoff, time.Hour); len(ws) != 0 {
		t.Fatalf("nothing older than the cutoff, %d windows", len(ws))
	}
	if ws := operatorPositionWindows(cutoff.Add(-time.Minute), cutoff, 0); len(ws) != 1 || ws[0].hi.Sub(ws[0].lo) != time.Minute {
		t.Fatalf("a window of 0 takes the default: %v", ws)
	}
}
