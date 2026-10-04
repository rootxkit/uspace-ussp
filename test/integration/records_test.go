//go:build integration

package integration

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/records"
	recstore "github.com/rootxkit/uspace-ussp/internal/records/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// seedTelemetry writes one sample a second from start for n seconds,
// leaving out the seconds in skip, as tsdb-writer would.
func seedTelemetry(t *testing.T, flightID, clientID string, start time.Time, n int, skip map[int]bool) {
	t.Helper()
	ts := tsOwner(t)
	for i := range n {
		if skip[i] {
			continue
		}
		at := start.Add(time.Duration(i) * time.Second)
		if _, err := ts.Exec(context.Background(), `INSERT INTO telemetry (flight_id, captured_at, ts, rx_ts, time_source, geom, alt_amsl_m, cell5, msg_id, source_client_id,
			    operator_position)
			VALUES ($1::uuid, $2, $2, $2, 'operator', ST_SetSRID(ST_MakePoint(44.785 + $3 * 0.00001, 41.715), 4326), 530 + $3 * 0.1, 'c5:417:447', $4, $5,
			    ST_SetSRID(ST_MakePoint(44.78, 41.71), 4326))`, flightID, at, float64(i), fmt.Sprintf("m-%s-%d", flightID, i), clientID); err != nil {
			t.Fatal(err)
		}
	}
}

func seedAlert(t *testing.T, s seeded, kind string, raised, cleared time.Time, version int64) {
	t.Helper()
	if _, err := relApp(t).Exec(context.Background(), `INSERT INTO alerts (kind, flight_id, intent_id, severity, state, raised_at, updated_at, cleared_at,
		    clear_reason, detail, policy_version)
		VALUES ($1, $2::uuid, $3::uuid, 'critical', 'cleared', $4, $5, $5, 'resolved', '{"d_cpa_h_m": 41.5}', $6)`, kind, s.flightID, s.intentID, raised, cleared, version); err != nil {
		t.Fatal(err)
	}
}

func recordBuilder(t *testing.T) *records.Builder {
	t.Helper()
	st, err := store.Open(context.Background(), store.Config{RelURL: mustEnv(t, "USSP_TEST_PG_URL"), TSURL: mustEnv(t, "USSP_TEST_TS_URL"), RelRole: store.AppRole, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return &records.Builder{Reader: recstore.Reader{S: st}, Series: recstore.Series{S: st}, USSPID: "USSP-DEV",
		Policy: func() policy.Record { return policy.Record{Version: 1, Values: policy.Defaults()} }}
}

var personal = []string{"name", "display_name", "username", "email", "contact_email", "phone", "address"}

func walkNoNames(t *testing.T, path string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			for _, f := range personal {
				if strings.EqualFold(k, f) || strings.HasSuffix(strings.ToLower(k), "_"+f) {
					t.Errorf("%s.%s: a personal-data member in the record", path, k)
				}
			}
			walkNoNames(t, path+"."+k, c)
		}
	case []any:
		for _, c := range x {
			walkNoNames(t, path+"[]", c)
		}
	}
}

