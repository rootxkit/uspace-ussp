package intent

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/vectors"

	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

const vectorsPath = "../../testdata/vectors/deconfliction.json"

type vecIntent struct {
	ID          string           `json:"id"`
	Priority    int              `json:"priority"`
	FiledAt     time.Time        `json:"filed_at"`
	UndulationM float64          `json:"undulation_m"`
	Volumes     []f3548.Volume4D `json:"volumes"`
}

// adapt reads a vector intent through this package's adapter (the one
// stored and peer intents go through).
func adapt(t *testing.T, in vecIntent) deconflict.Intent {
	t.Helper()
	out := deconflict.Intent{ID: in.ID, Priority: in.Priority, RankAt: in.FiledAt}
	for _, v := range in.Volumes {
		dv, err := wireVolume(v, in.UndulationM)
		if err != nil {
			t.Fatal(err)
		}
		out.Volumes = append(out.Volumes, dv)
	}
	return out
}

type pairPolicy struct {
	BufferM         float64 `json:"buffer_m"`
	VerticalBufferM float64 `json:"vertical_buffer_m"`
}

type pairExpected struct {
	Conflict *bool   `json:"conflict"`
	Winner   *string `json:"winner"`
	Rule     *string `json:"rule"`
	Error    *string `json:"error"`
}

type decisionExpected struct {
	Decision            string   `json:"decision"`
	State               string   `json:"state"`
	Reasons             []string `json:"reasons"`
	Conditions          []string `json:"conditions"`
	AuthorisationNumber bool     `json:"authorisation_number"`
	DeviationThresholds bool     `json:"deviation_thresholds"`
	InUSpaceAirspace    bool     `json:"in_uspace_airspace"`
}

type fixtures struct {
	Now         time.Time       `json:"now"`
	UndulationM float64         `json:"undulation_m"`
	SystemID    string          `json:"system_id"`
	Request     json.RawMessage `json:"request"`
}

// TestVectorsDeconfliction runs every case of the repo's vector file
// through this package: pair cases through the stored-intent adapter
// and deconflict.Check both ways, decision cases through Decide with
// the case's CIS, registry, terrain, DSS and existing intents.
func TestVectorsDeconfliction(t *testing.T) {
	f, err := vectors.Read(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var fx fixtures
	if err := vectors.StrictUnmarshal(f.Fixtures, &fx); err != nil {
		t.Fatal(err)
	}
	pairs, decisions := 0, 0
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var probe struct {
			Check string `json:"check"`
		}
		if err := json.Unmarshal(c.Input, &probe); err != nil {
			t.Fatal(err)
		}
		switch probe.Check {
		case "pair":
			pairs++
			runPair(t, c)
		case "decision":
			decisions++
			runDecision(t, c, fx)
		default:
			t.Fatalf("unknown check %q", probe.Check)
		}
	})
	t.Logf("deconfliction.json: %d pair cases, %d decision cases, %d in all", pairs, decisions, len(f.Cases))
	if pairs < 30 || decisions < 15 {
		t.Fatalf("%d pair and %d decision cases ran", pairs, decisions)
	}
}

func runPair(t *testing.T, c vectors.Case) {
	var raw struct {
		Check  string     `json:"check"`
		Policy pairPolicy `json:"policy"`
		A      vecIntent  `json:"a"`
		B      vecIntent  `json:"b"`
	}
	var want pairExpected
	c.Decode(t, &raw, &want)
	p := deconflict.Policy{HorizontalBufferM: raw.Policy.BufferM, VerticalBufferM: raw.Policy.VerticalBufferM}
	a, b := adapt(t, raw.A), adapt(t, raw.B)
	ab, e1 := deconflict.Check(a, []deconflict.Intent{b}, p)
	ba, e2 := deconflict.Check(b, []deconflict.Intent{a}, p)
	if want.Error != nil {
		if e1 == nil || e2 == nil || !strings.Contains(e1.Error(), *want.Error) {
			t.Fatalf("errors %v / %v, want %s", e1, e2, *want.Error)
		}
		return
	}
	if e1 != nil || e2 != nil || (len(ab) == 1) != *want.Conflict || (len(ba) == 1) != *want.Conflict {
		t.Fatalf("%v %v / %v %v, want conflict %v", ab, e1, ba, e2, *want.Conflict)
	}
	if *want.Conflict {
		winner := "b"
		if ab[0].MineWins {
			winner = "a"
		}
		if winner != *want.Winner || ab[0].MineWins == ba[0].MineWins || ab[0].Rule != *want.Rule {
			t.Fatalf("winner %s %+v / %+v, want %s %s", winner, ab[0], ba[0], *want.Winner, *want.Rule)
		}
	}
}

