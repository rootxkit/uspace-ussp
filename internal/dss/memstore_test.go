package dss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// memStore is the Store contract in memory (the pgstore is proven by the
// integration tests).
type memStore struct {
	mu        sync.Mutex
	now       time.Time
	outbox    []store.OutboxItem
	seq       int64
	locks     map[string]*sync.Mutex
	peers     map[string]PeerRecord
	cstrs     map[string]ConstraintRecord
	subs      map[string]SubscriptionRecord
	state     StateRecord
	exchanges []Exchange
	reports   map[string]json.RawMessage
	audits    []store.Event
	failClaim error
	failAudit error
	failLock  error
	failState error
	purged    PurgeCounts
	// inLock counts the Lock calls running now (a write that holds a
	// transaction).
	inLock atomic.Int32
}

func newMemStore() *memStore {
	return &memStore{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), locks: map[string]*sync.Mutex{}, peers: map[string]PeerRecord{},
		cstrs: map[string]ConstraintRecord{}, subs: map[string]SubscriptionRecord{}, reports: map[string]json.RawMessage{}}
}

func (m *memStore) Now(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now, nil
}

func (m *memStore) advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	m.mu.Unlock()
}

func (m *memStore) Lock(_ context.Context, class int32, id string, fn func() error) error {
	if m.failLock != nil {
		return m.failLock
	}
	k := fmt.Sprintf("%d/%s", class, id)
	m.mu.Lock()
	l, ok := m.locks[k]
	if !ok {
		l = &sync.Mutex{}
		m.locks[k] = l
	}
	m.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	m.inLock.Add(1)
	defer m.inLock.Add(-1)
	return fn()
}

// enqueue is what intent.Tx.Enqueue and QueueDSS do.
func (m *memStore) enqueue(kind, entity string, version int64, payload any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		if it := &m.outbox[i]; it.Kind == kind && it.EntityID == entity && it.EntityVersion == version {
			return
		}
	}
	b, _ := json.Marshal(payload)
	m.seq++
	m.outbox = append(m.outbox, store.OutboxItem{ID: m.seq, Kind: kind, EntityID: entity, EntityVersion: version, Payload: b, NextAt: m.now, CreatedAt: m.now})
}

func (m *memStore) pending(kinds ...string) []store.OutboxItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.OutboxItem
	for i := range m.outbox {
		if it := &m.outbox[i]; it.DoneAt == nil && slices.Contains(kinds, it.Kind) {
			out = append(out, *it)
		}
	}
	return out
}

func (m *memStore) Claim(_ context.Context, kinds []string, n int) ([]store.OutboxItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failClaim != nil {
		return nil, m.failClaim
	}
	var out []store.OutboxItem
	for i := range m.outbox {
		it := &m.outbox[i]
		if it.DoneAt != nil || it.NextAt.After(m.now) || !slices.Contains(kinds, it.Kind) || len(out) >= n {
			continue
		}
		it.Attempts++
		it.NextAt = m.now.Add(time.Minute)
		out = append(out, *it)
	}
	return out, nil
}

func (m *memStore) Done(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		if m.outbox[i].ID == id && m.outbox[i].DoneAt == nil {
			t := m.now
			m.outbox[i].DoneAt = &t
			return nil
		}
	}
	return store.ErrNotPending
}

func (m *memStore) Fail(_ context.Context, id int64, cause error, backoff time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		if m.outbox[i].ID == id && m.outbox[i].DoneAt == nil {
			msg := cause.Error()
			m.outbox[i].NextAt, m.outbox[i].LastError = m.now.Add(backoff), &msg
			return nil
		}
	}
	return store.ErrNotPending
}

func (m *memStore) DoneByKey(_ context.Context, kind, entity string, version int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		it := &m.outbox[i]
		if it.Kind == kind && it.EntityID == entity && it.EntityVersion == version && it.DoneAt == nil {
			t := m.now
			it.DoneAt = &t
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) Backlog(_ context.Context, kinds []string) (int64, float64, int32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	var oldest float64
	var att int32
	for i := range m.outbox {
		if it := &m.outbox[i]; it.DoneAt == nil && slices.Contains(kinds, it.Kind) {
			n++
			oldest = max(oldest, m.now.Sub(it.CreatedAt).Seconds())
			att = max(att, it.Attempts)
		}
	}
	return n, oldest, att, nil
}

func (m *memStore) UpsertPeerIntent(_ context.Context, p PeerRecord) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.peers[p.EntityID]; ok && p.Version < cur.Version {
		return false, nil
	}
	p.FetchedAt = m.now
	m.peers[p.EntityID] = p
	return true, nil
}

