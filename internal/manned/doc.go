// Package manned is this USSP's manned traffic inputs (docs/PLAN.md §4
// manned, brief WP-14): the ANSP's manned-traffic stream (spec 02 F4)
// and this USSP's own e-conspicuity receiver, both published on
// man.v1.<cell3>.<cell5>.<icao24> as track/manned/v1 for the CPA path,
// the traffic product and the record.
//
//   - ANSPStream reads GET /v1/manned-traffic/stream (a WebSocket of
//     envelope frames dispatched on schema, M12, M29) with an ecosystem
//     token of scope ansp.traffic (aud the ANSP's host) and mTLS per
//     USSP_MTLS_MODE, after a bootstrap from /v1/manned-traffic/snapshot.
//     A track/manned/v1 body is checked (unknown members ignored), placed
//     by uspace-core timeplace.PlaceNetwork against its own time and
//     republished with trust surveillance, source ansp_feed and the ANSP's
//     adapter as source_instance; a console/status/v1 body is the feed's
//     own status (an adapter the ANSP says is stale or switched off is
//     that on our src.v1, with the ANSP's time, never an empty sky); an
//     unknown schema is counted and skipped. It reconnects forever; after
//     policy manned_unavailable_s without a frame manned traffic is
//     unavailable since the last frame.
//   - Econspicuity reads a readsb/dump1090 aircraft.json over HTTP at
//     1 Hz, a BaseStation (SBS) stream, or a recorded aircraft.json
//     replay (one document per line, timestamps re-based), keeps only the
//     members the schema has, and publishes them with trust broadcast and
//     source adsb_rx (R-05: a broadcast is never authenticated), placed
//     at arrival with the feed's own age of the position subtracted
//     (T-12).
//
// Both follow the source switches (ansp_feed, adsb_rx, with their
// instances): a type switched off closes the stream or stops the polls
// at once and its tracks age out source_disabled at their consumers.
// Neither judges anything: the AMSL altitude and the CPA are the
// consumers' (internal/traffic over uspace-core). Before a record is
// published the echo guard (Own) recognises one of this USSP's own
// flights heard back, whose callsign or registration is the UA
// registration of an active own flight, and leaves it out, counted, so
// it is never judged as a second aircraft beside its own track (PLAN §15
// Q23). Nothing here has a send path towards an aircraft (CLAUDE.md
// rule 1).
package manned
