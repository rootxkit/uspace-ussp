// Package cis is this USSP's cache of the Common Information Service
// (spec 02 F3; docs/PLAN.md §3.2, §4, §5.1, §7; brief WP-4): it pulls the
// CISP's datasets, receives its signed change notifications, keeps the
// current version of each dataset in the database and in memory, and
// answers what the zones, the U-space airspaces and the restrictions say
// at a point, over an envelope or at an instant.
//
// The parts:
//
//   - Client: the CISP's F3 pull API (GET /v1/{dataset} with
//     If-None-Match, GET /v1/{dataset}/versions/{v}, GET /v1/changes,
//     POST /v1/subscriptions), generated from the pinned copy
//     api/clients/cisp.yaml (internal/cis/cispclient). Every call carries
//     an ecosystem token with cis.read whose aud is the CISP's host (M18),
//     has a deadline and reads at most MaxBodyBytes.
//   - Receiver: POST /v1/cis/notifications. A compact JWS verified by
//     uspace-core's CompactVerifier against the allow-listed issuers (the
//     CISP and, on its degraded direct path, the ANSP; M5) with aud = this
//     host (M19); the delivery id (jti) is remembered in the database, so
//     a replay is acknowledged 204 and counted, never acted on twice.
//   - Cache: the writer. A pull is accepted whole or refused whole
//     (ed318.Parse with ed269.DefaultLimits, never repaired, spec 06 T9);
//     a refusal keeps the previous version and is reported on /readyz.
//     A 60 s reconciliation pulls every dataset conditionally, so a
//     missed notification costs at most one period.
//   - Evaluator: what intent, geo and monitor call, in process. Every
//     judgement is uspace-core's: ed318.ToZones builds the zones,
//     ed318.Applies says when a zone applies, zones.Index,
//     Zone.ContainsHorizontally and zones.JudgeVertical say whether a
//     point is inside. This package maps the stored features onto those
//     inputs and never judges a second time (CLAUDE.md rule 3).
//   - Projection: the zone/applicable/v1 entries per cell5 for the KV
//     bucket cis_current, through the Projector interface (WP-6
//     implements it on internal/bus; MemoryProjector until then).
//
// Nothing here is hidden (CLAUDE.md rule 7): a dataset never loaded is
// "unknown (no version loaded)", an old one is stale with its age, a
// refused publication says its first problem, and every answer of the
// Evaluator carries the cis_version and cis_age_s it rests on.
//
// Applicability is judged with ed318.Applies over the feature's
// limitedApplicability at the centre of each part's bounding box (where
// ed318.ToZones resolves daylight events), not with the fixed windows
// ToZones builds: ED-318 allows daylight schedules without end dates,
// which ToZones refuses and Applies judges (the CISP publishes such a
// zone with a warning that consumers judge it with Applies). A zone
// whose applicability cannot be evaluated is returned as unknown, never
// dropped.
package cis
