package manned

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// FuzzANSPStreamTake: whatever frame the ANSP sends, Take never panics,
// counts it exactly once as read or unreadable, and every track it
// publishes is one the CPA path reads, from the ANSP's feed; the status
// and the probe still answer.
func FuzzANSPStreamTake(f *testing.F) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	good, _ := json.Marshal(map[string]any{"schema": "track/manned/v1", "msg_id": "01J9ZQ3K8M2V7W5X4Y3Z2A1B0C", "producer": "ansp/manned-feed",
		"ts": now, "rx_ts": now, "captured_at": now, "time_source": "source_clock", "backlog": false, "body": mannedBody()})
	f.Add(good)
	f.Add([]byte(`{"schema":"console/status/v1","body":{"degraded":["adsb_north"],"sources":[{"source":"ansp_feed","source_instance":"a-1","state":"stale","since":"2026-10-04T11:59:00Z"}]}}`))
	f.Add([]byte(`{"schema":"console/status/v1","body":{"adapters":[{"id":"a-1","state":"down","last_frame_at":null}]}}`))
	f.Add([]byte(`{"schema":"console/snapshot/v1","body":{"manned":[{"schema":"track/manned/v1"}]}}`))
	f.Add([]byte(`{"schema":"x/unknown/v9"}`))
	f.Add([]byte(`{"schema":""}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, data []byte) {
		s := &sink{}
		st := &ANSPStream{Sink: s, Counters: &core.Counters{}, Now: func() time.Time { return now }}
		st.Take(context.Background(), data)
		read, unread := st.Counters.Get(CounterANSPFrames), st.Counters.Get(CounterANSPUnreadable)
		if read != 1 && unread != 1 {
			t.Fatalf("counted %d read and %d unreadable for one frame", read, unread)
		}
		for _, raw := range s.raw("man.v1.") {
			m, err := traffic.DecodeManned(raw)
			if err != nil || m.Body.Trust != core.TrustSurveillance || m.Body.Source != SourceANSPFeed {
				t.Fatalf("published what the CPA path refuses: %v %s", err, raw)
			}
		}
		if len(st.Statuses()) == 0 {
			t.Fatal("no status")
		}
		if state, detail := st.Probe(context.Background()); state == "" || detail == "" {
			t.Fatalf("probe answered %q %q", state, detail)
		}
	})
}

// FuzzAircraftJSON: whatever a receiver serves, TakeDocument never
// panics, and every track it publishes is broadcast from adsb_rx and one
// the CPA path reads.
func FuzzAircraftJSON(f *testing.F) {
	f.Add([]byte(`{"now":1700000000.5,"aircraft":[{"hex":"4ca7b5","flight":"RYR1AB","alt_baro":37000,"alt_geom":37500,"gs":450,"track":123.4,"baro_rate":-64,"squawk":"7700","emergency":"general","lat":51.5,"lon":-0.1,"seen_pos":0.4,"seen":0.1,"mlat":["lat"],"nic":8,"nac_p":9}]}`))
	f.Add([]byte(`{"now":1,"aircraft":[{"hex":"~abc","alt_baro":"ground","lat":1,"lon":1}]}`))
	f.Add([]byte(`{"now":1,"aircraft":[{"hex":"4ca7b5","lat":1e308,"lon":-1e308,"seen_pos":-5,"track":360}]}`))
	f.Add([]byte(`{"aircraft":null}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		s := &sink{}
		e := &Econspicuity{ReceiverID: "rx-1", Sink: s}
		_ = e.TakeDocument(context.Background(), data, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
		for _, raw := range s.raw("man.v1.") {
			m, err := traffic.DecodeManned(raw)
			if err != nil || m.Body.Trust != core.TrustBroadcast || m.Body.Source != SourceAdsbRx {
				t.Fatalf("published what the CPA path refuses: %v %s", err, raw)
			}
		}
		_ = e.Status()
	})
}

// FuzzBaseStationLine: whatever line a BaseStation port sends, TakeSBS
// never panics, and every track it publishes is one the CPA path reads.
func FuzzBaseStationLine(f *testing.F) {
	f.Add("MSG,3,1,1,4CA7B5,1,2026/10/04,12:00:00.000,2026/10/04,12:00:00.000,,3000,,,41.75,44.85,,,0,0,0,0")
	f.Add("MSG,4,1,1,4CA7B5,1,,,,,,,120,90,,,-640,,,,,0")
	f.Add("MSG,6,1,1,4CA7B5,1,,,,,,,,,,,,7700,0,1,0,0")
	f.Add("MSG,1,1,1,4CA7B5,1,,,,,GEO123,,,,,,,,,,,")
	f.Add("MSG,3,1,1,4CA7B5,1,,,,,,NaN,,,1e999,-1e999,,,,,,")
	f.Add("garbage")
	f.Fuzz(func(t *testing.T, line string) {
		s := &sink{}
		e := &Econspicuity{ReceiverID: "rx-1", Sink: s}
		now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
		e.TakeSBS(context.Background(), "MSG,4,1,1,4CA7B5,1,,,,,,,120,90,,,-640,,,,,0", now)
		e.TakeSBS(context.Background(), line, now)
		for _, raw := range s.raw("man.v1.") {
			if _, err := traffic.DecodeManned(raw); err != nil {
				t.Fatalf("published what the CPA path refuses: %v %s", err, raw)
			}
		}
	})
}
