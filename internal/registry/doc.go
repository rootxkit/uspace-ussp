// Package registry is this USSP's view of the authority's registry
// (spec 02 F8; docs/PLAN.md §4, §5.1, §7, §8; brief WP-5): the validity
// of an operator registration number, a UAS serial and a remote pilot,
// status only, cached, and the identification of our own flights over
// that cache.
//
// The parts:
//
//   - Client: the authority's F8 API (GET and POST /v1/registry/validate,
//     GET /v1/registry/changes), generated from the pinned copy
//     api/clients/authority.yaml (internal/registry/authclient). Every
//     call carries an ecosystem token with registry.validate whose aud is
//     the authority's host (M18), the purpose as the query parameter the
//     contract names, a deadline of DefaultTimeout and a body cap. An
//     answer is decoded strictly and checked against what was asked: a
//     field the contract does not name (a name, an address) or an echoed
//     secret part of a registration number is refused and counted
//     registry_pii_refused, so the USSP never stores a field it did not
//     ask for (CLAUDE.md rule 8, spec 06 §5).
//   - Cache: Validate answers from registry_validity within the policy's
//     TTL (24 h for an answer the registry holds, 5 min for unknown) with
//     cache_age_s, else asks the authority. When the authority cannot
//     answer, an uncached key is unknown with the reason
//     registry_unavailable, counted: never an error, never valid (the
//     caller decides what unknown means). Every write goes to the table
//     and to the KV projection registry_validity in one transaction
//     (G-08).
//   - Feed: polls GET /v1/registry/changes every 30 s and deletes the
//     answers a change names, from the table and from the projection, in
//     the transaction that moves the cursor. An answer whose fetch raced
//     an invalidation of its key is not written.
//   - Lookup: an identify.Lookup over our own fleet (the aircraft and
//     operators our clients are bound to) and the cached answers, for the
//     hot path over the projection and for api over the table. The
//     judgement is uspace-core's (identify.ResolveBound,
//     ResolveBroadcast, ResolveRemoteID); a missing projection, or a
//     fleet key without a fresh answer, resolves as
//     identify.Unavailable (registry_unavailable), never as unidentified
//     and never as registered (G-08, SC-22).
//
// Keys are compared the way uspace-core compares them: an operator
// number by regnum.CompareKey (its public part, case-insensitive; the
// secret part is never sent, stored or echoed, G-04) and a serial by
// serial.Normalize, with folded matches left to identify.Snapshot (G-05).
package registry
