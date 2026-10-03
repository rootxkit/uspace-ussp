package tsdbwriter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

const flightID = "0b6d5ee6-1f5e-4f57-9a40-2d4c8f5b8f10"

// message is an enveloped message of schema with body, at captured.
func message(t testing.TB, schema string, captured time.Time, body map[string]any) []byte {
	t.Helper()
	m := struct {
		bus.Envelope
		Body map[string]any `json:"body"`
	}{bus.NewEnvelope(schema, "ussp/telemetry-ingest", core.Times{TS: &captured, RxTS: captured.Add(100 * time.Millisecond),
		CapturedAt: captured, Source: core.TimeSourceClock}), body}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func trackBodyOf(flight any) map[string]any {
	return map[string]any{
		"track_id": "trk-1", "trust": "authenticated", "source": "operator_ws", "source_instance": "client-1",
		"position": map[string]any{"lat": 41.7151, "lng": 44.8271}, "alt_wgs84_m": 650.0, "alt_amsl_m": 630.0, "alt_source": "geodetic",
		"alt_pressure_m": nil, "height_m": 120.0, "height_ref": "TakeoffLocation", "speed_ms": 12.5, "track_deg": 90.0,
		"vspeed_ms": 0.5, "accuracy_h_m": 3.0, "accuracy_v_m": 4.0, "status": "Airborne", "emergency": false,
		"identification": map[string]any{}, "flight_id": flight, "intent_id": nil,
	}
}

func TestDecodeTelemetry(t *testing.T) {
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	d, err := DecodeTelemetry(message(t, "track/telemetry/v1", at, trackBodyOf(flightID)))
	if err != nil || len(d.Rows) != 1 || d.Rows[0].Table != &store.TableTelemetry || !d.CapturedAt.Equal(at) || !bus.ValidULID(d.MsgID) {
		t.Fatalf("%+v %v", d, err)
	}
	v := d.Rows[0].Values
	if len(v) != len(store.TableTelemetry.Columns) || v[1] != flightID || v[7] != 41.7151 || v[8] != 44.8271 || v[21] != "client-1" || v[22] != "c5:1317:2248" {
		t.Fatalf("values %v", v)
	}
	// A track without a flight is skipped, not an error.
	d, err = DecodeTelemetry(message(t, "track/telemetry/v1", at, trackBodyOf(nil)))
	if err != nil || d.Skip != SkipNotOwnFlight || len(d.Rows) != 0 {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestDecodeTelemetryRefuses(t *testing.T) {
	at := time.Now()
	for name, mut := range map[string]func(map[string]any){
		"flight not uuid":      func(b map[string]any) { b["flight_id"] = "f-1" },
		"no position":          func(b map[string]any) { delete(b, "position") },
		"latitude":             func(b map[string]any) { b["position"] = map[string]any{"lat": 91.0, "lng": 44.0} },
		"track 360":            func(b map[string]any) { b["track_deg"] = 360.0 },
		"height without ref":   func(b map[string]any) { b["height_ref"] = nil },
		"no source instance":   func(b map[string]any) { b["source_instance"] = "" },
		"speed is a string":    func(b map[string]any) { b["speed_ms"] = "fast" },
		"ref without a height": func(b map[string]any) { b["height_m"] = nil },
	} {
		b := trackBodyOf(flightID)
		mut(b)
		if d, err := DecodeTelemetry(message(t, "track/telemetry/v1", at, b)); err == nil {
			t.Errorf("%s: accepted %+v", name, d)
		}
	}
	for name, data := range map[string][]byte{
		"not json":     []byte("{"),
		"wrong schema": message(t, "track/manned/v1", at, trackBodyOf(flightID)),
		"bad envelope": []byte(`{"schema":"track/telemetry/v1","msg_id":"x","body":{}}`),
		"body array":   []byte(strings.Replace(string(message(t, "track/telemetry/v1", at, nil)), `"body":null`, `"body":[]`, 1)),
	} {
		if _, err := DecodeTelemetry(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDecodePeer(t *testing.T) {
	b := trackBodyOf(nil)
	b["trust"], b["source"], b["source_instance"], b["track_id"] = "provider", "network_rid", "peer-uss-1", "rid-9"
	d, err := DecodePeer(message(t, "track/telemetry/v1", time.Now(), b))
	if err != nil || len(d.Rows) != 1 || d.Rows[0].Table != &store.TablePeerFlights {
		t.Fatalf("%+v %v", d, err)
	}
	v := d.Rows[0].Values
	if v[1] != "peer-uss-1" || v[2] != "rid-9" || !strings.Contains(v[4].(string), `"track_id":"rid-9"`) || v[5] != "c5:1317:2248" {
		t.Fatalf("values %v", v)
	}
	if _, err := DecodePeer([]byte("x")); err == nil {
		t.Fatal("junk accepted")
	}
}

func mannedBodyOf(source, trust string) map[string]any {
	return map[string]any{
		"icao24": "4ca7b5", "callsign": "TST123", "position": map[string]any{"lat": 41.721, "lng": 44.793},
		"alt_pressure_m": 1524.0, "alt_wgs84_m": 1561.0, "gs_ms": 72.5, "track_deg": 134.0, "vrate_ms": -2.5, "emergency": false,
		"squawk": "4521", "source_class": "ads_b", "quality": map[string]any{"nic": 8}, "trust": trust, "source": source,
		"source_instance": "adsb-tbs", "state": "live",
	}
}

func TestDecodeManned(t *testing.T) {
	d, err := DecodeManned(message(t, "track/manned/v1", time.Now(), mannedBodyOf("ansp_feed", "surveillance")))
	if err != nil || d.Rows[0].Table != &store.TableManned || len(d.Rows[0].Values) != len(store.TableManned.Columns) {
		t.Fatalf("%+v %v", d, err)
	}
	v := d.Rows[0].Values
	if v[1] != "4ca7b5" || *(v[13].(*string)) != "false" || *(v[15].(*string)) != `{"nic":8}` || v[17] != "surveillance" {
		t.Fatalf("values %v", v)
	}
	d, err = DecodeManned(message(t, "track/manned/v1", time.Now(), mannedBodyOf("adsb_rx", "broadcast")))
	if err != nil || d.Rows[0].Table != &store.TableEconspicuity || len(d.Rows[0].Values) != len(store.TableEconspicuity.Columns) {
		t.Fatalf("%+v %v", d, err)
	}
	for name, b := range map[string]map[string]any{
		"receiver not broadcast": mannedBodyOf("adsb_rx", "surveillance"),
		"icao upper": func() map[string]any {
			b := mannedBodyOf("ansp_feed", "surveillance")
			b["icao24"] = "4CA7B5"
			return b
		}(),
		"track 400": func() map[string]any {
			b := mannedBodyOf("ansp_feed", "surveillance")
			b["track_deg"] = 400.0
			return b
		}(),
		"no position": func() map[string]any { b := mannedBodyOf("ansp_feed", "surveillance"); delete(b, "position"); return b }(),
	} {
		if _, err := DecodeManned(message(t, "track/manned/v1", time.Now(), b)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDecodeTrafficAndConformance(t *testing.T) {
	body := map[string]any{"client_id": "c1", "for": map[string]any{"bbox": []float64{44.7, 41.6, 44.9, 41.8}},
		"tracks":   []any{map[string]any{"track_id": "t1", "trust": "surveillance", "state": "live", "age_s": 0.5, "position": map[string]any{"lat": 1, "lng": 2}}},
		"degraded": []any{map[string]any{"input": "cis", "since": nil, "reason": "stale"}}, "policy_version": 3}
	d, err := DecodeTraffic(message(t, "traffic/product/v1", time.Now(), body))
	if err != nil || d.Rows[0].Table != &store.TableTrafficProducts {
		t.Fatalf("%+v %v", d, err)
	}
	v := d.Rows[0].Values
	if *(v[4].(*float64)) != 41.6 || *(v[5].(*float64)) != 44.7 || v[8] != `[{"track_id":"t1","trust":"surveillance","state":"live","age_s":0.5}]` ||
		v[10] != int64(3) || v[9].([]string)[0] != "cis" {
		t.Fatalf("values %v", v)
	}
	body["for"] = map[string]any{"intent_id": flightID}
	delete(body, "degraded")
	if d, err = DecodeTraffic(message(t, "traffic/product/v1", time.Now(), body)); err != nil || d.Rows[0].Values[4] != (*float64)(nil) || *(d.Rows[0].Values[3].(*string)) != flightID {
		t.Fatalf("an intent: %+v %v", d, err)
	}
	for name, mut := range map[string]func(map[string]any){
		"bbox 3":        func(b map[string]any) { b["for"] = map[string]any{"bbox": []float64{1, 2, 3}} },
		"bbox reversed": func(b map[string]any) { b["for"] = map[string]any{"bbox": []float64{44, 42, 45, 41}} },
		"no policy":     func(b map[string]any) { delete(b, "policy_version") },
		"no tracks":     func(b map[string]any) { delete(b, "tracks") },
		"no client":     func(b map[string]any) { delete(b, "client_id") },
		"intent":        func(b map[string]any) { b["for"] = map[string]any{"intent_id": "i"} },
	} {
		b := map[string]any{"client_id": "c1", "for": map[string]any{}, "tracks": []any{}, "policy_version": 1}
		mut(b)
		if _, err := DecodeTraffic(message(t, "traffic/product/v1", time.Now(), b)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c, err := DecodeConformance(message(t, "conformance/state/v1", time.Now(), map[string]any{
		"flight_id": flightID, "state": "nonconforming", "distance_outside_m": 42.0, "height_over_m": nil}))
	if err != nil || c.Rows[0].Table != &store.TableConformanceSamples || c.Rows[0].Values[3] != "nonconforming" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := DecodeConformance(message(t, "conformance/state/v1", time.Now(), map[string]any{"flight_id": "x", "state": "a"})); err == nil {
		t.Fatal("bad flight accepted")
	}
}

// Every stream's tables have staging columns, an INSERT from their
// staging table, and decoders that fill exactly those columns.
func TestStreamsAndTablesAgree(t *testing.T) {
	if len(Streams) != 5 {
		t.Fatal(len(Streams))
	}
	for _, s := range Streams {
		for _, tb := range s.Tables {
			if len(tb.Columns) == 0 || !strings.Contains(tb.Insert, tb.Stage()) || !strings.Contains(tb.Insert, "ON CONFLICT DO NOTHING") ||
				!strings.HasPrefix(tb.DDL(), "CREATE TEMP TABLE IF NOT EXISTS "+tb.Stage()) || len(tb.ColumnNames()) != len(tb.Columns) {
				t.Errorf("%s: %+v", tb.Name, tb)
			}
		}
	}
}
