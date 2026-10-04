package telemetryingest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// writeGeoid writes a constant GeographicLib grid (N = 20 m everywhere).
func writeGeoid(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("P5\n# Description ingest test grid, N = 20 m\n# Offset 20\n# Scale 1\n2 3\n65535\n")
	b.Write(make([]byte, 2*2*3))
	path := filepath.Join(t.TempDir(), "geoid.pgm")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkGeoidLoad loads USSP_GEOID_FILE as telemetry-ingest does and
// checks that the grid says wantMapped and /readyz's geoid detail says
// it (WP-19). geoid_unix_test.go wants a mapping, geoid_other_test.go
// the read into memory core falls back to elsewhere.
func checkGeoidLoad(t *testing.T, wantMapped bool) {
	t.Helper()
	g, desc, mapped, missing := loadGeoid(config.Config{GeoidFile: writeGeoid(t)})
	if g == nil || missing != "" || desc != "ingest test grid, N = 20 m" || mapped != wantMapped {
		t.Fatalf("loadGeoid: %v %q, mapped %v, %q; want mapped %v", g, desc, mapped, missing, wantMapped)
	}
	if n, err := g.UndulationM(core.LatLon{LatDeg: 41.7, LonDeg: 44.8}); err != nil || n != 20 {
		t.Fatalf("N %v, %v", n, err)
	}
	want := "mapped: false"
	if wantMapped {
		want = "mapped: true"
	}
	if st, detail := geoidProbe(g, false, mapped, missing)(t.Context()); st != obs.StateUp || detail != want {
		t.Fatalf("readyz geoid %s %q, want up %q", st, detail, want)
	}
}

// Without a grid the geoid is down with the reason; a grid the caller
// configured is up without a mapping claim.
func TestGeoidProbeOtherStates(t *testing.T) {
	g, _, mapped, missing := loadGeoid(config.Config{GeoidFile: filepath.Join(t.TempDir(), "absent.pgm")})
	if g != nil || mapped || missing == "" {
		t.Fatalf("absent: %v, mapped %v, %q", g, mapped, missing)
	}
	if st, detail := geoidProbe(g, false, mapped, missing)(t.Context()); st != obs.StateDown || detail != missing {
		t.Fatalf("absent: readyz geoid %s %q", st, detail)
	}
	caller, _, _, _ := loadGeoid(config.Config{GeoidFile: writeGeoid(t)})
	if st, detail := geoidProbe(caller, true, false, "")(t.Context()); st != obs.StateUp || detail != "configured by the caller" {
		t.Fatalf("by the caller: readyz geoid %s %q", st, detail)
	}
}
