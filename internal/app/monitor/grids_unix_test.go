//go:build linux || darwin

package monitor

import "testing"

// On linux and darwin the monitor maps USSP_GEOID_FILE and each tile of
// USSP_TERRAIN_DIR read-only, shared in the page cache with the other
// processes on the host. The twin for every other platform is
// grids_other_test.go.
func TestGridsLoadThroughTheMappedPath(t *testing.T) {
	checkGridLoad(t, true)
}
