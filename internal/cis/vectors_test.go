package cis

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed269"
	"github.com/rootxkit/uspace-core/ed318"
	"github.com/rootxkit/uspace-core/vectors"
	"github.com/rootxkit/uspace-core/zones"
)

// The core vectors that name ussp run through this package's adapter
// (CLAUDE.md testing rules): a zone reaches the judgement the way a
// pulled dataset does (an ED-318 collection as the CISP serves it,
// ParseVersion, buildEntries, an Evaluator) and is judged by
// Evaluator.JudgePoint, never by a second implementation.
//
// The two ED-269 files (zones_applicability, zones_vertical) give ED-269
// zones; the CIS publishes ED-318, so each zone is mapped as a publisher
// migrating from ED-269 maps it, by core's ed318.FromED269 (as the CISP's
// own vector test does). ED-318 requires a zone authority and an ED-269
// zone may have none: a zone without one gets a placeholder authority,
// which no judgement reads.

// fixturePoint is inside every zones_vertical zone (the file's header).
var fixturePoint = core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271}

const placeholderAuthority = `[{"name":"Vector authority","purpose":"AUTHORIZATION"}]`

// ed318FromED269Zone maps one ED-269 zone onto an ED-318 feature, as
// served: ed269.ParseZone, ed318.FromED269, ed318.Export.
func ed318FromED269Zone(t *testing.T, zone json.RawMessage, zoneType string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(zone, &m); err != nil {
		t.Fatal(err)
	}
	if a := bytes.TrimSpace(m["zoneAuthority"]); len(a) == 0 || string(a) == "[]" {
		m["zoneAuthority"] = json.RawMessage(placeholderAuthority)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	gz, probs := ed269.ParseZone(raw, ed269.Limits{})
	if probs != nil {
		t.Fatalf("ed269.ParseZone: %v", probs)
	}
	fc, err := ed318.FromED269(&ed269.Document{Zones: []ed269.GeoZone{*gz}}, ed318.Metadata{}, "en-GB")
	if err != nil {
		t.Fatalf("ed318.FromED269: %v", err)
	}
	if zoneType != "" {
		fc.Features[0].Properties.Type = core.ZoneType(zoneType)
	}
	feats, err := exportFeatures(fc)
	if err != nil {
		t.Fatal(err)
	}
	return feats[0]
}

// evaluatorWith installs one zones version holding feature.
func evaluatorWith(t *testing.T, feature json.RawMessage, pol zones.Policy) *Evaluator {
	t.Helper()
	clk := newClock()
	e := NewEvaluator(EvaluatorConfig{StaleS: func() float64 { return 300 }, Now: clk.Now, ZonesPolicy: func() zones.Policy { return pol }})
	v, rf := ParseVersion(Zones, collection(Zones, 1, feature), "", 0)
	if rf != nil {
		t.Fatalf("ParseVersion: %v", rf)
	}
	es, rf := buildEntries(v)
	if rf != nil {
		t.Fatalf("buildEntries: %v", rf)
	}
	e.install(v, es, clk.Now())
	return e
}

func TestVectorsZonesApplicability(t *testing.T) {
	f := vectors.Load(t, "zones_applicability.json")
	ran := 0
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		ran++
		var in struct {
			Applicability json.RawMessage `json:"applicability"`
			At            string          `json:"at"`
		}
		var exp struct {
			Applies bool `json:"applies"`
		}
		c.Decode(t, &in, &exp)
		at, err := time.Parse(time.RFC3339Nano, in.At)
		if err != nil {
			t.Fatal(err)
		}
		zone, _ := json.Marshal(map[string]any{
			"identifier": "TAP001", "country": "GEO", "name": "Applicability vector", "type": "COMMON",
			"restriction": "PROHIBITED", "reason": []string{"SENSITIVE"}, "applicability": in.Applicability,
			"zoneAuthority": json.RawMessage(placeholderAuthority),
			"geometry": []any{map[string]any{
				"uomDimensions": "M", "lowerLimit": 0, "lowerVerticalReference": "AMSL", "upperLimit": 1000, "upperVerticalReference": "AMSL",
				"horizontalProjection": map[string]any{"type": "Polygon", "coordinates": []any{[]any{
					[]any{44.80, 41.70}, []any{44.85, 41.70}, []any{44.85, 41.73}, []any{44.80, 41.73}, []any{44.80, 41.70},
				}}},
			}},
		})
		e := evaluatorWith(t, ed318FromED269Zone(t, zone, ""), zones.DefaultPolicy())
		res := e.JudgePoint(fixturePoint, geodetic(500), zones.Env{}, at)
		got := len(res.Zones) == 1 && res.Zones[0].Applicability == Applies
		if got != exp.Applies {
			t.Errorf("at %s: applies %v (zones %+v), want %v", in.At, got, res.Zones, exp.Applies)
		}
		if exp.Applies && res.Zones[0].Result.Raise == nil {
			t.Errorf("an applying PROHIBITED zone raised nothing at 500 m AMSL")
		}
	})
	t.Logf("%d zones_applicability cases ran through the evaluator", ran)
}

