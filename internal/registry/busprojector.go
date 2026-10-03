package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// KVWriter is what BusProjector needs of internal/bus.Projector.
type KVWriter interface {
	Put(ctx context.Context, bucket, key string, value []byte) error
	Delete(ctx context.Context, bucket, key string) error
	Keys(ctx context.Context, bucket string) ([]string, error)
}

// KVKey is the registry_validity key of k: the entity type and the key
// as a bus.KeyToken, so any serial or registration number is a valid
// key and two never share one.
func KVKey(k Key) string { return string(k.Entity) + "." + bus.KeyToken(k.Key) }

// BusProjector writes the cached answers to the KV bucket
// registry_validity (Projector): every put as its Entry in JSON under
// KVKey, every delete as a delete. Each operation is bounded by the KV
// writer; the first error is returned and the caller's transaction rolls
// back (B-09).
type BusProjector struct {
	KV KVWriter
}

var (
	_ Projector   = BusProjector{}
	_ FoldDeleter = BusProjector{}
)

// keyOfKV reverses KVKey; ok is false for a key it did not write.
func keyOfKV(kv string) (Key, bool) {
	entity, token, found := strings.Cut(kv, ".")
	if !found {
		return Key{}, false
	}
	key, err := bus.KeyFromToken(token)
	if err != nil {
		return Key{}, false
	}
	return Key{Entity: EntityType(entity), Key: key}, true
}

// DeleteFolds implements FoldDeleter: it lists the bucket and deletes
// every key an invalidation's fold names.
func (b BusProjector) DeleteFolds(ctx context.Context, inv []Invalidation) ([]Key, error) {
	if len(inv) == 0 {
		return nil, nil
	}
	keys, err := b.KV.Keys(ctx, BucketRegistryValidity)
	if err != nil {
		return nil, err
	}
	var out []Key
	for _, kv := range keys {
		k, ok := keyOfKV(kv)
		if !ok || !invalidates(inv, k) {
			continue
		}
		if err := b.KV.Delete(ctx, BucketRegistryValidity, kv); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// ProjectRegistry implements Projector.
func (b BusProjector) ProjectRegistry(ctx context.Context, put []Entry, del []Key) error {
	for _, k := range del {
		if err := b.KV.Delete(ctx, BucketRegistryValidity, KVKey(k)); err != nil {
			return err
		}
	}
	for i := range put {
		data, err := json.Marshal(put[i])
		if err != nil {
			return fmt.Errorf("registry_validity: %w", err)
		}
		if err := b.KV.Put(ctx, BucketRegistryValidity, KVKey(put[i].Key), data); err != nil {
			return err
		}
	}
	return nil
}
