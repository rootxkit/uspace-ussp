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
