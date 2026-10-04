package traffic

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// seedExamples adds every example (valid and invalid) of a schema as a
// seed.
func seedExamples(f *testing.F, name string) {
	f.Helper()
	dir := filepath.Join("..", "..", "schemas", filepath.FromSlash(name), "examples")
	for _, d := range []string{dir, filepath.Join(dir, "invalid")} {
		es, err := os.ReadDir(d)
		if err != nil {
			f.Fatal(err)
		}
		for _, e := range es {
			if e.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(d, e.Name()))
			if err != nil {
				f.Fatal(err)
			}
			f.Add(raw)
		}
	}
	f.Add([]byte(`{"body":null}`))
	f.Add([]byte(`[]`))
}

// The readers of untrusted bytes never panic, and what they accept maps
// onto the monitor's track (no NaN position reaches core's grid, C-09).
func FuzzDecodeTrack(f *testing.F) {
	seedExamples(f, "track/telemetry/v1")
	f.Fuzz(func(t *testing.T, data []byte) {
		tr, err := DecodeTrack(data)
		if err != nil {
			return
		}
		in := TrackInputOf(NSTrack, tr)
		if !in.Position.Valid() {
			t.Fatalf("accepted an invalid position %+v", in.Position)
		}
		_, _ = TrackOf(&in)
	})
}

func FuzzDecodeManned(f *testing.F) {
	seedExamples(f, "track/manned/v1")
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := DecodeManned(data)
		if err != nil {
			return
		}
		in := MannedInputOf(m, flatGeoid{}, policy.Defaults().AltPolicy())
		if !in.Position.Valid() {
			t.Fatalf("accepted an invalid position %+v", in.Position)
		}
		_, _ = TrackOf(&in)
	})
}

func FuzzDecodeSaved(f *testing.F) {
	f.Add([]byte(`{"key":"conflict:a:b","raised_at":"2026-10-03T12:00:00Z","aircraft":[{"id":"a"},{"id":"b"}]}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := DecodeSaved("k", data)
		if err == nil && (s.Key == "" || s.Aircraft[0].ID == "") {
			t.Fatalf("accepted an incomplete state %+v", s)
		}
	})
}
