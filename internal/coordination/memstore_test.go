package coordination

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// memStore is Store in memory with a settable clock, for the unit
// tests (the PostgreSQL store runs in test/integration).
type memStore struct {
	mu       sync.Mutex
	now      time.Time
	trs      []Transition
	cands    []Candidate
	intents  map[string]Intent // ended intents by id
	checks   map[string]Check
	notices  []*memNotice
	notified map[int64]time.Time
	ackRefs  map[int64]string
	fail     error // every call fails with it when set
}

type memNotice struct {
	Notice
	id          int64
	state       string
	attempts    int
	nextAt      time.Time
	lastError   string
	ackID       string
	receivedAt  time.Time
	pollNext    *time.Time
	polls       int
	ackedAt     *time.Time
	ackedBy     string
	escalatedAt *time.Time
	failedAt    *time.Time
	createdAt   time.Time
}

func newMemStore() *memStore {
	return &memStore{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), intents: map[string]Intent{}, checks: map[string]Check{},
		notified: map[int64]time.Time{}, ackRefs: map[int64]string{}}
}

func (m *memStore) advance(d time.Duration) { m.mu.Lock(); m.now = m.now.Add(d); m.mu.Unlock() }

func (m *memStore) hasNoticeFor(stateID int64) bool {
	for _, n := range m.notices {
		if n.StateID == stateID {
			return true
		}
	}
	return false
}

func (m *memStore) Transitions(_ context.Context, _ time.Duration, n int) ([]Transition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Transition
	for i := range m.trs {
		if !m.hasNoticeFor(m.trs[i].StateID) && len(out) < n {
			out = append(out, m.trs[i])
		}
	}
	return out, nil
}

func (m *memStore) Candidates(_ context.Context, _ time.Duration, n int) ([]Candidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Candidate
	for i := range m.cands {
		if _, done := m.checks[m.cands[i].ID]; !done && len(out) < n {
			out = append(out, m.cands[i])
		}
	}
	return out, nil
}

func (m *memStore) Ended(_ context.Context, n int) ([]Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Intent
	for id := range m.intents {
		in := m.intents[id]
		c, ok := m.checks[id]
		if !ok || !c.Controlled || in.LocalState != "ended" {
			continue
		}
		if slices.ContainsFunc(m.notices, func(x *memNotice) bool { return x.IntentID == id && x.Kind == KindEnded }) {
			continue
		}
		if len(out) < n {
			out = append(out, in)
		}
	}
	return out, nil
}

func (m *memStore) Enqueue(_ context.Context, intentID string, check *Check, ns []Notice) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	if check != nil {
		if _, ok := m.checks[intentID]; ok {
			return 0, nil
		}
		m.checks[intentID] = *check
	}
	q := 0
	for _, n := range ns {
		if slices.ContainsFunc(m.notices, func(x *memNotice) bool { return x.Ref == n.Ref }) {
			continue
		}
		x := &memNotice{Notice: n, id: int64(len(m.notices) + 1), state: "pending", nextAt: m.now, createdAt: m.now}
		if n.FailReason != "" {
			t := m.now
			x.state, x.failedAt, x.lastError = "failed", &t, n.FailReason
		}
		m.notices = append(m.notices, x)
		q++
	}
	return q, nil
}

func (m *memStore) Claim(_ context.Context, n int, lease time.Duration) ([]Queued, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Queued
	seen := map[string]bool{}
	for _, x := range m.notices {
		if x.state != "pending" {
			continue
		}
		first := !seen[x.IntentID]
		seen[x.IntentID] = true
		if !first || x.nextAt.After(m.now) || len(out) >= n {
			continue
		}
		x.attempts++
		x.nextAt = m.now.Add(lease)
		out = append(out, Queued{ID: x.id, Ref: x.Ref, Kind: x.Kind, IntentID: x.IntentID, StateID: x.StateID, Body: x.Body, Attempts: x.attempts})
	}
	return out, nil
}

func (m *memStore) get(id int64) *memNotice {
	for _, x := range m.notices {
		if x.id == id {
			return x
		}
	}
	return nil
}

func (m *memStore) Received(_ context.Context, id int64, r Receipt, poll time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	x := m.get(id)
	if x == nil || x.state != "pending" {
		return nil
	}
	x.state, x.ackID, x.receivedAt, x.lastError = "received", r.AckID, m.now, ""
	if AckRequired(x.Kind) && poll > 0 {
		t := m.now.Add(poll)
		x.pollNext = &t
	}
	if x.StateID > 0 {
		if _, ok := m.notified[x.StateID]; !ok {
			m.notified[x.StateID] = m.now
		}
	}
	return nil
}

