package conformance

import (
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
)

// Neighbour is a flight in the worker's track table, as the nearby
// fan-out reads it.
type Neighbour struct {
	FlightID            string
	IntentID            string
	AuthorisationNumber string
	Cell5               string
	Position            core.LatLon
	// SeenAt is the placement of its last sample; Flying its state then.
	SeenAt time.Time
	Flying bool
}

// MaxNearbyPerSource bounds the nearby alerts one deviating aircraft
// raises (E-10): the nearest first; the rest are counted.
const MaxNearbyPerSource = 256

// Counters of the fan-out.
const (
	CounterNearbyRaised         = "conformance_nearby_raised"
	CounterNearbyOverBound      = "conformance_nearby_over_bound"
	CounterNearbyUnmeasured     = "conformance_nearby_not_measured"
	CounterNearbyNeighbourStale = "conformance_nearby_neighbour_stale"
)

// Nearby is the nonconformance_nearby fan-out of Art. 13(2): while a
// source alert (nonconformance or lost_link) is active, every other
// flying flight within the radius of the deviating aircraft's last
// position gets one nonconformance_nearby alert (warning), raised once
// and kept until the source alert clears, which clears it with the same
// reason. A neighbour that ends loses its alerts as flight_ended. Not
// safe for concurrent use: the worker owns it.
type Nearby struct {
	Counters *core.Counters
	// bySource maps a source alert's id to the nearby alerts it raised,
	// by neighbour flight.
	bySource map[string]map[string]*Alert
}

func (n *Nearby) counters() *core.Counters {
	if n.Counters == nil {
		n.Counters = &core.Counters{}
	}
	return n.Counters
}

// Refresh raises nonconformance_nearby for every neighbour newly within
// radiusM of the source's last position and seen within maxAge of wall,
// and refreshes the detail of those already raised. src is the active
// source alert; pos its aircraft's last position.
func (n *Nearby) Refresh(src Alert, pos core.LatLon, neighbours []Neighbour, radiusM float64, maxAge time.Duration,
	wall time.Time, policyVersion int64) []AlertEvent {
	if n.bySource == nil {
		n.bySource = map[string]map[string]*Alert{}
	}
	if !core.IsFinite(radiusM) || radiusM <= 0 || !pos.Valid() {
		n.counters().Inc(CounterNearbyUnmeasured)
		return nil
	}
	type cand struct {
		nb Neighbour
		d  float64
	}
	var cands []cand
	for _, nb := range neighbours {
		if nb.FlightID == src.FlightID || !nb.Flying {
			continue
		}
		if wall.Sub(nb.SeenAt) > maxAge {
			n.counters().Inc(CounterNearbyNeighbourStale)
			continue
		}
		d, err := geodesy.DistanceM(pos, nb.Position)
		if err != nil || !core.IsFinite(d) {
			n.counters().Inc(CounterNearbyUnmeasured)
			continue
		}
		if d <= radiusM {
			cands = append(cands, cand{nb, d})
		}
	}
	slices.SortFunc(cands, func(a, b cand) int {
		switch {
		case a.d < b.d:
			return -1
		case a.d > b.d:
			return 1
		}
		return 0
	})
	set := n.bySource[src.ID]
	if set == nil {
		set = map[string]*Alert{}
		n.bySource[src.ID] = set
	}
	var out []AlertEvent
	for _, c := range cands {
		detail := map[string]any{
			"source_alert_id": src.ID, "source_kind": src.Kind, "source_flight_id": src.FlightID,
			"distance_m": c.d, "radius_m": radiusM,
			"source_position": map[string]float64{"lat": pos.LatDeg, "lng": pos.LonDeg},
		}
		if a, ok := set[c.nb.FlightID]; ok {
			a.Detail, a.UpdatedAt, a.PolicyVersion, a.Cell5 = detail, wall, policyVersion, c.nb.Cell5
			continue
		}
		if len(set) >= MaxNearbyPerSource {
			n.counters().Inc(CounterNearbyOverBound)
			continue
		}
		a := &Alert{
			ID: alertID(KindNonconformanceNearby+"|"+c.nb.FlightID+"|"+src.ID, wall), Kind: KindNonconformanceNearby,
			Severity: core.SeverityWarning, FlightID: c.nb.FlightID, IntentID: c.nb.IntentID,
			AuthorisationNumber: c.nb.AuthorisationNumber, Cell5: c.nb.Cell5, Detail: detail,
			RaisedAt: wall, UpdatedAt: wall, CapturedAt: src.CapturedAt, PolicyVersion: policyVersion,
		}
		set[c.nb.FlightID] = a
		n.counters().Inc(CounterNearbyRaised)
		out = append(out, AlertEvent{State: AlertRaised, Alert: a.clone()})
	}
	return out
}

