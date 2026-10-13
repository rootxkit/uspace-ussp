package testif

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

type secReq = map[string][]string

// fileScopes reads the scope each operation of a pinned file requires,
// by "METHOD base+path": the operation's `security` or, without one, the
// file's (Q17: the file wins).
func fileScopes(t *testing.T, file, base string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "api", "standards", "interuss", filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Security []secReq                        `yaml:"security"`
		Paths    map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	scopesOf := func(reqs []secReq) []string {
		var out []string
		for _, r := range reqs {
			for _, ss := range r {
				out = append(out, ss...)
			}
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	out := map[string][]string{}
	for p, ops := range doc.Paths {
		for m := range ops {
			if m == "parameters" {
				continue
			}
			var op struct {
				Security *[]secReq `yaml:"security"`
			}
			node := ops[m]
			if err := node.Decode(&op); err != nil {
				t.Fatal(err)
			}
			reqs := doc.Security
			if op.Security != nil {
				reqs = *op.Security
			}
			out[strings.ToUpper(m)+" "+base+p] = scopesOf(reqs)
		}
	}
	return out
}

// The access tables are the pinned files' operations with the scopes
// their `security` blocks name, and Scopes is exactly their union.
func TestScopesAreThePinnedFiles(t *testing.T) {
	want := map[string][]string{}
	maps.Copy(want, fileScopes(t, "rid/v1/injection.yaml", BaseInjection))
	maps.Copy(want, fileScopes(t, "rid/v1/observation.yaml", BaseObservation))
	maps.Copy(want, fileScopes(t, "flight_planning/v1/flight_planning.yaml", BaseFlightPlanning))
	got := map[string]httpx.Access{}
	maps.Copy(got, InjectionAccess())
	maps.Copy(got, ObservationAccess())
	maps.Copy(got, FlightPlanningAccess())
	var union []string
	for pattern, scopes := range want {
		a, ok := got[pattern]
		if !ok {
			t.Errorf("%s of the pinned files has no access entry", pattern)
			continue
		}
		s := slices.Sorted(slices.Values(a.Scopes))
		if !slices.Equal(s, scopes) {
			t.Errorf("%s: entry %v, the file names %v", pattern, s, scopes)
		}
		union = append(union, scopes...)
		delete(got, pattern)
	}
	for pattern := range got {
		t.Errorf("access entry %s is not an operation of the pinned files", pattern)
	}
	slices.Sort(union)
	union = slices.Compact(union)
	if closed := slices.Sorted(slices.Values(Scopes)); !slices.Equal(union, closed) {
		t.Errorf("Scopes %v, the files name %v", closed, union)
	}
}

// The scopes are outside the catalogue: every production process's
// ValidateAccess refuses a route that names one (D13), and the test
// listener's admits it; the listener's refuses a catalogue scope, a
// session and a public route.
func TestScopesAreNotCatalogueScopes(t *testing.T) {
	for _, s := range Scopes {
		if auth.KnownScope(s) || slices.Contains(auth.EcosystemScopes, s) {
			t.Errorf("%s is in the catalogue", s)
		}
		a := httpx.Access{Scopes: []string{s}}
		if err := auth.ValidateAccess(a); err == nil {
			t.Errorf("auth.ValidateAccess admits %s: a production route could name it", s)
		}
		if err := ValidateAccess(a); err != nil {
			t.Errorf("the test listener refuses its own scope %s: %v", s, err)
		}
	}
	for _, a := range []httpx.Access{
		{Scopes: []string{"utm.strategic_coordination"}},
		{Scopes: []string{auth.ScopeTelemetry}},
		{Public: true},
		{Sessions: []httpx.SessionAccess{{Realm: auth.RealmConsole}}},
		{AllScopes: []string{ScopePlan, ScopeObserve}},
		{},
	} {
		if err := ValidateAccess(a); err == nil {
			t.Errorf("the test listener admits %s", a)
		}
	}
}
