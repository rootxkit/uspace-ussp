// Package ridsp is this USSP's ASTM F3411-22a network identification
// Service Provider (docs/PLAN.md §3.1 rid-sp, §6.2; brief WP-9).
//
//   - Window (rid-sp): per flight, the last
//     f3411.NetMaxNearRealTimeDataPeriodSeconds (60 s) of this USSP's own
//     tracks from trk.v1 (trust authenticated, source operator_ws, a
//     flight id), at most policy rid_recent_positions_max_count samples a
//     flight, indexed by cell5 for view queries. Nothing older than 60 s
//     is ever served; no database is on the request path.
//   - Server (rid-sp): the generated strict F3411 USS server. GET
//     /uss/flights (rid.display_provider): the standard's view
//     lat1,lng1,lat2,lng2, a diagonal above NetMaxDisplayAreaDiagonalKm
//     answered 413 with the standard's ErrorResponse, every flight with a
//     position inside the view in the window, its RIDAircraftState mapped
//     from our track (Table 1's special values for what is unknown) and
//     recent_positions for the requested duration (at most 60 s). GET
//     /uss/flights/{id}/details (rid.display_provider only: the remote
//     pilot's position is personal data, spec 06 §5): uas_id, operator_id
//     (the public part of the registration number), operator_location,
//     operation_description (the authorisation number) and
//     eu_classification (Annex IV). POST
//     /uss/identification_service_areas/{id} (rid.service_provider): an
//     ISA notification from a peer Service Provider for our Display
//     Provider views, kept in the KV bucket rid_isa_notifications for
//     WP-14, 204.
//   - ISA (api): the Identification Service Area of every flight in the
//     DSS. api records the flight facts; in the same transaction it plans
//     the ISA (dss_isas) and queues the DSS write (dss_outbox isa_put or
//     isa_delete), so a fact and its DSS work commit together. The
//     ISAWorker works through those items with backoff: PUT
//     /rid/v2/dss/identification_service_areas/{id}[/{version}] with the
//     intent's volumes as extents (or, without an intent, a circle of
//     session_isa_radius_m around the flight's position reaching
//     session_isa_horizon_s ahead, renewed while the flight goes on),
//     DELETE with the version on the flight's end. With what the DSS
//     answered it records and, in the same transaction, queues one
//     dss_outbox isa_notify item per subscriber the DSS listed; a
//     separate loop with a total time budget POSTs them to
//     {url}/uss/identification_service_areas/{id}, so a subscriber that
//     does not answer never holds an ISA write. A put and a delete of
//     one ISA hold its advisory lock, so two api replicas never
//     interleave them. A write the DSS keeps refusing (a 4xx, an answer
//     that cannot be used, a version conflict) is given up after
//     DefaultMaxRefusals in a row, counted and reported on /readyz; a
//     DSS that does not answer is retried through any outage. The DSS being down never stops
//     GET /uss/flights; /readyz says dss down since T and the outbox
//     replays on recovery.
//   - Push (rid-sp, optional, USSP_AUTHORITY_PUSH=on): WS
//     /v1/authority/flights, 1 Hz RIDFlight frames of every airborne
//     flight to the authority (rid.display_provider), with a bounded ten
//     minute buffer while nobody is connected; what the buffer sheds is a
//     counted gap on the next status frame (02 F7). Off by default (D12);
//     the Service Provider path never depends on it.
//
// What we serve is what the operator sent, authenticated; nothing here
// fuses a broadcast into it. Wire shapes are uspace-core's f3411 types
// (generated from the pinned standard file), never written here.
package ridsp
