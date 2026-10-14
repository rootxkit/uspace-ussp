package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A held flight's rows are not kept "whatever their age": telemetry's
// retention policy (telemetry_retention_days) drops their chunks like
// any other, and the oldest backup kept is USSP_BACKUP_KEEP_DAYS old, so
// nothing older than the sum can come back. The backup runbook and the
// changelog state the ceiling, by name and with the defaults, and the
// figure is the defaults' (this policy row's, deploy/backup.sh's).
func TestBackupHeldCeilingIsStated(t *testing.T) {
	root := filepath.Join("..", "..")
	sh, err := os.ReadFile(filepath.Join(root, "deploy", "backup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`setting USSP_BACKUP_KEEP_DAYS ([0-9]+)\)`).FindSubmatch(sh)
	if m == nil {
		t.Fatal("deploy/backup.sh has no USSP_BACKUP_KEEP_DAYS default")
	}
	keep, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	retention := Defaults().TelemetryRetentionDays
	want := []string{
		"telemetry_retention_days + USSP_BACKUP_KEEP_DAYS",
		fmt.Sprintf("%d + %d = %d days", retention, keep, retention+keep),
	}
	for _, f := range []string{filepath.Join("docs", "RUNBOOKS", "backup.md"), "CHANGELOG.md"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(b)), " ")
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s does not state the held-data ceiling: no %q", f, w)
			}
		}
	}
}
