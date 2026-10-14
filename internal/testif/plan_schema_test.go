package testif

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	fp "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/flightplanning"
)

func schemaEnum(t *testing.T, schema, member string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "schemas", filepath.FromSlash(schema), "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]struct {
			Enum []any `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range s.Properties[member].Enum {
		if v, ok := v.(string); ok {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s has no enumeration %s", schema, member)
	}
	slices.Sort(out)
	return out
}

// Every state intent/decision/v1 can carry has a row, every row is such
// a state, and every Rejected row has a Completed twin (E-01); the
// actions are IntentPatch's.
func TestPlanTableCoversEveryDecision(t *testing.T) {
	states := schemaEnum(t, "intent/decision/v1", "state")
	if rows := slices.Sorted(maps.Keys(planTable)); !slices.Equal(rows, states) {
		t.Errorf("the table maps %v, intent/decision/v1 states are %v", rows, states)
	}
	var completed, rejected, failed bool
	for _, r := range planTable {
		completed = completed || r.result == fp.PlanningActivityResultCompleted
		rejected = rejected || r.result == fp.PlanningActivityResultRejected
		failed = failed || r.result == fp.PlanningActivityResultFailed
	}
	if !completed || !rejected || !failed {
		t.Errorf("the table lacks a result: completed %v rejected %v failed %v", completed, rejected, failed)
	}
	for _, s := range terminal {
		if !slices.Contains(states, s) {
			t.Errorf("terminal state %s is not a state of the schema", s)
		}
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	actions := doc.Components.Schemas["IntentPatch"].Properties["action"].Enum
	if !slices.Equal(actions, []string{actionActivate, actionModify, actionEnd}) {
		t.Errorf("IntentPatch actions %v", actions)
	}
}

// Every value the Annex IV tables write is one of intent/request/v1's,
// and the file's values each table leaves out are the ones that have no
// Annex IV meaning (Unknown, Undeclared, Other: refused or unlabelled).
func TestAnnexIVMappingIsTheSchemas(t *testing.T) {
	check := func(member string, written []string) {
		t.Helper()
		enum := schemaEnum(t, "intent/request/v1", member)
		for _, v := range written {
			if !slices.Contains(enum, v) {
				t.Errorf("%s: %q is not in the schema's %v", member, v, enum)
			}
		}
	}
	check("category", slices.Collect(maps.Values(categories)))
	check("mode", slices.Collect(maps.Values(modes)))
	check("class_label", slices.Collect(maps.Values(classes)))
	check("flight_type", []string{flightTypeNormal, flightTypeSpecial})
	check("subcategory", openSubcategories)
	fx, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	check("identification_technology", append(slices.Collect(maps.Values(fx.IdentificationTechnologies)), idTechBoth))
	if len(categories) != 3 || len(modes) != 2 || len(classes) != 7 {
		t.Errorf("tables %d %d %d", len(categories), len(modes), len(classes))
	}
	for _, left := range []bool{
		categories[fp.FlightAuthorisationDataOperationCategoryUnknown] == "",
		modes[fp.OperationModeUndeclared] == "",
		classes[fp.UASClassOther] == "",
	} {
		if !left {
			t.Error("a value without Annex IV meaning is mapped")
		}
	}
}
