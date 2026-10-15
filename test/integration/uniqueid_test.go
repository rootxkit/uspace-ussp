package integration

import "testing"

// A clock just after a second boundary (the 00:43:04.003 run that
// panicked with slice bounds [-2:]) used to give an 11-character token.
func TestUniqueAtIsLongEnoughAtSecondBoundaries(t *testing.T) {
	for _, nano := range []int64{
		0, 1, 3_000_000, 9_999_999, 999_999_999,
		1_792_024_984_003_000_000, // 2026-10-15T00:43:04.003Z
		1_792_022_400_000_000_000, // 2026-10-15T00:00:00Z
	} {
		for _, n := range []int64{1, 9999} {
			u := uniqueAt(nano, n)
			if len(u) != 13 {
				t.Errorf("uniqueAt(%d, %d) = %q, want 13 characters", nano, n, u)
			}
			_ = u[len(u)-12:] // must not panic
		}
	}
	if a, b := uniqueAt(5, 1), uniqueAt(5, 2); a == b {
		t.Errorf("sequence did not distinguish %q and %q", a, b)
	}
}
