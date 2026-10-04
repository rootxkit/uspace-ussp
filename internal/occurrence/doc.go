// Package occurrence is this USSP's occurrence reporting to the
// authority (Reg. (EU) 376/2014 Art. 4; spec 02 F7, 04 §3.3; brief
// WP-15): a report within 72 h of the USSP becoming aware of an
// occurrence, queued and retried, every undelivered report listed for
// the console with the time to its deadline, and one past its deadline a
// critical console item.
//
// What is reported. Automatically (the Detector, a sweep of the alerts
// api records): an airprox when a proximity alert's closest approach is
// within the policy's airprox_report_m horizontally and
// airprox_report_v_m vertically (a vertical separation that was not
// judged counts as within: reported, never skipped, and the narrative
// says so); a nonconformance in a PROHIBITED zone (a zone incursion
// alert on a PROHIBITED zone); a lost link inside U-space airspace; a
// flight that declared an emergency. By a supervisor on the console
// (POST /v1/admin/occurrences): any alert of a flight, as reported.
// became_aware_at is the event's time and deadline_at 72 h after it. An
// event is reported once, whoever flags it first: an alert by its id, a
// proximity conflict by its pair (both flights' alerts are one report).
//
// What is sent. occurrence/v1 as spec 04 §3.3 names its fields. The
// authority owns that schema (M14) and has published neither it nor POST
// /v1/occurrences in the OpenAPI api/clients/authority.yaml pins (a spec
// gap, recorded in docs/RUNBOOKS/WP-15.md): no client is written by hand
// (CLAUDE.md rule 9), so without a Deliverer the reports stay queued,
// undelivered and visible, and /readyz says why. reporter.person_ref is
// an opaque reference (a staff account id, or "system") sent in clear
// over TLS (reconciliation M13). Registration numbers are their public
// part. Occurrence data is for safety only (376/2014 Art. 15(2)):
// nothing here links a report to a violation.
package occurrence
