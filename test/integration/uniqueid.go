// Package integration holds the suite that runs against real
// TimescaleDB, PostGIS and NATS (build tag integration).
package integration

import "fmt"

// uniqueAt builds the suite's unique token from a clock reading and a
// sequence number. The sub-second part is zero-padded to nine digits
// so the token is always at least 13 characters: tests slice the last
// 12 (or fewer) characters of it, and an unpadded value is short
// whenever the clock sits just after a second boundary.
func uniqueAt(unixNano int64, n int64) string {
	return fmt.Sprintf("%09d%04d", unixNano%1_000_000_000, n)
}
