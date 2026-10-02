package ridsp

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }

func fptr(v float64) *float64 { return &v }
func sptr(s string) *string   { return &s }

// flightN is a version 4 UUID per n.
func flightN(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// origin is where the tests fly (Tbilisi).
var origin = core.LatLon{LatDeg: 41.7151, LonDeg: 44.8271}

// body is a fully known track body of flight n at p.
func body(n int, p core.LatLon) telemetry.TrackBody {
	id := flightN(n)
	ref := string(f3411.GroundLevel)
	st := string(f3411.Airborne)
	serial := fmt.Sprintf("TEST%04d", n)
	op := "GEO-TEST-0001"
	return telemetry.TrackBody{
		TrackID: id, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, SourceInstance: "op-client-1",
		Position:  telemetry.Position{Lat: p.LatDeg, Lng: p.LonDeg},
		AltWGS84M: fptr(560), AltAMSLM: fptr(540), AltSource: core.AltGeodetic, AltPressureM: fptr(545),
		HeightM: fptr(80), HeightRef: &ref, SpeedMS: fptr(12.5), TrackDeg: fptr(90), VSpeedMS: fptr(1.5),
		AccuracyHM: fptr(10), AccuracyVM: fptr(10), Status: &st,
		Identification: core.Identification{Status: core.IdentRegistered, Reason: core.ReasonSessionBinding, Serial: &serial, OperatorReg: &op},
		FlightID:       &id, AccuracyH: f3411.HA10m, AccuracyV: f3411.VA10m, TimestampAccuracyS: fptr(0.1), Seq: 1,
	}
}

// sample is a Sample of flight n at p captured at at.
func sample(n int, p core.LatLon, at time.Time) Sample {
	c5, _, err := cell.Key(p)
	if err != nil {
		panic(err)
	}
	return Sample{CapturedAt: at, Cell5: c5, Body: body(n, p)}
}

// trackMsg is the trk.v1 message of b captured at at.
func trackMsg(t *testing.T, b telemetry.TrackBody, at time.Time) []byte {
	t.Helper()
	m := telemetry.Track{Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer,
		core.Times{TS: &at, RxTS: at, CapturedAt: at, Source: core.TimeSourceClock}), Body: b}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// offset is p moved by dNorthM and dEastM (small distances).
func offset(p core.LatLon, dNorthM, dEastM float64) core.LatLon {
	const mPerDegLat = 111_320.0
	return core.LatLon{LatDeg: p.LatDeg + dNorthM/mPerDegLat, LonDeg: p.LonDeg + dEastM/(mPerDegLat*0.7466)}
}

// newWindow is a window on a test clock.
func newWindow(c *clock) *Window {
	return &Window{Now: c.now, Counters: &core.Counters{}}
}

// fakeIntents is an Intents over a map.
type fakeIntents map[string]IntentFacts

func (f fakeIntents) Intent(id string) (IntentFacts, bool) { v, ok := f[id]; return v, ok }
