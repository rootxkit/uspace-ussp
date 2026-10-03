package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// LiveSessions both ways: the session sessions_live holds for its
// account, before its expiry and idle end, is live (E-01); absent, of
// another account, expired or idle, it is refused; with the projection
// unread or no reader it is unavailable, never admitted.
func TestLiveSessions(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	live := LiveSession{Subject: "acc-1", Realm: RealmConsole, ExpiresAt: now.Add(time.Hour), IdleUntil: now.Add(time.Minute)}
	held := map[string]LiveSession{"jti-1": live}
	loaded := true
	l := LiveSessions{Now: func() time.Time { return now }, Get: func(jti string) (LiveSession, bool, bool) {
		s, ok := held[jti]
		return s, ok, loaded
	}}
	ctx := context.Background()
	if err := l.CheckSession(ctx, "jti-1", "acc-1"); err != nil {
		t.Fatalf("live session: %v", err)
	}
	for name, c := range map[string]struct {
		jti, sub string
		s        *LiveSession
	}{
		"unknown":         {"jti-2", "acc-1", nil},
		"another account": {"jti-1", "acc-2", nil},
		"expired":         {"jti-1", "acc-1", &LiveSession{Subject: "acc-1", ExpiresAt: now, IdleUntil: now.Add(time.Minute)}},
		"idle":            {"jti-1", "acc-1", &LiveSession{Subject: "acc-1", ExpiresAt: now.Add(time.Hour), IdleUntil: now.Add(-time.Second)}},
	} {
		if c.s != nil {
			held["jti-1"] = *c.s
		}
		if err := l.CheckSession(ctx, c.jti, c.sub); !errors.Is(err, ErrSessionRefused) {
			t.Errorf("%s: %v, want refused", name, err)
		}
		held["jti-1"] = live
	}
	loaded = false
	if err := l.CheckSession(ctx, "jti-1", "acc-1"); !errors.Is(err, ErrSessionsUnavailable) || errors.Is(err, ErrSessionRefused) {
		t.Fatalf("unread: %v", err)
	}
	if err := (LiveSessions{}).CheckSession(ctx, "jti-1", "acc-1"); !errors.Is(err, ErrSessionsUnavailable) {
		t.Fatalf("no reader: %v", err)
	}
}
