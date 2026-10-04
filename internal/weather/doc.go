// Package weather is the optional weather information service of
// Reg. (EU) 2021/664 Art. 12 (brief WP-16; PLAN §1 S6, §15 Q12): METAR,
// SPECI and TAF from a configured source (USSP_WEATHER_SOURCE), parsed
// from their report groups into the Art. 12(2) minimum content with the
// unit in every name (wind_speed_ms, gust_ms, cloud_base_ft_agl,
// visibility_m, temp_c, dew_point_c, qnh_hpa), stored in
// weather_products, answered on GET /v1/weather, and consulted, never
// judged, by a flight authorisation (Art. 10(3), weather_checked_ref).
//
// Everything about the source is bounded and fails closed: one fetch
// has a deadline and a byte limit, a report that does not parse whole
// is refused and counted (never stored in part), a report whose station
// or time disagrees with the source's envelope is refused, and a fetch
// that fails stores nothing. Nothing is hidden (E-02): no source
// configured answers 503 weather_unavailable with reason
// not_configured; a failing source answers its last products with
// stale true and the failure time; the source's state is kept in
// weather_source_status, on the database clock, so it survives a
// restart.
//
// Weather never rejects an intent and never commands anything: an
// advisory is a condition the operator reads (CLAUDE.md rule 2).
package weather
