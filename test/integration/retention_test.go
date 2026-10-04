//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/records"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

func positionsKept(t *testing.T, flightID string) int64 {
	t.Helper()
	return count(t, tsOwner(t), "SELECT count(*) FROM telemetry WHERE flight_id = $1::uuid AND operator_position IS NOT NULL", flightID)
}

// The retention of the hypertables against TimescaleDB: telemetry's
// retention policy follows the policy row (and is put back after);
// the remote pilot's position goes from telemetry older than the
// policy's days, except a held flight's and except younger rows (E-01:
// each removal is paired with what it keeps).
func TestIntegrationRetention(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	ret := store.TSRetention{Pool: tsOwner(t)}
	before, err := ret.TelemetryRetentionDays(ctx)
	if err != nil || before < policy.TelemetryRetentionFloorDays {
		t.Fatalf("the migration's retention: %d %v", before, err)
	}
	t.Cleanup(func() {
		if err := ret.SetTelemetryRetentionDays(context.Background(), before); err != nil {
			t.Error(err)
		}
	})

	held, loose := unique(), unique()
	ids := map[string]string{}
	for _, k := range []string{held, loose} {
		ids[k] = fmt.Sprintf("%08x-0000-4000-8000-%012s", time.Now().UnixNano()&0xffffffff, k[len(k)-12:])
	}
	now := time.Now().UTC()
	for k, id := range ids {
		for i, age := range []time.Duration{100 * 24 * time.Hour, 99 * 24 * time.Hour, 10 * 24 * time.Hour} {
			if _, err := tsOwner(t).Exec(ctx, `INSERT INTO telemetry (flight_id, captured_at, rx_ts, time_source, geom, cell5, msg_id, operator_position)
				VALUES ($1::uuid, $2, $2, 'operator', ST_SetSRID(ST_MakePoint(44.78, 41.71), 4326), 'c5:417:447', $3,
				        ST_SetSRID(ST_MakePoint(44.77, 41.70), 4326))`, id, now.Add(-age), fmt.Sprintf("ret-%s-%d", k, i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	pol := policy.Defaults()
	pol.TelemetryRetentionDays = before + 30
	r := &records.Retention{Store: ret, Policy: func() (policy.Values, bool) { return pol, true },
		Holds: func() ([]string, bool) { return []string{ids[held]}, true }}
	done, err := r.RunOnce(ctx)
	if err != nil || !done {
		t.Fatal(done, err)
	}
	if got, _ := ret.TelemetryRetentionDays(ctx); got != before+30 {
		t.Fatalf("telemetry retention %d, want %d", got, before+30)
	}
	if n := positionsKept(t, ids[loose]); n != 1 {
		t.Errorf("the flight nobody holds kept %d positions, want only its 10-day-old one", n)
	}
	if n := positionsKept(t, ids[held]); n != 3 {
		t.Errorf("the held flight kept %d positions, want all 3", n)
	}
	if n := count(t, tsOwner(t), "SELECT count(*) FROM telemetry WHERE flight_id = $1::uuid", ids[loose]); n != 3 {
		t.Errorf("rows removed with the positions: %d left", n)
	}
}
