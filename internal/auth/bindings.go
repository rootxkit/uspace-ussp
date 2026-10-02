package auth

import (
	"context"
	"slices"
	"sync"

	"github.com/rootxkit/uspace-core/serial"
)

// BucketClientBindings is the KV bucket of the client-serial bindings
// (D6): client_id -> the serial_fold values it may send telemetry for.
const BucketClientBindings = "client_bindings"

// BindingsProjector writes one client's bindings to the client_bindings
// projection. WP-6 implements it on the NATS KV bucket; until then
// MemoryBindings stands in. Writers call it inside the transaction that
// changes client_serial_bindings, so a failed projection rolls the
// change back (B-09).
type BindingsProjector interface {
	ProjectClientBindings(ctx context.Context, clientID string, folds []string) error
}

// BindingsReader answers the question telemetry-ingest asks of the
// projection (06 T3): may client send telemetry for serial?
type BindingsReader interface {
	Allowed(clientID, serial string) bool
}

// MemoryBindings is the in-memory client_bindings projection: a
// BindingsProjector and a BindingsReader in one process, until WP-6's
// KV projector replaces it. Safe for concurrent use.
type MemoryBindings struct {
	mu    sync.RWMutex
	folds map[string][]string
	// Fail, when set, makes every projection fail (tests of the B-09
	// rollback).
	Fail error
}

// NewMemoryBindings is an empty projection.
func NewMemoryBindings() *MemoryBindings { return &MemoryBindings{folds: map[string][]string{}} }

// ProjectClientBindings replaces clientID's folds.
func (m *MemoryBindings) ProjectClientBindings(_ context.Context, clientID string, folds []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if len(folds) == 0 {
		delete(m.folds, clientID)
		return nil
	}
	f := slices.Clone(folds)
	slices.Sort(f)
	m.folds[clientID] = slices.Compact(f)
	return nil
}

// Folds is the projected folds of clientID, sorted.
func (m *MemoryBindings) Folds(clientID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Clone(m.folds[clientID])
}

// Allowed reports whether serial's FoldKey is bound to clientID. A
// serial bound to another client is refused.
func (m *MemoryBindings) Allowed(clientID, s string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, found := slices.BinarySearch(m.folds[clientID], serial.FoldKey(s))
	return found
}
