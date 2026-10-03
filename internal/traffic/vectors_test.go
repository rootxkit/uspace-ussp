package traffic

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-core/alerting"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed269"
	coresources "github.com/rootxkit/uspace-core/sources"
	"github.com/rootxkit/uspace-core/vectors"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// The core vectors that name ussp (cpa.json, alert_lifecycle.json) run
// through this package's adapter (CLAUDE.md testing rules): every vector
// aircraft becomes the track/telemetry/v1 message this USSP's ingest
// publishes, is read back by DecodeTrack, mapped by TrackInputOf and
// TrackOf (the only mapping onto alerting.Track) and judged by one core
// alerting.Monitor configured by ConfigOf from the policy values. No
// pair is judged here a second time.
//
// What the wire cannot carry is said, never bent: track/telemetry/v1
// has no Remote ID transmitter address (this USSP holds no direct
// broadcast tracks), so the two transmitter cases are skipped and
// reported as such; a vector source is the shared enumeration's
// (relay -> operator_ws, remote_id -> direct_rid) and the counters it
// names per source are compared under the same renaming; an aircraft
// without a station gets the instance the ingest always gives one
// ("vector-default"), and one without an identification the
// session-bound one every operator flight carries.

var vectorSource = map[string]string{"relay": telemetry.SourceOperatorWS, "remote_id": "direct_rid"}

const defaultStation = "vector-default"

// noWire are the owned cases the wire cannot express, with why.
var noWire = map[string]string{
	"unidentified-and-serial-of-same-transmitter-never-pair": "track/telemetry/v1 carries no transmitter address (spec gap, PR)",
	"two-serials-on-one-transmitter-do-pair":                 "track/telemetry/v1 carries no transmitter address (spec gap, PR)",
}

// vecTrack is the track/telemetry/v1 message of one vector aircraft at
// placement capturedS, receipt rxS and source time tsS (seconds since
// the epoch, which the monitor's wall clock is on), round-tripped
// through JSON and DecodeTrack.
type vecTrack struct {
	id, source, station string
	lat, lon            float64
	altM                *float64
	altSource           core.AltSource
	vn, ve, vd          float64
	flying              *bool
	capturedS, rxS, tsS float64
	backlog             bool
	identification      *core.Identification
}

func (v vecTrack) message(t *testing.T) Input {
	t.Helper()
	speed := math.Hypot(v.vn, v.ve)
	track := math.Mod(math.Atan2(v.ve, v.vn)*180/math.Pi+360, 360)
	if track >= 360 {
		track = 0
	}
	vs := -v.vd
	status := "Airborne"
	if v.flying != nil && !*v.flying {
		status = "Ground"
	}
	ident := core.Identification{Status: core.IdentRegistered, Reason: core.ReasonSessionBinding, Basis: core.BasisAuthenticated}
	if v.identification != nil {
		ident = *v.identification
		if ident.Basis == "" {
			ident.Basis = core.BasisAsBroadcast
		}
	}
	ts := timeOfS(v.tsS)
	tr := telemetry.Track{
		Envelope: bus.Envelope{Schema: telemetry.SchemaTrack, MsgID: bus.NewULID(timeOfS(v.rxS)), Producer: telemetry.Producer,
			TS: &bus.Stamp{Time: ts}, RxTS: bus.Stamp{Time: timeOfS(v.rxS)}, CapturedAt: bus.Stamp{Time: timeOfS(v.capturedS)},
			TimeSource: core.TimeSourceClock, Backlog: v.backlog},
		Body: telemetry.TrackBody{TrackID: v.id, Trust: core.TrustAuthenticated, Source: v.source, SourceInstance: v.station,
			Position: telemetry.Position{Lat: v.lat, Lng: v.lon}, AltAMSLM: v.altM, AltSource: v.altSource,
			SpeedMS: &speed, TrackDeg: &track, VSpeedMS: &vs, Status: &status, Identification: ident},
	}
	raw, err := json.Marshal(&tr)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeTrack(raw)
	if err != nil {
		t.Fatalf("DecodeTrack: %v\n%s", err, raw)
	}
	return TrackInputOf(NSTrack, back)
}

// unprefix strips the namespace of a monitor id.
func unprefix(id string) string { return strings.TrimPrefix(id, NSTrack+":") }

// ---- cpa.json ----