func (m *memStore) PeerIntent(_ context.Context, id string) (*PeerRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

func (m *memStore) DeletePeerIntent(_ context.Context, id, manager string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok || p.Manager != manager {
		return false, nil
	}
	delete(m.peers, id)
	return true, nil
}

func (m *memStore) MarkPeerUnavailable(_ context.Context, base string, unavailable bool) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id := range m.peers {
		if p := m.peers[id]; p.USSBaseURL == base && p.PeerUnavailable != unavailable {
			p.PeerUnavailable = unavailable
			m.peers[id] = p
			n++
		}
	}
	return n, nil
}

func (m *memStore) UpsertConstraint(_ context.Context, c ConstraintRecord) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.cstrs[c.EntityID]; ok && c.Version < cur.Version {
		return false, nil
	}
	c.FetchedAt = m.now
	m.cstrs[c.EntityID] = c
	return true, nil
}

func (m *memStore) Constraint(_ context.Context, id string) (*ConstraintRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cstrs[id]
	if !ok {
		return nil, nil
	}
	return &c, nil
}

func (m *memStore) DeleteConstraint(_ context.Context, id, manager string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cstrs[id]
	if !ok || c.Manager != manager {
		return false, nil
	}
	delete(m.cstrs, id)
	return true, nil
}

func (m *memStore) Subscriptions(context.Context) ([]SubscriptionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SubscriptionRecord, 0, len(m.subs))
	for id := range m.subs {
		out = append(out, m.subs[id])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) UpsertSubscription(_ context.Context, s SubscriptionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.subs[s.ID]; ok {
		s.NotificationIndex = max(s.NotificationIndex, cur.NotificationIndex)
	}
	m.subs[s.ID] = s
	return nil
}

func (m *memStore) DeleteSubscription(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.subs, id)
	return nil
}

func (m *memStore) Notified(_ context.Context, id string, idx int32) (int32, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.subs[id]
	if !ok {
		return 0, false, nil
	}
	prev := s.NotificationIndex
	s.NotificationIndex = max(prev, idx)
	m.subs[id] = s
	return prev, true, nil
}

func (m *memStore) State(context.Context) (StateRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failState != nil {
		return StateRecord{}, m.failState
	}
	return m.state, nil
}

func (m *memStore) SetAvailability(_ context.Context, a, by string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Availability == a {
		return false, nil
	}
	t := m.now
	m.state.Availability, m.state.SetBy, m.state.SetAt = a, by, &t
	return true, nil
}

func (m *memStore) SetReachable(_ context.Context, up bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.now
	if up {
		m.state.ReachableSince, m.state.UnreachableSince = &t, nil
	} else {
		m.state.ReachableSince, m.state.UnreachableSince = nil, &t
	}
	return nil
}

func (m *memStore) InsertExchange(_ context.Context, e Exchange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exchanges = append(m.exchanges, e)
	return nil
}

func (m *memStore) Exchanges(_ context.Context, id string, limit int) ([]Exchange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Exchange
	for i := range m.exchanges {
		if e := &m.exchanges[i]; e.EntityID == id && len(out) < limit {
			out = append(out, *e)
		}
	}
	return out, nil
}

func (m *memStore) InsertReport(_ context.Context, id, _ string, ex json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports[id] = ex
	return nil
}

func (m *memStore) Purge(_ context.Context, peersBefore, logBefore time.Time, _ int64) (PurgeCounts, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out PurgeCounts
	for id := range m.peers {
		if m.peers[id].FetchedAt.Before(peersBefore) {
			delete(m.peers, id)
			out.PeerIntents++
		}
	}
	for id := range m.cstrs {
		if m.cstrs[id].FetchedAt.Before(peersBefore) {
			delete(m.cstrs, id)
			out.Constraints++
		}
	}
	_ = logBefore
	m.purged = out
	return out, nil
}

func (m *memStore) Audit(_ context.Context, e store.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAudit != nil {
		return m.failAudit
	}
	m.audits = append(m.audits, e)
	return nil
}

func (m *memStore) auditsOf(eventType string) []store.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Event
	for i := range m.audits {
		if m.audits[i].EventType == eventType {
			out = append(out, m.audits[i])
		}
	}
	return out
}

// fakeIntents is the intent service as the writer sees it: records with
// their DSS state, the transitions of intent/dss.go reduced to what the
// writer can observe, and hooks for the checks.
type fakeIntents struct {
	mu   sync.Mutex
	st   *memStore
	recs map[string]*intent.Record
	held map[string]*intent.DSSHeld
	// check, when set, answers PeerCheck; nil is ok.
	check func(r *intent.Record) intent.PeerCheckResult
	// conflicts answers PeerConflicts.
	conflicts []intent.PeerConflict
	holds     []string
	displaced []string
	failHeld  error
}

