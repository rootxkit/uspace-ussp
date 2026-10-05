package conformance

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

var (
	tt0    = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	origin = core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271}
)

const (
	flightA = "5b3f1d2e-7c4a-4e8b-9f10-2a3b4c5d6e7f"
	flightB = "6c4f1d2e-7c4a-4e8b-9f10-2a3b4c5d6e70"
	intentA = "8d0e7b51-3c1e-4a5f-9a43-0b6f4c2a7e01"
)

func circleAuth() Authorisation {
	return Authorisation{
		IntentID: intentA, AuthorisationNumber: "USSP-DEV-1",
		Volumes: []Volume{{Shape: deconflict.Shape{Circle: &geodesy.Circle{Center: origin, RadiusM: 500}},
			LowerAMSLM: 500, UpperAMSLM: 600, Start: tt0.Add(-time.Hour), End: tt0.Add(time.Hour)}},
		Thresholds: Thresholds{HM: 50, VM: 15, TS: 60},
	}
}

// fieldOf is the field a *core.FieldError names, "" for any other error.
func fieldOf(err error) string {
	var fe *core.FieldError
	if errors.As(err, &fe) {
		return fe.Field
	}
	return ""
}

func testConfig() Config {
	return ConfigOf(policy.Record{Version: 3, Values: policy.Defaults()})
}

func alt(v float64) *float64 { return &v }

func flying(v bool) *bool { return &v }

func at(s float64) time.Time { return tt0.Add(time.Duration(s * float64(time.Second))) }

func input(p core.LatLon, s float64, a *Authorisation) Input {
	return Input{Sample: Sample{Position: p, AltAMSLM: alt(550), AltSource: core.AltGeodetic, CapturedAt: at(s)},
		Flying: flying(true), RxAt: at(s), Auth: a, Cell5: "c5:1317:2248"}
}

func TestConfigOfAndValidate(t *testing.T) {
	c := testConfig()
	if c.ClearAfterS != 3 || c.LostLinkS != 15 || c.LiveMaxAgeS != 10 || c.PolicyVersion != 3 || c.PressureUncertaintyM != 250 {
		t.Fatalf("%+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*Config){
		func(c *Config) { c.ClearAfterS = 0 },
		func(c *Config) { c.LostLinkS = math.NaN() },
		func(c *Config) { c.LiveMaxAgeS = -1 },
		func(c *Config) { c.AheadToleranceS = -1 },
		func(c *Config) { c.PressureUncertaintyM = math.Inf(1) },
		func(c *Config) { c.ClearAfterS, c.AheadToleranceS = 2, 1 },
	} {
		c := testConfig()
		bad(&c)
		if c.Validate() == nil {
			t.Errorf("%+v accepted", c)
		}
		// An invalid configuration judges nothing and is counted (E-15).
		tr := NewTracker(flightA, intentA, "", nil)
		a := circleAuth()
		ev := tr.Observe(input(origin, 0, &a), c, at(0))
		if ev.Admitted || ev.Refusal != "config_invalid" || tr.Counters.Get(CounterConfigInvalid) != 1 {
			t.Errorf("observe on an invalid config: %+v", ev)
		}
		tr.Tick(c, at(30))
		if tr.Counters.Get(CounterConfigInvalid) != 2 || tr.Snapshot().State != StateUnknown {
			t.Errorf("tick on an invalid config: %v", tr.Snapshot())
		}
	}
}

func TestValidateRefusesUnjudgeableAuthorisations(t *testing.T) {
	cases := map[string]func(*Authorisation){
		"volumes":           func(a *Authorisation) { a.Volumes = nil },
		"volumes[0].band":   func(a *Authorisation) { a.Volumes[0].LowerAMSLM = 700 },
		"volumes[0].window": func(a *Authorisation) { a.Volumes[0].End = a.Volumes[0].Start.Add(-time.Second) },
		"volumes[0].outline": func(a *Authorisation) {
			a.Volumes[0].Shape = deconflict.Shape{}
		},
	}
	for field, mut := range cases {
		a := circleAuth()
		mut(&a)
		if err := a.Validate(); err == nil || fieldOf(err) != field {
			t.Errorf("%s: %v", field, err)
		}
	}
	a := circleAuth()
	a.Volumes = make([]Volume, MaxVolumes+1)
	if err := a.Validate(); fieldOf(err) != "volumes" {
		t.Errorf("over the bound: %v", err)
	}
	// E-10 pair: the bound itself is accepted.
	a = circleAuth()
	for len(a.Volumes) < MaxVolumes {
		a.Volumes = append(a.Volumes, a.Volumes[0])
	}
	if err := a.Validate(); err != nil {
		t.Errorf("at the bound: %v", err)
	}
	if _, err := Judge(Sample{Position: origin}, circleAuth(), Policy{}); fieldOf(err) != "captured_at" {
		t.Errorf("no time: %v", err)
	}
}

func TestFlyingOf(t *testing.T) {
	s := func(v string) *string { return &v }
	for in, want := range map[string]*bool{"Airborne": flying(true), "Emergency": flying(true), "Ground": flying(false),
		"Undeclared": nil, "RemoteIDSystemFailure": nil, "nonsense": nil} {
		got := FlyingOf(s(in))
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("%s: %v", in, got)
		}
	}
	if FlyingOf(nil) != nil {
		t.Error("no status flies")
	}
}

