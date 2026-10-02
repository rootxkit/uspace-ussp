package registry

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// KVWriter is what BusProjector needs of internal/bus.Projector.
type KVWriter interface {
	Put(ctx context.Context, bucket, key string, value []byte) error
	Delete(ctx context.Context, bucket, key string) error
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

var _ Projector = BusProjector{}

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
