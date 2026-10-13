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
// What is sent. occurrence/v1 as the authority owns it (M14): the
// OccurrenceReport of the pinned api/clients/authority.yaml, posted to
// its POST /v1/occurrences through the client generated from that copy
// (Client; scope occurrences.write, aud the authority's host). The
// queued body names the class category; one queued before the contract
// was pinned names it kind and is mapped (WireOf), a function of the
// bytes, so every try of a report is the same report. Delivery runs
// after the report's commit, from the queue (state, attempts, next_at,
// the lease of a claim), so a restart resumes it. The authority is
// idempotent on (token sub, report_ref): a try after an answer that was
// lost, or a 5xx it answered after its commit, is its replay (200, the
// first id), never a refusal. Retried: a timeout, 408, 429, 5xx, at most
// USSP_OCCURRENCE_MAX_ATTEMPTS tries with a doubling wait up to
// USSP_OCCURRENCE_BACKOFF_MAX_S (defaults pending GCAA). Permanent: 409
// report_ref_conflict (failed on the first answer, counted as a
// conflict, an alarm), another 4xx, and a body that does not map
// (refused before a token is asked for). Without an authority
// configured nothing is claimed: the reports stay queued, undelivered
// and visible, and /readyz says why. reporter.person_ref is
// an opaque reference (a staff account id, or "system") sent in clear
// over TLS (reconciliation M13). Registration numbers are their public
// part. Occurrence data is for safety only (376/2014 Art. 15(2)):
// nothing here links a report to a violation.
package occurrence