func stateBodyOf(t *testing.T) intent.StateBody {
	t.Helper()
	var b intent.StateBody
	raw := `{"intent_id":"` + intentA + `","authorisation_number":"USSP-DEV-1","deviation_thresholds":{"h_m":50,"v_m":15,"t_s":60},
	"volumes":[{"volume":{"outline_circle":{"center":{"lat":41.7151,"lng":44.8271},"radius":{"value":500,"units":"M"}},
	"altitude_lower":{"value":520,"reference":"W84","units":"M"},"altitude_upper":{"value":620,"reference":"W84","units":"M"}},
	"time_start":{"value":"2026-11-01T11:00:00Z","format":"RFC3339"},"time_end":{"value":"2026-11-01T13:00:00Z","format":"RFC3339"}}],
	"volumes_amsl":[{"lower_amsl_m":500,"upper_amsl_m":600,"undulation_m":20,"lower_w84_m":520,"upper_w84_m":620}]}`
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAuthorisationOf(t *testing.T) {
	a, err := AuthorisationOf(stateBodyOf(t))
	if err != nil || a.AuthorisationNumber != "USSP-DEV-1" || len(a.Volumes) != 1 || a.Volumes[0].UpperAMSLM != 600 ||
		a.Volumes[0].Shape.Circle == nil || a.Thresholds.TS != 60 {
		t.Fatalf("%+v %v", a, err)
	}
	for field, mut := range map[string]func(*intent.StateBody){
		"deviation_thresholds": func(b *intent.StateBody) { b.DeviationThresholds = nil },
		"volumes":              func(b *intent.StateBody) { b.Volumes = nil },
		"volumes_amsl":         func(b *intent.StateBody) { b.VolumesAMSL = nil },
		"volumes[0]":           func(b *intent.StateBody) { b.Volumes[0].Volume.OutlineCircle = nil },
		"volumes[0].band": func(b *intent.StateBody) {
			b.VolumesAMSL[0].LowerAMSLM = 900
		},
	} {
		b := stateBodyOf(t)
		mut(&b)
		if _, err := AuthorisationOf(b); fieldOf(err) != field {
			t.Errorf("%s: %v", field, err)
		}
	}
	b := stateBodyOf(t)
	b.Volumes[0].TimeEnd = &f3548.Time{Value: tt0.Add(-2 * time.Hour)}
	if _, err := AuthorisationOf(b); err == nil {
		t.Error("a window ending before it starts read")
	}
}

// The nonconformance_nearby fan-out: a flight 500 m away receives it and
// loses it when the source clears; a flight 5 km away receives nothing
// (E-01 pair); stale, grounded and the source itself are skipped.
func TestNearbyPairs(t *testing.T) {
	n := &Nearby{}
	src := Alert{ID: "src-1", Kind: KindNonconformance, FlightID: flightA, CapturedAt: tt0}
	near := Neighbour{FlightID: flightB, Position: geodesy.Destination(origin, 0, 500), SeenAt: tt0, Flying: true, Cell5: "c5:1:1"}
	far := Neighbour{FlightID: "far", Position: geodesy.Destination(origin, 0, 5000), SeenAt: tt0, Flying: true}
	stale := Neighbour{FlightID: "stale", Position: origin, SeenAt: tt0.Add(-time.Minute), Flying: true}
	ground := Neighbour{FlightID: "ground", Position: origin, SeenAt: tt0, Flying: false}
	self := Neighbour{FlightID: flightA, Position: origin, SeenAt: tt0, Flying: true}
	ev := n.Refresh(src, origin, []Neighbour{near, far, stale, ground, self}, 2000, 15*time.Second, tt0, 4)
	if len(ev) != 1 || ev[0].Alert.FlightID != flightB || ev[0].Alert.Kind != KindNonconformanceNearby ||
		ev[0].Alert.Severity != core.SeverityWarning || ev[0].State != AlertRaised || ev[0].Alert.PolicyVersion != 4 {
		t.Fatalf("raised %+v", ev)
	}
	if d, _ := ev[0].Alert.Detail["distance_m"].(float64); math.Abs(d-500) > 0.01 {
		t.Errorf("distance %v", ev[0].Alert.Detail["distance_m"])
	}
	if n.Counters.Get(CounterNearbyNeighbourStale) != 1 {
		t.Error("stale neighbour not counted")
	}
	// Refreshed, not raised again.
	if again := n.Refresh(src, origin, []Neighbour{near}, 2000, 15*time.Second, tt0.Add(time.Second), 4); len(again) != 0 {
		t.Fatalf("raised again: %+v", again)
	}
	if a := n.Active(); len(a) != 1 || !a[0].UpdatedAt.Equal(tt0.Add(time.Second)) {
		t.Fatalf("active %+v", a)
	}
	cl := n.Clear("src-1", ClearResolved, tt0.Add(5*time.Second))
	if len(cl) != 1 || cl[0].State != AlertCleared || cl[0].ClearReason != ClearResolved || len(n.Active()) != 0 {
		t.Fatalf("cleared %+v", cl)
	}
	// An unusable radius raises nothing and is counted (E-15).
	if ev := n.Refresh(src, origin, []Neighbour{near}, 0, time.Minute, tt0, 4); len(ev) != 0 || n.Counters.Get(CounterNearbyUnmeasured) != 1 {
		t.Fatalf("zero radius: %+v", ev)
	}
	// A neighbour that ends loses its alert as flight_ended.
	n.Refresh(src, origin, []Neighbour{near}, 2000, time.Minute, tt0, 4)
	if d := n.DropFlight(flightB, tt0); len(d) != 1 || d[0].ClearReason != ClearFlightEnded {
		t.Fatalf("drop %+v", d)
	}
}

// E-10: past MaxNearbyPerSource the rest are counted, the nearest kept.
func TestNearbyBound(t *testing.T) {
	n := &Nearby{}
	src := Alert{ID: "src", Kind: KindLostLink, FlightID: flightA}
	var nbs []Neighbour
	for i := range MaxNearbyPerSource + 3 {
		nbs = append(nbs, Neighbour{FlightID: "f" + string(rune('a'+i%26)) + strings.Repeat("x", i/26),
			Position: geodesy.Destination(origin, 0, float64(10+i)), SeenAt: tt0, Flying: true})
	}
	ev := n.Refresh(src, origin, nbs, 2000, time.Minute, tt0, 1)
	if len(ev) != MaxNearbyPerSource || n.Counters.Get(CounterNearbyOverBound) != 3 {
		t.Fatalf("%d raised, %d over", len(ev), n.Counters.Get(CounterNearbyOverBound))
	}
	if d, _ := ev[0].Alert.Detail["distance_m"].(float64); d > 11 {
		t.Errorf("nearest not first: %v", d)
	}
}

func TestStateMessageRoundTrip(t *testing.T) {
	tr := NewTracker(flightA, intentA, "USSP-DEV-1", nil)
	a := circleAuth()
	ev := tr.Observe(input(geodesy.Destination(origin, 90, 600), 0, &a), testConfig(), at(0))
	if len(ev.Transitions) != 1 || ev.Transitions[0].To != StateNonconforming {
		t.Fatalf("%+v", ev)
	}
	m := StateMessageOf(tr.Snapshot(), ev, core.Times{RxTS: at(0), CapturedAt: at(0), Source: core.TimeSourceClock}, 3)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	b := back.Body
	if b.State != StateNonconforming || !b.Transition || b.PreviousState == nil || *b.PreviousState != StateUnknown ||
		!b.Judged || b.DistanceOutsideM == nil || math.Abs(*b.DistanceOutsideM-100) > 0.02 || *b.Reason != ReasonThresholdExceeded ||
		b.PolicyVersion != 3 || *b.AuthorisationNumber != "USSP-DEV-1" || b.Position == nil {
		t.Fatalf("%+v", b)
	}
	// A sample whose vertical did not run carries no height, never 0.
	tr2 := NewTracker(flightA, intentA, "", nil)
	in := input(origin, 0, &a)
	in.Sample.AltSource, in.Sample.AltAMSLM = core.AltNone, nil
	ev = tr2.Observe(in, testConfig(), at(0))
	m = StateMessageOf(tr2.Snapshot(), ev, core.Times{RxTS: at(0), CapturedAt: at(0), Source: core.TimeSourceClock}, 3)
	if m.Body.HeightOverM != nil || m.Body.VerticalKnown == nil || *m.Body.VerticalKnown || m.Body.State != StateUnknown {
		t.Fatalf("vertical not evaluated: %+v", m.Body)
	}
}

func TestDecodeStateRefusals(t *testing.T) {
	good := StateMessageOf(Snapshot{FlightID: flightA, State: StateConforming, BaseState: StateConforming}, Events{},
		core.Times{RxTS: tt0, CapturedAt: tt0, Source: core.TimeSourceClock}, 1)
	mut := func(f func(*StateMessage)) []byte {
		c := *good
		f(&c)
		raw, _ := json.Marshal(c)
		return raw
	}
	neg := -1.0
	bad := map[string][]byte{
		"not json":     []byte("{"),
		"schema":       mut(func(m *StateMessage) { m.Schema = "alert/v1" }),
		"flight":       mut(func(m *StateMessage) { m.Body.FlightID = "x" }),
		"intent":       mut(func(m *StateMessage) { s := "x"; m.Body.IntentID = &s }),
		"state":        mut(func(m *StateMessage) { m.Body.State = "fine" }),
		"base":         mut(func(m *StateMessage) { m.Body.BaseState = StateLostLink }),
		"previous":     mut(func(m *StateMessage) { s := State("x"); m.Body.PreviousState = &s }),
		"reason":       mut(func(m *StateMessage) { s := strings.Repeat("r", 65); m.Body.Reason = &s }),
		"negative":     mut(func(m *StateMessage) { m.Body.DistanceOutsideM = &neg }),
		"envelope":     mut(func(m *StateMessage) { m.MsgID = "x" }),
		"over a bound": make([]byte, MaxMessageBytes+1),
	}
	for name, raw := range bad {
		if _, err := DecodeState(raw); err == nil {
			t.Errorf("%s decoded", name)
		}
	}
	raw, _ := json.Marshal(good)
	if _, err := DecodeState(raw); err != nil {
		t.Errorf("the good one refused: %v", err)
	}
}

func TestAlertMessageOf(t *testing.T) {
	a := Alert{ID: "a", Kind: KindLostLink, Severity: core.SeverityCritical, FlightID: flightA, IntentID: intentA,
		RaisedAt: tt0, UpdatedAt: tt0, CapturedAt: tt0, PolicyVersion: 2}
	m := AlertMessageOf(AlertEvent{State: AlertCleared, ClearReason: ClearFlightEnded, Alert: a}, tt0)
	if m.Body.Detail == nil || *m.Body.ClearReason != ClearFlightEnded || m.Body.State != AlertCleared || m.Body.AuthorisationNumber != nil ||
		m.Schema != SchemaAlert || m.Validate() != nil {
		t.Fatalf("%+v", m)
	}
}

func trackMessage(t *testing.T, mut func(*telemetry.Track)) []byte {
	t.Helper()
	id, status := flightA, "Airborne"
	tr := &telemetry.Track{
		Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer, core.Times{RxTS: tt0, CapturedAt: tt0, Source: core.TimeSourceClock}),
		Body: telemetry.TrackBody{TrackID: id, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS,
			Position: telemetry.Position{Lat: origin.LatDeg, Lng: origin.LonDeg}, AltAMSLM: alt(550), AltSource: core.AltGeodetic,
			Status: &status, FlightID: &id},
	}
	if mut != nil {
		mut(tr)
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeTrack(t *testing.T) {
	tr, ours, err := DecodeTrack(trackMessage(t, nil))
	if err != nil || !ours || *tr.Body.FlightID != flightA {
		t.Fatalf("%v %v", ours, err)
	}
	in := InputOf(tr)
	if in.Cell5 == "" || in.Flying == nil || !*in.Flying || !in.Sample.CapturedAt.Equal(tt0) {
		t.Fatalf("%+v", in)
	}
	for name, mut := range map[string]func(*telemetry.Track){
		"peer":       func(tr *telemetry.Track) { tr.Body.Trust = core.TrustProvider },
		"no flight":  func(tr *telemetry.Track) { tr.Body.FlightID = nil },
		"other feed": func(tr *telemetry.Track) { tr.Body.Source = "remote_id" },
	} {
		if _, ours, err := DecodeTrack(trackMessage(t, mut)); ours || err != nil {
			t.Errorf("%s: ours %v err %v", name, ours, err)
		}
	}
	bad := "x"
	for name, mut := range map[string]func(*telemetry.Track){
		"schema":   func(tr *telemetry.Track) { tr.Schema = "x/v1" },
		"flight":   func(tr *telemetry.Track) { tr.Body.FlightID = &bad },
		"intent":   func(tr *telemetry.Track) { tr.Body.IntentID = &bad },
		"position": func(tr *telemetry.Track) { tr.Body.Position.Lat = 95 },
		"envelope": func(tr *telemetry.Track) { tr.MsgID = "" },
	} {
		if _, _, err := DecodeTrack(trackMessage(t, mut)); err == nil {
			t.Errorf("%s decoded", name)
		}
	}
	if _, _, err := DecodeTrack([]byte("[")); err == nil {
		t.Error("not JSON decoded")
	}
	if _, _, err := DecodeTrack(make([]byte, bus.TrackMsgBytes+1)); err == nil {
		t.Error("over the bound decoded")
	}
}

// Lost link: the presence and absence pair at the bound, the lost_link
// alert's detail, and a Drop that clears both alerts as flight_ended.
func TestLostLinkBoundAndDrop(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	tr := NewTracker(flightA, intentA, "", nil)
	tr.Observe(input(geodesy.Destination(origin, 90, 600), 0, &a), cfg, at(0))
	if ev := tr.Tick(cfg, at(14.9)); len(ev.Alerts) != 0 {
		t.Fatalf("lost before 15 s: %+v", ev)
	}
	ev := tr.Tick(cfg, at(15))
	if len(ev.Alerts) != 1 || ev.Alerts[0].Alert.Kind != KindLostLink || tr.Snapshot().State != StateLostLink {
		t.Fatalf("not lost at 15 s: %+v", ev)
	}
	if got := tr.Active(); len(got) != 2 {
		t.Fatalf("active %+v", got)
	}
	ev = tr.Drop("", at(20))
	if len(ev.Alerts) != 2 || ev.Alerts[0].ClearReason != ClearFlightEnded || ev.Alerts[1].ClearReason != ClearFlightEnded {
		t.Fatalf("drop %+v", ev)
	}
	if ev := tr.Drop(ClearResolved, at(21)); len(ev.Alerts) != 0 {
		t.Fatal("cleared twice")
	}
}

// A sample the judgement refuses neither refreshes nor clears (C-09).
func TestJudgementFailureHoldsTheState(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	tr := NewTracker(flightA, intentA, "", nil)
	tr.Observe(input(geodesy.Destination(origin, 90, 600), 0, &a), cfg, at(0))
	broken := circleAuth()
	broken.Thresholds.HM = 0
	ev := tr.Observe(input(origin, 5, &broken), cfg, at(5))
	if ev.Unjudged != "judgement_failed" || len(ev.Alerts) != 0 || tr.Snapshot().State != StateNonconforming ||
		tr.Counters.Get(CounterJudgementFailed) != 1 {
		t.Fatalf("%+v %v", ev, tr.Snapshot())
	}
	// The AuthErr path of an intent that does not read: counted, held.
	in := input(origin, 6, nil)
	in.AuthErr = core.Fieldf("deviation_thresholds", "missing")
	if ev := tr.Observe(in, cfg, at(6)); ev.Unjudged != "judgement_failed" || tr.Snapshot().State != StateNonconforming {
		t.Fatalf("%+v", ev)
	}
	// An invalid position is refused before admission.
	in = input(core.LatLon{LatDeg: math.NaN()}, 7, &a)
	if ev := tr.Observe(in, cfg, at(7)); ev.Admitted || ev.Refusal != "invalid" {
		t.Fatalf("%+v", ev)
	}
}

// A flight that has an authorisation but whose projection was never
// read is unknown with that reason, never conforming (SC-22); the
// authorisation appearing resolves it at the next sample.
func TestProjectionUnavailableThenResolves(t *testing.T) {
	cfg := testConfig()
	tr := NewTracker(flightA, intentA, "", nil)
	in := input(origin, 0, nil)
	in.MissingReason = UnknownProjectionUnavailable
	tr.Observe(in, cfg, at(0))
	if s := tr.Snapshot(); s.State != StateUnknown || s.Reason != UnknownProjectionUnavailable {
		t.Fatalf("%+v", s)
	}
	a := circleAuth()
	ev := tr.Observe(input(origin, 1, &a), cfg, at(1))
	if s := tr.Snapshot(); s.State != StateConforming || len(ev.Transitions) != 1 || ev.Transitions[0].From != StateUnknown {
		t.Fatalf("%+v %+v", s, ev)
	}
}

func FuzzDecodeState(f *testing.F) {
	good := StateMessageOf(Snapshot{FlightID: flightA, State: StateConforming, BaseState: StateConforming}, Events{},
		core.Times{RxTS: tt0, CapturedAt: tt0, Source: core.TimeSourceClock}, 1)
	raw, _ := json.Marshal(good)
	f.Add(raw)
	f.Add([]byte(`{"schema":"conformance/state/v1","body":{"flight_id":"` + flightA + `","state":"x"}}`))
	f.Add([]byte("null"))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := DecodeState(data)
		if err == nil && (m.Schema != SchemaState || !states[m.Body.State]) {
			t.Fatalf("accepted %+v", m)
		}
	})
}

