package telemetry

import (
	"fmt"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/rid"
	"github.com/rootxkit/uspace-core/timeplace"
	"github.com/rootxkit/uspace-core/vectors"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// rid_time.json network cases (owner ussp) through the ingest's
// placement adapter (PlaceOne): the response timestamp is a batch's
// sent_at, none is a WebSocket sample. The adapter maps core's sources
// at its boundary (a believed state is source_clock here, the vectors'
// "broadcast"); everything else is core's and compared exactly.
func TestVectorsRidTimeNetworkThroughTheIngest(t *testing.T) {
	type input struct {
		StateTimestamp    time.Time  `json:"state_timestamp"`
		ResponseTimestamp *time.Time `json:"response_timestamp"`
		ReceivedAt        time.Time  `json:"received_at"`
		MaxAgeS           float64    `json:"max_age_s"`
		TimeToleranceS    float64    `json:"time_tolerance_s"`
		MaxLatencyS       float64    `json:"max_latency_s"`
	}
	type expected struct {
		TS         time.Time `json:"ts"`
		CapturedAt time.Time `json:"captured_at"`
		TimeSource string    `json:"time_source"`
		Note       *string   `json:"note"`
	}
	// The vectors name a believed network state "broadcast" (core
	// timeplace doc, "Source mapping"); the ingest calls it source_clock.
	mapped := map[string]core.TimeSource{"broadcast": core.TimeSourceClock, "receiver": core.TimeReceiver}
	f := vectors.Load(t, "rid_time.json")
	ran := 0
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		ran++
		var in input
		c.Decode(t, &in, nil)
		pol := PlacePolicy{MaxAge: seconds(in.MaxAgeS), AheadTolerance: seconds(in.TimeToleranceS), BacklogAfter: seconds(in.MaxLatencyS)}
		p, note, shown := PlaceOne(in.StateTimestamp, in.ResponseTimestamp, in.ReceivedAt, pol)
		if c.ExpectedIsNull() {
			if shown {
				t.Fatalf("shown %+v, want not taken (rejected_too_old)", p)
			}
			return
		}
		var exp expected
		c.Decode(t, nil, &exp)
		if !shown {
			t.Fatal("not shown")
		}
		vectors.EqualTime(t, "ts", p.TS, exp.TS)
		vectors.EqualTime(t, "captured_at", p.CapturedAt, exp.CapturedAt)
		if want, ok := mapped[exp.TimeSource]; !ok || p.Source != want {
			t.Errorf("time_source %q, want %q (vector %q)", p.Source, want, exp.TimeSource)
		}
		wantNote := timeplace.NoteNone
		if exp.Note != nil {
			wantNote = timeplace.NetworkNote(*exp.Note)
		}
		if note != wantNote {
			t.Errorf("note %q, want %q", note, wantNote)
		}
	})
	if ran != 7 {
		t.Fatalf("ran %d network cases, want 7", ran)
	}
}

// codeOf is the F3411 VerticalAccuracy of a MAV_ODID_VER_ACC code (the
// inverse of the ingest's mapping, for feeding the vectors' codes).
func codeOf(t *testing.T, code uint8) f3411.VerticalAccuracy {
	t.Helper()
	for a, c := range verticalCode {
		if c == code {
			return a
		}
	}
	t.Fatalf("no category for code %d", code)
	return ""
}

// pressure_altitude.json (owner ussp) through the ingest's altitude
// adapter: a sample with the vector's altitudes and accuracy code (as
// its F3411 category), the geoid's undulation, the policy's threshold
// and hold, taken by the ingest itself; a stateless case whose hold is
// in force is preceded by a poor sample one second earlier, which puts
// the hold in force through the selector the ingest keeps per stream.
func TestVectorsPressureAltitudeThroughTheIngest(t *testing.T) {
	type stepIn struct {
		NowS             float64  `json:"now_s"`
		VertAccuracyCode uint8    `json:"vert_accuracy_code"`
		AltPressureM     *float64 `json:"alt_pressure_m"`
	}
	type input struct {
		AltHAEM             *float64 `json:"alt_hae_m"`
		AltPressureM        *float64 `json:"alt_pressure_m"`
		VertAccuracyCode    *uint8   `json:"vert_accuracy_code"`
		GeoidUndulationM    *float64 `json:"geoid_undulation_m"`
		MinVerticalAccuracy *uint8   `json:"min_vertical_accuracy"`
		HoldPressure        *bool    `json:"hold_pressure"`
		PressureHoldS       *float64 `json:"pressure_hold_s"`
		Steps               []stepIn `json:"steps"`
	}
	type result struct {
		AltAMSLM  *float64 `json:"alt_amsl_m"`
		AltSource *string  `json:"alt_source"`
	}
	type expected struct {
		AltAMSLM  *float64 `json:"alt_amsl_m"`
		AltSource *string  `json:"alt_source"`
		PerStep   []result `json:"per_step"`
	}
	f := vectors.Load(t, "pressure_altitude.json")
	tol, ok := f.FloatTolerance("alt_amsl_m")
	if !ok {
		t.Fatal("no alt_amsl_m tolerance")
	}
	compare := func(t *testing.T, field string, got rid.AltResult, want result) {
		t.Helper()
		vectors.NearPtr(t, field+".alt_amsl_m", got.AltAMSLM, want.AltAMSLM, tol)
		ws := core.AltNone
		if want.AltSource != nil {
			ws = core.AltSource(*want.AltSource)
		}
		if got.Source != ws {
			t.Errorf("%s.alt_source %q, want %q", field, got.Source, ws)
		}
	}
	ran := 0
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		ran++
		var in input
		var exp expected
		c.Decode(t, &in, &exp)
		in2 := New(Config{Policy: func() policy.Record { return policy.Record{Values: policy.Defaults()} }})
		if in.GeoidUndulationM != nil {
			in2.cfg.Geoid = undulation(*in.GeoidUndulationM)
		}
		pol := policy.Defaults()
		if in.MinVerticalAccuracy != nil {
			pol.PressureFallbackAccuracyCode = int(*in.MinVerticalAccuracy)
		}
		if in.PressureHoldS != nil {
			pol.PressureHoldS = *in.PressureHoldS
		}
		st := &stream{}
		sample := func(nowS float64, code uint8, pressure *float64) rid.AltResult {
			fr := frame(snA, 1, t0)
			fr.AltWGS84M, fr.AltPressureM, fr.AccuracyV = in.AltHAEM, pressure, codeOf(t, code)
			at := time.Unix(0, 0).Add(time.Duration(nowS * float64(time.Second)))
			r, _ := in2.altitude(st, &fr, placed{capturedAt: at}, pol)
			return r
		}
		if in.Steps == nil {
			if *in.HoldPressure {
				// The hold is in force: a poor sample with a pressure
				// altitude one second earlier.
				sample(99, 1, f64(1))
			}
			compare(t, "result", sample(100, *in.VertAccuracyCode, in.AltPressureM), result{AltAMSLM: exp.AltAMSLM, AltSource: exp.AltSource})
			return
		}
		for i, s := range in.Steps {
			compare(t, fmt.Sprintf("per_step[%d]", i), sample(s.NowS, s.VertAccuracyCode, s.AltPressureM), exp.PerStep[i])
		}
	})
	if ran != 16 {
		t.Fatalf("ran %d cases, want 16", ran)
	}
}
