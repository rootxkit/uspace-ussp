package flights

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// memKV is a flight_binding bucket in memory.
type memKV struct {
	mu      sync.Mutex
	m       map[string][]byte
	failPut bool
	allErr  error
	// ops, when set, receives every write ("put <key>", "delete <key>").
	ops chan string
}

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (k *memKV) Put(_ context.Context, key string, v []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failPut {
		return errors.New("bucket full")
	}
	k.m[key] = append([]byte(nil), v...)
	if k.ops != nil {
		k.ops <- "put " + key
	}
	return nil
}

func (k *memKV) Delete(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, key)
	if k.ops != nil {
		k.ops <- "delete " + key
	}
	return nil
}

func (k *memKV) All(_ context.Context, maxKeys int) ([]bus.RevEntry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.allErr != nil {
		return nil, k.allErr
	}
	var out []bus.RevEntry
	for key, v := range k.m {
		if len(out) < maxKeys {
			out = append(out, bus.RevEntry{Key: key, Value: v})
		}
	}
	return out, nil
}

func (k *memKV) len() int { k.mu.Lock(); defer k.mu.Unlock(); return len(k.m) }

// persistedRig is a binder rig whose flights a KVSaver keeps in kv.
func persistedRig(kv *memKV, clk *clock) (*binderRig, *KVSaver) {
	r := newBinderRig()
	if clk != nil {
		r.clk = clk
		r.b.Now = clk.now
	}
	s := &KVSaver{KV: kv}
	r.b.Save, r.b.Forget = s.Save, s.Forget
	return r, s
}

// restart is a new process: a new binder over the same bucket, with its
// saved flights taken back (restore true) or not (the behaviour before).
func restart(t *testing.T, kv *memKV, clk *clock, restore bool) (*binderRig, *KVSaver) {
	t.Helper()
	r, s := persistedRig(kv, clk)
	if restore {
		snaps, err := s.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		r.b.Restore(snaps)
	}
	return r, s
}

// The restart row of WP-19, which the chaos run found: telemetry-ingest
// killed while two aircraft flew. Restored, the aircraft go on with
// their flight ids and no new flight starts.
func TestARestartKeepsTheFlightOfAnAircraftInTheAir(t *testing.T) {
	kv, clk := newMemKV(), &clock{at: t0}
	r, s := persistedRig(kv, clk)
	id := r.b.Bind("a", "c1", "S1", str("i1"), str("GEO-TEST-AUTH-1"), str("GEOTESTOP0001"), p0, t0, true)
	s.Flush(context.Background())
	if kv.len() != 1 {
		t.Fatalf("saved %d flights, want 1", kv.len())
	}

	clk.add(9 * time.Second) // the process is gone for 9 s
	r2, _ := restart(t, kv, clk, true)
	if got := r2.b.Bind("a", "c1", "S1", str("i1"), str("GEO-TEST-AUTH-1"), str("GEOTESTOP0001"), p0, clk.now(), true); got != id {
		t.Fatalf("after the restart the aircraft flies %s, want its flight %s", got, id)
	}
	if len(r2.events) != 0 {
		t.Fatalf("facts after the restart: %v, want none (no new flight)", r2.kinds())
	}
	if r2.b.Counters.Get(CounterRestored) != 1 || r2.b.Len() != 1 {
		t.Fatalf("restored %d, held %d", r2.b.Counters.Get(CounterRestored), r2.b.Len())
	}
}

// Its twin without the restore is what the run saw: a new flight id for
// the same aircraft, the old flight left without its aircraft.
func TestWithoutTheRestoreARestartStartsANewFlight(t *testing.T) {
	kv, clk := newMemKV(), &clock{at: t0}
	r, s := persistedRig(kv, clk)
	id := r.b.Bind("a", "c1", "S1", str("i1"), nil, nil, p0, t0, true)
	s.Flush(context.Background())
	clk.add(9 * time.Second)
	r2, _ := restart(t, kv, clk, false)
	if got := r2.b.Bind("a", "c1", "S1", str("i1"), nil, nil, p0, clk.now(), true); got == id {
		t.Fatal("without the restore the aircraft kept its flight")
	}
	if k := r2.kinds(); len(k) != 1 || k[0] != EventStarted {
		t.Fatalf("facts: %v, want a new start", k)
	}
}

// A flight saved before a restart that lasted longer than
// flight_end_after_s ends at the first tick after it, telemetry_lost, as
// it would have without the restart; its key leaves the bucket.
func TestARestoredSilentFlightEndsAtTheNextTick(t *testing.T) {
	kv, clk := newMemKV(), &clock{at: t0}
	r, s := persistedRig(kv, clk)
	id := r.b.Bind("a", "c1", "S1", nil, nil, nil, p0, t0, true)
	s.Flush(context.Background())

	clk.add(time.Duration(policy.Defaults().FlightEndAfterS*float64(time.Second)) + time.Second)
	r2, s2 := restart(t, kv, clk, true)
	if n := r2.b.Tick(); n != 1 {
		t.Fatalf("Tick ended %d flights, want 1", n)
	}
	if len(r2.events) != 1 || r2.events[0].Body.Event != EventEnded || r2.events[0].Body.FlightID != id ||
		*r2.events[0].Body.EndReason != EndTelemetryLost {
		t.Fatalf("facts: %+v", r2.events)
	}
	s2.Flush(context.Background())
	if kv.len() != 0 {
		t.Fatalf("the ended flight is still saved (%d keys)", kv.len())
	}
}