func FuzzDecodeTrack(f *testing.F) {
	f.Add(trackMessage(&testing.T{}, nil))
	f.Add([]byte(`{"schema":"track/telemetry/v1","body":{"position":{"lat":1e308,"lng":-1e308}}}`))
	f.Fuzz(func(_ *testing.T, data []byte) {
		tr, ours, err := DecodeTrack(data)
		if err != nil || !ours {
			return
		}
		// Whatever decodes is judged without a panic.
		a := circleAuth()
		NewTracker(*tr.Body.FlightID, "", "", nil).Observe(func() Input { in := InputOf(tr); in.Auth = &a; return in }(), testConfig(), tt0)
	})
}

// An intent_active value is read by AuthorisationOf and judged: nothing
// a writer of that bucket puts there panics the judgement.
func FuzzAuthorisationOf(f *testing.F) {
	raw, _ := json.Marshal(stateBodyOfFuzz())
	f.Add(raw, 41.7151, 44.8271, 550.0)
	f.Add([]byte(`{"volumes":[{"volume":{"outline_polygon":{"vertices":[]}}}],"volumes_amsl":[{}],"deviation_thresholds":{"h_m":1,"v_m":1,"t_s":1}}`),
		0.0, 0.0, 0.0)
	f.Fuzz(func(_ *testing.T, data []byte, lat, lon, altM float64) {
		var b intent.StateBody
		if json.Unmarshal(data, &b) != nil {
			return
		}
		a, err := AuthorisationOf(b)
		if err != nil {
			return
		}
		_, _ = Judge(Sample{Position: core.LatLon{LatDeg: lat, LonDeg: lon}, AltAMSLM: &altM, AltSource: core.AltPressure, CapturedAt: tt0},
			a, Policy{PressureUncertaintyM: 250})
	})
}

