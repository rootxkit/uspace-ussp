package monitor

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// FuzzANSPFeedTake: whatever frame the ANSP sends, take never panics,
// counts it exactly once as read or unreadable, dispatches a read frame
// on its schema alone, and keeps a console/status/v1's degraded list as
// sent; the probe still answers after it.
func FuzzANSPFeedTake(f *testing.F) {
	f.Add([]byte(`{"schema":"track/manned/v1","msg_id":"m1","body":{"icao24":"4ca1f0"}}`))
	f.Add([]byte(`{"schema":"console/status/v1","body":{"degraded":["adsb_north","flarm"]}}`))
	f.Add([]byte(`{"schema":"console/status/v1","body":{"degraded":null}}`))
	f.Add([]byte(`{"schema":"console/snapshot/v1","body":{}}`))
	f.Add([]byte(`{"schema":"x/unknown/v9"}`))
	f.Add([]byte(`{"schema":""}`))
	f.Add([]byte(`{"schema":"console/status/v1","body":{"degraded":"not a list"}}`))
	f.Add([]byte(`not json`))
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte) {
		c := &core.Counters{}
		fd := &ANSPFeed{Counters: c, Now: func() time.Time { return t0 }}
		fd.set(true, "")
		fd.take(data)
		read, unread := c.Get(CounterANSPFrames), c.Get(CounterANSPUnreadable)
		if read+unread != 1 {
			t.Fatalf("counted %d read and %d unreadable for one frame", read, unread)
		}
		var fr struct {
			Schema string `json:"schema"`
			Body   struct {
				Degraded []string `json:"degraded"`
			} `json:"body"`
		}
		ok := json.Unmarshal(data, &fr) == nil && fr.Schema != ""
		if ok != (read == 1) {
			t.Fatalf("readable %v, counted read %d", ok, read)
		}
		if ok {
			want := map[string]string{"track/manned/v1": CounterANSPTracksHeld, "console/status/v1": CounterANSPStatusFrames}[fr.Schema]
			for _, k := range []string{CounterANSPTracksHeld, CounterANSPStatusFrames, CounterANSPUnknownSchema} {
				n := c.Get(k)
				switch {
				case k == want && n != 1, k != want && k != CounterANSPUnknownSchema && n != 0:
					t.Fatalf("schema %q counted %s %d", fr.Schema, k, n)
				}
			}
			known := fr.Schema == "track/manned/v1" || fr.Schema == "console/status/v1" || fr.Schema == "console/snapshot/v1"
			if (c.Get(CounterANSPUnknownSchema) == 1) == known {
				t.Fatalf("schema %q counted unknown %d", fr.Schema, c.Get(CounterANSPUnknownSchema))
			}
			if fr.Schema == "console/status/v1" && !slices.Equal(fd.degraded, fr.Body.Degraded) {
				t.Fatalf("degraded %q, sent %q", fd.degraded, fr.Body.Degraded)
			}
		}
		if st, detail := fd.Probe(context.Background()); st == "" || detail == "" {
			t.Fatalf("probe answered %q %q", st, detail)
		}
	})
}
