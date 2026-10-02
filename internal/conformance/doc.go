// Package conformance is the conformance monitoring judgement of 2021/664
// Art. 13(1) (docs/PLAN.md D2, §15 Q1; brief WP-10): one flight against
// its authorised volumes, deviation thresholds and time window, and the
// state machine that turns those judgements into the flight's
// conformance state and its alerts. It is one of the two judgements this
// repository owns because only a USSP makes it; it is pinned by
// testdata/vectors/conformance.json and proposed upstream.
//
// # The judgement
//
// Judge is pure and stateless. A sample is inside when one volume active
// at its captured_at holds it horizontally and vertically (closed sets:
// an outline or a limit itself is inside). Otherwise it is outside, with
// the reason of spec 03 §3 (outside_volume_h, above_upper, below_lower,
// before_start, after_end), distance_outside_m (horizontal distance to the
// nearest authorised outline), height_over_m (above the upper or below
// the lower AMSL limit) and time_outside_s, and within_threshold when
// every excess is within the deviation thresholds h_m, v_m and t_s
// (Art. 10(2)(d)). Separation and conformance are judged in AMSL (D-01):
// the volumes arrive as AMSL from WP-7 and nothing here converts.
//
// A sample without a usable altitude (alt_source none, or no AMSL value)
// is judged on the horizontal and the time window only, and the verdict
// says vertical_known false: it is never inside by default, it is
// undetermined. A pressure altitude is judged against the band widened by
// Policy.PressureUncertaintyM each way (R-09), and within_band is false
// when only the widened test passes (spec 04 §3.1). A threshold that is
// zero, negative or not finite refuses the judgement (E-15).
//
// # The state machine
//
// A Tracker follows one flight. It admits samples like uspace-core's
// alerting does (backlog, late, placed ahead, out of order, disabled
// source and invalid samples are counted and judge nothing; C-05: only a
// flying sample is judged) and keeps the alerting lifecycle's semantics:
//
//   - conforming -> nonconforming when outside persists for longer than
//     t_s, or at once when an excess is beyond its threshold
//     (threshold_exceeded); the nonconformance alert (critical) is raised
//     once and refreshed with the current numbers (C-06, C-08);
//   - nonconforming -> conforming once samples have shown the flight
//     inside for longer than ConformanceClearAfterS since it was last
//     outside (C-06); the alert clears resolved with the last numbers
//     that showed it outside and the numbers of the judgement that
//     cleared it (C-14). An undetermined sample neither refreshes nor
//     clears: nothing unjudged clears an alert (C-09);
//   - nonconforming -> contingent after F3548's
//     MaxRecoverableTimeInNonconformingStateSeconds (60 s) without
//     returning. Contingent is kept until the flight ends (F3548 has no
//     way back to Activated from Contingent);
//   - lost_link after LostLinkS of no live sample from a flight whose
//     last sample was flying: the lost_link alert (critical) is raised,
//     and the silence is non-conformance (telemetry_lost), so the 60 s to
//     contingent run from it. The next live sample clears lost_link; the
//     flight returns to conforming only after the hysteresis, counted
//     from that sample. Silence is never evidence that a deviation
//     ended (T-10);
//   - unknown while no judgement ran: no authorisation (intent_active has
//     no entry), no flying sample yet, or the source switched off. It is
//     never reported as conforming.
//
// Alerts clear only with a reason: resolved (shown inside), flight_ended
// (Drop) or source_disabled (the source was switched off, B-11).
//
// It does no I/O and keeps no clock: the caller passes the wall time on
// every call, publishes what a call returns, and calls Tick about once a
// second. A Tracker is not safe for concurrent use.
package conformance