func stateBodyOfFuzz() intent.StateBody {
	var b intent.StateBody
	_ = json.Unmarshal([]byte(`{"intent_id":"`+intentA+`","deviation_thresholds":{"h_m":50,"v_m":15,"t_s":60},
	"volumes":[{"volume":{"outline_polygon":{"vertices":[{"lat":41.7,"lng":44.8},{"lat":41.7,"lng":44.81},{"lat":41.71,"lng":44.81}]},
	"altitude_lower":{"value":520,"reference":"W84","units":"M"},"altitude_upper":{"value":620,"reference":"W84","units":"M"}},
	"time_start":{"value":"2026-11-01T11:00:00Z","format":"RFC3339"},"time_end":{"value":"2026-11-01T13:00:00Z","format":"RFC3339"}}],
	"volumes_amsl":[{"lower_amsl_m":500,"upper_amsl_m":600,"undulation_m":20}]}`), &b)
	return b
}

// C-05: a sample whose status says neither flying nor on the ground
// (Undeclared) does not land the aircraft: a flight last seen airborne
// still raises lost_link when it falls silent after it; one last seen on
// the ground does not (E-01 pair).
func TestFlyingUnknownKeepsTheFlyingState(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	for _, tc := range []struct {
		name  string
		first bool
		lost  bool
	}{{"airborne then undeclared", true, true}, {"ground then undeclared", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker(flightA, intentA, "", nil)
			tr.Observe(input(origin, 0, &a), cfg, at(0)) // judged: authorised
			in := input(origin, 1, &a)
			in.Flying = flying(tc.first)
			tr.Observe(in, cfg, at(1))
			in = input(origin, 2, &a)
			in.Flying = nil
			if ev := tr.Observe(in, cfg, at(2)); ev.Unjudged != "flying_unknown" {
				t.Fatalf("%+v", ev)
			}
			if got := tr.Snapshot().LastFlying; got != tc.first {
				t.Fatalf("last flying %v after an undeclared sample, want %v", got, tc.first)
			}
			ev := tr.Tick(cfg, at(2+cfg.LostLinkS))
			if lost := len(ev.Alerts) == 1 && ev.Alerts[0].Alert.Kind == KindLostLink; lost != tc.lost {
				t.Fatalf("lost_link %v, want %v: %+v", lost, tc.lost, ev)
			}
		})
	}
}

