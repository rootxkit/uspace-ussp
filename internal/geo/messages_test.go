package geo

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/cis"
)

// schema compiles schemas/<name> with every schema of the repository as
// a resource (offline).
func schema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	root := filepath.Join("..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "schema.json" {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		return c.AddResource("https://schemas.uspace.ge/"+filepath.ToSlash(rel)+".json", doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("https://schemas.uspace.ge/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validate(t *testing.T, s *jsonschema.Schema, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("%s\n%v", raw, err)
	}
	return raw
}

// geo/changed/v1 validates, reads back, and refuses what is not one.
func TestChangedMessage(t *testing.T) {
	c := cis.Change{Dataset: cis.Restrictions, Version: 9, PreviousVersion: 8, FeatureIDs: []string{"TRS001"}, Reason: cis.ChangeInstalled,
		CISVersion: "zones:4,uspace_airspace:2,restrictions:9", At: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	raw := validate(t, schema(t, "geo/changed/v1"), ChangedMessageOf(c, c.At))
	m, err := DecodeChanged(raw)
	if err != nil || m.Body.Dataset != cis.Restrictions || m.Body.FeatureIDs[0] != "TRS001" {
		t.Fatalf("%+v %v", m, err)
	}
	for name, mut := range map[string]func(*cis.Change){
		"dataset": func(c *cis.Change) { c.Dataset = "weather" },
		"version": func(c *cis.Change) { c.Version = 0 },
		"reason":  func(c *cis.Change) { c.Reason = "other" },
		"id":      func(c *cis.Change) { c.FeatureIDs = []string{""} },
		"many":    func(c *cis.Change) { c.FeatureIDs = make([]string, cis.MaxChangedFeatures+1) },
	} {
		b := c
		mut(&b)
		raw, _ := json.Marshal(ChangedMessageOf(b, c.At))
		if _, err := DecodeChanged(raw); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, raw := range [][]byte{[]byte(`{`), []byte(`{"schema":"alert/v1"}`), bytes.Repeat([]byte(" "), MaxChangedBytes+1)} {
		if _, err := DecodeChanged(raw); err == nil {
			t.Errorf("%q accepted", raw[:min(len(raw), 20)])
		}
	}
}

func FuzzDecodeChanged(f *testing.F) {
	c := cis.Change{Dataset: cis.Zones, Version: 1, FeatureIDs: []string{"TZP001"}, Reason: cis.ChangeInstalled, At: time.Unix(0, 0).UTC()}
	raw, _ := json.Marshal(ChangedMessageOf(c, c.At))
	f.Add(raw)
	f.Add([]byte(`{"schema":"geo/changed/v1","body":{"dataset":"zones","version":1,"feature_ids":null}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := DecodeChanged(data)
		if err == nil && (m.Body.Version < 1 || m.Body.FeatureIDs == nil) {
			t.Fatalf("accepted %+v", m.Body)
		}
	})
}

// The fan-out: every subscriber gets each change; a full subscriber
// loses its oldest, counted (E-10); a cancelled one gets nothing; an
// unreadable cis.v1 message is counted.
func TestChangesFanOut(t *testing.T) {
	c := &Changes{}
	a, cancelA := c.Subscribe()
	b, cancelB := c.Subscribe()
	cancelB()
	for v := int64(1); v <= SubscriberQueue+3; v++ {
		c.Publish(cis.Change{Dataset: cis.Zones, Version: v})
	}
	if got := len(a); got != SubscriberQueue {
		t.Fatalf("held %d", got)
	}
	if first := <-a; first.Version != 4 || c.Counters.Get(CounterChangesDropped) != 3 {
		t.Fatalf("oldest kept: %d, dropped %d", first.Version, c.Counters.Get(CounterChangesDropped))
	}
	if len(b) != 0 {
		t.Fatal("a cancelled subscriber got changes")
	}
	cancelA()
	c.Take([]byte(`{`))
	if c.Counters.Get(CounterChangesUnread) != 1 {
		t.Fatal("unreadable not counted")
	}
	ch := cis.Change{Dataset: cis.Zones, Version: 2, Reason: cis.ChangeInstalled, FeatureIDs: []string{"X"}}
	raw, _ := json.Marshal(ChangedMessageOf(ch, time.Now()))
	s, cancel := c.Subscribe()
	defer cancel()
	c.Take(raw)
	if got := <-s; got.Version != 2 {
		t.Fatalf("%+v", got)
	}
}