type cpaVecState struct {
	LatDeg        float64        `json:"lat_deg"`
	LonDeg        float64        `json:"lon_deg"`
	AltAMSLM      float64        `json:"alt_amsl_m"`
	VNMS          float64        `json:"vn_ms"`
	VEMS          float64        `json:"ve_ms"`
	VDMS          float64        `json:"vd_ms"`
	CapturedAtS   float64        `json:"captured_at_s"`
	VerticalKnown bool           `json:"vertical_known"`
	DescribedAs   map[string]any `json:"described_as"`
}

type vecFloat float64

func (f *vecFloat) UnmarshalJSON(b []byte) error {
	if string(b) == `"NaN"` {
		*f = vecFloat(math.NaN())
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = vecFloat(v)
	return nil
}

type cpaVecPolicy struct {
	TCPAMaxS         vecFloat `json:"t_cpa_max_s"`
	DHorizontalMinM  vecFloat `json:"d_horizontal_min_m"`
	DVerticalMinM    vecFloat `json:"d_vertical_min_m"`
	NeighbourRadiusM vecFloat `json:"neighbour_radius_m"`
}

type cpaVecInput struct {
	A                cpaVecState   `json:"a"`
	B                cpaVecState   `json:"b"`
	NeighbourMaxAgeS float64       `json:"neighbour_max_age_s"`
	Policy           *cpaVecPolicy `json:"policy"`
}

type cpaVecExpected struct {
	Judged          bool     `json:"judged"`
	TCPAS           *float64 `json:"t_cpa_s"`
	DCPAHorizontalM *float64 `json:"d_cpa_horizontal_m"`
	DAltAtCPAM      *float64 `json:"d_alt_at_cpa_m"`
	DHorizontalNowM *float64 `json:"d_horizontal_now_m"`
	DAltNowM        *float64 `json:"d_alt_now_m"`
	VerticalKnown   *bool    `json:"vertical_known"`
	Conflict        *bool    `json:"conflict"`
	LoSStartS       *float64 `json:"los_start_s"`
	NotJudged       *string  `json:"not_judged"`
}

// cpaSelectRadiusM is the monitor's search radius in the cpa.json run:
// cpa.json judges pairs, not the monitor's selection of them, and some
// of its pairs start 1 km apart, beyond the policy's 800 m search
// radius. The radius selects a pair; it does not change the judgement
// of mid-latitude pairs (cpa.Evaluate reads it near a pole only).
const cpaSelectRadiusM = 2000

func cpaTrack(s cpaVecState, id string, wallS float64) vecTrack {
	alt := s.AltAMSLM
	v := vecTrack{id: id, source: telemetry.SourceOperatorWS, station: "s-" + id, lat: s.LatDeg, lon: s.LonDeg,
		altM: &alt, altSource: core.AltGeodetic, vn: s.VNMS, ve: s.VEMS, vd: s.VDMS,
		capturedS: s.CapturedAtS, rxS: wallS, tsS: s.CapturedAtS}
	if !s.VerticalKnown {
		v.altM, v.altSource = nil, core.AltNone
	}
	return v
}

func TestVectorsCPAThroughTheMonitorAdapter(t *testing.T) {
	f := vectors.Load(t, "cpa.json")
	var hp cpaVecPolicy
	f.Header(t, "policy", &hp)
	conflicts, quiet := 0, 0
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var in cpaVecInput
		var exp cpaVecExpected
		c.Decode(t, &in, &exp)
		cp := hp
		if in.Policy != nil {
			cp = *in.Policy
		}
		v := policy.Defaults()
		v.CPATCPAMaxS, v.CPAHorizontalMinM, v.CPAVerticalMinM = float64(cp.TCPAMaxS), float64(cp.DHorizontalMinM), float64(cp.DVerticalMinM)
		v.CPANeighbourRadiusM, v.CPANeighbourMaxAgeS = cpaSelectRadiusM, in.NeighbourMaxAgeS
		cfg := ConfigOf(v)
		for _, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
			m := alerting.NewMonitor(cfg)
			wall := math.Max(in.A.CapturedAtS, in.B.CapturedAtS)
			states := map[string]cpaVecState{"a": in.A, "b": in.B}
			var ev alerting.Events
			for _, k := range order {
				inp := cpaTrack(states[k], k, wall).message(t)
				tr, _ := TrackOf(&inp)
				ev = m.Observe(tr, wall)
			}
			conflict := exp.Judged && exp.Conflict != nil && *exp.Conflict
			if !conflict {
				if len(ev.Raised) != 0 || len(m.Active()) != 0 {
					t.Fatalf("order %v: raised %+v, the vector says no conflict (judged %v)", order, ev.Raised, exp.Judged)
				}
				if !exp.Judged && exp.NotJudged != nil {
					if n := m.Counters().Get(alerting.CounterPairsNotJudged + "_" + *exp.NotJudged); n == 0 {
						t.Errorf("order %v: not counted as not judged (%s)", order, *exp.NotJudged)
					}
				}
				quiet++
				continue
			}
			if len(ev.Raised) != 1 || ev.Raised[0].Kind != alerting.KindConflict {
				t.Fatalf("order %v: raised %+v, want one conflict", order, ev.Raised)
			}
			d := ev.Raised[0].Detail
			vectors.Near(t, "t_cpa_s", d["t_cpa_s"].(float64), *exp.TCPAS, 0.01)
			vectors.Near(t, "d_cpa_horizontal_m", d["d_cpa_horizontal_m"].(float64), *exp.DCPAHorizontalM, 0.01)
			vectors.Near(t, "d_horizontal_now_m", d["d_horizontal_now_m"].(float64), *exp.DHorizontalNowM, 0.01)
			vectors.Near(t, "los_start_s", d["los_start_s"].(float64), *exp.LoSStartS, 0.01)
			if vk := d["vertical_separation_known"].(bool); vk != *exp.VerticalKnown {
				t.Errorf("vertical known %v, want %v", vk, *exp.VerticalKnown)
			}
			if exp.DAltAtCPAM == nil {
				if d["d_alt_at_cpa_m"] != nil {
					t.Errorf("d_alt_at_cpa_m %v, want null", d["d_alt_at_cpa_m"])
				}
			} else {
				vectors.Near(t, "d_alt_at_cpa_m", d["d_alt_at_cpa_m"].(float64), *exp.DAltAtCPAM, 0.01)
			}
			if ids := ev.Raised[0].Aircraft; unprefix(ids[0]) != "a" || unprefix(ids[1]) != "b" {
				t.Errorf("aircraft %v", ids)
			}
			conflicts++
		}
	})
	t.Logf("cpa.json through the adapter, both orders: %d conflicts raised, %d pairs raising nothing", conflicts, quiet)
}