// The done-when record against the real databases: a flight with one
// gap the work queue explains, a proximity and a nonconformance alert
// both raised and cleared, its timeline and the products its operator
// was shown; no personal-data member anywhere (a walk of the JSON), no
// free text of the request, the registration's secret part left out.
// Then the alert table made unreadable: alerts unavailable, never an
// empty list (B-13), and readable again the twin says included.
func TestIntegrationFlightRecord(t *testing.T) {
	ctx := context.Background()
	start := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	s := seedFlight(t, "ended", nil, start)
	seedTelemetry(t, s.flightID, s.clientID, start, 120, map[int]bool{40: true, 41: true, 42: true, 43: true, 44: true, 45: true, 46: true})
	b := recordBuilder(t)
	reader := b.Reader.(recstore.Reader)
	if err := reader.RecordGap(ctx, records.Gap{MsgID: "gap-" + unique(), SourceInstance: s.clientID, Cause: "ingest_queue_age",
		Started: start.Add(41 * time.Second), Ended: start.Add(46 * time.Second), Dropped: 6}); err != nil {
		t.Fatal(err)
	}
	seedState(t, s.flightID, "conforming", "", start, 0, 0)
	seedState(t, s.flightID, "nonconforming", "above_upper", start.Add(60*time.Second), 0, 18)
	seedState(t, s.flightID, "conforming", "", start.Add(90*time.Second), 0, 0)
	seedAlert(t, s, "proximity", start.Add(20*time.Second), start.Add(30*time.Second), 1)
	seedAlert(t, s, "nonconformance", start.Add(60*time.Second), start.Add(90*time.Second), 1)
	for i := range 3 {
		if _, err := tsOwner(t).Exec(ctx, `INSERT INTO traffic_products (client_id, at, intent_id, tracks_shown, degraded, policy_version, msg_id)
			VALUES ($1, $2, $3::uuid, '[{"id":"trk:peer","trust":"authenticated","age_s":1}]', '{}', 1, $4)`,
			s.clientID, start.Add(time.Duration(10*(i+1))*time.Second), s.intentID, "p-"+unique()); err != nil {
			t.Fatal(err)
		}
	}
	r, err := b.Flight(ctx, s.flightID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	walkNoNames(t, "record", doc)
	for _, leak := range []string{"Nino", "555-0100", s.regPublic + "-abc", "wp15@example.invalid", "WP-15 operator"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the record carries %q", leak)
		}
	}
	tel := r.Telemetry
	if tel.State != records.StateIncluded || tel.Samples != 113 || len(tel.Holes) != 1 || tel.Holes[0].Cause != "ingest_queue_age" || tel.Holes[0].GapS != 8 ||
		tel.MaxAltAMSLM == nil || *tel.MaxAltAMSLM < 541 {
		t.Fatalf("telemetry %+v", tel)
	}
	if r.Alerts.State != records.StateIncluded || len(r.Alerts.Items) != 2 || r.Alerts.Items[0].Kind != "proximity" || r.Alerts.Items[0].ClearedAt == nil ||
		r.Alerts.Items[1].Kind != "nonconformance" || r.Alerts.Items[1].ClearReason == nil {
		t.Fatalf("alerts %+v", r.Alerts)
	}
	if len(r.Conformance.Items) != 3 || r.Conformance.Items[1].State != "nonconforming" || len(r.TrafficProducts.Items) != 3 ||
		len(r.Intent.Versions) != 2 || r.Intent.Intent == nil || r.Flight.OperatorRegPublic == nil || *r.Flight.OperatorRegPublic != s.regPublic {
		t.Fatalf("timeline %d products %d versions %d flight %+v", len(r.Conformance.Items), len(r.TrafficProducts.Items), len(r.Intent.Versions), r.Flight)
	}
	t.Logf("record of %s: %d bytes, %d samples, holes %+v", s.flightID, len(raw), tel.Samples, tel.Holes)

	owner := relOwner(t)
	if _, err := owner.Exec(ctx, "REVOKE SELECT ON alerts FROM ussp_app"); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			if _, err := owner.Exec(context.Background(), "GRANT SELECT ON alerts TO ussp_app"); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(restore)
	r, err = b.Flight(ctx, s.flightID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Alerts.State != records.StateUnavailable || r.Alerts.Items != nil || !strings.Contains(r.Alerts.Reason, "permission denied") {
		t.Fatalf("an unreadable alert table: %+v", r.Alerts)
	}
	if r.Telemetry.State != records.StateIncluded || r.Conformance.State != records.StateIncluded {
		t.Fatal("the unreadable alert table took other sections down")
	}
	restore()
	if r, _ = b.Flight(ctx, s.flightID); r.Alerts.State != records.StateIncluded {
		t.Fatalf("readable again: %+v", r.Alerts.Section)
	}
}

// The done-when bundle against the real databases: built for a day, its
// hash is the file's, served by Open, and the day is no longer missing
// while the days around it are.
func TestIntegrationDailyBundle(t *testing.T) {
	ctx := context.Background()
	// A day of its own far in the past (record_bundles is keyed by date).
	day := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, rand.IntN(7000))
	s := seedFlight(t, "ended", nil, day.Add(10*time.Hour))
	b := recordBuilder(t)
	reader := b.Reader.(recstore.Reader)
	d := &records.Daily{Builder: b, Store: reader, Dir: t.TempDir()}
	got, ok, err := d.Build(ctx, day)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	raw, err := os.ReadFile(filepath.Join(d.Dir, got.Ref))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != got.Hash || got.Flights != 1 {
		t.Fatalf("bundle %+v", got)
	}
	stored, f, err := d.Open(ctx, day)
	if err != nil || stored.Hash != got.Hash {
		t.Fatalf("open: %+v %v", stored, err)
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	var lines int
	for sc.Scan() {
		var r records.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.FlightID != s.flightID {
			t.Fatalf("line %d: %v %s", lines, err, r.FlightID)
		}
		lines++
	}
	_ = f.Close()
	if lines != 1 {
		t.Fatalf("%d lines", lines)
	}
	missing, err := reader.MissingDays(ctx, day.AddDate(0, 0, -1), day.AddDate(0, 0, 1))
	if err != nil || len(missing) != 2 || !missing[0].Equal(day.AddDate(0, 0, -1)) || !missing[1].Equal(day.AddDate(0, 0, 1)) {
		t.Fatalf("missing %v %v", missing, err)
	}
	if n := count(t, relApp(t), "SELECT count(*) FROM events WHERE entity_type = 'record_bundle' AND entity_id = $1", day.Format(time.DateOnly)); n != 1 {
		t.Errorf("events rows for the bundle: %d", n)
	}
}
