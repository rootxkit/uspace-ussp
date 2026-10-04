package api

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// writeGeoid writes a constant GeographicLib grid (N = 20 m everywhere).
func writeGeoid(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("P5\n# Description api test grid, N = 20 m\n# Offset 20\n# Scale 1\n2 3\n65535\n")
	b.Write(make([]byte, 2*2*3))
	path := filepath.Join(t.TempDir(), "geoid.pgm")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkGeoidLoad loads USSP_GEOID_FILE as the api process does and
// checks that the grid says wantMapped and /readyz's geoid detail says
// it (WP-19). geoid_unix_test.go wants a mapping, geoid_other_test.go
// the read into memory core falls back to elsewhere.
func checkGeoidLoad(t *testing.T, wantMapped bool) {
	t.Helper()
	rt := &proc.Runtime{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rt.Config.GeoidFile = writeGeoid(t)
	g, mapped, why := loadGeoid(rt)
	if g == nil || why != "" || mapped != wantMapped {
		t.Fatalf("loadGeoid: %v, mapped %v, %q; want mapped %v", g, mapped, why, wantMapped)
	}
	if n, err := g.UndulationM(core.LatLon{LatDeg: 41.7, LonDeg: 44.8}); err != nil || n != 20 {
		t.Fatalf("N %v, %v", n, err)
	}
	want := "mapped: false"
	if wantMapped {
		want = "mapped: true"
	}
	if st, detail := geoidProbe(g, mapped, why)(t.Context()); st != obs.StateUp || detail != want {
		t.Fatalf("readyz geoid %s %q, want up %q", st, detail, want)
	}
}

// Without a grid the geoid is down with the reason on /readyz.
func TestGeoidNotLoadedIsDown(t *testing.T) {
	rt := &proc.Runtime{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, path := range []string{"", filepath.Join(t.TempDir(), "absent.pgm")} {
		rt.Config.GeoidFile = path
		g, mapped, why := loadGeoid(rt)
		if g != nil || mapped || why == "" {
			t.Fatalf("%q: %v, mapped %v, %q", path, g, mapped, why)
		}
		if st, detail := geoidProbe(g, mapped, why)(t.Context()); st != obs.StateDown || detail != why {
			t.Fatalf("%q: readyz geoid %s %q", path, st, detail)
		}
	}
}
