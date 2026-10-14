//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// The days without a bundle are UTC dates whatever the session's
// TimeZone: the job (records.Daily.BuildDue) and its alarm (Probe) both
// read them from MissingRecordDays, and the bundles are keyed by UTC
// day. Pacific/Apia skipped 2011-12-30 (UTC-10 to UTC+14): a series
// stepped through timestamptz in that zone left the day out, so it was
// neither built nor alarmed. The presence: a day with a bundle in the
// range is left out, and only it.
func TestIntegrationMissingRecordDaysIgnoreTheSessionTimeZone(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	first := time.Date(2011, 12, 28, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 0, 4)
	owner := relOwner(t)
	if _, err := owner.Exec(ctx, "DELETE FROM record_bundles WHERE date BETWEEN $1 AND $2", store.Date(first), store.Date(last)); err != nil {
		t.Fatal(err)
	}
	kept := first.AddDate(0, 0, 4)
	if _, err := owner.Exec(ctx, `INSERT INTO record_bundles (date, built_at, content_hash, storage_ref, flights)
		VALUES ($1, now(), repeat('a', 64), '2012-01-01-aaaaaaaaaaaa.jsonl.gz', 0)`, store.Date(kept)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), "DELETE FROM record_bundles WHERE date BETWEEN $1 AND $2", store.Date(first), store.Date(last))
	})
	for _, tz := range []string{"UTC", "Pacific/Apia", "America/Santiago", "Asia/Tbilisi"} {
		conn, err := pgx.Connect(ctx, mustEnv(t, "USSP_TEST_PG_URL"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "SELECT set_config('TimeZone', $1, false)", tz); err != nil {
			t.Fatal(err)
		}
		days, err := relational.New(conn).MissingRecordDays(ctx, relational.MissingRecordDaysParams{FirstDay: store.Date(first), LastDay: store.Date(last)})
		_ = conn.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, d := range days {
			got = append(got, store.DateTime(d).Format(time.DateOnly))
		}
		want := []string{"2011-12-28", "2011-12-29", "2011-12-30", "2011-12-31"}
		if len(got) != len(want) {
			t.Fatalf("TimeZone %s: %v, want %v", tz, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("TimeZone %s: %v, want %v", tz, got, want)
			}
		}
	}
}
