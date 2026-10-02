//go:build integration

package integration

import "testing"

// TestPlantedFailure is planted to prove that a failing test fails the job.
func TestPlantedFailure(t *testing.T) {
	t.Fatal("planted failure: this test must fail the job")
}
