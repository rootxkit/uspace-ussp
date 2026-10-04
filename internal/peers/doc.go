// Package peers is this USSP's Display Provider towards the peer USSPs
// (docs/PLAN.md §4 peers, brief WP-14; spec 02 F6, F7): their flights,
// discovered through the DSS and polled from them, published on
// peer.v1.<cell3>.<cell5>.<rid_flight_id> as track/telemetry/v1 with
// trust provider for the CPA path, the traffic product and the record
// (peer_flights, 24 h).
//
//   - Areas of interest are the U-space airspaces (or the configured
//     box), cut into views of at most F3411's 7 km diagonal (Tile).
//   - Discovery: one DSS subscription per area for ISAs (PUT
//     /rid/v2/dss/subscriptions/{id}, at most 24 h, renewed at 80 %;
//     NetDSSMaxSubscriptionPerArea is ten and one is used), an ISA
//     search per view at the start and every SearchEvery, and the ISA
//     notifications the DSS's subscribers send, which rid-sp keeps in
//     rid_isa_notifications (WP-9's receiver) and this package reads.
//     An ISA whose uss_base_url is this USSP's own is never polled:
//     peers are discovered, never configured (spec 00 §7).
//   - Polling: one poller per peer and view, GET {uss_base_url}/uss/
//     flights?view= at 1 Hz with a token of scope rid.display_provider
//     (aud the peer's host), one request in flight, a deadline of
//     NetSpDataResponseTime99thPercentileSeconds (3 s). A peer that
//     misses F3411's 95th percentile (1 s) more than once in its last 20
//     answers is slow and polled at 0.5 Hz, said on its status. The
//     answer is read through uspace-core's bounded unmarshal; one with
//     more flights than policy peer_flights_max_count is refused whole
//     and counted, never shown in part as if complete.
//   - Each flight's current state is placed by uspace-core
//     timeplace.PlaceNetwork against the answer's timestamp (older than
//     60 s: not shown, counted), its AMSL altitude chosen by
//     rid.SelectAltitude through the geoid, its identification resolved
//     by ResolveBroadcast on the broadcast basis (a peer's claim about
//     its operator is not ours to vouch for, R-14), and published with
//     trust provider, source network_rid and the peer's base URL as
//     source_instance; never upgraded to authenticated.
//   - A peer that does not answer is down since its first failure: its
//     src.v1 status says so, and traffic-ws marks its flights
//     peer_unavailable for policy peer_unavailable_s, then they age out.
//   - The network_rid switch (type or a peer's base URL as instance)
//     stops the polls at once (SC-16) and they resume when it is on.
//   - Details (Details) are fetched only for a view of at most 2 km, on
//     request, never on the 1 Hz path.
//
// The echo guard (Own) leaves out a flight that is one of this USSP's
// own (its RID flight id is the flight id of one of ours), counted, so
// it is never judged as a second aircraft beside its own track (PLAN
// §15 Q23). Nothing here has a send path towards an aircraft.
package peers