// SC-22: a flight whose vertical has never been evaluated (no altitude,
// or alt_source none) is unknown with that reason, never conforming;
// the first sample with an altitude judges it (E-01 pair), and a flight
// already judged keeps its state on an undetermined sample (C-09).
func TestNoAltitudeEverStaysUnknown(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	tr := NewTracker(flightA, intentA, "", nil)
	for i := range 3 {
		in := input(origin, float64(i), &a)
		in.Sample.AltAMSLM, in.Sample.AltSource = nil, core.AltNone
		tr.Observe(in, cfg, at(float64(i)))
		if s := tr.Snapshot(); s.State != StateUnknown || s.Reason != UnknownVerticalNotEvaluated {
			t.Fatalf("sample %d: %+v", i, s)
		}
	}
	ev := tr.Observe(input(origin, 3, &a), cfg, at(3))
	if s := tr.Snapshot(); s.State != StateConforming || len(ev.Transitions) != 1 || ev.Transitions[0].From != StateUnknown {
		t.Fatalf("%+v %+v", s, ev)
	}
	in := input(origin, 4, &a)
	in.Sample.AltAMSLM, in.Sample.AltSource = nil, core.AltNone
	if ev := tr.Observe(in, cfg, at(4)); tr.Snapshot().State != StateConforming || len(ev.Transitions) != 0 {
		t.Fatalf("an undetermined sample moved a judged flight: %+v", ev)
	}
}

