// Command genvectors writes testdata/vectors/conformance.json: the cases
// are written here by hand, each expected value by construction (a
// sample placed d metres outside an outline with uspace-core's
// geodesy.Destination is d metres outside); the generator only computes
// the positions and never calls the judgement. Regenerate rather than
// edit:
//
//	go run ./internal/conformance/genvectors > testdata/vectors/conformance.json
package main

import (
	"encoding/json"
	"os"
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
)

var (
	t0       = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	centre   = core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271}
	sqCentre = geodesy.Destination(centre, 90, 3000)
	corrB    = geodesy.Destination(centre, 0, 2000)
)

const (
	undulationM = 20.0
	lowerAMSL   = 500.0
	upperAMSL   = 600.0
	radiusM     = 500.0
	halfSideDeg = 0.005
)

type m = map[string]any

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func point(p core.LatLon) m { return m{"lat": p.LatDeg, "lng": p.LonDeg} }

func w84(v float64) m { return m{"value": v, "reference": "W84", "units": "M"} }

func timeOf(t time.Time) m { return m{"value": ts(t), "format": "RFC3339"} }

func circleVol(c core.LatLon, start, end time.Time) m {
	return m{
		"volume": m{
			"outline_circle": m{"center": point(c), "radius": m{"value": radiusM, "units": "M"}},
			"altitude_lower": w84(lowerAMSL + undulationM), "altitude_upper": w84(upperAMSL + undulationM),
		},
		"time_start": timeOf(start), "time_end": timeOf(end),
	}
}

func squareVerts() []core.LatLon {
	c := sqCentre
	return []core.LatLon{
		{LatDeg: c.LatDeg - halfSideDeg, LonDeg: c.LonDeg - halfSideDeg},
		{LatDeg: c.LatDeg - halfSideDeg, LonDeg: c.LonDeg + halfSideDeg},
		{LatDeg: c.LatDeg + halfSideDeg, LonDeg: c.LonDeg + halfSideDeg},
		{LatDeg: c.LatDeg + halfSideDeg, LonDeg: c.LonDeg - halfSideDeg},
	}
}

func squareVol(start, end time.Time) m {
	verts := squareVerts()
	vs := make([]m, 0, len(verts))
	for _, p := range verts {
		vs = append(vs, point(p))
	}
	return m{
		"volume": m{
			"outline_polygon": m{"vertices": vs},
			"altitude_lower":  w84(lowerAMSL + undulationM), "altitude_upper": w84(upperAMSL + undulationM),
		},
		"time_start": timeOf(start), "time_end": timeOf(end),
	}
}

func band() m {
	return m{"lower_amsl_m": lowerAMSL, "upper_amsl_m": upperAMSL, "undulation_m": undulationM,
		"lower_w84_m": lowerAMSL + undulationM, "upper_w84_m": upperAMSL + undulationM}
}

// state is an intent_active value (intent/state/v1) with the volumes.
func state(id string, vols ...m) m {
	bands := make([]m, len(vols))
	for i := range vols {
		bands[i] = band()
	}
	return m{
		"intent_id": id, "version": 2, "local_state": "activated", "dss_state": "Activated", "priority": 0,
		"exempt_art_1_3": false, "authorisation_number": "USSP-DEV-20261101-000001", "operator_reg": "GEO-TEST-0001",
		"uas_serial": "TEST0001", "volumes": vols, "volumes_amsl": bands,
		"deviation_thresholds": m{"h_m": 50, "v_m": 15, "t_s": 60}, "flight_id": nil, "cell_set": []string{},
		"time_start": ts(t0.Add(-10 * time.Minute)), "time_end": ts(t0.Add(30 * time.Minute)),
		"in_uspace_airspace": true, "policy_version": 1, "change_reason": nil, "updated_at": ts(t0.Add(-20 * time.Minute)),
		"class_label": nil, "ua_registration": nil,
	}
}

// sample is a judge sample at position p, t_s seconds after now.
func sample(p core.LatLon, alt any, src string, tS float64) m {
	return m{"lat_deg": p.LatDeg, "lon_deg": p.LonDeg, "alt_amsl_m": alt, "alt_source": src, "t_s": tS}
}

// east is the circle's centre moved d metres east: d - radius outside.
func east(d float64) core.LatLon { return geodesy.Destination(centre, 90, d) }