func merge(t *testing.T, base, over json.RawMessage) map[string]any {
	t.Helper()
	var m, o map[string]any
	if err := json.Unmarshal(base, &m); err != nil {
		t.Fatal(err)
	}
	if len(over) > 0 {
		if err := json.Unmarshal(over, &o); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range o {
		m[k] = v
	}
	return m
}

func runDecision(t *testing.T, c vectors.Case, fx fixtures) {
	var dec vecDecision
	var want decisionExpected
	c.Decode(t, &dec, &want)

	pol := policy.Record{Version: 1, Values: policy.Defaults()}
	if len(dec.Policy) > 0 {
		if err := json.Unmarshal(dec.Policy, &pol.Values); err != nil {
			t.Fatal(err)
		}
	}
	req, err := Decode(encode(t, merge(t, fx.Request, dec.Request)))
	if err != nil {
		t.Fatal(err)
	}
	n, probs, err := Validate(req, ValidateEnv{Geoid: fakeGeoid{n: fx.UndulationM}, Now: fx.Now, SpecialPriority: pol.Values.SpecialOperationPriority})
	if err != nil || probs != nil {
		t.Fatalf("validate: %v %v", err, probs)
	}
	fc := &fakeCIS{basis: freshBasis()}
	if dec.CIS.Stale {
		fc.basis.Stale, fc.basis.CISAgeS = true, 900
	}
	for i := range dec.CIS.Features {
		ft := &dec.CIS.Features[i]
		fc.features = append(fc.features, ft.candidate(t))
	}
	// registry: a status per entity (operator, uas, pilot), the reason
	// of an unknown answer, and what it holds for the UAS.
	reg := &fakeRegistry{status: map[string]registry.Status{}, reason: dec.Registry["reason"],
		classLabel: dec.Registry["uas_class_label"], mtomBand: dec.Registry["uas_mtom_band"]}
	for k, v := range dec.Registry {
		switch k {
		case "reason", "uas_class_label", "uas_mtom_band":
		default:
			reg.status[k] = registry.Status(v)
		}
	}
	// The vectors judge deconfliction and predate weather (WP-16): their
	// world has a weather product in force, so no weather condition is
	// added to what they pin.
	d := &Decider{CIS: fc, Integrity: fakeIntegrity{out: dec.CIS.Outdated}, Registry: reg, DSS: fakeDSS{ok: dec.DSSAvailable, reason: "the DSS is down (vector)"},
		Weather: &fakeWeather{}, SystemID: fx.SystemID, Counters: &core.Counters{}}
	if dec.Terrain != nil {
		d.Terrain = fakeTerrain{minM: dec.Terrain.MinM, maxM: dec.Terrain.MaxM, ok: true}
	}
	others := make([]deconflict.Intent, 0, len(dec.Existing))
	for i := range dec.Existing {
		others = append(others, adapt(t, dec.Existing[i]))
	}
	got, flagged := d.Decide(t.Context(), n, pol, fx.Now, "00000000-0000-4000-8000-00000000000a", fx.Now, others)
	gotReasons := reasons(got)
	var conds []string
	for _, x := range got.Conditions {
		conds = append(conds, x.Code)
	}
	sort.Strings(conds)
	if got.Decision != want.Decision || got.State != want.State || !slices.Equal(gotReasons, want.Reasons) || !slices.Equal(conds, want.Conditions) ||
		(got.AuthorisationNumber != nil) != want.AuthorisationNumber || (got.DeviationThresholds != nil) != want.DeviationThresholds ||
		got.InUSpaceAirspace != want.InUSpaceAirspace {
		t.Fatalf("got %s/%s reasons %v conditions %v number %v thresholds %v inside %v\nwant %+v\nconflicts %+v",
			got.Decision, got.State, gotReasons, conds, got.AuthorisationNumber, got.DeviationThresholds, got.InUSpaceAirspace, want, got.Conflicts)
	}
	if got.AuthorisationNumber != nil && !strings.HasPrefix(*got.AuthorisationNumber, fx.SystemID+"-GEOTESTOP0001-") {
		t.Errorf("authorisation number %s", *got.AuthorisationNumber)
	}
	// Every refusal names what refused: the item, the zone, the airspace,
	// the intent or the registry key.
	for _, x := range got.Conflicts {
		if x.Effect == EffectRejects && x.Item == nil && x.Ref == "" && x.Kind != KindCIS && x.Kind != KindPolicy {
			t.Errorf("a refusal names nothing: %+v", x)
		}
	}
	if (len(flagged) > 0) != slices.Contains(want.Reasons, ReasonIntentFlagged) {
		t.Errorf("flagged %v", flagged)
	}
}

type vecDecision struct {
	Check   string          `json:"check"`
	Policy  json.RawMessage `json:"policy"`
	Request json.RawMessage `json:"request"`
	CIS     struct {
		Stale    bool      `json:"stale"`
		Outdated []string  `json:"outdated"`
		Features []feature `json:"features"`
	} `json:"cis"`
	Registry map[string]string `json:"registry"`
	Terrain  *struct {
		MinM float64 `json:"min_m"`
		MaxM float64 `json:"max_m"`
	} `json:"terrain"`
	DSSAvailable bool        `json:"dss_available"`
	Existing     []vecIntent `json:"existing"`
}
