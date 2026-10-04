package peers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/timeplace"
	"github.com/rootxkit/uspace-core/vectors"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

type flatGeoid struct{}

func (flatGeoid) UndulationM(core.LatLon) (float64, error) { return 20, nil }

func f32(v float32) *float32 { return &v }

func flight(id string, ts time.Time) f3411.RIDFlight {
	lat, lng := 41.75, 44.85
	ha, va := f3411.HA10m, f3411.VA3m
	st := f3411.Airborne
	return f3411.RIDFlight{Id: id, AircraftType: f3411.Helicopter, CurrentState: &f3411.RIDAircraftState{
		Timestamp: f3411.Time{Format: f3411.RFC3339, Value: ts}, TimestampAccuracy: 0.2, OperationalStatus: &st, SpeedAccuracy: f3411.SA1mps,
		Position: f3411.RIDAircraftPosition{Lat: &lat, Lng: &lng, Alt: f32(620), PressureAltitude: f32(600), AccuracyH: &ha, AccuracyV: &va,
			Height: &f3411.RIDHeight{Distance: f32(80), Reference: f3411.TakeoffLocation}},
		Speed: f32(12.5), Track: f32(90), VerticalSpeed: f32(1),
	}}
}

const schemaBase = "https://schemas.uspace.ge/"

func compile(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	root := filepath.Join("..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	var names []string
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "schema.json" {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		names = append(names, filepath.ToSlash(rel))
		return c.AddResource(schemaBase+filepath.ToSlash(rel)+".json", doc)
	}); err != nil {
		t.Fatal(err)
	}
	out := map[string]*jsonschema.Schema{}
	for _, n := range names {
		s, err := c.Compile(schemaBase + n + ".json")
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = s
	}
	return out
}

func validate(t *testing.T, ss map[string]*jsonschema.Schema, name string, raw []byte) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := ss[name].Validate(inst); err != nil {
		t.Fatalf("%s does not validate: %v\n%s", name, err, raw)
	}
}