// ---- alert_lifecycle.json ----

type lcAircraft struct {
	ID                  string               `json:"id"`
	NorthM              *float64             `json:"north_m"`
	EastM               *float64             `json:"east_m"`
	LatDeg              float64              `json:"lat_deg"`
	LonDeg              float64              `json:"lon_deg"`
	AltAMSLM            *float64             `json:"alt_amsl_m"`
	VN                  float64              `json:"vn"`
	VE                  float64              `json:"ve"`
	VD                  float64              `json:"vd"`
	Flying              *bool                `json:"flying"`
	CapturedAtS         *float64             `json:"captured_at_s"`
	RxAtS               *float64             `json:"rx_at_s"`
	StationClockOffsetS *float64             `json:"station_clock_offset_s"`
	Backlog             bool                 `json:"backlog"`
	Source              *string              `json:"source"`
	Station             *string              `json:"station"`
	AltSource           *string              `json:"alt_source"`
	Identification      *core.Identification `json:"identification"`
	Transmitter         *string              `json:"transmitter"`
	Identified          *bool                `json:"identified"`
}

type lcStep struct {
	TS         float64     `json:"t_s"`
	Op         string      `json:"op"`
	Aircraft   *lcAircraft `json:"aircraft"`
	SourceType string      `json:"source_type"`
	InstanceID *string     `json:"instance_id"`
	Enabled    *bool       `json:"enabled"`
}

type lcConfig struct {
	ClearAfterS      *float64          `json:"clear_after_s"`
	StaleAfterS      *float64          `json:"stale_after_s"`
	NeighbourMaxAgeS *float64          `json:"neighbour_max_age_s"`
	LiveMaxAgeS      *float64          `json:"live_max_age_s"`
	MaxAircraft      *int              `json:"max_aircraft"`
	MaxSourceShare   *float64          `json:"max_source_share"`
	Zones            []json.RawMessage `json:"zones"`
}

