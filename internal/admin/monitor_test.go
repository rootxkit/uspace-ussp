package admin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// Q35: an instance that reports its input down is a monitoring outage
// on the inputs page (down, saying so); once back, the grace before a
// silent flight is judged lost is said beside up; an instance that
// reports neither is up with nothing said (presence and absence).
func TestMonitorSaysAMonitoringOutage(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	status := func(mut func(*bus.MonitorStatus)) *Inputs {
		st := bus.MonitorStatus{Instance: "m1", At: now}
		mut(&st)
		return &Inputs{Monitors: func(context.Context) ([]bus.MonitorEntry, error) {
			return []bus.MonitorEntry{{Status: st, Stored: now}}, nil
		}}
	}
	down := now.Add(-time.Minute)
	m := status(func(s *bus.MonitorStatus) { s.InputDownSince = &down }).monitor(context.Background(), now, 60)
	if m.State != "down" || !strings.Contains(m.Detail, "monitoring outage") || !strings.Contains(m.Detail, "no lost_link is judged") {
		t.Fatalf("input down: %+v", m)
	}
	until := now.Add(10 * time.Second)
	m = status(func(s *bus.MonitorStatus) { s.LostLinkSuspendedUntil = &until }).monitor(context.Background(), now, 60)
	if m.State != "up" || !strings.Contains(m.Detail, "after a monitoring outage") || !strings.Contains(m.Detail, until.Format(time.RFC3339)) {
		t.Fatalf("back, within the grace: %+v", m)
	}
	past := now.Add(-time.Second)
	m = status(func(s *bus.MonitorStatus) { s.LostLinkSuspendedUntil = &past }).monitor(context.Background(), now, 60)
	if m.State != "up" || strings.Contains(m.Detail, "outage") {
		t.Fatalf("after the grace: %+v", m)
	}
	m = status(func(*bus.MonitorStatus) {}).monitor(context.Background(), now, 60)
	if m.State != "up" || strings.Contains(m.Detail, "outage") {
		t.Fatalf("no outage: %+v", m)
	}
}
