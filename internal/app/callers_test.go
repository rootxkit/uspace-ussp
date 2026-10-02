package app_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// repoRoot is the module root, two levels up from internal/app.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s", root)
	}
	return root
}

// callers lists the non-test Go files under dirs (relative to the
// module root, slash-separated) whose code matches pattern.
func callers(t *testing.T, pattern *regexp.Regexp, dirs ...string) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if pattern.Match(b) {
				rel, _ := filepath.Rel(root, p)
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(out)
	return out
}

// The migrator runs only from the migrate subcommand (D5, reconciliation
// M36): no process start, no route and no cmd/ main calls Migrate or
// MigrateDown. The pattern is pinned by finding the one caller there is
// (E-01: a pattern that matched nothing would pass vacuously).
func TestOnlyTheMigrateSubcommandMigrates(t *testing.T) {
	migrator := regexp.MustCompile(`\.(Migrate|MigrateDown)\(`)
	got := callers(t, migrator, "cmd", "internal")
	want := []string{"internal/app/proc/migrate.go", "internal/store/store.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("migrator called from %v, want only %v (store.go is Store.Migrate delegating to the pool, and nothing calls Store.Migrate)", got, want)
	}
	if got := callers(t, regexp.MustCompile(`func main\(\)`), "cmd"); len(got) != 8 {
		t.Fatalf("the cmd/ walk found %d mains, want 8: %v", len(got), got)
	}
}

// Audit is the only writer of events: the generated InsertEvent is
// called from internal/store/audit.go and nowhere else.
func TestAuditIsTheOnlyWriterOfEvents(t *testing.T) {
	got := callers(t, regexp.MustCompile(`\.InsertEvent\(`), "cmd", "internal")
	if !slices.Equal(got, []string{"internal/store/audit.go"}) {
		t.Fatalf("InsertEvent called from %v", got)
	}
}
