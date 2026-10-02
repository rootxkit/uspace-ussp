package bus

import (
	"context"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// KVStore reads and writes single keys of one bucket for a process that
// owns the bucket's values (rid-sp's rid_isa_notifications), each call
// bounded by Timeout (DefaultKVTimeout).
type KVStore struct {
	JS      jetstream.JetStream
	Bucket  string
	Timeout time.Duration
}

func (s KVStore) open(ctx context.Context) (jetstream.KeyValue, context.Context, context.CancelFunc, error) {
	t := s.Timeout
	if t <= 0 {
		t = DefaultKVTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	kv, err := s.JS.KeyValue(ctx, s.Bucket)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return kv, ctx, cancel, nil
}

// Get is key's value; false when the bucket holds no such key.
func (s KVStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	kv, ctx, cancel, err := s.open(ctx)
	if err != nil {
		return nil, false, err
	}
	defer cancel()
	e, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return e.Value(), true, nil
}

// Put writes key.
func (s KVStore) Put(ctx context.Context, key string, value []byte) error {
	kv, ctx, cancel, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = kv.Put(ctx, key, value)
	return err
}

// ErrRevisionConflict is a write on a revision that is no longer the
// key's (another writer was first).
var ErrRevisionConflict = errors.New("kv: the key was written meanwhile")

// RevEntry is one key's value with its revision.
type RevEntry struct {
	Key   string
	Value []byte
	Rev   uint64
}

// GetRev is key's value and revision; false when the bucket holds no
// such key.
func (s KVStore) GetRev(ctx context.Context, key string) (RevEntry, bool, error) {
	kv, ctx, cancel, err := s.open(ctx)
	if err != nil {
		return RevEntry{}, false, err
	}
	defer cancel()
	e, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return RevEntry{}, false, nil
	}
	if err != nil {
		return RevEntry{}, false, err
	}
	return RevEntry{Key: key, Value: e.Value(), Rev: e.Revision()}, true, nil
}

// PutRev writes key on the revision rev (0: the key must not exist,
// or have been deleted) and returns the new revision:
// ErrRevisionConflict when rev is not the key's.
func (s KVStore) PutRev(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	kv, ctx, cancel, err := s.open(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()
	var n uint64
	if rev == 0 {
		n, err = kv.Create(ctx, key, value)
	} else {
		n, err = kv.Update(ctx, key, value, rev)
	}
	if errors.Is(err, jetstream.ErrKeyExists) {
		return 0, ErrRevisionConflict
	}
	return n, err
}

// Delete removes key; a key that is not there is not an error.
func (s KVStore) Delete(ctx context.Context, key string) error {
	kv, ctx, cancel, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	err = kv.Delete(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	return err
}

// All are the bucket's current values (at most maxKeys), read through a
// watch up to its marker within the store's timeout.
func (s KVStore) All(ctx context.Context, maxKeys int) ([]RevEntry, error) {
	kv, ctx, cancel, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	w, err := kv.WatchAll(ctx, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, err
	}
	defer func() { _ = w.Stop() }()
	var out []RevEntry
	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case e := <-w.Updates():
			if e == nil {
				return out, nil
			}
			if len(out) < maxKeys {
				out = append(out, RevEntry{Key: e.Key(), Value: e.Value(), Rev: e.Revision()})
			}
		}
	}
}
