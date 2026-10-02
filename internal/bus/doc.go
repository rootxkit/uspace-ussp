// Package bus is the NATS JetStream side of every process (docs/PLAN.md
// D6, §3.2, §7; spec 05 §2, §3, §5, §6):
//
//   - Conn: one connection per process with its own credentials, never
//     given up on (B-08): the process starts while NATS is down, says
//     "nats: down" on /readyz and reconnects in the background;
//   - Topology, Ensure, Verify, Maintainer: the streams TRK, MAN, PEER,
//     ALRT, CONF, IDENT, INTENT, CIS, TRAFFIC, INGEST and the KV buckets
//     cis_current, policy, source_control, registry_validity,
//     client_bindings, intent_active, created when missing by whichever
//     process gets there first and never changed in place; what differs
//     from this build is drift on /readyz;
//   - Subjects (Trk, Man, Alrt, ...; Parse): typed builders and parsers,
//     where a wrong token count is an error, never a panic;
//   - Envelope: the 04 §2 envelope every message embeds, with core.Times
//     conversion and the ULID msg_id;
//   - Publisher: core publish for trk, man, peer, src and ctl (the hot
//     path never waits), JetStream publish with the msg_id as dedupe id
//     for the durable subjects, each counted;
//   - Projector: the KV write behind every Projector interface of
//     WP-1/2/4/5, bounded in time, so a writer refuses with 503 when the
//     KV cannot take it (B-09);
//   - Follower: a bucket key followed by watch, push and a re-read every
//     300 s, read with its age; a missing bucket is "no value", never a
//     refusal (SC-22).
//
// TRK, MAN and PEER capture the core subjects the hot path publishes so
// tsdb-writer can read them durably; MAN and PEER are this package's
// addition to PLAN §7's table, which lists those subjects as core only.
package bus
