package conformance

import (
	"encoding/json"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/vectors"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// vectorsPath is the repo's conformance vector file.
const vectorsPath = "../../testdata/vectors/conformance.json"

type vecFixtures struct {
	Now            time.Time                  `json:"now"`
	Authorisations map[string]json.RawMessage `json:"authorisations"`
	SequenceConfig vecConfig                  `json:"sequence_config"`
	SampleDefaults map[string]any             `json:"sample_defaults"`
	FlightID       string                     `json:"flight_id"`
}

type vecConfig struct {
	ClearAfterS          float64 `json:"clear_after_s"`
	LostLinkS            float64 `json:"lost_link_s"`
	LiveMaxAgeS          float64 `json:"live_max_age_s"`
	AheadToleranceS      float64 `json:"ahead_tolerance_s"`
	PressureUncertaintyM float64 `json:"pressure_uncertainty_m"`
	PolicyVersion        int64   `json:"policy_version"`
}

type vecSample struct {
	LatDeg         float64  `json:"lat_deg"`
	LonDeg         float64  `json:"lon_deg"`
	AltAMSLM       *float64 `json:"alt_amsl_m"`
	AltSource      string   `json:"alt_source"`
	TS             float64  `json:"t_s"`
	Status         string   `json:"status"`
	CapturedAtS    *float64 `json:"captured_at_s"`
	RxAtS          *float64 `json:"rx_at_s"`
	Backlog        bool     `json:"backlog"`
	SourceDisabled bool     `json:"source_disabled"`
}

type vecVerdict struct {
	Outcome          Outcome `json:"outcome"`
	Reason           string  `json:"reason"`
	DistanceOutsideM float64 `json:"distance_outside_m"`
	HeightOverM      float64 `json:"height_over_m"`
	TimeOutsideS     float64 `json:"time_outside_s"`
	WithinThreshold  bool    `json:"within_threshold"`
	VerticalKnown    bool    `json:"vertical_known"`
	WithinBand       bool    `json:"within_band"`
	Error            *string `json:"error"`
}

type vecClear struct {
	Kind           string         `json:"kind"`
	Reason         string         `json:"reason"`
	Detail         map[string]any `json:"detail"`
	ClearingDetail map[string]any `json:"clearing_detail"`
}

type vecStep struct {
	State   State      `json:"state"`
	Raised  []string   `json:"raised"`
	Cleared []vecClear `json:"cleared"`
}

type vecActive struct {
	Kind   string         `json:"kind"`
	Detail map[string]any `json:"detail"`
}

type vecSeqExpected struct {
	PerStep     []vecStep         `json:"per_step"`
	ActiveAfter []vecActive       `json:"active_after"`
	Counters    map[string]uint64 `json:"counters"`
}

// only is f with the cases of one check, so each test runs its own and
// skips none.
func only(t *testing.T, f *vectors.File, check string) *vectors.File {
	t.Helper()
	g := *f
	g.Cases = nil
	for _, c := range f.Cases {
		var probe struct {
			Check string `json:"check"`
		}
		if err := json.Unmarshal(c.Input, &probe); err != nil {
			t.Fatal(err)
		}
		if probe.Check == check {
			g.Cases = append(g.Cases, c)
		}
	}
	return &g
}

func loadConformanceVectors(t *testing.T) (*vectors.File, vecFixtures) {
	t.Helper()
	f, err := vectors.Read(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 40 {
		t.Fatalf("%d cases; the brief asks for at least 40", len(f.Cases))
	}
	var fx vecFixtures
	if err := json.Unmarshal(f.Fixtures, &fx); err != nil {
		t.Fatal(err)
	}
	return f, fx
}

// stateBody is the fixture's intent_active value with the case's
// threshold override (an explicit null removes them).
func stateBody(t *testing.T, fx vecFixtures, name string, input map[string]json.RawMessage) intent.StateBody {
	t.Helper()
	raw, ok := fx.Authorisations[name]
	if !ok {
		t.Fatalf("no fixture authorisation %q", name)
	}
	var b intent.StateBody
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	if th, ok := input["thresholds"]; ok {
		b.DeviationThresholds = nil
		if string(th) != "null" {
			b.DeviationThresholds = &intent.Thresholds{}
			if err := json.Unmarshal(th, b.DeviationThresholds); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b
}

// track builds the track/telemetry/v1 message of a vector sample, so the
// sample reaches the judgement through InputOf as a monitor's does.
func track(t *testing.T, fx vecFixtures, s vecSample) *telemetry.Track {
	t.Helper()
	captured := s.TS
	if s.CapturedAtS != nil {
		captured = *s.CapturedAtS
	}
	rx := captured
	if s.RxAtS != nil {
		rx = *s.RxAtS
	}
	at := func(sec float64) time.Time { return fx.Now.Add(time.Duration(sec * float64(time.Second))) }
	status := s.Status
	if status == "" {
		status = "Airborne"
	}
	id := fx.FlightID
	tr := &telemetry.Track{
		Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer, core.Times{RxTS: at(rx), CapturedAt: at(captured),
			Source: core.TimeSourceClock, Backlog: s.Backlog}),
		Body: telemetry.TrackBody{
			TrackID: id, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS,
			Position: telemetry.Position{Lat: s.LatDeg, Lng: s.LonDeg}, AltAMSLM: s.AltAMSLM,
			AltSource: core.AltSource(s.AltSource), Status: &status, FlightID: &id,
		},
	}
	return tr
}

func near(got, want, tolM float64) bool { return math.Abs(got-want) <= tolM }

func horizontalTol(d float64) float64 { return 0.01 + 1e-4*math.Abs(d) }

func TestVectorsConformanceJudge(t *testing.T) {
	f, fx := loadConformanceVectors(t)
	ran := 0
	only(t, f, "judge").RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var input map[string]json.RawMessage
		if err := json.Unmarshal(c.Input, &input); err != nil {
			t.Fatal(err)
		}
		ran++
		var in struct {
			Check         string          `json:"check"`
			Authorisation string          `json:"authorisation"`
			Sample        vecSample       `json:"sample"`
			RawPolicy     json.RawMessage `json:"policy"`
			Thresholds    json.RawMessage `json:"thresholds"`
		}
		var want vecVerdict
		c.Decode(t, &in, &want)
		var pol struct {
			PressureUncertaintyM float64 `json:"pressure_uncertainty_m"`
		}
		vectors.Unmarshal(t, in.RawPolicy, &pol)
		body := stateBody(t, fx, in.Authorisation, input)
		auth, err := AuthorisationOf(body)
		var v Verdict
		if err == nil {
			v, err = Judge(InputOf(track(t, fx, in.Sample)).Sample, auth, Policy{PressureUncertaintyM: pol.PressureUncertaintyM})
		}
		if want.Error != nil {
			if err == nil || fieldOf(err) != *want.Error {
				t.Fatalf("error %v (field %q), want one naming %s", err, fieldOf(err), *want.Error)
			}
			return
		}
		if err != nil {
			t.Fatalf("judgement refused: %v", err)
		}
		if v.Outcome != want.Outcome || v.Reason != want.Reason || v.WithinThreshold != want.WithinThreshold ||
			v.VerticalKnown != want.VerticalKnown || v.WithinBand != want.WithinBand {
			t.Fatalf("verdict %+v, want %+v", v, want)
		}
		if !near(v.DistanceOutsideM, want.DistanceOutsideM, horizontalTol(want.DistanceOutsideM)) ||
			v.HeightOverM != want.HeightOverM || v.TimeOutsideS != want.TimeOutsideS {
			t.Fatalf("numbers %v %v %v, want %v %v %v", v.DistanceOutsideM, v.HeightOverM, v.TimeOutsideS,
				want.DistanceOutsideM, want.HeightOverM, want.TimeOutsideS)
		}
	})
	if ran < 40 {
		t.Errorf("%d judge cases ran", ran)
	}
}

// matches reports whether every listed detail field equals got's (a
// number within the horizontal tolerance).
func matches(t *testing.T, got, want map[string]any) bool {
	t.Helper()
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			return false
		}
		switch wv := w.(type) {
		case float64:
			gv, ok := g.(float64)
			if !ok || !near(gv, wv, horizontalTol(wv)) {
				return false
			}
		default:
			if g != w {
				return false
			}
		}
	}
	return true
}

