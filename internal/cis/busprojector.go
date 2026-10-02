package cis

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/cell"
)

// KeyBasis is the cis_current key written last by every projection: the
// CIS version and age the cell entries were built from. A follower that
// finds a basis and no entry for its cell knows the cell has no zone; a
// follower that finds no basis knows nothing is loaded (SC-22).
const KeyBasis = "basis"

// DefaultProjectTimeout bounds one whole projection.
const DefaultProjectTimeout = 30 * time.Second

// KVWriter is what BusProjector needs of internal/bus.Projector.
type KVWriter interface {
	Put(ctx context.Context, bucket, key string, value []byte) error
	Delete(ctx context.Context, bucket, key string) error
	Keys(ctx context.Context, bucket string) ([]string, error)
}

// BusProjector writes a Projection to the KV bucket cis_current: one key
// per cell5 (cell.KVToken of its name), the key "all" for the zones
// listed everywhere, the keys of cells no longer listed deleted, and the
// basis last. Each operation is bounded by the KV writer, the whole
// projection by Timeout; an error is the cache's to count and show
// (cis_projection_failed, /readyz).
type BusProjector struct {
	KV      KVWriter
	Timeout time.Duration

	// run keeps one projection at a time; nothing reads under it.
	run     sync.Mutex
	written []string // keys written by the last projection; nil before
}

var _ Projector = (*BusProjector)(nil)

// BasisValue is the value under KeyBasis.
type BasisValue struct {
	Basis
	At    time.Time `json:"at"`
	Cells int       `json:"cells"`
}

// ProjectCIS implements Projector.
func (b *BusProjector) ProjectCIS(ctx context.Context, p *Projection) error {
	b.run.Lock()
	defer b.run.Unlock()
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = DefaultProjectTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	previous := b.written
	if previous == nil {
		keys, err := b.KV.Keys(ctx, BucketCISCurrent)
		if err != nil {
			return err
		}
		previous = keys
	}
	names := make([]string, 0, len(p.Cells))
	for name := range p.Cells {
		names = append(names, name)
	}
	slices.Sort(names)
	keys := make([]string, 0, len(names))
	for _, name := range names {
		e := p.Cells[name]
		data, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("cis_current %s: %w", name, err)
		}
		key := cell.KVToken(name)
		if err := b.KV.Put(ctx, BucketCISCurrent, key, data); err != nil {
			return err
		}
		keys = append(keys, key)
	}
	for _, k := range previous {
		if k != KeyBasis && !slices.Contains(keys, k) {
			if err := b.KV.Delete(ctx, BucketCISCurrent, k); err != nil {
				return err
			}
		}
	}
	data, err := json.Marshal(BasisValue{Basis: p.Basis, At: p.At, Cells: len(keys)})
	if err != nil {
		return err
	}
	if err := b.KV.Put(ctx, BucketCISCurrent, KeyBasis, data); err != nil {
		return err
	}
	b.written = keys
	return nil
}
