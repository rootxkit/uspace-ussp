//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	ridsppg "github.com/rootxkit/uspace-ussp/internal/ridsp/pgstore"
)

// The ISA lock on PostgreSQL, as two api replicas take it (two pools):
// while one holds an ISA's lock, the other's Lock of the same ISA waits
// until it is released, and a Lock of another ISA does not wait (E-01).
func TestIntegrationRIDSPISALockSerialises(t *testing.T) {
	ensureSchemas(t)
	a, b := ridsppg.WorkStore{S: appStore(t)}, ridsppg.WorkStore{S: appStore(t)}
	ctx := context.Background()
	isa, other := newUUID(), newUUID()
	held, release := make(chan struct{}), make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- a.Lock(ctx, isa, func() error { close(held); <-release; return nil })
	}()
	<-held

	began := time.Now()
	if err := b.Lock(ctx, other, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("the lock of another ISA waited %v", took)
	}

	entered := make(chan time.Time, 1)
	bDone := make(chan error, 1)
	go func() { bDone <- b.Lock(ctx, isa, func() error { entered <- time.Now(); return nil }) }()
	select {
	case <-entered:
		t.Fatal("the same ISA's lock was taken while the other replica held it")
	case <-time.After(500 * time.Millisecond):
	}
	released := time.Now()
	close(release)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	if err := <-bDone; err != nil {
		t.Fatal(err)
	}
	if at := <-entered; at.Before(released) {
		t.Fatal("entered before the release")
	}
}
