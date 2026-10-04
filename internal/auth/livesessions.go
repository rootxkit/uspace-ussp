package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// BucketSessionsLive is the KV bucket of the live sessions (D6): the
// session jti -> LiveSession, written by api in the transaction that
// starts or ends a session, read by the processes without the
// relational database (traffic-ws), so a session signed out, ended or
// idle in api is refused there too (audit B2).
const BucketSessionsLive = "sessions_live"

// LiveSession is one live session as the sessions_live bucket holds it.
type LiveSession struct {
	Subject   string    `json:"sub"`
	Realm     string    `json:"realm"`
	ExpiresAt time.Time `json:"exp"`
	// IdleUntil is when the session ends unless it is used in api again
	// (its last use plus the idle timeout).
	IdleUntil time.Time `json:"idle_until"`
	// OperatorID is the operator of a portal session (brief WP-17):
	// traffic-ws admits it to the streams of that operator's intents
	// only. Empty for a console session.
	OperatorID string `json:"operator_id,omitempty"`
}

// SessionsProjector writes the sessions_live projection. The session
// writers call it inside the transaction that starts or ends the
// session, so a failed projection rolls the change back (B-09): a
// session that cannot be projected is never started, and one whose end
// cannot be projected is not ended.
type SessionsProjector interface {
	ProjectSession(ctx context.Context, jti string, s LiveSession) error
	EndSession(ctx context.Context, jti string) error
}

// ErrSessionsUnavailable is a session check while the projection has not
// been read: the session can be neither admitted nor refused.
var ErrSessionsUnavailable = errors.New("the live sessions (sessions_live) have not been read since this process started")

// LiveSessions is the SessionChecker of a process without the relational
// database: a session is live when sessions_live holds its jti for that
// account, not expired and not idle. Get reads the projection: found is
// false for a jti it does not hold, loaded false until it was read once.
// Nothing unknown is admitted: an absent jti is refused, an unread
// projection is unavailable (503), never a pass.
type LiveSessions struct {
	Get func(jti string) (s LiveSession, found, loaded bool)
	Now func() time.Time
}

// CheckSession implements SessionChecker.
func (l LiveSessions) CheckSession(_ context.Context, jti, sub string) error {
	if l.Get == nil {
		return ErrSessionsUnavailable
	}
	s, found, loaded := l.Get(jti)
	if !loaded {
		return ErrSessionsUnavailable
	}
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	switch {
	case !found:
		return fmt.Errorf("%w: unknown or ended session", ErrSessionRefused)
	case s.Subject != sub:
		return fmt.Errorf("%w: the session is not this account's", ErrSessionRefused)
	case !now.Before(s.ExpiresAt):
		return fmt.Errorf("%w: the session expired", ErrSessionRefused)
	case !s.IdleUntil.IsZero() && now.After(s.IdleUntil):
		return fmt.Errorf("%w: the session was idle", ErrSessionRefused)
	}
	return nil
}

// MemorySessions is an in-memory sessions_live: a SessionsProjector and
// the Get of a LiveSessions in one process (tests of api's writes and
// of the B-09 rollback). Safe for concurrent use.
type MemorySessions struct {
	mu sync.Mutex
	m  map[string]LiveSession
	// Fail, when set, makes every projection fail.
	Fail error
}

// NewMemorySessions is an empty projection.
func NewMemorySessions() *MemorySessions { return &MemorySessions{m: map[string]LiveSession{}} }

// ProjectSession implements SessionsProjector.
func (m *MemorySessions) ProjectSession(_ context.Context, jti string, s LiveSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.m[jti] = s
	return nil
}

// EndSession implements SessionsProjector.
func (m *MemorySessions) EndSession(_ context.Context, jti string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	delete(m.m, jti)
	return nil
}

// Get is the Get of a LiveSessions over m (always loaded).
func (m *MemorySessions) Get(jti string) (LiveSession, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.m[jti]
	return s, ok, true
}

// Len is how many sessions m holds.
func (m *MemorySessions) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}
