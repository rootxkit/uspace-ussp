package bus

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
)

// Counters of a SeenWindow.
const (
	CounterSeenPutFailed  = "replay_window_put_failed"
	CounterSeenPutDropped = "replay_window_put_dropped"
)

// Bounds of a SeenWindow.
const (
	// DefaultSeenTimeout bounds one read on the hot path.
	DefaultSeenTimeout = 250 * time.Millisecond
	// seenQueue bounds the writes waiting for the bucket (E-10): about
	// ten seconds of 1000 aircraft at their combined live and backlog
	// rates is far more than a healthy bucket ever holds back.
	seenQueue = 16_384
)

// SeenWindow is the telemetry replay window in the telemetry_seen
// bucket (B-05): every instance of telemetry-ingest reads it, so a
// sample replayed to another replica or after a restart is still
// acknowledged as a duplicate. The bucket's TTL (SeenTTL) bounds it by
// time; a reader compares a key's own time of taking with its policy
// window. Get blocks for at most Timeout; Put queues and Run writes, so
// the publisher never waits for the bucket. A write lost (queue full, a
// failed put, the process ending before Run wrote it) lets one replay
// through, which is published twice; each is counted. Safe for
// concurrent use.
type SeenWindow struct {
	JS jetstream.JetStream
	// Bucket is BucketTelemetrySeen when empty.
	Bucket   string
	Timeout  time.Duration
	Counters *core.Counters
	Logger   *slog.Logger

	once sync.Once
	ch   chan seenPut
	mu   sync.Mutex
	kv   jetstream.KeyValue
}

type seenPut struct {
	key string
	val []byte
}

func (w *SeenWindow) init() {
	w.once.Do(func() {
		w.ch = make(chan seenPut, seenQueue)
		if w.Counters == nil {
			w.Counters = &core.Counters{}
		}
	})
}

func (w *SeenWindow) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	w.mu.Lock()
	kv := w.kv
	w.mu.Unlock()
	if kv != nil {
		return kv, nil
	}
	name := w.Bucket
	if name == "" {
		name = BucketTelemetrySeen
	}
	kv, err := w.JS.KeyValue(ctx, name)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.kv = kv
	w.mu.Unlock()
	return kv, nil
}

// Get reads key: its value and true, or false when the bucket holds no
// such key (never written, deleted or past its TTL); an error when the
// bucket could not be read within Timeout.
func (w *SeenWindow) Get(ctx context.Context, key string) ([]byte, bool, error) {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultSeenTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	kv, err := w.bucket(ctx)
	if err != nil {
		return nil, false, err
	}
	e, err := kv.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return e.Value(), true, nil
}

// Put queues key for Run; a full queue drops it (counted).
func (w *SeenWindow) Put(key string, val []byte) {
	w.init()
	select {
	case w.ch <- seenPut{key: key, val: val}:
	default:
		w.Counters.Inc(CounterSeenPutDropped)
	}
}

// Run writes the queued keys until ctx ends.
func (w *SeenWindow) Run(ctx context.Context) {
	w.init()
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-w.ch:
			if err := w.put(ctx, p); err != nil {
				w.Counters.Inc(CounterSeenPutFailed)
				if w.Logger != nil {
					w.Logger.LogAttrs(ctx, slog.LevelDebug, "replay window write failed",
						slog.String("bucket", BucketTelemetrySeen), slog.String("error", err.Error()))
				}
			}
		}
	}
}

func (w *SeenWindow) put(ctx context.Context, p seenPut) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	kv, err := w.bucket(ctx)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, p.key, p.val)
	return err
}
