package schemas_test

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const idBase = "https://schemas.uspace.ge/"

type schemaFile struct {
	name string // intent/request/v1
	dir  string
	doc  any
}

func load(t *testing.T) []schemaFile {
	t.Helper()
	var out []schemaFile
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "schema.json" {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		dir := filepath.Dir(p)
		out = append(out, schemaFile{name: filepath.ToSlash(dir), dir: dir, doc: doc})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// offline refuses every URL that was not added as a resource: no schema
// is fetched from the network.
type offline struct{}

func (offline) Load(string) (any, error) { return nil, fs.ErrNotExist }

func examples(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// Every schema is draft 2020-12 with the $id and title of spec 04 §1,
// and has valid and invalid examples; every valid example validates and
// every invalid one is refused (LESSONS E-01).
func TestSchemasAndExamplesBothWays(t *testing.T) {
	files := load(t)
	if len(files) == 0 {
		t.Fatal("no schema.json under schemas/")
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(offline{})
	for _, f := range files {
		m, _ := f.doc.(map[string]any)
		if id, _ := m["$id"].(string); id != idBase+f.name+".json" {
			t.Errorf("%s: $id %q", f.name, id)
		}
		if title, _ := m["title"].(string); title != f.name {
			t.Errorf("%s: title %q", f.name, title)
		}
		if s, _ := m["$schema"].(string); s != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s: $schema %q", f.name, s)
		}
		if err := c.AddResource(idBase+f.name+".json", f.doc); err != nil {
			t.Fatal(err)
		}
	}
	valid, invalid := 0, 0
	for _, f := range files {
		sch, err := c.Compile(idBase + f.name + ".json")
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		for _, want := range []bool{true, false} {
			dir := filepath.Join(f.dir, "examples")
			if !want {
				dir = filepath.Join(dir, "invalid")
			}
			es := examples(t, dir)
			if len(es) == 0 {
				t.Errorf("%s: no examples in %s", f.name, dir)
			}
			for _, e := range es {
				raw, err := os.ReadFile(e)
				if err != nil {
					t.Fatal(err)
				}
				inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					t.Fatalf("%s: %v", e, err)
				}
				err = sch.Validate(inst)
				switch {
				case want && err != nil:
					t.Errorf("%s does not validate: %v", e, err)
				case !want && err == nil:
					t.Errorf("%s validates but is in invalid/", e)
				case want:
					valid++
				default:
					invalid++
					t.Logf("refused %s: %s", path.Base(filepath.ToSlash(e)), strings.ReplaceAll(err.Error(), "\n", " "))
				}
			}
		}
	}
	t.Logf("%d schemas, %d valid examples validated, %d invalid examples refused", len(files), valid, invalid)
}