func TestVectorsConformanceSequences(t *testing.T) {
	f, fx := loadConformanceVectors(t)
	cfg := Config(fx.SequenceConfig)
	ran := 0
	only(t, f, "sequence").RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var input map[string]json.RawMessage
		if err := json.Unmarshal(c.Input, &input); err != nil {
			t.Fatal(err)
		}
		ran++
		var in struct {
			Check         string          `json:"check"`
			Authorisation *string         `json:"authorisation"`
			Thresholds    json.RawMessage `json:"thresholds"`
			Steps         []struct {
				TS     float64    `json:"t_s"`
				Op     string     `json:"op"`
				Sample *vecSample `json:"sample"`
			} `json:"steps"`
		}
		var want vecSeqExpected
		c.Decode(t, &in, &want)
		if len(want.PerStep) != len(in.Steps) {
			t.Fatalf("%d steps, %d expectations", len(in.Steps), len(want.PerStep))
		}
		var auth *Authorisation
		var authErr error
		intentID := ""
		if in.Authorisation != nil {
			body := stateBody(t, fx, *in.Authorisation, input)
			intentID = body.IntentID
			a, err := AuthorisationOf(body)
			if err != nil {
				authErr = err
			} else {
				auth = &a
			}
		}
		counters := &core.Counters{}
		tr := NewTracker(fx.FlightID, intentID, "", counters)
		for i, st := range in.Steps {
			wall := fx.Now.Add(time.Duration(st.TS * float64(time.Second)))
			var ev Events
			switch st.Op {
			case "observe":
				s := *st.Sample
				s.TS = st.TS
				if s.AltSource == "" {
					s.AltSource = "geodetic"
				}
				if _, set := rawHas(c.Input, i, "alt_amsl_m"); !set {
					v := 550.0
					s.AltAMSLM = &v
				}
				inp := InputOf(track(t, fx, s))
				inp.SourceDisabled = s.SourceDisabled
				inp.Auth, inp.AuthErr = auth, authErr
				if auth == nil && authErr == nil {
					inp.MissingReason = UnknownAuthorisationMissing
				}
				ev = tr.Observe(inp, cfg, wall)
			case "tick":
				ev = tr.Tick(cfg, wall)
			case "drop":
				ev = tr.Drop(ClearFlightEnded, wall)
			default:
				t.Fatalf("step %d: unknown op %q", i, st.Op)
			}
			w := want.PerStep[i]
			if got := tr.Snapshot().State; got != w.State {
				t.Fatalf("step %d: state %s, want %s (events %+v)", i, got, w.State, ev)
			}
			var raised []string
			var cleared []AlertEvent
			for _, a := range ev.Alerts {
				switch a.State {
				case AlertRaised:
					raised = append(raised, a.Alert.Kind)
				case AlertCleared:
					cleared = append(cleared, a)
				}
			}
			if !slices.Equal(raised, w.Raised) && (len(raised) != 0 || len(w.Raised) != 0) {
				t.Fatalf("step %d: raised %v, want %v", i, raised, w.Raised)
			}
			if len(cleared) != len(w.Cleared) {
				t.Fatalf("step %d: cleared %+v, want %+v", i, cleared, w.Cleared)
			}
			for k, wc := range w.Cleared {
				g := cleared[k]
				if g.Alert.Kind != wc.Kind || g.ClearReason != wc.Reason {
					t.Fatalf("step %d: cleared %s %s, want %s %s", i, g.Alert.Kind, g.ClearReason, wc.Kind, wc.Reason)
				}
				if !matches(t, g.Alert.Detail, wc.Detail) || !matches(t, g.ClearingDetail, wc.ClearingDetail) {
					t.Fatalf("step %d: clear detail %v / %v, want %v / %v", i, g.Alert.Detail, g.ClearingDetail, wc.Detail, wc.ClearingDetail)
				}
			}
		}
		active := tr.Active()
		if len(active) != len(want.ActiveAfter) {
			t.Fatalf("active after %+v, want %+v", active, want.ActiveAfter)
		}
		for k, wa := range want.ActiveAfter {
			if active[k].Kind != wa.Kind || !matches(t, active[k].Detail, wa.Detail) {
				t.Fatalf("active %s %v, want %s %v", active[k].Kind, active[k].Detail, wa.Kind, wa.Detail)
			}
		}
		for name, n := range want.Counters {
			if got := counters.Get(name); got != n {
				t.Errorf("counter %s = %d, want %d", name, got, n)
			}
		}
	})
	if ran < 20 {
		t.Errorf("%d sequence cases ran", ran)
	}
}

// rawHas reports whether step i's sample in the raw input names key
// (an explicit null included), so a default applies only when absent.
func rawHas(input json.RawMessage, i int, key string) (json.RawMessage, bool) {
	var in struct {
		Steps []struct {
			Sample map[string]json.RawMessage `json:"sample"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(input, &in); err != nil || i >= len(in.Steps) {
		return nil, false
	}
	v, ok := in.Steps[i].Sample[key]
	return v, ok
}
