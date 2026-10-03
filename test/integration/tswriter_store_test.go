//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/tsdbwriter"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Past the in-memory window the unique index is the dedupe: the same
// messages republished write nothing and are counted from the
// database's ON CONFLICT.
func TestIntegrationWriterDedupeByTheIndex(t *testing.T) {
	r := newWriterRig(t, writerOpts{dedupeWindow: time.Nanosecond})
	msgs := make([][]byte, 100)
	for i := range msgs {
		msgs[i] = r.track(time.Now().Add(time.Duration(i) * time.Millisecond))
	}
	r.publish(msgs)
	r.waitRows(100, 30*time.Second)
	r.publish(msgs)
	deadline := time.Now().Add(30 * time.Second)
	for r.counter(tsdbwriter.CounterDedupeHits) < 100 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if r.rows() != 100 || r.counter(tsdbwriter.CounterDedupeHits) != 100 {
		t.Fatalf("rows %d counters %v", r.rows(), r.pipes[0].Counters.Snapshot())
	}
}

func envelopeOf(t *testing.T, schema string, at time.Time, body map[string]any) []byte {
	t.Helper()
	m := struct {
		bus.Envelope
		Body map[string]any `json:"body"`
	}{bus.NewEnvelope(schema, "ussp/monitor", core.Times{TS: &at, RxTS: at, CapturedAt: at, Source: core.TimeSourceClock}), body}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Every table's staging copy and INSERT run against the real schema:
// one row each written, the same rows again are duplicates, a gap and
// the position commit beside them (at on the database clock), and a row
// the database refuses is a data error (the writer's rejected path) that
// writes nothing of its batch.
func TestIntegrationTSWriterEveryTable(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	w := store.TSWriter{Pool: tsOwner(t)}
	at := time.Now().UTC().Truncate(time.Millisecond)
	flight := newFlightID()
	track := map[string]any{
		"track_id": "rid-1", "trust": "provider", "source": "network_rid", "source_instance": "peer-1",
		"position": map[string]any{"lat": 41.7, "lng": 44.8}, "alt_wgs84_m": 600.0, "alt_amsl_m": 580.0, "alt_source": "geodetic",
		"alt_pressure_m": nil, "height_m": 50.0, "height_ref": "GroundLevel", "speed_ms": 10.0, "track_deg": 10.0, "vspeed_ms": 0.0,
		"accuracy_h_m": 3.0, "accuracy_v_m": 3.0, "status": "Airborne", "emergency": false, "identification": map[string]any{},
		"flight_id": flight, "intent_id": nil,
	}
	manned := func(source, trust string) map[string]any {
		return map[string]any{"icao24": "4ca7b5", "callsign": "TST1", "position": map[string]any{"lat": 41.7, "lng": 44.8},
			"alt_pressure_m": 1500.0, "alt_wgs84_m": 1540.0, "gs_ms": 70.0, "track_deg": 100.0, "vrate_ms": 0.0, "emergency": false,
			"source_class": "ads_b", "quality": map[string]any{"nic": 8}, "trust": trust, "source": source, "source_instance": "rx-1", "state": "live"}
	}
	var ds []tsdbwriter.Decoded
	add := func(d tsdbwriter.Decoded, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, d)
	}
	add(tsdbwriter.DecodeTelemetry(envelopeOf(t, "track/telemetry/v1", at, track)))
	add(tsdbwriter.DecodePeer(envelopeOf(t, "track/telemetry/v1", at, track)))
	add(tsdbwriter.DecodeManned(envelopeOf(t, "track/manned/v1", at, manned("ansp_feed", "surveillance"))))
	add(tsdbwriter.DecodeManned(envelopeOf(t, "track/manned/v1", at, manned("adsb_rx", "broadcast"))))
	add(tsdbwriter.DecodeTraffic(envelopeOf(t, "traffic/product/v1", at, map[string]any{"client_id": "c1",
		"for": map[string]any{"intent_id": flight, "bbox": []float64{44.7, 41.6, 44.9, 41.8}}, "tracks": []any{},
		"degraded": []any{map[string]any{"input": "cis_stale", "since": nil, "reason": "integration"}}, "policy_version": 1})))
	add(tsdbwriter.DecodeConformance(envelopeOf(t, "conformance/state/v1", at, map[string]any{"flight_id": flight, "state": "conforming",
		"distance_outside_m": 0.0, "height_over_m": nil})))
	stream := "WP6-EVERY-" + flight
	b := store.WriteBatch{Stream: stream, Rows: map[*store.CopyTable][][]any{}, Position: 6, Gaps: []store.Gap{{
		DedupeKey: "test:" + flight, Stream: stream, Subject: "trk.v1.>", FromSeq: 1, ToSeq: 2, Cause: store.CauseStreamRemoved,
		Count: 2, CountUnit: store.UnitRows, AfterAt: &at, BeforeAt: &at, Detail: "integration",
	}}}
	for _, d := range ds {
		for _, r := range d.Rows {
			b.Rows[r.Table] = append(b.Rows[r.Table], r.Values)
		}
	}
	got, err := w.Write(ctx, b)
	if err != nil || got.Inserted != 6 || got.Duplicates != 0 || got.Gaps != 1 {
		t.Fatalf("first write %+v %v", got, err)
	}
	got, err = w.Write(ctx, b)
	if err != nil || got.Inserted != 0 || got.Duplicates != 6 || got.Gaps != 0 {
		t.Fatalf("second write %+v %v", got, err)
	}
	if pos, ok, err := w.Position(ctx, stream); err != nil || !ok || pos != 6 {
		t.Fatalf("position %d %v %v", pos, ok, err)
	}
	if _, ok, err := w.Position(ctx, "never-written-"+flight); ok || err != nil {
		t.Fatalf("unknown stream: %v %v", ok, err)
	}
	owner := tsOwner(t)
	msgOf := func(table string) string {
		for _, d := range ds {
			if d.Rows[0].Table.Name == table {
				return d.MsgID
			}
		}
		return ""
	}
	for table, check := range map[string][2]string{
		"telemetry":           {"flight_id = $1::uuid AND ST_Y(geom) = 41.7 AND ts IS NOT NULL", flight},
		"peer_flights":        {"state->>'flight_id' = $1 AND cell5 = 'c5:1317:2248'", flight},
		"manned_tracks":       {"msg_id = $1 AND quality = '{\"nic\":8}'", msgOf("manned_tracks")},
		"econspicuity_tracks": {"msg_id = $1 AND receiver_id = 'rx-1'", msgOf("econspicuity_tracks")},
		"traffic_products":    {"intent_id = $1::uuid AND ST_XMin(bbox) = 44.7 AND degraded = '{cis_stale}'", flight},
		"conformance_samples": {"flight_id = $1::uuid AND state = 'conforming'", flight},
	} {
		if n := count(t, owner, "SELECT count(*) FROM "+table+" WHERE "+check[0], check[1]); n != 1 {
			t.Errorf("%s: %d rows", table, n)
		}
	}
	var gapAt time.Time
	if err := owner.QueryRow(ctx, "SELECT at FROM writer_gaps WHERE dedupe_key = $1", "test:"+flight).Scan(&gapAt); err != nil ||
		time.Since(gapAt).Abs() > time.Minute {
		t.Fatalf("gap at %v %v", gapAt, err)
	}
	// track_deg 400 past the decoder: the CHECK refuses it.
	bad := slices.Clone(ds[0].Rows[0].Values)
	bad[0], bad[15] = bus.NewULID(at), 400.0
	_, err = w.Write(ctx, store.WriteBatch{Stream: stream, Rows: map[*store.CopyTable][][]any{&store.TableTelemetry: {bad}}, Position: 7})
	if !store.IsDataError(err) {
		t.Fatalf("refused row: %v", err)
	}
	if pos, _, _ := w.Position(ctx, stream); pos != 6 {
		t.Fatalf("position moved with a refused batch: %d", pos)
	}
	if store.IsDataError(errors.New("dial: connection refused")) {
		t.Fatal("a connection error is not a data error")
	}
	if !strings.Contains(store.TableTelemetry.DDL(), "ON COMMIT DELETE ROWS") {
		t.Fatal("staging rows outlive their transaction")
	}
}
