// Package geo is geo-awareness and the zone alerts of this USSP
// (docs/PLAN.md §4; brief WP-12; spec 01 §3 S2, 02 F5, 04 §3.3;
// 2021/664 Art. 9, 10(10)).
//
// The parts:
//
//   - Service: GET /v1/geo and GET /v1/geo/intents/{id}, answered from
//     the CIS cache of the api process (internal/cis.Evaluator), never
//     from the CISP in the request path: the U-space airspaces with
//     their Art. 3(4) requirements, the zones with their ED-318
//     properties verbatim, and the restrictions with their state, each
//     with updated_at, version and valid_from/to, and the cis_version,
//     cis_age_s and stale the answer rests on (Art. 9(1)-(2)).
//   - ZoneSet and ZoneSource: the zones the monitor judges, built from
//     the cis_current projection (internal/cis.Projection) by uspace-core
//     ed318.ToZones with their applicability periods, rebuilt within one
//     tick of a new projection (Z-12).
//   - Tracker: the zone and identification kinds of uspace-core's
//     alerting.Monitor for one monitor worker's flights. Core judges:
//     the containment, the applicability at the sample's placement
//     (T-09), the vertical limits in their own reference, the severity
//     per type (zones.Severity: PROHIBITED critical, REQ_AUTHORIZATION
//     warning, CONDITIONAL at the policy's info or warning, never above
//     the restriction, Z-10), a limit it cannot judge (SC-13), the
//     hysteresis, the stale, landed and source_disabled clears, and the
//     identification alerts (G-02, G-03). This package maps core's
//     raises and clears onto alert/v1 zone_incursion, identification and
//     identification_mismatch, and carries each active alert across a
//     rebuild of core's monitor (a new zone set, a new policy), a
//     restart and a handover, under its id, until core judges it again or
//     the evidence ends it. U-space airspace is information, not an
//     incursion: USPACE features are never judged here, and the 120 m
//     height limit is the authority's (zones.Policy.MaxHeightAGLM stays
//     nil).
//   - Rechecker: the standing re-check of Art. 10(10). Every installed
//     version of zones, uspace_airspace or restrictions (PLAN §15.1
//     Q20), and every constraint notification (WP-13), re-applies WP-7's
//     CIS steps to the active intents the changed features overlap
//     (intent.Service.Recheck): an accepted intent that now conflicts is
//     withdrawn, an activated one is marked for its operator; either way
//     the operator is told by restriction_activated and the decision's
//     change_reason. It never touches the aircraft: a withdrawal tells a
//     person (CLAUDE.md rule 1).
//   - Changes: the geo change push (geo/changed/v1) the traffic stream
//     forwards to its subscribers so they refetch /v1/geo.
//
// Nothing here judges a second time (CLAUDE.md rule 3): every
// containment, applicability, vertical and severity judgement is
// uspace-core's, reached through internal/cis or alerting.Monitor.
package geo