func TestVectorsZonesVertical(t *testing.T) {
	f := vectors.Load(t, "zones_vertical.json")
	ran := 0
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		ran++
		var in struct {
			Zone     json.RawMessage `json:"zone"`
			Aircraft struct {
				AltAMSLM  *float64 `json:"alt_amsl_m"`
				AltSource string   `json:"alt_source"`
			} `json:"aircraft"`
			Terrain              json.RawMessage `json:"terrain"`
			GeoidUndulationM     *float64        `json:"geoid_undulation_m"`
			MaxHeightAGLM        *float64        `json:"max_height_agl_m"`
			PressureUncertaintyM float64         `json:"pressure_uncertainty_m"`
			ConditionalSeverity  string          `json:"conditional_severity"`
			ZoneType             *string         `json:"zone_type"`
		}
		var exp struct {
			Raised []struct {
				Kind     string          `json:"kind"`
				Severity string          `json:"severity"`
				Aircraft []string        `json:"aircraft"`
				Detail   json.RawMessage `json:"detail"`
			} `json:"raised"`
			Counters map[string]uint64 `json:"counters"`
			Reasons  []string          `json:"reasons"`
		}
		c.Decode(t, &in, &exp)
		env := zones.Env{UndulationM: in.GeoidUndulationM}
		var word string
		if err := json.Unmarshal(in.Terrain, &word); err == nil {
			switch word {
			case "none":
				env.Ground = zones.GroundNotConfigured
			case "unknown here":
				env.Ground = zones.GroundUnknown
			default:
				t.Fatalf("terrain %q", word)
			}
		} else {
			var k struct {
				GroundM float64 `json:"ground_m"`
			}
			vectors.Unmarshal(t, in.Terrain, &k)
			env.Ground, env.GroundM = zones.GroundKnown, k.GroundM
		}
		pol := zones.DefaultPolicy()
		pol.PressureUncertaintyM = in.PressureUncertaintyM
		pol.ConditionalSeverity = core.Severity(in.ConditionalSeverity)
		ac := zones.Aircraft{AltAMSLM: in.Aircraft.AltAMSLM, AltSource: core.AltSource(in.Aircraft.AltSource)}

		var res zones.Result
		if b := bytes.TrimSpace(in.Zone); len(b) == 0 || string(b) == "null" {
			// The height limit: a U-space airspace's max_height_agl_m
			// through Evaluator.JudgeHeightLimit.
			e := NewEvaluator(EvaluatorConfig{ZonesPolicy: func() zones.Policy { return pol }})
			res = e.JudgeHeightLimit(ac, env, in.MaxHeightAGLM)
			checkCounters(t, e, exp.Counters)
		} else {
			zt := ""
			if in.ZoneType != nil {
				zt = *in.ZoneType
			}
			e := evaluatorWith(t, ed318FromED269Zone(t, in.Zone, zt), pol)
			pr := e.JudgePoint(fixturePoint, ac, env, time.Time{})
			if len(pr.Zones) != 1 {
				t.Fatalf("the fixture point is in %d zones: %+v", len(pr.Zones), pr)
			}
			res = pr.Zones[0].Result
			checkCounters(t, e, exp.Counters)
		}
		got := []string{}
		for _, r := range res.Reasons.List() {
			got = append(got, string(r))
		}
		want := slices.Clone(exp.Reasons)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("reasons %v, want %v", got, want)
		}
		if len(exp.Raised) == 0 {
			if res.Raise != nil {
				t.Fatalf("raised %+v, want nothing", *res.Raise)
			}
			return
		}
		w := exp.Raised[0]
		if res.Raise == nil {
			t.Fatalf("raised nothing (not evaluated %v), want %s %s", res.NotEvaluated, w.Kind, w.Severity)
		}
		if res.Raise.Kind != w.Kind || string(res.Raise.Severity) != w.Severity {
			t.Errorf("raised %s %s, want %s %s", res.Raise.Kind, res.Raise.Severity, w.Kind, w.Severity)
		}
		var wd zones.Detail
		vectors.Unmarshal(t, w.Detail, &wd)
		gd := res.Raise.Detail
		if gd.Identifier != wd.Identifier || gd.Restriction != wd.Restriction {
			t.Errorf("identifier/restriction %q %q, want %q %q", gd.Identifier, gd.Restriction, wd.Identifier, wd.Restriction)
		}
		eqBool(t, "vertical_known", gd.VerticalKnown, wd.VerticalKnown)
		eqBool(t, "within_band", gd.WithinBand, wd.WithinBand)
		eqBool(t, "limit_not_judged", gd.LimitNotJudged, wd.LimitNotJudged)
		if !slices.Equal(gd.NotJudged, wd.NotJudged) {
			t.Errorf("not_judged %v, want %v", gd.NotJudged, wd.NotJudged)
		}
		vectors.NearPtr(t, "height_agl_m", gd.HeightAGLM, wd.HeightAGLM, 0.1)
		vectors.NearPtr(t, "alt_hae_m", gd.AltHAEM, wd.AltHAEM, 0.1)
		vectors.NearPtr(t, "max_height_agl_m", gd.MaxHeightAGLM, wd.MaxHeightAGLM, 0.1)
	})
	t.Logf("%d zones_vertical cases ran through the evaluator", ran)
}