// A restart or a handover: a tracker restored from its persisted state
// keeps the flight nonconforming with the same alert, and returns it to
// conforming, clearing that alert resolved, only after the full
// hysteresis counted from the restore, whatever the saved state said of
// the time already spent inside; a restored conforming flight stays
// conforming (E-01 pair).
func TestRestoreNeedsTheFullHysteresis(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	old := NewTracker(flightA, intentA, "", nil)
	old.Observe(input(geodesy.Destination(origin, 90, 600), 0, &a), cfg, at(0))
	old.Observe(input(origin, 1, &a), cfg, at(1))
	old.Observe(input(origin, 2, &a), cfg, at(2))
	raised := old.Active()[0].ID
	saved := old.State()
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var back TrackerState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	tr := RestoreTracker(back, nil, at(3.5))
	if s := tr.Snapshot(); s.State != StateNonconforming || len(tr.Active()) != 1 || tr.Active()[0].ID != raised || tr.Active()[0].Cell5 == "" {
		t.Fatalf("restored %+v %+v", s, tr.Active())
	}
	// A sample outside newer than the saved state but older than the
	// restore (the track replay at a start) does not move the hysteresis
	// back before the restore.
	tr.Observe(input(geodesy.Destination(origin, 90, 600), 2.5, &a), cfg, at(3.5))
	// 3.5 s since the last sample outside, but only 0.5 s and then
	// 2.5 s since the restore: held.
	for _, s := range []float64{4, 6} {
		if ev := tr.Observe(input(origin, s, &a), cfg, at(s)); len(ev.Transitions) != 0 || len(ev.Alerts) != 0 {
			t.Fatalf("returned %v s after the restore, before the hysteresis: %+v", s-3.5, ev)
		}
	}
	ev := tr.Observe(input(origin, 3.5+cfg.ClearAfterS+0.1, &a), cfg, at(3.5+cfg.ClearAfterS+0.1))
	if tr.Snapshot().State != StateConforming || len(ev.Alerts) != 1 || ev.Alerts[0].Alert.ID != raised || ev.Alerts[0].ClearReason != ClearResolved {
		t.Fatalf("%+v", ev)
	}
	ok := RestoreTracker(tr.State(), nil, at(20))
	if s := ok.Snapshot(); s.State != StateConforming || len(ok.Active()) != 0 {
		t.Fatalf("%+v", s)
	}
	if ev := ok.Observe(input(origin, 21, &a), cfg, at(21)); len(ev.Transitions) != 0 {
		t.Fatalf("%+v", ev)
	}
}

