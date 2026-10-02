// Package flights is the life of a flight (docs/PLAN.md §4, §5.1
// flights; brief WP-8): which flight a sample of an aircraft belongs to,
// when the flight is telemetry_lost, when it ends, and the flights row.
//
//   - Binder (telemetry-ingest): one flight per aircraft of a client at a
//     time. A sample that flies an activated intent binds to that
//     intent's flight; a sample without one (outside U-space airspace,
//     where internal/telemetry lets it through) to the aircraft's flight
//     without an intent. A sample of another intent than the running
//     flight's ends that flight (intent_ended) and starts the next. The
//     flight id is a version 4 UUID, which is also the track id
//     (track_id = flight_id). Tick, once a second: policy
//     telemetry_lost_s since the newest live sample's captured_at
//     (capped at now) marks the flight telemetry_lost, which is not an
//     end (the next live sample resumes it); flight_end_after_s without
//     any sample ends it (end_reason telemetry_lost); an intent that left
//     intent_active (ended, withdrawn) ends its flight (intent_ended); the
//     operator's end sample ends it (operator_ended). Every fact is a
//     flight/event/v1 message on flight.v1.<event>.<flight_id> (JetStream,
//     FLIGHT): started, telemetry_lost, telemetry_resumed, ended.
//   - Recorder (api): consumes FLIGHT and records each fact in the
//     flights table with its audit row in one transaction (pgstore),
//     then acknowledges it (B-05); a fact that cannot be recorded stays
//     in the stream and is retried. Every event carries the whole flight,
//     so the first one to arrive creates the row and an ended flight is
//     never reopened.
//
// The intent's flight_id in intent_active is not written here: every
// track carries both flight_id and intent_id, and the flights row joins
// them for the record (a deviation from the WP-8 brief, stated in the
// pull request).
package flights
