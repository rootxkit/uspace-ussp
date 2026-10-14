package records

import (
	"context"
	"testing"
	"time"
	_ "time/tzdata" // the clocks below in zones with DST, whatever the host has

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// windowDates are the days the boundary clocks are pinned on: the EU and
// US DST changes, a month end, a year end and a leap day.
var windowDates = []time.Time{
	time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC),   // US DST starts
	time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC),  // EU DST starts
	time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), // the CI run that failed at 01:28
	time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), // EU DST ends
	time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),  // US DST ends, a month's first day
	time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),   // a year's first day
	time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),  // a leap day
}

func windowZones(t *testing.T) []*time.Location {
	t.Helper()
	out := []*time.Location{time.UTC}
	for _, name := range []string{"Europe/Berlin", "America/New_York", "Asia/Tbilisi"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, loc)
	}
	return out
}

func emptyDaily(t *testing.T, now time.Time) (*Daily, *memReader) {
	t.Helper()
	m, s := fixture()
	m.now = now
	return &Daily{Builder: builder(m, s), Store: m, Dir: t.TempDir(), Counters: &core.Counters{}}, m
}

// A fresh api (no bundle yet) builds every day its alarm then reads, at
// every hour's boundary (a second before it and on it), on DST change
// days, a month's and a year's first day and a leap day, with the
// database clock in UTC and in zones with DST: the alarm is never
// raised by the window alone. The CI smoke failed on "records: day
// 2026-10-06 missing" at 01:28 UTC: from 01:00 to 02:00 the job read
// back from now-25h and the alarm from now-26h, so the alarm's first
// day was one the job never built. The presence: a day of the alarm's
// window removed afterwards is named.
func TestBuildDueCoversTheProbeAtEveryHour(t *testing.T) {
	ctx := context.Background()
	zones := windowZones(t)
	for _, date := range windowDates {
		for h := range 24 {
			for i, off := range []time.Duration{-time.Second, 0} {
				at := date.Add(time.Duration(h)*time.Hour + off)
				now := at.In(zones[(2*h+i)%len(zones)])
				d, m := emptyDaily(t, now)
				if _, err := d.BuildDue(ctx); err != nil {
					t.Fatalf("%s: %v", now.Format(time.RFC3339), err)
				}
				state, detail := d.Probe()(ctx)
				if state != obs.StateUp {
					t.Fatalf("a fresh api at %s (%s UTC): %s %s", now.Format(time.RFC3339), at.UTC().Format("15:04:05"), state, detail)
				}
				missed := Day(now.Add(-MissAfter))
				delete(m.bundles, missed.AddDate(0, 0, -(CatchUpDays-1)).Format(time.DateOnly))
				if state, detail := d.Probe()(ctx); state != obs.StateDegraded ||
					detail != "records: day "+missed.AddDate(0, 0, -(CatchUpDays-1)).Format(time.DateOnly)+" missing" {
					t.Fatalf("at %s, the alarm's first day removed: %s %s", now.Format(time.RFC3339), state, detail)
				}
			}
		}
	}
}

// A running api, its job run every minute (Daily.Run's interval) and its
// alarm read after each run, from a fresh start at 01:30 UTC through
// three days across the EU DST end (2026-10-25) and UTC midnight, the
// clock in Europe/Berlin: never degraded. The day's bundle is built at
// 01:00 UTC the next day and not before.
func TestRunningDailyIsNeverDegradedByTheWindow(t *testing.T) {
	ctx := context.Background()
	berlin := windowZones(t)[1]
	start := time.Date(2026, 10, 24, 1, 30, 0, 0, time.UTC)
	d, m := emptyDaily(t, start.In(berlin))
	for now := start; now.Before(start.Add(72 * time.Hour)); now = now.Add(time.Minute) {
		m.mu.Lock()
		m.now = now.In(berlin)
		m.mu.Unlock()
		if _, err := d.BuildDue(ctx); err != nil {
			t.Fatalf("%s: %v", now.Format(time.RFC3339), err)
		}
		if state, detail := d.Probe()(ctx); state != obs.StateUp {
			t.Fatalf("at %s UTC: %s %s", now.Format(time.RFC3339), state, detail)
		}
		yesterday := Day(now).AddDate(0, 0, -1).Format(time.DateOnly)
		_, built := m.bundles[yesterday]
		if due := now.Sub(Day(now)) >= BuildAfter-24*time.Hour; built != due {
			t.Fatalf("at %s UTC: %s built %v, due %v", now.Format(time.RFC3339), yesterday, built, due)
		}
	}
}
