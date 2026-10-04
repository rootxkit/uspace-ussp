//go:build !(linux || darwin)

package api

import "testing"

// Where core cannot map a file it reads it into memory, and /readyz says
// so. The twin for linux and darwin is geoid_unix_test.go.
func TestGeoidLoadsThroughTheFallbackRead(t *testing.T) {
	checkGeoidLoad(t, false)
}
