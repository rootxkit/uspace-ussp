// Package dss is this USSP's F3548-21 strategic coordination through the
// InterUSS DSS and with the peer USSPs (brief WP-13; docs/PLAN.md §6.2,
// §9; spec 02 F6).
//
// Client is every DSS and USS-to-USS call of PLAN §6.2 over the
// generated client of internal/stdapi/f3548: an ecosystem token for the
// scope the standard names with aud = the host of the target (the DSS of
// USSP_DSS_BASE_URL, a peer's uss_base_url; M18), a deadline, an answer
// bounded to f3548.MaxMessageBytes, a retry with backoff on a 5xx, a 429
// or no answer (a write only on a 429 or a 503, the answers that say it
// was not made), never on a 4xx; a 409 AirspaceConflictResponse is a
// *ConflictError naming the references whose ovns the key lacked.
//
// Writer mirrors this USSP's intents in the DSS. Every version of an
// intent the DSS must hold (inside U-space airspace, or anywhere with
// USSP_DSS_FOR_ALL=on) is an outbox item queued in the transaction that
// wrote it (internal/intent queuingTx). The writer takes the item, holds
// the intent's lock (store.LockClassOIR) and makes the DSS agree with the
// intent as it is then: for a pending_dss intent it queries the
// operational intent references and the constraint references of the
// extents, fetches each peer's details from its manager (trust provider,
// stored in peer_intents; a manager that does not answer marks its intents
// peer_unavailable and their stored copies are used), judges the intent
// with intent.PeerCheck (WP-7's deconfliction), PUTs it Accepted with the
// key of every ovn seen and an implicit subscription, and on a 409 fetches
// what the DSS names, judges again and writes once more; then
// intent.DSSAuthorise records what the DSS holds and authorises the
// intent (never before: a pending_dss intent is authorised only by a DSS
// answer), and the subscribers the DSS listed are told through the outbox
// within UssOiChangeNotificationMaxSeconds. A peer this intent displaced
// (a higher priority) is told inline within 900 ms first (PLAN §15 Q16),
// else counted peer_notify_late. Activation, nonconformance, contingency
// and the end are the same PUT with the new state, or a DELETE at the ovn
// held. The state written is only ever one of f3548.DSSStates.
//
// Subscriptions keeps one DSS subscription per U-space airspace of
// cis_current (its box widened by policy.PeerSubscriptionMarginM),
// renewed at 80 % of its window; Availability reads this USSP's
// availability every 60 s (Down stops new writes); Purger deletes peer
// data older than ExternalDataMaxRetentionTimeHours that no decision
// names; Server is the F3548 USS endpoints api serves; ExchangeLog keeps
// every DSS and peer exchange for GET /uss/v1/log_sets. Probe and Merge
// make the readiness entry dss.
//
// The DSS is a broker, not an authority: a peer's intent is never altered
// and nothing is authorised over it. Nothing here sends anything to an
// aircraft: a uss_base_url is a USS, and the only calls to it are the
// standard's.
package dss