// A fresh tracker (no persisted state) whose intent is nonconforming or
// contingent takes that state before its first judgement: an inside
// sample does not show conforming; nonconforming returns after the full
// hysteresis, contingent never (F3548). An activated intent is judged
// as before (E-01 pair).
func TestFreshTrackerTakesTheIntentState(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	for _, tc := range []struct {
		intent string
		first  State
		after  State
	}{{IntentNonconforming, StateNonconforming, StateConforming}, {IntentContingent, StateContingent, StateContingent}, {"activated", StateConforming, StateConforming}} {
		t.Run(tc.intent, func(t *testing.T) {
			tr := NewTracker(flightA, intentA, "", nil)
			in := input(origin, 0, &a)
			in.IntentState = tc.intent
			tr.Observe(in, cfg, at(0))
			if s := tr.Snapshot(); s.State != tc.first {
				t.Fatalf("first sample: %+v", s)
			}
			in = input(origin, cfg.ClearAfterS+0.5, &a)
			in.IntentState = tc.intent
			tr.Observe(in, cfg, at(cfg.ClearAfterS+0.5))
			if s := tr.Snapshot(); s.State != tc.after {
				t.Fatalf("after the hysteresis: %+v", s)
			}
		})
	}
	// Seeded before any sample: the same.
	tr := NewTracker(flightA, intentA, "", nil)
	if ev := tr.SeedFromIntent(IntentNonconforming, at(0)); len(ev.Transitions) != 1 || tr.Snapshot().Reason != ReasonRestored {
		t.Fatalf("%+v", ev)
	}
	if ev := tr.Observe(input(origin, 1, &a), cfg, at(1)); len(ev.Transitions) != 0 {
		t.Fatalf("%+v", ev)
	}
}

