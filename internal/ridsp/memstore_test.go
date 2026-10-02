package ridsp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/store"
)

// memStore is PlanStore and WorkStore in memory: dss_isas, the flights'
// ends and intents, the intents' volumes and the outbox with its
// idempotency, leases and backoff, on a clock a test may move.
type memStore struct {
	mu      sync.Mutex
	offset  time.Duration
	isas    map[string]*ISARecord
	ended   map[string]bool
	volumes map[string][]byte
	items   []*store.OutboxItem
	nextID  int64
	errs    map[string]error // a method name -> the error it returns
	lastErr map[string]string
}

func newMemStore() *memStore {
	return &memStore{isas: map[string]*ISARecord{}, ended: map[string]bool{}, volumes: map[string][]byte{},
		errs: map[string]error{}, lastErr: map[string]string{}}
}

func (m *memStore) fail(name string) error { return m.errs[name] }

func (m *memStore) clock() time.Time { return time.Now().Add(m.offset).UTC() }

// advance moves the store's clock (the database clock) forward.
func (m *memStore) advance(d time.Duration) { m.mu.Lock(); m.offset += d; m.mu.Unlock() }

func (m *memStore) Now(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("Now"); err != nil {
		return time.Time{}, err
	}
	return m.clock(), nil
}

func (m *memStore) FlightISA(_ context.Context, flightID string) (ISARecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("FlightISA"); err != nil {
		return ISARecord{}, false, err
	}
	for _, r := range m.isas {
		if r.FlightID == flightID {
			return *r, true, nil
		}
	}
	return ISARecord{}, false, nil
}

func (m *memStore) FlightEnded(_ context.Context, flightID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ended[flightID], m.fail("FlightEnded")
}

func (m *memStore) IntentVolumes(_ context.Context, intentID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.volumes[intentID]
	if !ok {
		return nil, errors.New("no such intent")
	}
	return v, nil
}

func (m *memStore) InsertISA(_ context.Context, r ISARecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.isas[r.ISAID]; !ok {
		c := r
		m.isas[r.ISAID] = &c
	}
	return nil
}

func (m *memStore) SetKind(_ context.Context, isaID, kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.isas[isaID]; r != nil {
		r.Kind = kind
	}
	return nil
}

func (m *memStore) Enqueue(_ context.Context, kind, entityID string, version int64, payload any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enqueueLocked(kind, entityID, version, payload)
}

func (m *memStore) enqueueLocked(kind, entityID string, version int64, payload any) (bool, error) {
	for _, it := range m.items {
		if it.Kind == kind && it.EntityID == entityID && it.EntityVersion == version {
			return false, nil
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	m.nextID++
	m.items = append(m.items, &store.OutboxItem{ID: m.nextID, Kind: kind, EntityID: entityID, EntityVersion: version,
		Payload: b, NextAt: m.clock(), CreatedAt: m.clock()})
	return true, nil
}

func (m *memStore) Claim(_ context.Context, n int) ([]store.OutboxItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("Claim"); err != nil {
		return nil, err
	}
	var out []store.OutboxItem
	now := m.clock()
	for _, it := range m.items {
		if len(out) >= n {
			break
		}
		if it.DoneAt != nil || it.NextAt.After(now) || !slices.Contains(ISAKinds, it.Kind) {
			continue
		}
		it.Attempts++
		it.NextAt = now.Add(store.DefaultLease)
		out = append(out, *it)
	}
	return out, nil
}

func (m *memStore) item(id int64) *store.OutboxItem {
	for _, it := range m.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

func (m *memStore) Done(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.item(id)
	if it == nil || it.DoneAt != nil {
		return store.ErrNotPending
	}
	t := m.clock()
	it.DoneAt, it.LastError = &t, nil
	return nil
}

func (m *memStore) Fail(_ context.Context, id int64, cause error, backoff time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.item(id)
	if it == nil || it.DoneAt != nil {
		return store.ErrNotPending
	}
	msg := cause.Error()
	it.NextAt, it.LastError = m.clock().Add(backoff), &msg
	return nil
}

func (m *memStore) ISA(_ context.Context, isaID string) (ISARecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("ISA"); err != nil {
		return ISARecord{}, false, err
	}
	r, ok := m.isas[isaID]
	if !ok {
		return ISARecord{}, false, nil
	}
	out := *r
	if m.ended[r.FlightID] {
		t := m.clock()
		out.FlightEndedAt = &t
	}
	return out, true, nil
}

func (m *memStore) Written(_ context.Context, r ISARecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("Written"); err != nil {
		return err
	}
	cur := m.isas[r.ISAID]
	if cur == nil {
		return errors.New("no such ISA")
	}
	cur.Version, cur.TimeStart, cur.TimeEnd, cur.Extents = r.Version, r.TimeStart, r.TimeEnd, r.Extents
	delete(m.lastErr, r.ISAID)
	return nil
}

func (m *memStore) Deleted(_ context.Context, isaID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.isas[isaID]; r != nil && r.DeletedAt == nil {
		t := m.clock()
		r.DeletedAt = &t
	}
	return nil
}

func (m *memStore) Failed(_ context.Context, isaID, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastErr[isaID] = msg
	return nil
}

func (m *memStore) Backlog(context.Context) (int64, float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("Backlog"); err != nil {
		return 0, 0, err
	}
	var n int64
	var oldest time.Time
	for _, it := range m.items {
		if it.DoneAt == nil && slices.Contains(ISAKinds, it.Kind) {
			n++
			if oldest.IsZero() || it.CreatedAt.Before(oldest) {
				oldest = it.CreatedAt
			}
		}
	}
	if n == 0 {
		return 0, 0, nil
	}
	return n, m.clock().Sub(oldest).Seconds(), nil
}

func (m *memStore) Renew(_ context.Context, beforeS float64, n int, queue func(ISARecord) (ISAPut, error)) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	queued := 0
	for _, r := range m.isas {
		if queued >= n {
			break
		}
		if r.Kind != ISAKindSession || r.DeletedAt != nil || r.Version == nil || m.ended[r.FlightID] ||
			!r.TimeEnd.Before(now.Add(time.Duration(beforeS*float64(time.Second)))) {
			continue
		}
		pending := false
		for _, it := range m.items {
			if it.Kind == store.OutboxISAPut && it.EntityID == r.ISAID && it.DoneAt == nil {
				pending = true
			}
		}
		if pending {
			continue
		}
		put, err := queue(*r)
		if err != nil {
			return queued, err
		}
		if ok, err := m.enqueueLocked(store.OutboxISAPut, r.ISAID, now.UnixMilli(), put); err != nil {
			return queued, err
		} else if ok {
			queued++
		}
	}
	return queued, nil
}

// pending are the undone items, oldest first.
func (m *memStore) pending() []store.OutboxItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.OutboxItem
	for _, it := range m.items {
		if it.DoneAt == nil {
			out = append(out, *it)
		}
	}
	return out
}

// due makes every pending item due now.
func (m *memStore) due() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.items {
		if it.DoneAt == nil {
			it.NextAt = m.clock()
		}
	}
}

// tokens is a TokenSource that names the audience and the scope in the
// token, so a test reads what was asked.
type tokens struct {
	mu   sync.Mutex
	err  error
	asks []string
}

func (t *tokens) Token(_ context.Context, base string, scopes ...string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return "", t.err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	tok := "aud=" + u.Hostname() + ";scope=" + scopes[0]
	t.asks = append(t.asks, tok)
	return tok, nil
}