// sqEast is d metres east of the square's east edge, at its middle.
func sqEast(d float64) core.LatLon {
	mid := core.LatLon{LatDeg: sqCentre.LatDeg, LonDeg: sqCentre.LonDeg + halfSideDeg}
	return geodesy.Destination(mid, 90, d)
}

type c struct {
	Name     string `json:"name"`
	Owner    []string
	Input    m
	Expected m
	Why      string
}

func (x c) MarshalJSON() ([]byte, error) {
	return json.Marshal(m{"name": x.Name, "owner": []string{"ussp"}, "input": x.Input, "expected": x.Expected, "why": x.Why})
}

func judge(name, auth string, s m, exp m, why string, extra ...m) c {
	in := m{"check": "judge", "authorisation": auth, "sample": s, "policy": m{"pressure_uncertainty_m": 250.0}}
	for _, e := range extra {
		for k, v := range e {
			in[k] = v
		}
	}
	return c{Name: name, Input: in, Expected: exp, Why: why}
}

func out(reason string, dist, height, tOut float64, within, vKnown, withinBand bool) m {
	return m{"outcome": "outside", "reason": reason, "distance_outside_m": dist, "height_over_m": height,
		"time_outside_s": tOut, "within_threshold": within, "vertical_known": vKnown, "within_band": withinBand}
}

func in(withinBand bool) m {
	return m{"outcome": "inside", "reason": "", "distance_outside_m": 0.0, "height_over_m": 0.0, "time_outside_s": 0.0,
		"within_threshold": false, "vertical_known": true, "within_band": withinBand}
}

func refused(field string) m { return m{"error": field} }

// Sequence steps.
func obs(tS float64, p core.LatLon, extra ...m) m {
	st := m{"t_s": tS, "op": "observe", "sample": m{"lat_deg": p.LatDeg, "lon_deg": p.LonDeg}}
	for _, e := range extra {
		for k, v := range e {
			st["sample"].(m)[k] = v
		}
	}
	return st
}

func tick(tS float64) m { return m{"t_s": tS, "op": "tick"} }

func step(state string, raised []string, cleared ...m) m {
	if raised == nil {
		raised = []string{}
	}
	if cleared == nil {
		cleared = []m{}
	}
	return m{"state": state, "raised": raised, "cleared": cleared}
}

func cl(kind, reason string) m { return m{"kind": kind, "reason": reason} }

func seq(name string, steps []m, per []m, final m, why string, extra ...m) c {
	in := m{"check": "sequence", "authorisation": "circle", "steps": steps}
	for _, e := range extra {
		for k, v := range e {
			in[k] = v
		}
	}
	exp := m{"per_step": per}
	for k, v := range final {
		exp[k] = v
	}
	return c{Name: name, Input: in, Expected: exp, Why: why}
}