func (m *memStore) Retry(_ context.Context, id int64, cause string, backoff time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	if x := m.get(id); x != nil && x.state == "pending" {
		x.nextAt, x.lastError = m.now.Add(backoff), cause
	}
	return nil
}

func (m *memStore) Fail(_ context.Context, id int64, cause string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	if x := m.get(id); x != nil && x.state == "pending" {
		t := m.now
		x.state, x.failedAt, x.lastError, x.pollNext = "failed", &t, cause, nil
	}
	return nil
}

func (m *memStore) DuePolls(_ context.Context, n int) ([]Polled, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Polled
	for _, x := range m.notices {
		if (x.state != "received" && x.state != "escalated") || x.pollNext == nil || x.pollNext.After(m.now) || len(out) >= n {
			continue
		}
		t := m.now.Add(30 * time.Second)
		x.pollNext = &t
		out = append(out, Polled{ID: x.id, Ref: x.Ref, Kind: x.Kind, AckID: x.ackID, StateID: x.StateID, State: x.state,
			ReceivedAt: x.receivedAt, SinceReceived: m.now.Sub(x.receivedAt), Polls: x.polls})
	}
	return out, nil
}

func (m *memStore) Polled(_ context.Context, id int64, next time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	if x := m.get(id); x != nil && (x.state == "received" || x.state == "escalated") {
		x.polls++
		x.pollNext = nil
		if next > 0 {
			t := m.now.Add(next)
			x.pollNext = &t
		}
	}
	return nil
}

func (m *memStore) Acknowledged(_ context.Context, id int64, s NoticeState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	x := m.get(id)
	if x == nil || (x.state != "received" && x.state != "escalated") {
		return nil
	}
	t := m.now
	x.state, x.ackedAt, x.ackedBy, x.pollNext = "acknowledged", &t, s.AcknowledgedBy, nil
	if x.StateID > 0 {
		if _, ok := m.ackRefs[x.StateID]; !ok {
			m.ackRefs[x.StateID] = x.ackID
		}
	}
	return nil
}

func (m *memStore) item(x *memNotice) Item {
	it := Item{ID: x.id, NoticeRef: x.Ref, Kind: string(x.Kind), IntentID: x.IntentID, State: x.state, CreatedAt: x.createdAt,
		Attempts: x.attempts, EscalatedAt: x.escalatedAt, FailedAt: x.failedAt, AgeS: m.now.Sub(x.createdAt).Seconds()}
	if x.lastError != "" {
		e := x.lastError
		it.LastError = &e
	}
	if x.state == "escalated" {
		it.AgeS = m.now.Sub(x.receivedAt).Seconds()
	}
	return it
}

func (m *memStore) Escalate(_ context.Context, after time.Duration) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Item
	for _, x := range m.notices {
		if AckRequired(x.Kind) && x.state == "received" && !x.receivedAt.After(m.now.Add(-after)) {
			t := m.now
			x.state, x.escalatedAt = "escalated", &t
			out = append(out, m.item(x))
		}
	}
	return out, nil
}

func (m *memStore) Open(_ context.Context, n int) ([]Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, false, m.fail
	}
	var out []Item
	for _, x := range m.notices {
		if x.state == "pending" || x.state == "escalated" || x.state == "failed" {
			out = append(out, m.item(x))
		}
	}
	if len(out) > n {
		return out[:n], true, nil
	}
	return out, false, nil
}

func (m *memStore) Summarise(context.Context) (Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return Summary{}, m.fail
	}
	var s Summary
	for _, x := range m.notices {
		switch x.state {
		case "pending":
			s.Pending++
			if x.lastError != "" {
				s.Retrying++
			}
			s.OldestPendingAgeS = max(s.OldestPendingAgeS, m.now.Sub(x.createdAt).Seconds())
		case "escalated":
			s.Escalated++
		case "failed":
			s.Failed++
		}
	}
	return s, nil
}

func (m *memStore) notice(ref string) *memNotice {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.notices {
		if x.Ref == ref {
			c := *x
			return &c
		}
	}
	return nil
}

var errStore = errors.New("store down")