// Q35: while the monitor's own input is down no silence is the
// aircraft's, so no lost_link; after the input is back the silence
// counts from its return, so lost_link comes LostLinkS later when no
// live sample came, and never when one did. Its twin with the input up
// is TestLostLinkBoundAndDrop.
func TestNoLostLinkWhileTheInputIsDown(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	tr := NewTracker(flightA, intentA, "", nil)
	tr.Observe(input(origin, 0, &a), cfg, at(0))
	for s := 1.0; s <= 60; s++ {
		if ev := tr.TickFeed(cfg, at(s), Feed{Down: true}); len(ev.Alerts) != 0 || len(ev.Transitions) != 0 {
			t.Fatalf("at %v s with the input down: %+v", s, ev)
		}
	}
	if tr.Snapshot().State != StateConforming {
		t.Fatalf("state %s after the outage, want conforming", tr.Snapshot().State)
	}
	back := Feed{BackAt: at(60)}
	if ev := tr.TickFeed(cfg, at(74.9), back); len(ev.Alerts) != 0 {
		t.Fatalf("lost before lost_link_s after the input came back: %+v", ev)
	}
	ev := tr.TickFeed(cfg, at(75), back)
	if len(ev.Alerts) != 1 || ev.Alerts[0].Alert.Kind != KindLostLink || tr.Snapshot().State != StateLostLink {
		t.Fatalf("no lost_link lost_link_s after the input came back without a sample: %+v", ev)
	}
	// The aircraft is heard again: the lost_link clears on its sample.
	ev = tr.Observe(input(origin, 76, &a), cfg, at(76))
	if len(ev.Alerts) != 1 || ev.Alerts[0].State != AlertCleared || ev.Alerts[0].ClearReason != ClearResolved {
		t.Fatalf("the sample after the return did not clear lost_link: %+v", ev)
	}
}

// A flight heard live after the input came back is judged from those
// samples: no lost_link while they come.
func TestSamplesAfterTheInputReturnsKeepTheLink(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	tr := NewTracker(flightA, intentA, "", nil)
	tr.Observe(input(origin, 0, &a), cfg, at(0))
	tr.TickFeed(cfg, at(30), Feed{Down: true})
	back := Feed{BackAt: at(40)}
	for s := 41.0; s <= 100; s += 5 {
		tr.Observe(input(origin, s, &a), cfg, at(s))
		if ev := tr.TickFeed(cfg, at(s+1), back); len(ev.Alerts) != 0 {
			t.Fatalf("at %v s: %+v", s+1, ev)
		}
	}
	if ev := tr.TickFeed(cfg, at(111), back); len(ev.Alerts) != 1 || ev.Alerts[0].Alert.Kind != KindLostLink {
		t.Fatalf("a real silence after the return raised nothing: %+v", ev)
	}
}

// A lost_link raised before the input went down is held through the
// outage and refreshed: nothing unjudged clears an alert.
func TestALostLinkIsHeldThroughAnOutage(t *testing.T) {
	cfg := testConfig()
	a := circleAuth()
	tr := NewTracker(flightA, intentA, "", nil)
	tr.Observe(input(origin, 0, &a), cfg, at(0))
	if ev := tr.Tick(cfg, at(15)); len(ev.Alerts) != 1 {
		t.Fatalf("%+v", ev)
	}
	if ev := tr.TickFeed(cfg, at(20), Feed{Down: true}); len(ev.Alerts) != 0 || !tr.Snapshot().LinkLost {
		t.Fatalf("the outage changed a raised lost_link: %+v", ev)
	}
	if got := tr.Active(); len(got) != 1 || got[0].Detail["silence_s"] != 20.0 {
		t.Fatalf("active %+v", got)
	}
}
