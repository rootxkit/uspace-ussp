package interuss_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/flightplanning"
	"github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/ridobservation"
)

const pinned = "../../../api/standards/interuss"

// source reads api/standards/interuss/SOURCE: file -> key -> value.
func source(t *testing.T) map[string]map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pinned, "SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	var cur map[string]string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			cur = map[string]string{}
			out[strings.Trim(line, "[]")] = cur
		case cur != nil && strings.Contains(line, " = "):
			k, v, _ := strings.Cut(line, " = ")
			cur[k] = v
		}
	}
	return out
}

// Every pinned file is named in SOURCE with its SHA-256, at the one
// commit the suite reads, and nothing under api/standards/interuss is
// left without a section (E-03). The negative twin: a file whose bytes
// differ from SOURCE fails (a hash of other bytes is not the recorded one).
func TestPinnedFilesAreTheSourceFiles(t *testing.T) {
	src := source(t)
	if len(src) == 0 {
		t.Fatal("SOURCE names no file")
	}
	var onDisk []string
	err := filepath.WalkDir(pinned, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") || strings.HasPrefix(d.Name(), "oapi-codegen.") {
			return err
		}
		rel, _ := filepath.Rel(pinned, p)
		onDisk = append(onDisk, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	commits := map[string]bool{}
	for _, f := range onDisk {
		sec, ok := src[f]
		if !ok {
			t.Errorf("%s has no section in SOURCE", f)
			continue
		}
		b, err := os.ReadFile(filepath.Join(pinned, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != sec["sha256"] {
			t.Errorf("%s hashes to %s, SOURCE records %s", f, got, sec["sha256"])
		}
		other := sha256.Sum256(append(slices.Clone(b), '\n'))
		if hex.EncodeToString(other[:]) == sec["sha256"] {
			t.Errorf("%s: a changed file still matches SOURCE", f)
		}
		if !strings.HasSuffix(sec["url"], "/"+sec["commit"]+"/"+f) || sec["path"] != f {
			t.Errorf("%s: url %s or path %s is not the file at commit %s", f, sec["url"], sec["path"], sec["commit"])
		}
		commits[sec["commit"]] = true
	}
	for f := range src {
		if !slices.Contains(onDisk, f) {
			t.Errorf("SOURCE names %s, which is not pinned", f)
		}
	}
	if len(commits) != 1 {
		t.Errorf("the files come from %d commits; the suite reads one: %v", len(commits), commits)
	}
	for _, want := range []string{"rid/v1/injection.yaml", "rid/v1/observation.yaml", "flight_planning/v1/flight_planning.yaml"} {
		if !slices.Contains(onDisk, want) {
			t.Errorf("%s is not pinned", want)
		}
	}
	if slices.ContainsFunc(onDisk, func(f string) bool { return strings.HasPrefix(f, "scd/") }) {
		t.Error("scd v1 is pinned; the brief builds flight_planning v1 only while the suite drives it")
	}
}

// operations reads the (method, path) pairs of a pinned file.
func operations(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pinned, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for p, ops := range doc.Paths {
		for m := range ops {
			if m == "parameters" {
				continue
			}
			out = append(out, strings.ToUpper(m)+" "+p)
		}
	}
	slices.Sort(out)
	return out
}

type recorder struct{ patterns []string }

func (r *recorder) HandleFunc(p string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, p)
}
func (r *recorder) ServeHTTP(http.ResponseWriter, *http.Request) {}

// The generated servers route exactly the operations their file names,
// under the base path the test listener gives them.
func TestGeneratedServersRouteEveryOperationOfTheirFile(t *testing.T) {
	cases := []struct {
		file  string
		mount func(*recorder)
	}{
		{"flight_planning/v1/flight_planning.yaml", func(r *recorder) {
			flightplanning.HandlerWithOptions(flightplanning.NewStrictHandler(nil, nil), flightplanning.StdHTTPServerOptions{BaseRouter: r})
		}},
		{"rid/v1/observation.yaml", func(r *recorder) {
			ridobservation.HandlerWithOptions(ridobservation.NewStrictHandler(nil, nil), ridobservation.StdHTTPServerOptions{BaseRouter: r})
		}},
	}
	for _, c := range cases {
		r := &recorder{}
		c.mount(r)
		slices.Sort(r.patterns)
		if want := operations(t, c.file); !slices.Equal(r.patterns, want) {
			t.Errorf("%s: generated routes %v, the file has %v", c.file, r.patterns, want)
		}
	}
}

// The injection file declares the path parameters of its two test
// routes "in: query", which is why no server is generated for it and
// internal/testif routes them. When a later pin fixes the file, this
// fails and the server is generated like the others.
func TestInjectionPathParametersAreDeclaredInQuery(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pinned, "rid", "v1", "injection.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]struct {
			Parameters []struct {
				Name string `yaml:"name"`
				In   string `yaml:"in"`
			} `yaml:"parameters"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/tests/{test_id}", "/tests/{test_id}/{version}"} {
		params := doc.Paths[p].Parameters
		if len(params) == 0 {
			t.Fatalf("%s has no parameters", p)
		}
		for _, prm := range params {
			if !strings.Contains(p, "{"+prm.Name+"}") || prm.In != "query" {
				t.Errorf("%s: parameter %s is in %s; generate the injection server from the file now", p, prm.Name, prm.In)
			}
		}
	}
	if got := operations(t, "rid/v1/injection.yaml"); !slices.Equal(got, []string{
		"DELETE /tests/{test_id}/{version}", "GET /user_notifications", "PUT /tests/{test_id}",
	}) {
		t.Errorf("injection operations %v", got)
	}
}
