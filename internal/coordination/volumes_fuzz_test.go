package coordination

import (
	"errors"
	"testing"
)

// FuzzCheckVolumes: whatever volumes an intent carries, a notice Build
// takes past checkVolumes validates against the pinned
// coordination/annex_v/v1 schema; one it refuses is a BuildError.
func FuzzCheckVolumes(f *testing.F) {
	in := testIntent()
	f.Add([]byte(in.Volumes))
	f.Add([]byte(`[{"volume":{"outline_circle":{"center":{"lat":41.7,"lng":44.8},"radius":{"value":50,"units":"M"}}},` +
		`"time_start":{"value":"2026-10-04T12:00:00Z","format":"RFC3339"},"time_end":{"value":"2026-10-04T12:30:00Z","format":"RFC3339"}}]`))
	f.Add([]byte(`[{"volume":{"outline_polygon":{"vertices":[{"lat":1,"lng":1}]}}}]`))
	f.Add([]byte(`[{"volume":{"outline_circle":{"center":{"lat":41.7,"lng":44.8},"radius":{"value":0,"units":"M"}}}}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"volume":{"outline_polygon":{"vertices":[{"lat":91,"lng":1},{"lat":1,"lng":2},{"lat":2,"lng":2}]}}}]`))
	f.Add([]byte(`[{"volume":{"outline_circle":{"center":{"lat":41.7,"lng":44.8},"radius":{"value":50,"units":"FT"}}}}]`))
	f.Add([]byte(`[{"volume":{"outline_circle":{"center":{"lat":41.7,"lng":44.8},"radius":{"value":50,"units":"M"}}},"time_start":{"value":"2026-10-04T12:00:00Z"}}]`))
	schema := annexV(f)
	f.Fuzz(func(t *testing.T, volumes []byte) {
		in := testIntent()
		in.Volumes = volumes
		_, body, err := Build(KindIntentNotice, "ref-1", testSystem, in, nil, "", sentAt)
		if err != nil {
			var be *BuildError
			if !errors.As(err, &be) {
				t.Fatalf("not a BuildError: %v", err)
			}
			return
		}
		if err := validateBody(t, schema, body); err != nil {
			t.Fatalf("built a notice the schema refuses: %v\n%s", err, body)
		}
	})
}
