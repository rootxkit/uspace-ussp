//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/ridsp"
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

// The refusals of an ISA's writes on PostgreSQL: below the bound the
// ISA is kept, at the bound it is given up and RefusedISAs names it with
// its error; a write the DSS takes clears it (E-01 both ways).
func TestIntegrationRIDSPISARefusals(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	ws := ridsppg.WorkStore{S: appStore(t)}
	isa := newUUID()
	if _, err := relOwner(t).Exec(ctx, `INSERT INTO dss_isas (isa_id, kind, time_start, time_end, extents)
		VALUES ($1, 'session', now(), now() + interval '1 hour', '{}')`, isa); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		gave, err := ws.Refused(ctx, isa, fmt.Sprintf("DSS refused the ISA: 400 (%d)", i), 3)
		if err != nil {
			t.Fatal(err)
		}
		if gave != (i == 3) {
			t.Fatalf("refusal %d: given up %v", i, gave)
		}
	}
	n, id, msg, err := ws.RefusedISAs(ctx)
	if err != nil || n < 1 || id != isa || !strings.Contains(msg, "(3)") {
		t.Fatalf("refused ISAs: %d %s %q %v", n, id, msg, err)
	}
	v := "v1"
	if err := ws.Written(ctx, ridsp.ISARecord{ISAID: isa, Version: &v, TimeStart: time.Now(), TimeEnd: time.Now().Add(time.Hour), Extents: []byte(`{}`)}, nil); err != nil {
		t.Fatal(err)
	}
	if _, id, _, err := ws.RefusedISAs(ctx); err != nil || id == isa {
		t.Fatalf("still given up after a write: %s %v", id, err)
	}
	if c := count(t, relOwner(t), "SELECT count(*) FROM dss_isas WHERE isa_id = $1 AND refusals = 0 AND refused_at IS NULL", isa); c != 1 {
		t.Fatalf("refusals not cleared: %d", c)
	}
}
