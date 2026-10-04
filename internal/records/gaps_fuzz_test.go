package records

import (
	"bytes"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// FuzzDecodeGap: whatever src.v1 carries, DecodeGap never panics, never
// both finds a gap and errs, and a gap it finds holds every bound it
// checks (the gap is stored as found).
func FuzzDecodeGap(f *testing.F) {
	f.Add(statusMsg(f, &telemetry.Gap{Cause: telemetry.GapQueueAge, SourceInstance: "client-1", GapStarted: t0, GapEnded: t0.Add(5 * time.Second),
		Dropped: 5, FromSeq: 10, ToSeq: 14}))
	f.Add(statusMsg(f, &telemetry.Gap{Cause: telemetry.GapAgedOutUnread, SourceInstance: telemetry.GapInstance, Dropped: 3, FromSeq: 1, ToSeq: 3}))
	f.Add(statusMsg(f, nil))
	f.Add([]byte(`{"msg_id":"m","body":{"gap":{"cause":"c","source_instance":"s","dropped":-1}}}`))
	f.Add([]byte(`{"msg_id":"m","body":{"gap":{"cause":"c","source_instance":"s","gap_started":"2026-10-04T12:00:00Z"}}}`))
	f.Add([]byte(`{"body":{"gap":null}}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		g, found, err := DecodeGap(data)
		if err != nil {
			if found {
				t.Fatalf("found a gap and refused it: %v", err)
			}
			return
		}
		if len(data) > maxStatusBytes {
			t.Fatalf("a message of %d bytes read", len(data))
		}
		if !found {
			if g != (Gap{}) {
				t.Fatalf("no gap, yet %+v", g)
			}
			return
		}
		switch {
		case g.MsgID == "" || len(g.MsgID) > 64:
			t.Fatalf("msg_id %q", g.MsgID)
		case g.Cause == "" || len(g.Cause) > 64:
			t.Fatalf("cause %q", g.Cause)
		case g.SourceInstance == "" || len(g.SourceInstance) > 128:
			t.Fatalf("source_instance %q", g.SourceInstance)
		case g.Dropped < 0:
			t.Fatalf("dropped %d", g.Dropped)
		case g.Started.IsZero() != g.Ended.IsZero() || g.Ended.Before(g.Started):
			t.Fatalf("window [%v, %v]", g.Started, g.Ended)
		}
		again, found2, err2 := DecodeGap(bytes.Clone(data))
		if err2 != nil || !found2 || again != g {
			t.Fatalf("not deterministic: %+v %v %v", again, found2, err2)
		}
	})
}
