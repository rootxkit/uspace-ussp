// Package records is this USSP's service record of every flight
// (Reg. (EU) 2021/664 Art. 15(1)(g); spec 02 F7; brief WP-15): what the
// authority reads on demand (GET /v1/records/flights/{id}, scope
// ussp.records) and in daily bundles (GET /v1/records/daily/{date}).
//
// A record is evidence (LESSONS B-13). Each of its sections is read on
// its own and says whether it could be: a section whose store cannot be
// read is {"state": "unavailable", "reason": ...}, never an empty list
// (an alert table that cannot be read is "alerts: unavailable", not "no
// alerts"). The telemetry is a summary (start, end, samples, box, the
// highest AMSL altitude) with every hole in it: a silence longer than
// the policy's record_gap_s, and every gap telemetry-ingest declared
// however short, each labelled with the cause that was recorded for it
// (a work-queue gap of src.v1, a tsdb-writer gap of the TRK stream) or
// "no recorded cause"; nothing is interpolated across a hole. Every
// number is the one that was shown or judged at the time, with the
// policy_version it was judged under, and the policy versions named are
// in the record with their values. A bounded section says how many
// items there were when it holds fewer (truncated), never thins
// silently.
//
// No names (spec 06 §5, CLAUDE.md rule 8): a record holds registration
// numbers by their public part only, serials, references, and none of
// the request's free text (a contingency or a lost-link procedure may
// name a person). The daily job builds the previous UTC day's bundle
// (every flight that started that day, one record per JSON line,
// gzip) into USSP_RECORDS_DIR with its SHA-256 in record_bundles; a day
// without a bundle after 02:00 UTC is degraded on /readyz (an alarm on
// this side; the authority's is its own).
package records