// Clear clears every nearby alert of the source alert sourceID with
// reason (the source's own clear reason).
func (n *Nearby) Clear(sourceID, reason string, wall time.Time) []AlertEvent {
	set := n.bySource[sourceID]
	delete(n.bySource, sourceID)
	return clearSet(set, reason, wall)
}

func clearSet(set map[string]*Alert, reason string, wall time.Time) []AlertEvent {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]AlertEvent, 0, len(keys))
	for _, k := range keys {
		out = append(out, clearEvent(set[k], reason, wall, nil))
	}
	return out
}

// DropFlight clears the nearby alerts the flight held as a neighbour
// (flight_ended). Its own source alerts are cleared by Clear when the
// tracker's Drop clears them.
func (n *Nearby) DropFlight(flightID string, wall time.Time) []AlertEvent {
	var out []AlertEvent
	srcs := make([]string, 0, len(n.bySource))
	for s := range n.bySource {
		srcs = append(srcs, s)
	}
	slices.Sort(srcs)
	for _, s := range srcs {
		if a, ok := n.bySource[s][flightID]; ok {
			out = append(out, clearEvent(a, ClearFlightEnded, wall, nil))
			delete(n.bySource[s], flightID)
		}
	}
	return out
}

// Active are the nearby alerts held, by source then neighbour.
func (n *Nearby) Active() []Alert {
	srcs := make([]string, 0, len(n.bySource))
	for s := range n.bySource {
		srcs = append(srcs, s)
	}
	slices.Sort(srcs)
	var out []Alert
	for _, s := range srcs {
		keys := make([]string, 0, len(n.bySource[s]))
		for k := range n.bySource[s] {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			out = append(out, n.bySource[s][k].clone())
		}
	}
	return out
}

// Of are the nearby alerts the source alert sourceID raised, for the
// source flight's persisted state.
func (n *Nearby) Of(sourceID string) []Alert {
	set := n.bySource[sourceID]
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Alert, 0, len(keys))
	for _, k := range keys {
		out = append(out, set[k].clone())
	}
	return out
}

// Restore takes over the nearby alerts of a source flight restored on
// this worker (a restart or a handover): they keep their ids, so the
// next refresh updates them and the source's clear clears them.
func (n *Nearby) Restore(as []SavedAlert) {
	if n.bySource == nil {
		n.bySource = map[string]map[string]*Alert{}
	}
	for i := range as {
		a := as[i].AlertOf()
		src, _ := a.Detail["source_alert_id"].(string)
		if src == "" || a.Kind != KindNonconformanceNearby {
			continue
		}
		set := n.bySource[src]
		if set == nil {
			set = map[string]*Alert{}
			n.bySource[src] = set
		}
		if len(set) < MaxNearbyPerSource {
			set[a.FlightID] = a
		}
	}
}

// Forget removes the nearby alerts of the source alert sourceID without
// clearing them: the source flight was handed over to another instance,
// which continues them.
func (n *Nearby) Forget(sourceID string) { delete(n.bySource, sourceID) }