func newFakeIntents(st *memStore) *fakeIntents {
	return &fakeIntents{st: st, recs: map[string]*intent.Record{}, held: map[string]*intent.DSSHeld{}}
}

func (f *fakeIntents) put(r *intent.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *r
	f.recs[r.ID] = &c
}

func (f *fakeIntents) get(id string) *intent.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.recs[id]
	if !ok {
		return nil
	}
	c := *r
	return &c
}

func (f *fakeIntents) heldOf(id string) *intent.DSSHeld {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.held[id]
	if h == nil {
		return nil
	}
	c := *h
	return &c
}

func (f *fakeIntents) Record(_ context.Context, id string) (*intent.Record, error) {
	return f.get(id), nil
}

func (f *fakeIntents) Held(_ context.Context, id string) (*intent.DSSHeld, error) {
	if f.failHeld != nil {
		return nil, f.failHeld
	}
	return f.heldOf(id), nil
}

func (f *fakeIntents) PeerCheck(_ context.Context, id string, version int) (intent.PeerCheckResult, error) {
	r := f.get(id)
	if r == nil || r.LocalState != intent.StatePendingDSS || r.Version != version {
		return intent.PeerCheckResult{Outcome: intent.PeerCheckStale}, nil
	}
	if f.check != nil {
		out := f.check(r)
		if out.Outcome == intent.PeerCheckRejected {
			f.mu.Lock()
			f.recs[id].LocalState, f.recs[id].Decision.State = intent.StateRejected, intent.StateRejected
			f.recs[id].Version++
			f.mu.Unlock()
		}
		return out, nil
	}
	return intent.PeerCheckResult{Outcome: intent.PeerCheckOK}, nil
}

func (f *fakeIntents) DSSHold(_ context.Context, _ string, _ int, reason, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holds = append(f.holds, reason)
	return nil
}

func (f *fakeIntents) lastHold() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.holds) == 0 {
		return ""
	}
	return f.holds[len(f.holds)-1]
}

func (f *fakeIntents) record(id string, held *intent.DSSHeld, notes []intent.OutboxSpec) {
	f.mu.Lock()
	if held == nil {
		delete(f.held, id)
	} else {
		c := *held
		f.held[id] = &c
	}
	f.mu.Unlock()
	for _, n := range notes {
		f.st.enqueue(n.Kind, n.EntityID, n.Version, n.Payload)
	}
}

func (f *fakeIntents) DSSAuthorise(_ context.Context, id string, version int, held intent.DSSHeld, notes []intent.OutboxSpec) (bool, error) {
	f.record(id, &held, notes)
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.recs[id]
	if r == nil || r.LocalState != intent.StatePendingDSS || r.Version != version {
		return false, nil
	}
	s := "Accepted"
	r.LocalState, r.Decision.State, r.Decision.Decision, r.Decision.DSSState = intent.StateAccepted, intent.StateAccepted, intent.DecisionAuthorised, &s
	r.Version++
	return true, nil
}

func (f *fakeIntents) DSSRecord(_ context.Context, id string, held *intent.DSSHeld, notes []intent.OutboxSpec) error {
	f.record(id, held, notes)
	return nil
}

func (f *fakeIntents) PeerConflicts(context.Context, intent.PeerIntent) ([]intent.PeerConflict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conflicts, nil
}

func (f *fakeIntents) PeerIntentDisplaced(_ context.Context, id, peer string) (intent.RecheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.displaced = append(f.displaced, id+"<"+peer)
	return intent.RecheckResult{IntentID: id, Outcome: intent.RecheckWithdrawn}, nil
}

// setState moves an intent to a local state with the DSS state its
// decision asks for, as the intent service's transitions do.
func (f *fakeIntents) setState(id, local string, dssState *string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.recs[id]
	r.LocalState, r.Decision.State, r.Decision.DSSState = local, local, dssState
	r.Version++
}

var errBoom = errors.New("boom")

func ptrS(s string) *string { return &s }

// mustState fails unless the held state is want ("" is none).
func mustState(t *testing.T, f *fakeIntents, id string, want f3548.OperationalIntentState) {
	t.Helper()
	h := f.heldOf(id)
	switch {
	case want == "" && h != nil:
		t.Fatalf("the DSS still holds %s: %+v", id, h)
	case want != "" && (h == nil || h.State != want):
		t.Fatalf("held %+v, want %s", h, want)
	}
}
