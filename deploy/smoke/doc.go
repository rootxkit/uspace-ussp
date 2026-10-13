// Package smoke is the staging smoke's Go half (deploy/smoke/run.sh, CI
// job `staging-smoke`; WP-20). It has no code outside its tests, which do
// nothing unless run.sh asks for them:
//
//   - TestStagingSmokePeers (STAGING_SMOKE_PEERS=1) runs this USSP's peers
//     for the length of the smoke: the fakes of internal/testfakes for the
//     authority (token service, JWKS, F8 registry, status notices), the
//     CISP (one PROHIBITED zone, no U-space airspace, no restriction) and
//     the ANSP, and internal/dss/fakedss for the DSS, each behind a proxy
//     on an address the stack's containers reach; it writes the settings
//     that point the stack at them, and serves a control listener on the
//     loopback (mark a registration valid, stop).
//   - TestStagingSmoke (STAGING_SMOKE=1) is a simulated operator through
//     the public host and the edge Caddy: it registers, signs in, makes a
//     client and binds a serial, takes a token, files and activates an
//     intent, streams telemetry over the operator WebSocket until every
//     sample is acknowledged, and leaves what it made in a state file; with
//     STAGING_SMOKE_PHASE=after-restart it reads that back after the stack
//     was restarted.
//
// Test-only: nothing here ships in an image, and the simulated operator
// has no send path to an aircraft (it only sends to this USSP).
package smoke
