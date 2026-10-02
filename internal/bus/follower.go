package bus

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
)

// DefaultReread is the period of a follower's full re-read (docs/PLAN.md
// §3.2: watched, re-read every 300 s).
const DefaultReread = 300 * time.Second

// DefaultFollowRetry is the wait before a follower opens a missing
// bucket or a broken watch again; a bucket that appears is read within
// it.
const DefaultFollowRetry = 500 * time.Millisecond

// Counters of a follower.
const (
	CounterFollowApplied      = "follow_applied"
	CounterFollowDecodeFailed = "follow_decode_failed"
	CounterFollowReadFailed   = "follow_read_failed"
	CounterFollowDeleted      = "follow_deleted"
)

// Follower keeps the last value of one key of a KV bucket: delivered by
// the bucket's watch (the current value first, then every update), and
// read again on every message of the push subject (ctl.*) and every
// Reread. Value reports it with its age, the time since the bucket last
// confirmed it, so a caller serves a stale projection with its age and
// never blocks on it (D6). Until a value is read Value says ok false and
// the caller applies its documented default; a missing bucket is that,
// never a refusal (SC-22). A value that does not decode is counted and
// the last one kept. Safe for concurrent use; no lock is held across
// I/O (B-06).
type Follower[T any] struct {
	JS     jetstream.JetStream
	Bucket string
	Key    string
	// Decode reads a value (JSON by default).
	Decode func([]byte) (T, error)
	// Apply, when set, is called with every value taken (in order).
	Apply func(T)
	// Core and Push are the push subject (ctl.*); a nil Core has none.
	Core *nats.Conn
	Push string
	// Reread (DefaultReread) and Retry (DefaultFollowRetry).
	Reread, Retry time.Duration
	Counters      *core.Counters
	Logger        *slog.Logger
	Now           func() time.Time

	mu        sync.Mutex
	val       T
	ok        bool
	confirmed time.Time
	rev       uint64
}

func (f *Follower[T]) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Follower[T]) counters() *core.Counters {
	if f.Counters == nil {
		f.Counters = &core.Counters{}
	}
	return f.Counters
}

// Value is the last value, its age in seconds since the bucket last
// confirmed it, and whether there is one.
func (f *Follower[T]) Value() (T, float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ok {
		var zero T
		return zero, 0, false
	}
	return f.val, f.now().Sub(f.confirmed).Seconds(), true
}

func (f *Follower[T]) decode(data []byte) (T, error) {
	if f.Decode != nil {
		return f.Decode(data)
	}
	var v T
	err := json.Unmarshal(data, &v)
	return v, err
}

// take applies an entry of revision rev (0 for a push-triggered read
// of the same revision); an older revision is ignored.
func (f *Follower[T]) take(data []byte, rev uint64) {
	v, err := f.decode(data)
	if err != nil {
		f.counters().Inc(CounterFollowDecodeFailed)
		if f.Logger != nil {
			f.Logger.Warn("KV value not readable; keeping the last one", slog.String("bucket", f.Bucket),
				slog.String("key", f.Key), slog.String("error", err.Error()))
		}
		return
	}
	f.mu.Lock()
	if f.ok && rev != 0 && rev < f.rev {
		f.mu.Unlock()
		return
	}
	changed := !f.ok || rev != f.rev
	f.val, f.ok, f.confirmed, f.rev = v, true, f.now(), rev
	f.mu.Unlock()
	if changed {
		f.counters().Inc(CounterFollowApplied)
		if f.Apply != nil {
			f.Apply(v)
		}
	}
}

// confirm marks the value current without a change (a read that found
// the same revision, or no key: a missing key keeps the last value).
func (f *Follower[T]) confirm() {
	f.mu.Lock()
	if f.ok {
		f.confirmed = f.now()
	}
	f.mu.Unlock()
}

// read reads the key once.
func (f *Follower[T]) read(ctx context.Context, kv jetstream.KeyValue) error {
	e, err := kv.Get(ctx, f.Key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		// No value (or deleted): keep the last value; never fail closed.
		return nil
	case err != nil:
		f.counters().Inc(CounterFollowReadFailed)
		return err
	}
	f.mu.Lock()
	same := f.ok && e.Revision() == f.rev
	f.mu.Unlock()
	if same {
		f.confirm()
		return nil
	}
	f.take(e.Value(), e.Revision())
	return nil
}

// Run follows until ctx ends.
func (f *Follower[T]) Run(ctx context.Context) {
	retry := f.Retry
	if retry <= 0 {
		retry = DefaultFollowRetry
	}
	reread := f.Reread
	if reread <= 0 {
		reread = DefaultReread
	}
	poke := make(chan struct{}, 1)
	if f.Core != nil && f.Push != "" {
		sub, err := f.Core.Subscribe(f.Push, func(*nats.Msg) {
			select {
			case poke <- struct{}{}:
			default:
			}
		})
		if err == nil {
			defer func() { _ = sub.Unsubscribe() }()
		}
	}
	for ctx.Err() == nil {
		f.session(ctx, poke, reread)
		wait(ctx, retry)
	}
}

// session opens the bucket, reads the key and follows its watch until
// the watch ends or ctx ends.
func (f *Follower[T]) session(ctx context.Context, poke <-chan struct{}, reread time.Duration) {
	kv, err := f.JS.KeyValue(ctx, f.Bucket)
	if err != nil {
		return // missing bucket or NATS down: no value yet, or the last one with its age
	}
	// The watch delivers the current value first, then a nil marker,
	// then every update: no put can fall between a read and the watch.
	w, err := kv.Watch(ctx, f.Key)
	if err != nil {
		return
	}
	defer func() { _ = w.Stop() }()
	t := time.NewTicker(reread)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e, open := <-w.Updates():
			if !open {
				return
			}
			if e == nil {
				f.confirm() // the initial values are in
				continue
			}
			if e.Operation() == jetstream.KeyValuePut {
				f.take(e.Value(), e.Revision())
			} else {
				f.counters().Inc(CounterFollowDeleted)
			}
		case <-poke:
			if f.read(ctx, kv) != nil {
				return
			}
		case <-t.C:
			if f.read(ctx, kv) != nil {
				return
			}
		}
	}
}

func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
