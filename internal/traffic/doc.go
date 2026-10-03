// Package traffic is traffic information and the CPA proximity alert
// (docs/PLAN.md §4; brief WP-11; spec 01 §3 S4, 02 F5, 04 §3.3).
//
// It has two halves, one per process:
//
//   - The CPA path of monitor (Engine): every track of this USSP's
//     flights (trk.v1), of the peers (peer.v1) and of manned aircraft
//     (man.v1) in the owned cells and their ring-1 neighbours is mapped
//     onto an alerting.Track (TrackOf, the only mapping) and fed to one
//     uspace-core alerting.Monitor for the owned cell set. Core judges:
//     the loss of separation over the whole window, the hysteresis, the
//     stale, landed and source_disabled clears, the bound on aircraft
//     that never clears an alert (C-18) and the per-source share, and it
//     ranks the active conflicts by the time to loss of separation. This
//     package only maps: a raised or cleared conflict becomes one
//     alert/v1 proximity message for each of this USSP's flights in the
//     pair, naming the other aircraft (peer {track_id, trust}), and every
//     active one is republished each tick with its current numbers
//     (C-08). Nothing here judges a pair a second time (CLAUDE.md rule 3).
//   - The product of traffic-ws (Picture, Product): the latest sample of
//     every track with its trust, source and age, aged out as stale or
//     source_disabled and never removed silently (B-11, SC-16), the
//     active proximity alerts of the subscriber and the inputs that are
//     degraded, assembled at 1 Hz per subscriber.
//
// Persistence (Engine.Store): core's monitor keeps its state in memory
// and v1.3.0 has no way to restore it, so each active proximity alert is
// saved in the KV bucket proximity_state (its raise time, its aircraft
// and its last numbers) when it is raised and at least every heartbeat,
// and removed when it clears. A monitor that starts, or that takes over
// cells from another instance, carries every saved alert it now owns as
// active: it is republished under its own alert ids until core raises
// the same pair again (the alert continues, with the same ids and no
// second raise), or until the evidence ends it: an aircraft of the pair
// ended its flight (flight_ended), had its source switched off
// (source_disabled), was not heard for the stale time (stale), or both
// aircraft were heard for longer than the hysteresis without core
// raising the pair (not_reconfirmed). A restart therefore never clears a
// live alert silently, and an alert it ends is raised again by core the
// moment the condition holds.
//
// Nothing here has a send path towards an aircraft (CLAUDE.md rule 1),
// and no message carries resolution advice (rule 2, X-15): a proximity
// alert carries numbers and the other aircraft's identity, nothing else.
package traffic
