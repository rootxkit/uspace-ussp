package weather

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

// answerSchema compiles WeatherAnswer of api/openapi.yaml.
func answerSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("https://ussp.test/openapi.json", inst); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("https://ussp.test/openapi.json#/components/schemas/WeatherAnswer")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validate(t *testing.T, s *jsonschema.Schema, a Answer) {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("the answer does not validate against WeatherAnswer: %v\n%s", err, b)
	}
}

// E-03: the answer the service writes is the one the contract
// publishes, never fetched, fresh with every field kind, and failing.
func TestAnswerMatchesTheContract(t *testing.T) {
	ctx := context.Background()
	s := answerSchema(t)
	r := newRig(t, "VRB03G25KT 260V320 1 1/2SM +TSRA BR FEW008CB VV008 22/// A2992 TEMPO FM1400 TL1530 TSRA BKN015CB")
	a, err := r.svc.Answer(ctx, tbilisi, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, s, a)
	if err := r.svc.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if a, err = r.svc.Answer(ctx, tbilisi, time.Time{}); err != nil || len(a.Products) != 2 {
		t.Fatal(a, err)
	}
	validate(t, s, a)
	r.f.Down()
	_ = r.svc.Poll(ctx)
	if a, err = r.svc.Answer(ctx, tbilisi, time.Time{}); err != nil || a.Source.Failure == nil {
		t.Fatal(a, err)
	}
	validate(t, s, a)
}