// One peer flight onto track/telemetry/v1: trust provider (never
// authenticated), source network_rid, the peer as source instance, no
// flight or intent of ours, AMSL geodetic through the geoid, the height
// with its reference, the accuracy bounds, unidentified as broadcast,
// placed against the answer's timestamp (the peer's skew cancels); a
// valid message the CPA path and traffic-ws read.
func TestFlightTrack(t *testing.T) {
	rx := time.Date(2026, 10, 4, 12, 0, 10, 0, time.UTC)
	resp := rx.Add(5 * time.Minute) // the peer's clock 5 min ahead
	f := flight("uss1.JA6kHYCcByQ-6AfU", resp.Add(-2*time.Second))
	m, subject, err := FlightTrack("https://peer.example/uss", &f, resp, rx, Conv{Geoid: flatGeoid{}, Policy: policy.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(subject, "peer.v1.c3:") || !strings.HasSuffix(subject, "._h"+subjectToken(f.Id)[2:]) {
		t.Fatalf("subject %q", subject)
	}
	b := m.Body
	if b.Trust != core.TrustProvider || b.Source != SourceNetworkRID || b.SourceInstance != "https://peer.example/uss" || b.TrackID != f.Id ||
		b.FlightID != nil || b.IntentID != nil || b.AltSource != core.AltGeodetic || *b.AltAMSLM != 600 || *b.HeightM != 80 ||
		*b.HeightRef != "TakeoffLocation" || *b.AccuracyHM != 10 || *b.AccuracyVM != 3 || *b.Status != "Airborne" || b.Emergency ||
		b.Identification.Status != core.IdentUnidentified || b.Identification.Basis != core.BasisAsBroadcast {
		t.Fatalf("body %+v", b)
	}
	if !m.CapturedAt.Equal(rx.Add(-2 * time.Second)) {
		t.Fatalf("placed at %v, want 2 s before receipt", m.CapturedAt)
	}
	raw, _ := json.Marshal(m)
	validate(t, compile(t), "track/telemetry/v1", raw)
	tr, err := traffic.DecodeTrack(raw)
	if err != nil {
		t.Fatalf("the CPA path refuses it: %v", err)
	}
	in := traffic.TrackInputOf(traffic.NSPeer, tr)
	if in.Own() || in.Trust != core.TrustProvider || in.ID != "peer:"+f.Id {
		t.Fatalf("input %+v", in)
	}
	// No geoid: the pressure altitude is not AMSL; an emergency status.
	em := f3411.Emergency
	f.CurrentState.OperationalStatus = &em
	m, _, err = FlightTrack("https://peer.example", &f, resp, rx, Conv{Policy: policy.Defaults()})
	if err != nil || m.Body.AltSource != core.AltNone || !m.Body.Emergency {
		t.Fatalf("no geoid %+v %v", m.Body, err)
	}
	// A vertical accuracy outside the table: the geodetic altitude is not
	// used, the pressure one stands in.
	va := f3411.VA150mPlus
	f.CurrentState.Position.AccuracyV = &va
	m, _, _ = FlightTrack("https://peer.example", &f, resp, rx, Conv{Geoid: flatGeoid{}, Policy: policy.Defaults()})
	if m.Body.AltSource != core.AltPressure {
		t.Fatalf("poor accuracy: %s", m.Body.AltSource)
	}
	// Special values are unknown, never zero.
	f.CurrentState.Speed, f.CurrentState.Track = f32(f3411.SpecialSpeed), f32(f3411.SpecialTrackDirection)
	m, _, _ = FlightTrack("https://peer.example", &f, resp, rx, Conv{Policy: policy.Defaults(), Resolver: resolver{}})
	if m.Body.SpeedMS != nil || m.Body.TrackDeg != nil || m.Body.Identification.Reason != "registry_unavailable" {
		t.Fatalf("special values %+v", m.Body)
	}
}

type resolver struct{}

func (resolver) ResolveBroadcast(_, _ *string) core.Identification {
	return core.Identification{Status: core.IdentUnknownOperator, Reason: "registry_unavailable", Basis: core.BasisAsBroadcast}
}

// Refusals: no current state, older than 60 s behind its answer, no
// position, an id too long; a token-safe id is its own subject token.
func TestFlightTrackRefusals(t *testing.T) {
	rx := time.Now()
	cv := Conv{Policy: policy.Defaults()}
	f := flight("f1", rx.Add(-61*time.Second))
	if _, _, err := FlightTrack("b", &f, rx, rx, cv); !errors.Is(err, ErrTooOld) {
		t.Fatalf("61 s old: %v", err)
	}
	f = flight("f1", rx)
	f.CurrentState = nil
	if _, _, err := FlightTrack("b", &f, rx, rx, cv); !errors.Is(err, ErrNoState) {
		t.Fatalf("no state: %v", err)
	}
	f = flight("f1", rx)
	f.CurrentState.Position.Lat = nil
	if _, _, err := FlightTrack("b", &f, rx, rx, cv); err == nil {
		t.Fatal("no position accepted")
	}
	f = flight(strings.Repeat("x", MaxFlightIDBytes+1), rx)
	if _, _, err := FlightTrack("b", &f, rx, rx, cv); err == nil {
		t.Fatal("a long id accepted")
	}
	if subjectToken("abc-123") != "abc-123" || subjectToken("a.b") == "a.b" || subjectToken("_x") == "_x" {
		t.Fatal("subjectToken")
	}
}

// rid_time.json's network cases (owned by ussp) through this adapter's
// placement (Place with the case's bounds), never a second judgement.
func TestVectorsRidTimeThroughThePeerPlacement(t *testing.T) {
	f := vectors.Load(t, "rid_time.json")
	type in struct {
		TimestampTenths   *uint16    `json:"timestamp_tenths"`
		TSAccuracyCode    *uint8     `json:"ts_accuracy_code"`
		StateTimestamp    *time.Time `json:"state_timestamp"`
		ResponseTimestamp *time.Time `json:"response_timestamp"`
		ReceivedAt        time.Time  `json:"received_at"`
		MaxAgeS           *float64   `json:"max_age_s"`
		TimeToleranceS    float64    `json:"time_tolerance_s"`
		MaxLatencyS       float64    `json:"max_latency_s"`
	}
	type exp struct {
		TS         time.Time `json:"ts"`
		CapturedAt time.Time `json:"captured_at"`
		TimeSource string    `json:"time_source"`
		Fallback   *string   `json:"fallback"`
		Note       *string   `json:"note"`
	}
	ran := 0
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var i in
		c.Decode(t, &i, nil)
		if i.StateTimestamp == nil || i.MaxAgeS == nil {
			t.Fatal("a ussp case that is not a network case")
		}
		ran++
		v := policy.Defaults()
		v.TelemetryAheadToleranceS = i.TimeToleranceS
		// The adapter's bounds must be the case's, or this would not be
		// the adapter's placement being checked.
		if pol := NetworkPolicy(v); pol.MaxAgeS != *i.MaxAgeS || pol.MaxLatencyS != i.MaxLatencyS {
			t.Fatalf("the case's bounds %v/%v are not the adapter's %+v", *i.MaxAgeS, i.MaxLatencyS, pol)
		}
		p, note, shown := Place(*i.StateTimestamp, i.ResponseTimestamp, i.ReceivedAt, v)
		if c.ExpectedIsNull() {
			if shown {
				t.Fatalf("shown: %+v, want not shown", p)
			}
			return
		}
		var e exp
		c.Decode(t, nil, &e)
		if !shown {
			t.Fatal("not shown")
		}
		vectors.EqualTime(t, "ts", p.TS, e.TS)
		vectors.EqualTime(t, "captured_at", p.CapturedAt, e.CapturedAt)
		want := map[string]core.TimeSource{"broadcast": core.TimeBroadcast, "receiver": core.TimeReceiver}[e.TimeSource]
		if p.Source != want {
			t.Fatalf("time_source %q, want %q", p.Source, e.TimeSource)
		}
		wantNote := timeplace.NoteNone
		if e.Note != nil {
			wantNote = timeplace.NetworkNote(*e.Note)
		}
		if note != wantNote {
			t.Fatalf("note %q, want %q", note, wantNote)
		}
	})
	if ran != 7 {
		t.Fatalf("ran %d network cases, want 7", ran)
	}
}