type lcInput struct {
	Config lcConfig `json:"config"`
	Steps  []lcStep `json:"steps"`
}

type lcAlert struct {
	Kind     string         `json:"kind"`
	Severity string         `json:"severity"`
	Aircraft []string       `json:"aircraft"`
	Detail   map[string]any `json:"detail"`
	Reason   string         `json:"reason"`
}

type lcExpected struct {
	PerStep []struct {
		Raised  []lcAlert `json:"raised"`
		Cleared []lcAlert `json:"cleared"`
	} `json:"per_step"`
	ActiveAfter []lcAlert         `json:"active_after"`
	Counters    map[string]uint64 `json:"counters"`
}

func (a *lcAircraft) track(tS float64) vecTrack {
	captured := tS
	if a.CapturedAtS != nil {
		captured = *a.CapturedAtS
	}
	rx := captured
	if a.RxAtS != nil {
		rx = *a.RxAtS
	}
	ts := captured
	if a.StationClockOffsetS != nil {
		ts += *a.StationClockOffsetS
	}
	alt := 550.0
	if a.AltAMSLM != nil {
		alt = *a.AltAMSLM
	}
	v := vecTrack{id: a.ID, source: telemetry.SourceOperatorWS, station: defaultStation, lat: a.LatDeg, lon: a.LonDeg,
		altM: &alt, altSource: core.AltGeodetic, vn: a.VN, ve: a.VE, vd: a.VD, flying: a.Flying,
		capturedS: captured, rxS: rx, tsS: ts, backlog: a.Backlog, identification: a.Identification}
	if a.AltSource != nil {
		v.altSource = core.AltSource(*a.AltSource)
	}
	if a.Source != nil {
		v.source = vectorSource[*a.Source]
	}
	if a.Station != nil {
		v.station = *a.Station
	}
	return v
}

// lcConfigOf is the monitor configuration of a case: ConfigOf under the
// policy values the case sets, the case's bounds on aircraft and the
// zones it judges (the zone path is WP-12's; core judges it here as it
// would there).
func lcConfigOf(t *testing.T, hp cpaVecPolicy, in lcConfig) alerting.Config {
	t.Helper()
	v := policy.Defaults()
	v.CPATCPAMaxS, v.CPAHorizontalMinM, v.CPAVerticalMinM = float64(hp.TCPAMaxS), float64(hp.DHorizontalMinM), float64(hp.DVerticalMinM)
	v.CPANeighbourRadiusM, v.CPANeighbourMaxAgeS = float64(hp.NeighbourRadiusM), 10
	set := func(dst *float64, src *float64) {
		if src != nil {
			*dst = *src
		}
	}
	set(&v.CPAClearAfterS, in.ClearAfterS)
	set(&v.CPAStaleAfterS, in.StaleAfterS)
	set(&v.CPANeighbourMaxAgeS, in.NeighbourMaxAgeS)
	set(&v.MonitorLiveMaxAgeS, in.LiveMaxAgeS)
	c := ConfigOf(v)
	if in.MaxAircraft != nil {
		c.MaxAircraft = *in.MaxAircraft
	}
	set(&c.MaxSourceShare, in.MaxSourceShare)
	for i, raw := range in.Zones {
		gz, problems := ed269.ParseZone(raw, ed269.Limits{})
		if problems != nil {
			t.Fatalf("zones[%d]: %v", i, problems)
		}
		z, err := zones.FromED269(gz)
		if err != nil {
			t.Fatalf("zones[%d]: %v", i, err)
		}
		c.Zones = append(c.Zones, z)
	}
	return c
}

func lcNormalised(a alerting.Alert, reason alerting.ClearReason) lcAlert {
	d := map[string]any{}
	for k, v := range a.Detail {
		if k == "los_start_s" && a.Kind == alerting.KindConflict {
			continue
		}
		if s, ok := v.([]string); ok {
			out := make([]any, len(s))
			for i := range s {
				out[i] = unprefix(s[i])
			}
			v = out
		}
		d[k] = v
	}
	ids := make([]string, len(a.Aircraft))
	for i := range a.Aircraft {
		ids[i] = unprefix(a.Aircraft[i])
	}
	slices.Sort(ids)
	return lcAlert{Kind: a.Kind, Severity: string(a.Severity), Aircraft: ids, Detail: d, Reason: string(reason)}
}