func checkCounters(t *testing.T, e *Evaluator, want map[string]uint64) {
	t.Helper()
	for name, n := range want {
		if got := e.Counters().Get(name); got != n {
			t.Errorf("counter %s = %d, want %d", name, got, n)
		}
	}
}

func eqBool(t *testing.T, field string, got, want *bool) {
	t.Helper()
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		t.Errorf("%s: got %v, want %v", field, got, want)
	}
}

// served adds the CISP's top-level members to an ED-318 document, as the
// CISP serves it.
func served(t *testing.T, doc json.RawMessage, d Dataset) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	m["cis_dataset"], _ = json.Marshal(string(d))
	m["cis_version"] = json.RawMessage("1")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestVectorsED318Roundtrip runs ed318_roundtrip.json's ussp cases
// through the ingest (ParseVersion and buildEntries: accepted whole and
// every zone built, or refused whole with the problem) and the
// evaluator's applicability. The ED-269 mapping cases (to_ed269,
// from_ed269) test a mapping the USSP never makes; for them the USSP's
// part is that the ED-318 side, the input of to_ed269 and the output of
// from_ed269, is accepted whole by this cache.
func TestVectorsED318Roundtrip(t *testing.T) {
	f := vectors.Load(t, "ed318_roundtrip.json")
	kinds := map[string]int{}
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var in struct {
			Kind          string                       `json:"kind"`
			Document      json.RawMessage              `json:"document"`
			ED269Document json.RawMessage              `json:"ed269_document"`
			Lang          string                       `json:"lang"`
			At            string                       `json:"at"`
			Where         *core.LatLon                 `json:"where"`
			Daylight      map[string]map[string]string `json:"daylight"`
		}
		var exp struct {
			Accepted    *bool           `json:"accepted"`
			Export      json.RawMessage `json:"export"`
			MustInclude *struct {
				FieldEndsWith  string `json:"field_endswith"`
				ReasonContains string `json:"reason_contains"`
			} `json:"must_include"`
			Mapped         *bool           `json:"mapped"`
			ED269          json.RawMessage `json:"ed269"`
			ED318          json.RawMessage `json:"ed318"`
			FieldEndsWith  string          `json:"field_endswith"`
			ReasonContains string          `json:"reason_contains"`
			Applies        *bool           `json:"applies"`
			NotEvaluated   *bool           `json:"not_evaluated"`
		}
		c.Decode(t, &in, &exp)
		kinds[in.Kind]++
		switch in.Kind {
		case "parse":
			v, rf := ParseVersion(Zones, served(t, in.Document, Zones), "", 0)
			if !*exp.Accepted {
				if rf == nil {
					t.Fatal("accepted, want refused")
				}
				m := exp.MustInclude
				for _, p := range rf.List {
					field, reason, _ := strings.Cut(p, ": ")
					if strings.HasSuffix(field, m.FieldEndsWith) && strings.Contains(reason, m.ReasonContains) {
						return
					}
				}
				t.Fatalf("no problem ending %q containing %q in %v", m.FieldEndsWith, m.ReasonContains, rf.List)
			}
			if rf != nil {
				t.Fatalf("refused: %v", rf)
			}
			if _, rf := buildEntries(v); rf != nil {
				t.Fatalf("accepted but not built: %v", rf)
			}
			out, err := ed318.Export(v.Collection)
			if err != nil {
				t.Fatal(err)
			}
			if !sameJSONWithout(t, out, exp.Export, "cis_dataset", "cis_version") {
				t.Errorf("export differs:\n got %s\nwant %s", out, exp.Export)
			}
		case "to_ed269":
			ingests(t, served(t, in.Document, Zones))
		case "from_ed269":
			ingests(t, served(t, exp.ED318, Zones))
		case "applies":
			v, rf := ParseVersion(Zones, served(t, in.Document, Zones), "", 0)
			if rf != nil {
				t.Fatalf("refused: %v", rf)
			}
			es, rf := buildEntries(v)
			if rf != nil {
				t.Fatalf("not built: %v", rf)
			}
			table := ed318.FixedDaylight{}
			for date, evs := range in.Daylight {
				table[date] = map[string]time.Time{}
				for ev, s := range evs {
					tm, err := time.Parse(time.RFC3339, s)
					if err != nil {
						t.Fatal(err)
					}
					table[date][ev] = tm
				}
			}
			e := NewEvaluator(EvaluatorConfig{Daylight: table})
			at, err := time.Parse(time.RFC3339, in.At)
			if err != nil {
				t.Fatal(err)
			}
			a, aerr := e.applicability(es[0], 0, at)
			if (a == Applies) != *exp.Applies {
				t.Errorf("applies %v, want %v (%v)", a, *exp.Applies, aerr)
			}
			if (a == Unknown) != *exp.NotEvaluated {
				t.Errorf("unknown %v, want not_evaluated %v (%v)", a == Unknown, *exp.NotEvaluated, aerr)
			}
			if aerr != nil && !strings.Contains(aerr.Error(), exp.ReasonContains) {
				t.Errorf("reason %q does not contain %q", aerr, exp.ReasonContains)
			}
		default:
			t.Fatalf("unknown kind %q", in.Kind)
		}
	})
	t.Logf("ed318_roundtrip cases by kind: %v", kinds)
}

// ingests asserts the cache accepts body whole and builds every zone.
func ingests(t *testing.T, body []byte) {
	t.Helper()
	v, rf := ParseVersion(Zones, body, "", 0)
	if rf != nil {
		t.Fatalf("the cache refuses the ED-318 side: %v", rf)
	}
	es, rf := buildEntries(v)
	if rf != nil || len(es) != len(v.Collection.Features) {
		t.Fatalf("built %d of %d: %v", len(es), len(v.Collection.Features), rf)
	}
}

func sameJSONWithout(t *testing.T, a, b []byte, drop ...string) bool {
	t.Helper()
	var x, y map[string]any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	for _, k := range drop {
		delete(x, k)
		delete(y, k)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
