// Package interuss holds the code generated from the pinned InterUSS
// automated-testing interface files of api/standards/interuss/ (docs/PLAN.md
// D13, §6.4; brief WP-21), one subpackage per interface:
//
//   - ridinjection: Remote ID test data injection (rid/v1/injection.yaml
//     and commons.yaml), types only: the file declares its path parameters
//     "in: query", from which no server can be generated, so
//     internal/testif routes PUT /tests/{test_id} and DELETE
//     /tests/{test_id}/{version} itself with these types;
//   - ridobservation: Remote ID display data observation
//     (rid/v1/observation.yaml), types and the strict server;
//   - flightplanning: flight_planning v1 (flight_planning/v1/*.yaml),
//     types and the strict server.
//
// Only the test-interfaces process (cmd/test-interfaces, internal/testif)
// imports them; no production process serves an InterUSS interface. The
// files are InterUSS's contract, not a national one, and are not in
// api/openapi.yaml. scd v1 is not pinned and not generated (SOURCE).
// Every *.gen.go comes from scripts/generate-interuss.sh; never edit one.
package interuss
