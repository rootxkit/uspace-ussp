package flights

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// Counters of a KVSaver.
const (
	// CounterSaveFailed counts writes to flight_binding that failed; the
	// flight is written again at its next save.
	CounterSaveFailed = "flights_save_failed"
	// CounterSaveDropped counts saves refused because MaxPending flights
	// were already waiting for the bucket (E-10).
	CounterSaveDropped = "flights_save_dropped"
	// CounterRestoreFailed counts a start whose saved flights could not
	// be read: every aircraft starts a new flight (the behaviour before
	// WP-19, said so on /readyz).
	CounterRestoreFailed = "flights_restore_failed"
	// CounterRestoreUnreadable counts saved flights that could not be
	// decoded (skipped).
	CounterRestoreUnreadable = "flights_restore_unreadable"
)

// KVStorer is the part of bus.KVStore a KVSaver uses.
type KVStorer interface {
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	All(ctx context.Context, maxKeys int) ([]bus.RevEntry, error)
}

// KVSaver keeps a Binder's running flights in the flight_binding bucket
// (bus.BucketFlightBinding): Save and Forget queue, newest per aircraft
// wins, and Run writes, so the binder never waits for the bucket. At
// most MaxPending aircraft wait at a time (E-10). A write that fails is
// counted and the flight is written again at its next save (at most
// SaveEvery later while it flies). Safe for concurrent use.
type KVSaver struct {
	KV         KVStorer
	Counters   *core.Counters
	Logger     *slog.Logger
	MaxPending int

	once    sync.Once
	mu      sync.Mutex
	pending map[string]*Snapshot // nil: delete the key
	wake    chan struct{}
}

func (s *KVSaver) init() {
	s.once.Do(func() {
		s.pending = map[string]*Snapshot{}
		s.wake = make(chan struct{}, 1)
		if s.Counters == nil {
			s.Counters = &core.Counters{}
		}
		if s.MaxPending <= 0 {
			s.MaxPending = MaxFlights
		}
	})
}

// BindingKey is an aircraft key's key in flight_binding: a hash, since
// an aircraft key holds characters a KV key may not.
func BindingKey(aircraftKey string) string {
	h := sha256.Sum256([]byte(aircraftKey))
	return hex.EncodeToString(h[:16])
}

// Save queues sn (Binder.Save).
func (s *KVSaver) Save(sn Snapshot) {
	s.init()
	c := sn
	s.queue(sn.Key, &c)
}

// Forget queues the deletion of key (Binder.Forget).
func (s *KVSaver) Forget(key string) {
	s.init()
	s.queue(key, nil)
}

func (s *KVSaver) queue(key string, sn *Snapshot) {
	s.mu.Lock()
	if _, waiting := s.pending[key]; !waiting && len(s.pending) >= s.MaxPending {
		s.mu.Unlock()
		s.Counters.Inc(CounterSaveDropped)
		return
	}
	s.pending[key] = sn
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Pending is how many aircraft wait for the bucket.
func (s *KVSaver) Pending() int {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// Run writes what is queued until ctx ends.
func (s *KVSaver) Run(ctx context.Context) {
	s.init()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			s.Flush(ctx)
		}
	}
}

// Flush writes everything queued now: each key once, with its newest
// value.
func (s *KVSaver) Flush(ctx context.Context) {
	s.init()
	s.mu.Lock()
	batch := s.pending
	s.pending = map[string]*Snapshot{}
	s.mu.Unlock()
	for key, sn := range batch {
		if err := s.write(ctx, key, sn); err != nil {
			s.Counters.Inc(CounterSaveFailed)
			if s.Logger != nil {
				s.Logger.LogAttrs(ctx, slog.LevelWarn, "flight binding not saved",
					slog.String("bucket", bus.BucketFlightBinding), slog.String("error", err.Error()))
			}
		}
	}
}

func (s *KVSaver) write(ctx context.Context, key string, sn *Snapshot) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if sn == nil {
		return s.KV.Delete(ctx, BindingKey(key))
	}
	b, err := json.Marshal(sn)
	if err != nil {
		return err
	}
	return s.KV.Put(ctx, BindingKey(key), b)
}

// Load reads every saved flight (at most MaxPending). A bucket that does
// not exist yet holds none: the first start of a deployment.
func (s *KVSaver) Load(ctx context.Context) ([]Snapshot, error) {
	s.init()
	entries, err := s.KV.All(ctx, s.MaxPending)
	if errors.Is(err, bus.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(entries))
	for _, e := range entries {
		var sn Snapshot
		if json.Unmarshal(e.Value, &sn) != nil || sn.Key == "" || BindingKey(sn.Key) != e.Key {
			s.Counters.Inc(CounterRestoreUnreadable)
			continue
		}
		out = append(out, sn)
	}
	return out, nil
}