func main() {
	start, end := t0.Add(-10*time.Minute), t0.Add(30*time.Minute)
	mid := t0.Add(5 * time.Minute)
	fixtures := m{
		"now": ts(t0),
		"authorisations": m{
			"circle":   state("8d0e7b51-3c1e-4a5f-9a43-0b6f4c2a7e01", circleVol(centre, start, end)),
			"square":   state("8d0e7b51-3c1e-4a5f-9a43-0b6f4c2a7e02", squareVol(start, end)),
			"corridor": state("8d0e7b51-3c1e-4a5f-9a43-0b6f4c2a7e03", circleVol(centre, start, mid), circleVol(corrB, mid, end)),
		},
		"sequence_config": m{"clear_after_s": 3.0, "lost_link_s": 15.0, "live_max_age_s": 10.0, "ahead_tolerance_s": 1.0,
			"pressure_uncertainty_m": 250.0, "policy_version": 7},
		"sample_defaults": m{"alt_amsl_m": 550.0, "alt_source": "geodetic", "status": "Airborne",
			"captured_at_s": "t_s", "rx_at_s": "captured_at_s", "backlog": false, "source_disabled": false},
		"flight_id": "5b3f1d2e-7c4a-4e8b-9f10-2a3b4c5d6e7f",
	}
	inside := centre
	geo := "geodetic"
	judges := []c{
		judge("circle-centre-inside", "circle", sample(inside, 550.0, geo, 0), in(true),
			"The centre of the circle inside the band and the window is inside."),
		judge("circle-just-inside-the-edge", "circle", sample(east(499.99), 550.0, geo, 0), in(true),
			"1 cm inside the radius is inside: the circle is judged by geodesic distance from its centre (Z-11)."),
		judge("circle-outside-by-1m", "circle", sample(east(501), 550.0, geo, 0), out("outside_volume_h", 1, 0, 0, true, true, true),
			"1 m outside the outline is outside, within the 50 m horizontal threshold; the vertical band still holds it (within_band)."),
		judge("circle-outside-by-h-minus-1", "circle", sample(east(549), 550.0, geo, 0), out("outside_volume_h", 49, 0, 0, true, true, true),
			"49 m outside is within h_m 50: the Art. 10(2)(d) tolerance."),
		judge("circle-outside-by-just-under-h", "circle", sample(east(549.99), 550.0, geo, 0), out("outside_volume_h", 49.99, 0, 0, true, true, true),
			"1 cm inside h_m is within the threshold. The closed bound itself is pinned on the vertical (above-upper-by-v), where the numbers are exact; a horizontal distance of exactly h_m is a floating-point coin toss."),
		judge("circle-outside-by-h-plus-1", "circle", sample(east(551), 550.0, geo, 0), out("outside_volume_h", 51, 0, 0, false, true, true),
			"51 m outside is beyond h_m 50: not within the threshold."),
		judge("at-the-upper-limit-inside", "circle", sample(inside, 600.0, geo, 0), in(true), "The upper limit itself is inside (closed band)."),
		judge("at-the-lower-limit-inside", "circle", sample(inside, 500.0, geo, 0), in(true), "The lower limit itself is inside (closed band)."),
		judge("above-upper-within-v", "circle", sample(inside, 610.0, geo, 0), out("above_upper", 0, 10, 0, true, true, false),
			"10 m above the authorised upper AMSL limit is above_upper, within v_m 15 (height conformance is against the authorised upper, PLAN §1)."),
		judge("above-upper-by-v", "circle", sample(inside, 615.0, geo, 0), out("above_upper", 0, 15, 0, true, true, false),
			"Exactly v_m above is within the threshold."),
		judge("above-upper-beyond-v", "circle", sample(inside, 620.0, geo, 0), out("above_upper", 0, 20, 0, false, true, false),
			"20 m above is beyond v_m 15."),
		judge("below-lower-within-v", "circle", sample(inside, 490.0, geo, 0), out("below_lower", 0, 10, 0, true, true, false),
			"10 m below the lower limit is below_lower, within v_m."),
		judge("below-lower-beyond-v", "circle", sample(inside, 480.0, geo, 0), out("below_lower", 0, 20, 0, false, true, false),
			"20 m below is beyond v_m 15."),
		judge("before-start-within-t", "circle", sample(inside, 550.0, geo, -630), out("before_start", 0, 0, 30, true, true, true),
			"30 s before the window opens is before_start, within t_s 60."),
		judge("before-start-beyond-t", "circle", sample(inside, 550.0, geo, -661), out("before_start", 0, 0, 61, false, true, true),
			"61 s before the window is beyond t_s 60."),
		judge("after-end-within-t", "circle", sample(inside, 550.0, geo, 1810), out("after_end", 0, 0, 10, true, true, true),
			"10 s after the window closes is after_end, within t_s."),
		judge("after-end-beyond-t", "circle", sample(inside, 550.0, geo, 1920), out("after_end", 0, 0, 120, false, true, true),
			"Two minutes after the window is beyond t_s."),
		judge("at-the-window-end-inside", "circle", sample(inside, 550.0, geo, 1800), in(true), "The window is closed: its end is inside."),
		judge("after-end-and-outside-reports-the-time", "circle", sample(east(560), 550.0, geo, 1810),
			out("after_end", 60, 0, 10, false, true, true),
			"Outside the window and the outline: the reason is the time, and the horizontal excess (60 m, beyond h_m) is reported and counted in the threshold."),
		judge("outside-horizontally-and-above", "circle", sample(east(520), 640.0, geo, 0), out("outside_volume_h", 20, 40, 0, false, true, false),
			"Outside both ways: the reason is horizontal, both excesses are reported, and the vertical one (40 m) is beyond v_m."),
		judge("pressure-inside-only-with-the-margin", "circle", sample(inside, 700.0, "pressure", 0), in(false),
			"A pressure altitude 100 m above the upper is inside the band widened by 250 m (R-09), so it is inside but within_band is false (04 §3.1)."),
		judge("pressure-inside-the-band", "circle", sample(inside, 550.0, "pressure", 0), in(true),
			"A pressure altitude inside the band as given is within_band."),
		judge("pressure-beyond-the-margin", "circle", sample(inside, 870.0, "pressure", 0), out("above_upper", 0, 20, 0, false, true, false),
			"870 m pressure is 20 m above the widened upper (850 m): above_upper, measured from the widened band."),
		judge("geodetic-at-the-same-altitude-is-outside", "circle", sample(inside, 700.0, geo, 0), out("above_upper", 0, 100, 0, false, true, false),
			"The pair of the pressure case: a geodetic 700 m has no margin and is 100 m above the upper."),
		judge("pressure-margin-zero", "circle", sample(inside, 610.0, "pressure", 0), out("above_upper", 0, 10, 0, true, true, false),
			"With no pressure margin configured, a pressure altitude is judged against the band as given.", m{"policy": m{"pressure_uncertainty_m": 0.0}}),
		judge("network-altitude-inside", "circle", sample(inside, 550.0, "network", 0), in(true), "A network altitude is a vertical position like a geodetic one."),
		judge("unknown-vertical-alt-source-none", "circle", sample(inside, nil, "none", 0),
			m{"outcome": "undetermined", "reason": "", "distance_outside_m": 0.0, "height_over_m": 0.0, "time_outside_s": 0.0,
				"within_threshold": false, "vertical_known": false, "within_band": false},
			"alt_source none: the vertical judgement is not evaluated and the verdict says so; it is never inside by default (E-15, SC-22)."),
		judge("unknown-vertical-value-with-none-source", "circle", sample(inside, 550.0, "none", 0),
			m{"outcome": "undetermined", "reason": "", "distance_outside_m": 0.0, "height_over_m": 0.0, "time_outside_s": 0.0,
				"within_threshold": false, "vertical_known": false, "within_band": false},
			"An altitude with source none is not a vertical position: undetermined."),
		judge("unknown-vertical-outside-horizontally", "circle", sample(east(600), nil, "none", 0), out("outside_volume_h", 100, 0, 0, false, false, false),
			"Without the vertical, the horizontal judgement still runs: 100 m outside is outside and beyond h_m."),
		judge("square-inside", "square", sample(sqCentre, 550.0, geo, 0), in(true), "Inside a polygon volume."),
		judge("square-outside-east-by-30m", "square", sample(sqEast(30), 550.0, geo, 0), out("outside_volume_h", 30, 0, 0, true, true, true),
			"30 m east of the square's east edge: the distance is to the nearest edge."),
		judge("square-outside-east-by-80m", "square", sample(sqEast(80), 550.0, geo, 0), out("outside_volume_h", 80, 0, 0, false, true, true),
			"80 m east of the edge is beyond h_m."),
		judge("square-above", "square", sample(sqCentre, 625.0, geo, 0), out("above_upper", 0, 25, 0, false, true, false),
			"The polygon's band is judged like the circle's."),
		judge("corridor-first-volume", "corridor", sample(centre, 550.0, geo, 0), in(true),
			"Two volumes in sequence: at T0 the first is active and holds the aircraft."),
		judge("corridor-second-volume", "corridor", sample(corrB, 550.0, geo, 600), in(true),
			"At T0 + 10 min the second volume is active and holds the aircraft."),
		judge("corridor-wrong-volume-for-the-time", "corridor", sample(corrB, 550.0, geo, 0), out("outside_volume_h", 1500, 0, 0, false, true, true),
			"At T0 the aircraft is where the second volume will be: only the first is active, 2000 - 500 = 1500 m away."),
		judge("corridor-handover-instant", "corridor", sample(corrB, 550.0, geo, 300), in(true),
			"At the handover instant both windows are closed sets, so the second volume already holds it."),
		judge("threshold-h-zero-refuses", "circle", sample(inside, 550.0, geo, 0), refused("deviation_thresholds.h_m"),
			"A zero horizontal threshold refuses the judgement; it never disarms it (E-15).", m{"thresholds": m{"h_m": 0, "v_m": 15, "t_s": 60}}),
		judge("threshold-v-negative-refuses", "circle", sample(inside, 550.0, geo, 0), refused("deviation_thresholds.v_m"),
			"A negative vertical threshold refuses the judgement (E-15).", m{"thresholds": m{"h_m": 50, "v_m": -1, "t_s": 60}}),
		judge("threshold-t-zero-refuses", "circle", sample(inside, 550.0, geo, 0), refused("deviation_thresholds.t_s"),
			"A zero time threshold refuses the judgement (E-15).", m{"thresholds": m{"h_m": 50, "v_m": 15, "t_s": 0}}),
		judge("thresholds-missing-refuses", "circle", sample(inside, 550.0, geo, 0), refused("deviation_thresholds"),
			"An intent without deviation thresholds cannot be judged and is never judged as conforming.", m{"thresholds": nil}),
		judge("pressure-margin-negative-refuses", "circle", sample(inside, 550.0, geo, 0), refused("pressure_uncertainty_m"),
			"A negative pressure margin refuses the judgement (E-15).", m{"policy": m{"pressure_uncertainty_m": -1.0}}),
		judge("invalid-position-refuses", "circle", sample(core.LatLon{LatDeg: 91, LonDeg: 44}, 550.0, geo, 0), refused("position"),
			"A position that is not WGS84 is not judged (C-09)."),
	}

	out80 := east(580) // 80 m outside: beyond h_m
	out10 := east(510) // 10 m outside: within h_m
	cfg := func(k string, v any) m { return m{"config": m{k: v}} }
	_ = cfg
	seqs := []c{
		seq("t-persistence-then-raise",
			[]m{obs(0, out10), obs(30, out10), obs(60, out10), obs(61, out10)},
			[]m{step("conforming", nil), step("conforming", nil), step("conforming", nil), step("nonconforming", []string{"nonconformance"})},
			m{"active_after": []m{{"kind": "nonconformance", "detail": m{"reason": "outside_volume_h", "distance_outside_m": 10.0}}}},
			"Outside within the thresholds is tolerated for t_s (60 s); it is nonconforming once it persists beyond t_s, with the verdict's reason."),
		seq("threshold-exceeded-raises-at-once",
			[]m{obs(0, out80)},
			[]m{step("nonconforming", []string{"nonconformance"})},
			m{"active_after": []m{{"kind": "nonconformance", "detail": m{"reason": "threshold_exceeded", "distance_outside_m": 80.0}}}},
			"An excess beyond h_m is nonconforming at once (threshold_exceeded), raised by the first sample: the alert latency starts at its captured_at."),
		seq("height-above-upper-beyond-v-raises",
			[]m{obs(0, inside, m{"alt_amsl_m": 620.0})},
			[]m{step("nonconforming", []string{"nonconformance"})},
			m{"active_after": []m{{"kind": "nonconformance", "detail": m{"reason": "threshold_exceeded", "height_over_m": 20.0, "verdict_reason": "above_upper"}}}},
			"Height above the authorised upper beyond v_m is a nonconformance (S-M2: including height above the authorised upper)."),
		seq("return-within-hysteresis-does-not-clear",
			[]m{obs(0, out80), obs(1, inside), obs(2, inside), obs(3, inside)},
			[]m{step("nonconforming", []string{"nonconformance"}), step("nonconforming", nil), step("nonconforming", nil), step("nonconforming", nil)},
			m{"active_after": []m{{"kind": "nonconformance"}}},
			"Shown inside for 3 s since last outside is not more than clear_after_s: the alert holds (C-06)."),
		seq("return-beyond-hysteresis-clears-resolved-with-numbers",
			[]m{obs(0, out80), obs(1, inside), obs(2, inside), obs(3.5, inside)},
			[]m{step("nonconforming", []string{"nonconformance"}), step("nonconforming", nil), step("nonconforming", nil),
				step("conforming", nil, m{"kind": "nonconformance", "reason": "resolved",
					"detail": m{"distance_outside_m": 80.0}, "clearing_detail": m{"clearing_distance_outside_m": 0.0}})},
			m{"active_after": []m{}},
			"Shown inside for more than 3 s since last outside: cleared resolved, carrying the last numbers that showed it outside and the numbers that cleared it (C-14)."),
		seq("refresh-carries-current-numbers",
			[]m{obs(0, out80), obs(1, east(590))},
			[]m{step("nonconforming", []string{"nonconformance"}), step("nonconforming", nil)},
			m{"active_after": []m{{"kind": "nonconformance", "detail": m{"distance_outside_m": 90.0, "reason": "threshold_exceeded"}}}},
			"Raised once; a later sample refreshes silently with the current numbers (C-06, C-08)."),
		seq("contingent-after-60s-without-returning",
			[]m{obs(0, out80), obs(10, out80), obs(20, out80), obs(30, out80), obs(40, out80), obs(50, out80), obs(59, out80), obs(60, out80)},
			[]m{step("nonconforming", []string{"nonconformance"}), step("nonconforming", nil), step("nonconforming", nil), step("nonconforming", nil),
				step("nonconforming", nil), step("nonconforming", nil), step("nonconforming", nil), step("contingent", nil)},
			m{"active_after": []m{{"kind": "nonconformance", "detail": m{"state": "contingent"}}}},
			"Nonconforming for MaxRecoverableTimeInNonconformingStateSeconds (60 s) without returning goes contingent (F3548, 02 F6); the alert stays active."),
		seq("return-before-60s-is-not-contingent",
			[]m{obs(0, out80), obs(10, out80), obs(20, out80), obs(30, out80), obs(39, out80), obs(40, inside), obs(42.5, inside), obs(52, inside), obs(61, inside)},
			[]m{step("nonconforming", []string{"nonconformance"}), step("nonconforming", nil), step("nonconforming", nil), step("nonconforming", nil),
				step("nonconforming", nil), step("nonconforming", nil), step("conforming", nil, cl("nonconformance", "resolved")),
				step("conforming", nil), step("conforming", nil)},
			m{"active_after": []m{}},
			"A flight back inside before 60 s returns to conforming and never goes contingent."),
		seq("contingent-stays-after-return",
			[]m{obs(0, out80), obs(10, out80), obs(20, out80), obs(30, out80), obs(40, out80), obs(50, out80), obs(60, out80),
				obs(61, inside), obs(64.5, inside)},
			[]m{step("nonconforming", []string{"nonconformance"}), step("nonconforming", nil), step("nonconforming", nil), step("nonconforming", nil),
				step("nonconforming", nil), step("nonconforming", nil), step("contingent", nil), step("contingent", nil),
				step("contingent", nil, cl("nonconformance", "resolved"))},
			m{"active_after": []m{}},
			"Contingent is kept until the flight ends (F3548 has no way back to Activated); the alert itself clears resolved when the aircraft is shown inside."),
		seq("lost-link-raised-and-cleared",
			[]m{obs(0, inside), tick(14), tick(15), obs(20, inside), obs(23.5, inside)},
			[]m{step("conforming", nil), step("conforming", nil), step("lost_link", []string{"lost_link"}),
				step("nonconforming", nil, cl("lost_link", "resolved")), step("conforming", nil)},
			m{"active_after": []m{}},
			"15 s without a live sample raises lost_link (02 F5); lost telemetry is non-conformance (telemetry_lost), so the flight is nonconforming until shown inside for the hysteresis after the link is back."),
		seq("lost-link-then-contingent",
			[]m{obs(0, inside), tick(15), tick(74), tick(75)},
			[]m{step("conforming", nil), step("lost_link", []string{"lost_link"}), step("lost_link", nil), step("contingent", nil)},
			m{"active_after": []m{{"kind": "lost_link", "detail": m{"silence_s": 75.0}}}},
			"Silence is not evidence of a return: 60 s after the link was lost the flight is contingent, and the lost_link alert carries the current silence (C-08)."),
		seq("lost-link-not-for-a-grounded-aircraft",
			[]m{obs(0, inside, m{"status": "Ground"}), tick(30)},
			[]m{step("unknown", nil), step("unknown", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_not_flying": 1}},
			"An aircraft whose last sample said Ground is not lost when it goes quiet (C-05)."),
		seq("lost-link-not-without-an-authorisation",
			[]m{obs(0, inside), tick(30)},
			[]m{step("unknown", nil), step("unknown", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_authorisation_missing": 1}},
			"A flight never judged against an authorisation has nothing to conform to: unknown, and no lost_link.", m{"authorisation": nil}),
		seq("backlog-samples-ignored",
			[]m{obs(0, out80, m{"backlog": true}), obs(1, inside)},
			[]m{step("unknown", nil), step("conforming", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_rejected_backlog": 1, "conformance_judged": 1}},
			"History is never alerted (T-04): a backlog sample far outside raises nothing."),
		seq("late-samples-ignored",
			[]m{obs(20, out80, m{"captured_at_s": 5.0, "rx_at_s": 9.0}), obs(21, inside)},
			[]m{step("unknown", nil), step("conforming", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_rejected_late": 1}},
			"A sample received 11 s before the wall is late beyond live_max_age_s 10 (T-05) and judges nothing."),
		seq("out-of-order-sample-ignored",
			[]m{obs(5, inside), obs(6, out80, m{"captured_at_s": 4.0, "rx_at_s": 6.0})},
			[]m{step("conforming", nil), step("conforming", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_rejected_out_of_order": 1}},
			"A sample placed before the one held never overwrites it (T-06)."),
		seq("placed-ahead-refused",
			[]m{obs(0, out80, m{"captured_at_s": 2.0, "rx_at_s": 0.0})},
			[]m{step("unknown", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_rejected_placed_ahead": 1}},
			"A sample placed 2 s ahead of its receipt (tolerance 1 s) is a clock ahead and is refused."),
		seq("disabled-source-clears-source-disabled",
			[]m{obs(0, out80), obs(1, out80, m{"source_disabled": true}), tick(30), obs(31, out80)},
			[]m{step("nonconforming", []string{"nonconformance"}), step("unknown", nil, cl("nonconformance", "source_disabled")),
				step("unknown", nil), step("nonconforming", []string{"nonconformance"})},
			m{"active_after": []m{{"kind": "nonconformance"}}, "counters": m{"conformance_rejected_source_disabled": 1}},
			"A switched-off source clears its alerts as source_disabled (B-11), is held unknown without a lost_link, and the next sample from the enabled source raises again."),
		seq("ground-sample-not-judged",
			[]m{obs(0, out80, m{"status": "Ground"}), obs(1, out80, m{"status": "Undeclared"})},
			[]m{step("unknown", nil), step("unknown", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_not_flying": 1, "conformance_flying_unknown": 1}},
			"Alert only on flying aircraft (C-05): Ground is not flying and Undeclared is not known to be."),
		seq("flight-end-clears-flight-ended",
			[]m{obs(0, out80), {"t_s": 5.0, "op": "drop"}},
			[]m{step("nonconforming", []string{"nonconformance"}), step("nonconforming", nil, cl("nonconformance", "flight_ended"))},
			m{"active_after": []m{}},
			"Ending the flight clears its alerts as flight_ended, never resolved."),
		seq("undetermined-never-clears",
			[]m{obs(0, out80), obs(1, inside, m{"alt_source": "none", "alt_amsl_m": nil}), obs(2, inside, m{"alt_source": "none", "alt_amsl_m": nil}),
				obs(6, inside, m{"alt_source": "none", "alt_amsl_m": nil})},
			[]m{step("nonconforming", []string{"nonconformance"}), step("nonconforming", nil), step("nonconforming", nil), step("nonconforming", nil)},
			m{"active_after": []m{{"kind": "nonconformance"}}, "counters": m{"conformance_vertical_not_evaluated": 3}},
			"A sample whose vertical was not evaluated does not show the flight inside: nothing unjudged clears an alert (C-09)."),
		seq("undetermined-first-judgement-stays-unknown",
			[]m{obs(0, inside, m{"alt_source": "none", "alt_amsl_m": nil}), obs(1, inside, m{"alt_source": "none", "alt_amsl_m": nil}), obs(2, inside)},
			[]m{step("unknown", nil), step("unknown", nil), step("conforming", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_vertical_not_evaluated": 2}},
			"With no altitude ever the flight was never shown inside its band: unknown (vertical_not_evaluated), never conforming by default (SC-22); the first sample with an altitude judges it."),
		seq("within-threshold-return-before-t-stays-conforming",
			[]m{obs(0, out10), obs(30, out10), obs(40, inside), obs(100, inside)},
			[]m{step("conforming", nil), step("conforming", nil), step("conforming", nil), step("conforming", nil)},
			m{"active_after": []m{}},
			"The E-01 twin of t-persistence-then-raise: back inside before t_s, nothing is raised."),
		seq("authorisation-missing-is-unknown",
			[]m{obs(0, out80)},
			[]m{step("unknown", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_authorisation_missing": 1}},
			"No intent_active entry: the flight is unknown, never conforming, and counted (SC-22).", m{"authorisation": nil}),
		seq("invalid-thresholds-judge-nothing",
			[]m{obs(0, out80), tick(30)},
			[]m{step("unknown", nil), step("unknown", nil)},
			m{"active_after": []m{}, "counters": m{"conformance_judgement_failed": 1}},
			"An authorisation whose thresholds refuse the judgement leaves the flight unknown: it is never read as conforming (E-15).",
			m{"thresholds": m{"h_m": 0, "v_m": 15, "t_s": 60}}),
	}
	cases := slices.Concat(judges, seqs)

	doc := struct {
		Description    string   `json:"description"`
		Source         []string `json:"source"`
		Units          m        `json:"units"`
		Tolerance      m        `json:"tolerance"`
		Owners         []string `json:"owners"`
		Generated      string   `json:"generated"`
		UtmCommit      string   `json:"utm_commit"`
		HysteresisRule string   `json:"hysteresis_rule"`
		Fixtures       m        `json:"fixtures"`
		Cases          []c      `json:"cases"`
	}{
		Description: "Conformance monitoring (2021/664 Art. 13(1); brief WP-10). judge cases: one sample against an intent_active value (intent/state/v1, the fixtures' authorisations, with optional thresholds and policy overrides), read through internal/conformance.AuthorisationOf and judged by conformance.Judge; expected is the verdict, or error naming the refused field. sequence cases: one fresh conformance.Tracker per case on the fixtures' sequence_config, flight and authorisation; each step is observe (a track sample at wall time t_s, read through conformance.InputOf; sample fields default to sample_defaults), tick (no sample) or drop (the flight ended). expected.per_step[i] is the reported state after step i and the alert kinds it raised and cleared (with the clear reason); active_after the alerts active at the end (detail fields listed are compared); counters the counts the case pins. Positions: lat_deg/lon_deg computed with uspace-core geodesy.Destination from the fixtures (the circle's centre, the square's east edge); the expected numbers are by construction, never computed by the judgement (internal/conformance/genvectors).",
		Source: []string{
			"Reg. (EU) 2021/664 Art. 10(2)(d), 13(1)", "ASTM F3548-21 MaxRecoverableTimeInNonconformingStateSeconds",
			"spec 02 F5, F6, F13; 03 §3 conformance_states; 04 §3.1, §3.3", "docs/WORKPACKAGES/WP-10.md",
			"uspace-core vectors/testdata/alert_lifecycle.json (hysteresis_rule)",
		},
		Units: m{
			"lat_deg/lon_deg": "WGS84 degrees", "alt_amsl_m": "metres AMSL (D-01)", "t_s": "seconds after fixtures.now, the monitor's wall clock",
			"distance_outside_m, height_over_m": "metres", "time_outside_s": "seconds",
		},
		Tolerance: m{
			"horizontal": "0.01 m plus 1e-4 of the distance (the tangent-plane nearest point of an edge)",
			"vertical":   "exact", "time": "exact (closed intervals)",
		},
		Owners:         []string{"ussp"},
		Generated:      "2026-10-02",
		UtmCommit:      "none (new in uspace-ussp WP-10)",
		HysteresisRule: "nonconforming returns to conforming, and the nonconformance alert clears resolved, when (placement of an inside sample) - (placement of the last outside sample) > clear_after_s; an undetermined sample neither refreshes nor shows inside",
		Fixtures:       fixtures,
		Cases:          cases,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(doc); err != nil {
		// No exit status outside cmd/: the half-written file fails the
		// vector test that reads it.
		_, _ = os.Stderr.WriteString("genvectors: " + err.Error() + "\n")
	}
}
