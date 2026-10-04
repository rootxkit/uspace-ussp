//go:build !(linux || darwin)

package monitor

import "testing"

// Where core cannot map a file it reads it into memory, and /readyz says
// so. The twin for linux and darwin is grids_unix_test.go.
func TestGridsLoadThroughTheFallbackRead(t *testing.T) {
	checkGridLoad(t, false)
}
