// Package testif is the test-interfaces process's logic (docs/PLAN.md D13,
// §6.4; brief WP-21): the InterUSS automated-testing interfaces
// uss_qualifier drives (RID injection, RID observation, flight_planning
// v1), served from the types and servers generated from the pinned files
// (internal/stdapi/interuss), each a client of this USSP's published API
// and nothing else:
//
//   - an injected flight becomes telemetry/v1 frames on WS /v1/telemetry,
//     sent at the injected timestamps, with the test operator's client
//     token, its serial bound through the portal's binding route;
//   - a flight plan becomes an intent/request/v1 on POST /v1/intents and
//     PATCH /v1/intents/{id}; the intent service's decision is the plan's
//     result, mapped by a table (plan.go);
//   - an observation is GET /v1/traffic/snapshot with a staff session.
//
// It imports no safety package (telemetry, flights, ridsp, intent, dss,
// conformance, traffic), no store and no bus (scripts/check-deps.sh), and
// opens no database and no NATS connection. It never answers for a
// judgement: a refusal on the real path is the interface's failure with
// the USSP's reason, never a retry past it or a rewritten sample.
//
// It is never in production: a separate image (uspace-ussp-testif), not
// in the production compose, refused at start without
// USSP_TEST_INTERFACES=on and a USSP_SYSTEM_ID containing TEST or DEV
// (Gate), and its scopes (Scopes) are outside the catalogue. Nothing here
// can reach an aircraft: it speaks only to this USSP, as an operator
// client does.
package testif