func lcValueEqual(got, want any) bool {
	switch w := want.(type) {
	case float64:
		g, ok := got.(float64)
		return ok && math.Abs(g-w) <= 0.05+1e-9
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	case nil:
		return got == nil
	}
	return got == want
}

func lcEqual(g, w lcAlert) bool {
	if g.Kind != w.Kind || g.Severity != w.Severity || g.Reason != w.Reason || !slices.Equal(g.Aircraft, w.Aircraft) || len(g.Detail) != len(w.Detail) {
		return false
	}
	for k, wv := range w.Detail {
		gv, ok := g.Detail[k]
		if !ok || !lcValueEqual(gv, wv) {
			return false
		}
	}
	return true
}

func lcCompare(t *testing.T, what string, got, want []lcAlert) {
	t.Helper()
	used := make([]bool, len(got))
	for _, w := range want {
		found := false
		for i, g := range got {
			if !used[i] && lcEqual(g, w) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			t.Errorf("%s: missing %+v (got %+v)", what, w, got)
		}
	}
	for i, g := range got {
		if !used[i] {
			t.Errorf("%s: unexpected %+v", what, g)
		}
	}
}

// lcCounter is a vector counter name under the wire's source names.
func lcCounter(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) == 3 {
		if s, ok := vectorSource[parts[1]]; ok {
			parts[1] = s
		}
	}
	return strings.Join(parts, "/")
}

func TestVectorsAlertLifecycleThroughTheMonitorAdapter(t *testing.T) {
	f := vectors.Load(t, "alert_lifecycle.json")
	var hp cpaVecPolicy
	f.Header(t, "policy", &hp)
	ran, skipped := 0, 0
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		if why, ok := noWire[c.Name]; ok {
			skipped++
			t.Skip(why)
		}
		ran++
		var in lcInput
		var exp lcExpected
		c.Decode(t, &in, &exp)
		m := alerting.NewMonitor(lcConfigOf(t, hp, in.Config))
		var st coresources.State
		for i, s := range in.Steps {
			var ev alerting.Events
			switch s.Op {
			case "observe":
				inp := s.Aircraft.track(s.TS).message(t)
				tr, _ := TrackOf(&inp)
				ev = m.Observe(tr, s.TS)
			case "tick":
				ev = m.Tick(s.TS)
			case "switch_source":
				st.Epoch, st.Version = "vectors", st.Version+1
				typ := vectorSource[s.SourceType]
				found := false
				for j := range st.Controls {
					if st.Controls[j].SourceType == typ && eqPtr(st.Controls[j].InstanceID, s.InstanceID) {
						st.Controls[j].Enabled, found = *s.Enabled, true
					}
				}
				if !found {
					st.Controls = append(st.Controls, coresources.Control{SourceType: typ, InstanceID: s.InstanceID, Enabled: *s.Enabled})
				}
				cp := st
				cp.Controls = slices.Clone(st.Controls)
				ev = m.SwitchSource(cp, s.TS)
			default:
				t.Fatalf("step %d: op %q", i, s.Op)
			}
			var raised, cleared []lcAlert
			for _, a := range ev.Raised {
				raised = append(raised, lcNormalised(a, ""))
			}
			for _, cl := range ev.Cleared {
				cleared = append(cleared, lcNormalised(cl.Alert, cl.Reason))
			}
			lcCompare(t, fmt.Sprintf("step %d raised", i), raised, exp.PerStep[i].Raised)
			lcCompare(t, fmt.Sprintf("step %d cleared", i), cleared, exp.PerStep[i].Cleared)
		}
		var active []lcAlert
		for _, a := range m.Active() {
			active = append(active, lcNormalised(a, ""))
		}
		lcCompare(t, "active_after", active, exp.ActiveAfter)
		for name, want := range exp.Counters {
			if g := m.Counters().Get(lcCounter(name)); g != want {
				t.Errorf("counter %s = %d, want %d", name, g, want)
			}
		}
	})
	if skipped != len(noWire) {
		t.Errorf("skipped %d, the wire gap lists %d", skipped, len(noWire))
	}
	t.Logf("alert_lifecycle.json through the adapter: %d cases ran, %d skipped (no transmitter on the wire)", ran, skipped)
}

func eqPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
