package app_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func repoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// kitVersion is the uspace-ui version web/package.json pins: an exact
// npm version or the version of a release tarball's URL.
func kitVersion(t *testing.T) string {
	t.Helper()
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(repoFile(t, "web/package.json")), &pkg); err != nil {
		t.Fatal(err)
	}
	pin := pkg.Dependencies["@rootxkit/uspace-ui"]
	m := regexp.MustCompile(`(?:^|/v|-)(\d+\.\d+\.\d+)(?:\.tgz)?$`).FindStringSubmatch(pin)
	if m == nil {
		t.Fatalf("no version in the uspace-ui pin %q", pin)
	}
	return m[1]
}

// section is the text of docs/PLAN.md from the heading that starts with
// heading to the next heading of the same or a higher level.
func section(t *testing.T, plan, heading string) string {
	t.Helper()
	i := strings.Index(plan, "\n"+heading)
	if i < 0 {
		t.Fatalf("docs/PLAN.md has no %q", heading)
	}
	level := strings.Index(heading, " ")
	rest := plan[i+1+len(heading):]
	for _, h := range regexp.MustCompile(`(?m)^#{1,6} `).FindAllStringIndex(rest, -1) {
		if h[1]-h[0]-1 <= level {
			return rest[:h[0]]
		}
	}
	return rest
}

// The portal is built on the kit version web/package.json pins, and
// docs/PLAN.md §15.1 records what that version lacks and what the
// portal does instead (brief WP-17): a new pin without its row fails
// here, so the gaps of the kit in use are always written down.
func TestPlanRecordsTheGapsOfThePinnedKit(t *testing.T) {
	v := kitVersion(t)
	decided := section(t, repoFile(t, "docs/PLAN.md"), "### 15.1 ")
	var row string
	for _, line := range strings.Split(decided, "\n") {
		if strings.HasPrefix(line, "| Q") && strings.Contains(line, "`uspace-ui` "+v+" gaps") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("docs/PLAN.md §15.1 has no row on the `uspace-ui` %s gaps", v)
	}
	for _, gap := range []string{"drawing tool", "console/status/v1", "KIT: pending uspace-ui"} {
		if !strings.Contains(row, gap) {
			t.Errorf("the `uspace-ui` %s gaps row does not name %q", v, gap)
		}
	}
}