// A flight restored while its aircraft already flies another one here
// (a sample was bound before the restore) is ended at once, counted; the
// running one stays.
func TestARestoredFlightSupersededIsEnded(t *testing.T) {
	kv, clk := newMemKV(), &clock{at: t0}
	r, s := persistedRig(kv, clk)
	old := r.b.Bind("a", "c1", "S1", nil, nil, nil, p0, t0, true)
	s.Flush(context.Background())
	r2, s2 := persistedRig(kv, clk)
	cur := r2.b.Bind("a", "c1", "S1", nil, nil, nil, p0, t0, true)
	snaps, err := s2.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2.events = nil
	if n := r2.b.Restore(snaps); n != 0 {
		t.Fatalf("restored %d", n)
	}
	if len(r2.events) != 1 || r2.events[0].Body.FlightID != old || r2.events[0].Body.Event != EventEnded {
		t.Fatalf("facts: %+v", r2.events)
	}
	if r2.b.Counters.Get(CounterRestoreSuperseded) != 1 || r2.b.Bind("a", "c1", "S1", nil, nil, nil, p0, t0, true) != cur {
		t.Fatal("the running flight was not kept")
	}
}

// A flight that receives samples is saved at its start and then every
// SaveEvery, not at every sample; telemetry_lost and resumed are saved
// at once.
func TestSavesAreSpacedBySaveEvery(t *testing.T) {
	clk := &clock{at: t0}
	r := newBinderRig()
	r.clk, r.b.Now = clk, clk.now
	var saves []Snapshot
	r.b.Save = func(s Snapshot) { saves = append(saves, s) }
	for i := 0; i < 25; i++ {
		r.b.Bind("a", "c1", "S1", nil, nil, nil, p0, clk.now(), true)
		clk.add(time.Second)
	}
	if len(saves) != 3 {
		t.Fatalf("%d saves in 25 s of samples, want 3 (at 0, 10 and 20 s)", len(saves))
	}
	clk.add(time.Duration(policy.Defaults().TelemetryLostS*float64(time.Second)) + time.Second)
	r.b.Tick()
	if len(saves) != 4 || !saves[3].Lost {
		t.Fatalf("telemetry_lost not saved at once: %d saves", len(saves))
	}
	r.b.Bind("a", "c1", "S1", nil, nil, nil, p0, clk.now(), true)
	if len(saves) != 5 || saves[4].Lost {
		t.Fatalf("telemetry_resumed not saved at once: %d saves", len(saves))
	}
}

// E-10: at most MaxPending aircraft wait for the bucket; one more is
// dropped and counted, a newer save of a waiting one replaces it.
func TestKVSaverIsBounded(t *testing.T) {
	kv := newMemKV()
	s := &KVSaver{KV: kv, MaxPending: 2}
	for i := 0; i < 3; i++ {
		s.Save(Snapshot{Key: fmt.Sprintf("k%d", i), FlightID: "f"})
	}
	s.Save(Snapshot{Key: "k0", FlightID: "f2"})
	if s.Pending() != 2 || s.Counters.Get(CounterSaveDropped) != 1 {
		t.Fatalf("pending %d, dropped %d", s.Pending(), s.Counters.Get(CounterSaveDropped))
	}
	s.Flush(context.Background())
	snaps, err := s.Load(context.Background())
	if err != nil || len(snaps) != 2 {
		t.Fatalf("loaded %d (%v)", len(snaps), err)
	}
	for _, sn := range snaps {
		if sn.Key == "k0" && sn.FlightID != "f2" {
			t.Fatalf("k0 kept its older save %s", sn.FlightID)
		}
	}
}

// A failed write is counted and does not stop the next; what cannot be
// decoded is skipped and counted; a bucket not created yet holds none; a
// bucket that cannot be read is an error.
func TestKVSaverFailures(t *testing.T) {
	kv := newMemKV()
	s := &KVSaver{KV: kv}
	kv.failPut = true
	s.Save(Snapshot{Key: "a", FlightID: "f"})
	s.Flush(context.Background())
	if s.Counters.Get(CounterSaveFailed) != 1 || kv.len() != 0 {
		t.Fatalf("failed %d, saved %d", s.Counters.Get(CounterSaveFailed), kv.len())
	}
	kv.failPut = false
	s.Save(Snapshot{Key: "a", FlightID: "f"})
	s.Flush(context.Background())
	kv.m[BindingKey("b")] = []byte("{not json")
	kv.m["other"] = []byte(`{"key":"c","flight_id":"g"}`) // under the wrong key
	snaps, err := s.Load(context.Background())
	if err != nil || len(snaps) != 1 || snaps[0].FlightID != "f" || s.Counters.Get(CounterRestoreUnreadable) != 2 {
		t.Fatalf("loaded %+v (%v), unreadable %d", snaps, err, s.Counters.Get(CounterRestoreUnreadable))
	}
	kv.allErr = bus.ErrBucketNotFound
	if snaps, err := s.Load(context.Background()); err != nil || len(snaps) != 0 {
		t.Fatalf("a bucket not yet created: %v %v", snaps, err)
	}
	kv.allErr = errors.New("nats: timeout")
	if _, err := s.Load(context.Background()); err == nil {
		t.Fatal("an unreadable bucket loaded")
	}
}

// Run writes what is queued and stops with its context.
func TestKVSaverRun(t *testing.T) {
	kv := newMemKV()
	kv.ops = make(chan string, 4)
	s := &KVSaver{KV: kv, Counters: &core.Counters{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	wait := func(want string) {
		t.Helper()
		select {
		case op := <-kv.ops:
			if op != want {
				t.Fatalf("bucket %s, want %s", op, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Run did not %s", want)
		}
	}
	s.Save(Snapshot{Key: "a", FlightID: "f"})
	wait("put " + BindingKey("a"))
	s.Forget("a")
	wait("delete " + BindingKey("a"))
	cancel()
	<-done
}
