//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/occurrence"
	occstore "github.com/rootxkit/uspace-ussp/internal/occurrence/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/records"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

// kvHolds writes record_holds on the real NATS, as api does.
type kvHolds struct{ kv *bus.Projector }

func (h kvHolds) Hold(ctx context.Context, id string, reasons []string, since time.Time) error {
	return h.kv.PutJSON(ctx, bus.BucketRecordHolds, bus.KeyToken(id), records.Hold{FlightID: id, Reasons: reasons, Since: since})
}

func seedProximity(t *testing.T, s seeded, pairID string, hM, vM any, raised time.Time) string {
	t.Helper()
	d, _ := json.Marshal(map[string]any{"d_cpa_h_m": hM, "d_alt_m": vM, "pair_id": pairID, "peer": map[string]any{"track_id": "man:4ca1f0", "trust": "surveillance"}})
	var id string
	if err := appPool(t).QueryRow(context.Background(), `INSERT INTO alerts (kind, flight_id, intent_id, severity, state, raised_at, updated_at, detail, policy_version)
		VALUES ('proximity', $1::uuid, $2::uuid, 'critical', 'raised', $3, $3, $4, 1) RETURNING id::text`, s.flightID, s.intentID, raised, d).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func reportsAbout(t *testing.T, flightID string) int64 {
	t.Helper()
	return count(t, appPool(t), "SELECT count(*) FROM occurrence_reports WHERE $1::uuid = ANY(flight_ids)", flightID)
}

// The done-when occurrence against the real database: an airprox from a
// proximity alert within the thresholds is queued with deadline_at 72 h
// after awareness and its flight held in record_holds; one beyond the
// thresholds is not (E-01 pair); delivered to the fake authority, and
// with the authority down tried again; a report past its deadline is a
// critical console item while one delivered in time is not.
func TestIntegrationOccurrences(t *testing.T) {
	ctx := context.Background()
	near := seedFlight(t, "activated", nil, time.Now().Add(-10*time.Minute))
	far := seedFlight(t, "activated", nil, time.Now().Add(-10*time.Minute))
	nearAlert := seedProximity(t, near, "pair-"+unique(), 20.0, 5.0, time.Now().Add(-time.Minute))
	seedProximity(t, far, "pair-"+unique(), 500.0, 5.0, time.Now().Add(-time.Minute))

	conn := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	if _, err := bus.Ensure(ctx, conn.JetStream(), bus.DefaultTopology()); err != nil {
		t.Fatal(err)
	}
	fake := authority.New()
	t.Cleanup(fake.Close)
	fake.Down()
	st := occstore.Store{S: appStore(t)}
	client, err := occurrence.NewClient(fake.URL(), authority.Tokens{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := &occurrence.Service{Store: st, Deliverer: client, Holds: kvHolds{bus.NewProjector(conn, nil)},
		Policy: func() policy.Values { return policy.Defaults() }, SystemID: "USSP-DEV", Counters: &core.Counters{}, Logger: quiet()}
	if err := svc.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	if n := reportsAbout(t, near.flightID); n != 1 {
		t.Fatalf("the airprox queued %d reports", n)
	}
	if n := reportsAbout(t, far.flightID); n != 0 {
		t.Errorf("a proximity beyond the thresholds queued %d reports", n)
	}
	var aware, deadline time.Time
	if err := appPool(t).QueryRow(ctx, "SELECT became_aware_at, deadline_at FROM occurrence_reports WHERE $1::uuid = ANY(flight_ids)", near.flightID).Scan(&aware, &deadline); err != nil {
		t.Fatal(err)
	}
	if deadline.Sub(aware) != 72*time.Hour {
		t.Errorf("deadline %v after awareness", deadline.Sub(aware))
	}
	kv, err := conn.JetStream().KeyValue(ctx, bus.BucketRecordHolds)
	if err != nil {
		t.Fatal(err)
	}
	e, err := kv.Get(ctx, bus.KeyToken(near.flightID))
	if err != nil || !strings.Contains(string(e.Value()), near.flightID) {
		t.Fatalf("the flight is not held: %v", err)
	}
	if _, err := kv.Get(ctx, bus.KeyToken(far.flightID)); err == nil {
		t.Error("a flight no report names is held")
	}

	// The authority down: tried again, the error kept for the console.
	if svc.DeliverDue(ctx) != 0 {
		t.Fatal("delivered while the authority was down")
	}
	items, _, err := st.Open(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	var mine *occurrence.Item
	for i := range items {
		if len(items[i].FlightIDs) > 0 && items[i].FlightIDs[0] == near.flightID {
			mine = &items[i]
		}
	}
	if mine == nil || mine.State != "pending" || mine.Attempts != 1 || mine.LastError == nil || mine.Critical || mine.TimeToDeadlineS < 71*3600 {
		t.Fatalf("while down: %+v", mine)
	}
	fake.Up()
	if _, err := appPool(t).Exec(ctx, "UPDATE occurrence_reports SET next_at = now() WHERE report_ref = $1", mine.ReportRef); err != nil {
		t.Fatal(err)
	}
	if svc.DeliverDue(ctx) < 1 {
		t.Fatal("not delivered once the authority was back")
	}
	got := fake.Occurrences()
	var sent occurrence.Payload
	if len(got) == 0 || json.Unmarshal(got[len(got)-1].Body, &sent) != nil || sent.Category != occurrence.KindAirprox || len(sent.Manned) != 1 ||
		sent.Manned[0].ICAO24 != "4ca1f0" || sent.ReportRef != mine.ReportRef {
		t.Fatalf("at the authority: %d reports, last %+v", len(got), sent)
	}
	var state string
	if err := appPool(t).QueryRow(ctx, "SELECT state FROM occurrence_reports WHERE report_ref = $1", mine.ReportRef).Scan(&state); err != nil || state != "delivered" {
		t.Fatalf("state %s %v", state, err)
	}

	// A supervisor's flag of the same alert answers the existing report.
	it, created, err := svc.Flag(ctx, "staff-1", nearAlert, "", "")
	if err != nil || created || it.ReportRef != mine.ReportRef {
		t.Fatalf("flag again: %+v %v %v", it, created, err)
	}

	// Past the deadline: critical (an old alert flagged now, its awareness
	// moved back by the test as if it were 73 h old).
	late := seedFlight(t, "activated", nil, time.Now().Add(-10*time.Minute))
	lateAlert := seedProximity(t, late, "pair-"+unique(), 900.0, 300.0, time.Now().Add(-time.Minute))
	lit, created, err := svc.Flag(ctx, "staff-1", lateAlert, occurrence.KindOther, "")
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if _, err := appPool(t).Exec(ctx, `UPDATE occurrence_reports SET became_aware_at = now() - interval '73 hours', deadline_at = now() - interval '1 hour',
		next_at = now() + interval '1 hour' WHERE report_ref = $1`, lit.ReportRef); err != nil {
		t.Fatal(err)
	}
	items, _, _ = st.Open(ctx, 500)
	critical := false
	for i := range items {
		if items[i].ReportRef == lit.ReportRef {
			critical = items[i].Critical && items[i].TimeToDeadlineS < 0
		}
		if items[i].ReportRef == mine.ReportRef {
			t.Error("a delivered report is listed")
		}
	}
	if !critical {
		t.Fatal("the report past its deadline is not critical")
	}
	if state, _ := occurrence.Probe(st, true)(ctx); state != "down" {
		t.Errorf("readyz %s with a report past its deadline", state)
	}
	if _, err := appPool(t).Exec(ctx, "UPDATE occurrence_reports SET state = 'delivered', submitted_at = now() WHERE report_ref = $1", lit.ReportRef); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.Flag(ctx, "staff-1", "00000000-0000-4000-8000-000000000000", "", "")
	var fe *occurrence.FlagError
	if !errors.As(err, &fe) || !fe.NotFound {
		t.Errorf("unknown alert: %v", err)
	}
}

// The held flights read in pages: the pages, each within its bound, are
// the whole held set in id order, past the size of one page (E-10).
func TestIntegrationOccurrenceHeldPages(t *testing.T) {
	ctx := context.Background()
	for range 3 {
		f := seedFlight(t, "activated", nil, time.Now().Add(-10*time.Minute))
		seedProximity(t, f, "pair-"+unique(), 20.0, 5.0, time.Now().Add(-time.Minute))
	}
	st := occstore.Store{S: appStore(t)}
	svc := &occurrence.Service{Store: st, Policy: policy.Defaults, SystemID: "USSP-DEV",
		Counters: &core.Counters{}, Logger: quiet()}
	if err := svc.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := appPool(t).Query(ctx, "SELECT DISTINCT f::text FROM occurrence_reports, unnest(flight_ids) AS f ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	rows.Close()
	if len(want) < 3 {
		t.Fatalf("only %d held flights seeded", len(want))
	}
	const page = 2
	var got []string
	after := ""
	for {
		ids, err := st.Held(ctx, after, page)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) > page {
			t.Fatalf("a page of %d, asked for %d", len(ids), page)
		}
		got = append(got, ids...)
		if len(ids) < page {
			break
		}
		after = ids[len(ids)-1]
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paged %v\nwant %v", got, want)
	}
}
