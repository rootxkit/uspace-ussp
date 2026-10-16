package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

func (b *boundSet) set(ks ...Key) { b.mu.Lock(); b.keys = ks; b.mu.Unlock() }

func (f *fixture) sweep(t *testing.T) {
	t.Helper()
	if err := f.feed.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// Plan §5 item 5, the presence half: a bound key whose re-fetch failed
// (the lookups down while the feed answered) is restored by the sweep
// once the lookups answer again, and /readyz is up again; while they
// are still down the sweep leaves it unavailable and degraded.
func TestSweepRestoresAFailedRefetch(t *testing.T) {
	f := newFixture(t)
	withRefetch(f, uasA, opA)
	f.fake.SetOperator(opNumber, "active", nil)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.fake.SetUAS(snA, "active", "", "")
	f.uas(t, snA)
	f.fake.Down("validate")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	probe := ReadinessProbe(f.cache, f.feed)
	if _, ok := f.store.row(uasA); ok || f.count(CounterRefetchFailed) != 1 {
		t.Fatalf("the failed re-fetch left a row (failed %d)", f.count(CounterRefetchFailed))
	}
	f.sweep(t) // still down
	if st, detail := probe(t.Context()); st != obs.StateDegraded || !strings.Contains(detail, "refetch_failed=1") {
		t.Fatalf("down: %s %s", st, detail)
	}
	if f.count(CounterRevalidateFailed) != 1 {
		t.Fatalf("revalidate failed %d", f.count(CounterRevalidateFailed))
	}
	f.fake.Up()
	f.sweep(t)
	if e, ok := f.store.row(uasA); !ok || e.Status != StatusValid || !f.projected(uasA) {
		t.Fatalf("not restored: %+v %v", e, ok)
	}
	if st, detail := probe(t.Context()); st != obs.StateUp || strings.Contains(detail, "refetch_failed") {
		t.Fatalf("restored: %s %s", st, detail)
	}
	if id := f.resolve(); id.Status != core.IdentRegistered {
		t.Fatalf("resolved %+v", id)
	}
	if f.count(CounterRevalidated) != 1 {
		t.Fatalf("revalidated %d", f.count(CounterRevalidated))
	}
}

// Plan §5 item 7, the sweep's half (E-10): the keys a poll deferred are
// restored by the sweep, and a sweep asks about at most the policy's
// registry_revalidate_max_count, missing keys first; the rest are
// counted and asked by the next sweep.
func TestSweepIsBoundedAndRestoresDeferredKeys(t *testing.T) {
	f := newFixture(t)
	var keys []Key
	for i := range 120 {
		sn := fmt.Sprintf("TEST-SN-%03d", i)
		f.fake.SetUAS(sn, "active", "", "")
		keys = append(keys, Key{EntityUAS, sn})
	}
	withRefetch(f, keys...)
	f.feed.cfg.MaxRefetchPerPoll = 40
	f.feed.cfg.Revalidate = func() Revalidate { return Revalidate{Interval: time.Minute, MaxKeys: 50} }
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := func() int {
		n := 0
		for _, k := range keys {
			if _, ok := f.store.row(k); ok {
				n++
			}
		}
		return n
	}
	if rows() != 40 || f.count(CounterRefetchDeferred) != 80 {
		t.Fatalf("after the poll %d rows, deferred %d", rows(), f.count(CounterRefetchDeferred))
	}
	f.sweep(t)
	if rows() != 90 || f.count(CounterRevalidateDeferred) != 30 || f.count(CounterRevalidated) != 50 {
		t.Fatalf("first sweep %d rows, deferred %d, revalidated %d", rows(), f.count(CounterRevalidateDeferred), f.count(CounterRevalidated))
	}
	f.sweep(t)
	if rows() != 120 || f.count(CounterRevalidateDeferred) != 30 {
		t.Fatalf("second sweep %d rows, deferred %d", rows(), f.count(CounterRevalidateDeferred))
	}
	// Every answer is fresh: a sweep now asks nothing.
	before := f.fake.Requests("validate") + f.fake.Requests("validate_batch")
	f.sweep(t)
	if f.fake.Requests("validate")+f.fake.Requests("validate_batch") != before {
		t.Fatal("a sweep asked about fresh answers")
	}
}

// Plan §5 item 8: the authority's feed is not gap-free. A change that
// commits under a sequence the feed has read past is never named; the
// sweep asks about the bound key once its answer is older than the
// period and the status changes. A key that is not bound is not asked
// (E-01 pair), and a fresh bound key not before its period.
func TestSweepBoundsAFeedGapInTime(t *testing.T) {
	f := newFixture(t)
	withRefetch(f, uasA)
	f.fake.SetUAS(snA, "active", "", "")
	f.fake.SetUAS("TEST-SN-UNBOUND", "active", "", "")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.uas(t, "TEST-SN-UNBOUND")
	if e, ok := f.store.row(uasA); !ok || e.Status != StatusValid {
		t.Fatalf("bound key %+v", e)
	}
	served := cursor(t, f.store).Since
	f.fake.InsertChangeBehind(served)
	f.fake.SetUAS(snA, "suspended", "", "")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e, _ := f.store.row(uasA); e.Status != StatusValid || f.count(CounterFeedInvalidated) != 0 {
		t.Fatalf("the feed saw a change behind its cursor: %+v", e)
	}
	purposes := len(f.fake.Purposes())
	f.sweep(t)
	if len(f.fake.Purposes()) != purposes {
		t.Fatal("a sweep asked about an answer younger than its period")
	}
	f.clock.Advance(61 * time.Second)
	f.sweep(t)
	if e, _ := f.store.row(uasA); e.Status != StatusSuspended {
		t.Fatalf("the sweep did not bring the change: %+v", e)
	}
	if got := f.fake.Purposes()[purposes:]; len(got) != 1 || got[0] != "identification" {
		t.Fatalf("lookups %v, want one for the bound key", got)
	}
	if e, _ := f.store.row(Key{EntityUAS, "TEST-SN-UNBOUND"}); e.FetchedAt.After(f.clock.Now().Add(-61 * time.Second)) {
		t.Fatalf("the sweep asked about an unbound key: %+v", e)
	}
}

// The sweep's period and bound are the policy's; a value that is not
// positive is never a disarmed sweep (E-15): the defaults stand in. A
// sweep without bound keys or a cache asks nothing; one whose bound
// keys cannot be read fails, counted.
func TestSweepPolicyAndFailures(t *testing.T) {
	v := policy.Defaults()
	if r := RevalidateFromPolicy(v); r.Interval != time.Minute || r.MaxKeys != 1000 {
		t.Fatalf("defaults %+v", r)
	}
	f := newFixture(t)
	f.feed.cfg.Revalidate = func() Revalidate { return Revalidate{} }
	if r := f.feed.revalidate(); r.Interval != time.Minute || r.MaxKeys != 1000 {
		t.Fatalf("a zero policy %+v", r)
	}
	f.sweep(t) // no bound keys, no cache: nothing
	bs, _ := withRefetch(f)
	f.sweep(t) // nothing bound
	bs.set(uasA)
	bs.err = errors.New("database down")
	if err := f.feed.Sweep(t.Context()); err == nil || f.count(CounterSweepFailed) != 1 {
		t.Fatalf("an unreadable bound-key set swept: %v", err)
	}
	if f.fake.Requests("validate")+f.fake.Requests("validate_batch") != 0 {
		t.Fatal("asked")
	}
}

// Run sweeps after its first poll: a bound key never looked up is
// answered without any lookup or change naming it.
func TestRunSweepsAfterTheFirstPoll(t *testing.T) {
	f := newFixture(t)
	withRefetch(f, uasA)
	f.fake.SetUAS(snA, "active", "", "")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { f.feed.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := f.store.row(uasA); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not sweep")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// Plan §5 item 10: the first fact of a flight looks up its serial and
// operator, purpose identification, once; a fresh key is not asked
// again; a serial F8 does not define asks nothing and is counted.
func TestWarmOnFirstSight(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	op := opNumber
	f.cache.Warm(t.Context(), snA, &op)
	if f.fake.Requests("validate") != 1 || f.fake.Purposes()[0] != "identification" || f.count(CounterWarmed) != 1 {
		t.Fatalf("validate %d purposes %v", f.fake.Requests("validate"), f.fake.Purposes())
	}
	if !f.projected(uasA) || !f.projected(opA) {
		t.Fatal("the answers were not projected")
	}
	f.cache.Warm(t.Context(), snA, &op)
	if f.fake.Requests("validate")+f.fake.Requests("validate_batch") != 1 {
		t.Fatal("a fresh key was asked again")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.cache.Warm(ctx, "TEST-SN-NEW", nil) // the caller's context is gone: E-14
	if f.fake.Requests("validate") != 2 {
		t.Fatal("a cancelled caller stopped the lookup")
	}
	f.cache.Warm(t.Context(), strings.Repeat("S", MaxSerialLen+1), nil)
	if f.count(CounterWarmRefused) != 1 || f.fake.Requests("validate") != 2 {
		t.Fatalf("refused %d", f.count(CounterWarmRefused))
	}
}
