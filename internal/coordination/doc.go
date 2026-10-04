// Package coordination is this USSP's half of the Annex V coordination
// with the ANSP (Reg. (EU) 2021/664 Art. 13(2), Annex V; spec 02 F13,
// 04 §3.5; cross-plan M2; brief WP-15).
//
// What is sent. Every notice is the ANSP's own schema
// coordination/annex_v/v1 (M14), built here with the generated types of
// internal/coordination/anspclient and pinned by the examples under
// schemas/examples/consumed/:
//
//   - intent_notice when an intent whose volumes touch a U-space airspace
//     in controlled airspace (in_controlled_airspace of cis_current) is
//     activated, and ended when it ends. An airspace whose requirement
//     block does not say, or that the CIS no longer holds, counts as
//     controlled: the ANSP is told rather than not, and the remarks say
//     why;
//   - nonconformance on every transition of a flight into nonconforming
//     or lost_link, contingent on every transition into contingent, with
//     the conformance numbers and the flight's last position.
//
// How. The Notifier reads the database (the conformance timeline that
// api records from conf.v1, the intents that api decides) every second
// and queues each notice in the transaction that decides it
// (coordination_notices, the outbox): a restart neither repeats nor
// loses one. The Sender posts what is queued after its commit, one
// intent's notices in order, outside any transaction, with a bounded
// number of tries and backoff; a 409 (notice_ref reused with another
// body) and every 4xx the ANSP will refuse again fail the notice for
// good. The ANSP's receipt (202, or 200 for a repeat) is stored with
// ats_notified_at on the conformance state; a notice that needs a
// person's acknowledgement is then read back every ats_ack_poll_s, the
// acknowledgement stored as ats_ack_ref, and a notice still
// unacknowledged after ats_ack_escalate_s is escalated on the console
// with its age. Nothing is hidden: every undelivered, failed and
// escalated notice is listed for the console (Open), summarised on
// /readyz (Probe) and counted.
//
// The package never commands an aircraft and never advises one: a
// notice informs a person at the ANSP (CLAUDE.md rules 1, 2).
package coordination
