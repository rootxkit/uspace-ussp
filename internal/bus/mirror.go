package bus

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
)

// Counters of a mirror.
const (
	CounterMirrorApplied      = "mirror_applied"
	CounterMirrorDeleted      = "mirror_deleted"
	CounterMirrorDecodeFailed = "mirror_decode_failed"
	CounterMirrorOverBound    = "mirror_over_bound"
)

// DefaultMirrorMaxKeys bounds a mirror (E-10).
const DefaultMirrorMaxKeys = 100_000

// Mirror keeps every key of a KV bucket in memory: the bucket's watch
// delivers the current values, then a marker, then every put and delete,
// so no change falls between a read and the watch. It is the hot path's
// reading of a projection that has one key per entity (client_bindings,
// intent_active, registry_validity, cis_current; D6): Snapshot serves
// what was read with its age, never blocks, and says whether the bucket
// was read at all, so a caller tells "nothing there" from "not known"
// (SC-22). A value that does not decode is counted and the key's last
// value kept; beyond MaxKeys a new key is counted and not kept. A watch
// that ends (NATS down, the bucket deleted) is opened again after Retry
// and the values are reread from scratch: a key deleted meanwhile is
// gone. Safe for concurrent use; no lock is held across I/O (B-06).
type Mirror[T any] struct {
	JS     jetstream.JetStream
	Bucket string
	// Decode reads a value (JSON by default).
	Decode func(key string, data []byte) (T, error)
	// OnChange, when set, is called after every applied put or delete
	// and once when the initial values are in (key "").
	OnChange func(key string)
	MaxKeys  int
	// Retry is the wait before a missing bucket or a broken watch is
	// opened again (DefaultFollowRetry).
	Retry    time.Duration
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time

	mu       sync.Mutex
	vals     map[string]T
	loaded   bool
	building map[string]T // the values of a watch whose marker has not come yet
	since    time.Time    // when the watch last delivered its marker
	live     bool         // a watch is open and past its marker
}

func (m *Mirror[T]) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Mirror[T]) counters() *core.Counters {
	if m.Counters == nil {
		m.Counters = &core.Counters{}
	}
	return m.Counters
}

func (m *Mirror[T]) maxKeys() int {
	if m.MaxKeys > 0 {
		return m.MaxKeys
	}
	return DefaultMirrorMaxKeys
}

// Seed replaces the values as one complete read of the bucket would,
// with age 0: a projection a process builds itself, and the tests of
// the mirror's readers.
func (m *Mirror[T]) Seed(vals map[string]T) {
	m.mu.Lock()
	m.vals, m.loaded, m.live, m.since = maps.Clone(vals), true, true, m.now()
	m.mu.Unlock()
	if m.OnChange != nil {
		m.OnChange("")
	}
}

// Snapshot returns a copy of the values, the age in seconds of the last
// complete read (0 while the watch is open and current), and whether
// the bucket was ever read. The copy is the caller's.
func (m *Mirror[T]) Snapshot() (map[string]T, float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return nil, 0, false
	}
	return maps.Clone(m.vals), m.ageLocked(), true
}

// Get returns the value of key, the age of the read and whether the
// bucket was ever read; found is false when the key is not there.
func (m *Mirror[T]) Get(key string) (v T, found bool, ageS float64, loaded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return v, false, 0, false
	}
	v, found = m.vals[key]
	return v, found, m.ageLocked(), true
}

// Len is the number of keys held.
func (m *Mirror[T]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.vals)
}

func (m *Mirror[T]) ageLocked() float64 {
	if m.live {
		return 0
	}
	return m.now().Sub(m.since).Seconds()
}

func (m *Mirror[T]) decode(key string, data []byte) (T, error) {
	if m.Decode != nil {
		return m.Decode(key, data)
	}
	var v T
	err := json.Unmarshal(data, &v)
	return v, err
}

// Run mirrors the bucket until ctx ends.
func (m *Mirror[T]) Run(ctx context.Context) {
	retry := m.Retry
	if retry <= 0 {
		retry = DefaultFollowRetry
	}
	for ctx.Err() == nil {
		m.session(ctx)
		m.mu.Lock()
		m.live, m.building = false, nil
		m.mu.Unlock()
		wait(ctx, retry)
	}
}

func (m *Mirror[T]) session(ctx context.Context) {
	kv, err := m.JS.KeyValue(ctx, m.Bucket)
	if err != nil {
		return // missing bucket or NATS down: the last values with their age
	}
	w, err := kv.WatchAll(ctx)
	if err != nil {
		return
	}
	defer func() { _ = w.Stop() }()
	m.mu.Lock()
	m.building = map[string]T{}
	m.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case e, open := <-w.Updates():
			if !open {
				return
			}
			m.take(e)
		}
	}
}

// take applies one watch entry: nil is the marker after the initial
// values.
func (m *Mirror[T]) take(e jetstream.KeyValueEntry) {
	if e == nil {
		m.mu.Lock()
		if m.building != nil {
			m.vals, m.building = m.building, nil
		}
		m.loaded, m.live, m.since = true, true, m.now()
		m.mu.Unlock()
		if m.OnChange != nil {
			m.OnChange("")
		}
		return
	}
	key := e.Key()
	if e.Operation() != jetstream.KeyValuePut {
		m.mu.Lock()
		target := m.target()
		_, had := target[key]
		delete(target, key)
		building := m.building != nil
		m.mu.Unlock()
		if had {
			m.counters().Inc(CounterMirrorDeleted)
		}
		if !building && m.OnChange != nil {
			m.OnChange(key)
		}
		return
	}
	v, err := m.decode(key, e.Value())
	if err != nil {
		m.counters().Inc(CounterMirrorDecodeFailed)
		if m.Logger != nil {
			m.Logger.Warn("KV value not readable; keeping the last one", slog.String("bucket", m.Bucket),
				slog.String("key", key), slog.String("error", err.Error()))
		}
		return
	}
	m.mu.Lock()
	target := m.target()
	if _, had := target[key]; !had && len(target) >= m.maxKeys() {
		m.mu.Unlock()
		m.counters().Inc(CounterMirrorOverBound)
		return
	}
	target[key] = v
	building := m.building != nil
	m.mu.Unlock()
	m.counters().Inc(CounterMirrorApplied)
	if !building && m.OnChange != nil {
		m.OnChange(key)
	}
}

// target is the map a watch entry goes to: the one being built before
// the marker, the live one after. Called with mu held.
func (m *Mirror[T]) target() map[string]T {
	if m.building != nil {
		return m.building
	}
	if m.vals == nil {
		m.vals = map[string]T{}
	}
	return m.vals
}
