package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// boundSet is a BoundKeys whose keys the test sets.
type boundSet struct {
	mu   sync.Mutex
	keys []Key
	err  error
}

func (b *boundSet) Bound(context.Context) ([]Key, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.keys), b.err
}

// recProjector is a MemoryProjector that records every write to it, in
// order, and runs after once each write is applied: what the hot path
// reads between two writes.
type recProjector struct {
	*MemoryProjector
	mu    sync.Mutex
	ops   []string
	after func()
}

func opName(op string, k Key) string { return op + " " + string(k.Entity) + "/" + k.Key }

func (r *recProjector) ProjectRegistry(ctx context.Context, put []Entry, del []Key) error {
	if err := r.MemoryProjector.ProjectRegistry(ctx, put, del); err != nil {
		return err
	}
	r.mu.Lock()
	for _, k := range del {
		r.ops = append(r.ops, opName("delete", k))
	}
	for i := range put {
		r.ops = append(r.ops, opName("put", put[i].Key))
	}
	r.mu.Unlock()
	if r.after != nil {
		r.after()
	}
	return nil
}

func (r *recProjector) DeleteFolds(ctx context.Context, inv []Invalidation, except []Key) ([]Key, error) {
	got, err := r.MemoryProjector.DeleteFolds(ctx, inv, except)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	for _, k := range got {
		r.ops = append(r.ops, opName("delete", k))
	}
	r.mu.Unlock()
	if r.after != nil {
		r.after()
	}
	return got, nil
}

// of are the recorded writes to k.
func (r *recProjector) of(k Key) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, op := range r.ops {
		if strings.HasSuffix(op, " "+string(k.Entity)+"/"+k.Key) {
			out = append(out, strings.Fields(op)[0])
		}
	}
	return out
}

// withRefetch gives the fixture's feed the bound keys and the cache's
// re-fetch, and both a recording projector.
func withRefetch(f *fixture, bound ...Key) (*boundSet, *recProjector) {
	bs := &boundSet{keys: bound}
	rec := &recProjector{MemoryProjector: f.proj}
	f.cache.cfg.Projector, f.feed.cfg.Projector = rec, rec
	f.feed.cfg.Bound, f.feed.cfg.Cache = bs, f.cache
	return bs, rec
}

var (
	uasA = Key{EntityUAS, snA}
	opA  = Key{EntityOperator, opNumber}
)

// resolve is the hot path's identification of snA flown by opNumber,
// from the projection as it stands.
func (f *fixture) resolve() core.Identification {
	op := opNumber
	fleet := Fleet{Operators: []FleetOperator{{ID: op, RegistrationNumber: op}},
		Aircraft: []FleetAircraft{{DroneID: snA, Serial: snA, OperatorID: &op}}}
	return FromProjection(fleet, f.proj, TTLFromPolicy(policy.Defaults()), f.clock.Now()).ResolveBound(snA)
}

// Plan §5 item 2, the staging order: a registration, a lookup before
// the feed has seen it, the key bound to a flight, then the first poll.
// The change is unseen at the fetch, so the answer is replaced in the
// same poll: one more lookup, the row written under the page's
// next_since, the projected key put and never deleted.
func TestChangeUnseenAtTheFetchIsReplacedInTheSamePoll(t *testing.T) {
	f := newFixture(t)
	_, rec := withRefetch(f, uasA)
	f.fake.SetUAS(snA, "active", "", "")
	f.uas(t, snA) // fetched under cursor 0
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	e, ok := f.store.row(uasA)
	if !ok || e.FetchedSince != 1 || e.Status != StatusValid {
		t.Fatalf("row %+v %v", e, ok)
	}
	if f.fake.Requests("validate") != 2 || !slices.Equal(f.fake.Purposes(), []string{"identification", "identification"}) {
		t.Fatalf("validate %d purposes %v", f.fake.Requests("validate"), f.fake.Purposes())
	}
	if ops := rec.of(uasA); !slices.Equal(ops, []string{"put", "put"}) {
		t.Fatalf("projection writes to the key %v, want two puts and no delete", ops)
	}
	if !f.projected(uasA) {
		t.Fatal("the projection lost the key")
	}
	if f.count(CounterFeedInvalidated) != 1 || f.count(CounterFeedRefetched) != 1 || f.count(CounterRefetchFailed) != 0 {
		t.Fatalf("invalidated %d refetched %d failed %d", f.count(CounterFeedInvalidated), f.count(CounterFeedRefetched), f.count(CounterRefetchFailed))
	}
	if a := f.uas(t, snA); a.Status != StatusValid || f.fake.Requests("validate") != 2 {
		t.Fatalf("the re-fetched answer is not served from the cache: %+v", a)
	}
}

// Plan §5 item 3, the absence twin of item 2: the same order with the
// key not bound deletes the row and the projected key and asks nothing;
// the next lookup re-fetches.
func TestChangeUnseenAtTheFetchOfAnUnboundKeyIsDeleted(t *testing.T) {
	f := newFixture(t)
	_, rec := withRefetch(f, Key{EntityUAS, "TEST-SN-OTHER"})
	f.fake.SetUAS(snA, "active", "", "")
	f.uas(t, snA)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.row(uasA); ok || f.projected(uasA) {
		t.Fatal("an unbound key's answer survived a change it did not see")
	}
	if ops := rec.of(uasA); !slices.Contains(ops, "delete") {
		t.Fatalf("writes %v", ops)
	}
	if f.fake.Requests("validate") != 1 || f.count(CounterFeedRefetched) != 0 || f.count(CounterFeedInvalidated) != 1 {
		t.Fatalf("validate %d refetched %d", f.fake.Requests("validate"), f.count(CounterFeedRefetched))
	}
	if a := f.uas(t, snA); a.Status != StatusValid || f.fake.Requests("validate") != 2 {
		t.Fatalf("next lookup: %+v", a)
	}
}

// Plan §5 item 4: a suspension mid-flight surfaces as suspended on the
// hot path in the poll that applies it, and no state of the projection
// between the writes ever resolves registry_unavailable; the operator's
// suspension the same; and back (active again) the aircraft is
// registered.
func TestSuspensionMidFlightSurfacesAsSuspended(t *testing.T) {
	f := newFixture(t)
	_, rec := withRefetch(f, uasA, opA)
	var seen []core.Identification
	rec.after = func() { seen = append(seen, f.resolve()) }
	f.fake.SetUAS(snA, "active", "", "")
	f.fake.SetOperator(opNumber, "active", nil)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cache.Validate(t.Context(), []Query{{Serial: snA, Operator: opNumber}}, PurposeIdentification); err != nil {
		t.Fatal(err)
	}
	if id := f.resolve(); id.Status != core.IdentRegistered {
		t.Fatalf("before %+v", id)
	}
	for _, step := range []struct {
		name   string
		change func()
		status core.IdentStatus
		reason core.IdentReason
	}{
		{"uas suspended", func() { f.fake.SetUAS(snA, "suspended", "", "") }, core.IdentSuspended, core.ReasonUASSuspended},
		{"uas active again", func() { f.fake.SetUAS(snA, "active", "", "") }, core.IdentRegistered, core.ReasonSessionBinding},
		{"operator suspended", func() { f.fake.SetOperator(opNumber, "suspended", nil) }, core.IdentSuspended, core.ReasonOperatorSuspended},
		{"operator active again", func() { f.fake.SetOperator(opNumber, "active", nil) }, core.IdentRegistered, core.ReasonSessionBinding},
	} {
		seen = nil
		step.change()
		if err := f.feed.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
		if id := f.resolve(); id.Status != step.status || id.Reason != step.reason {
			t.Fatalf("%s: %+v", step.name, id)
		}
		if len(seen) == 0 {
			t.Fatalf("%s: the projection was not written", step.name)
		}
		for _, id := range seen {
			if id.Reason == core.ReasonRegistryUnavailable {
				t.Fatalf("%s: a state between the writes resolved registry_unavailable: %+v", step.name, seen)
			}
		}
	}
	for _, k := range []Key{uasA, opA} {
		if slices.Contains(rec.of(k), "delete") {
			t.Fatalf("%v was deleted from the projection: %v", k, rec.of(k))
		}
	}
}

// Plan §5 item 5(b): the feed answers but the lookups do not. A bound
// key a change named is deleted, never kept valid; it resolves
// registry_unavailable, is counted registry_refetch_failed, and /readyz
// is degraded naming it. A later re-fetch that is answered clears it
// (E-02 both ways).
func TestRefetchWithTheLookupsDown(t *testing.T) {
	f := newFixture(t)
	bs, _ := withRefetch(f, uasA, opA)
	f.fake.SetUAS(snA, "active", "", "")
	f.fake.SetOperator(opNumber, "active", nil)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Bound and named but never cached, both were fetched by that poll.
	if f.count(CounterFeedRefetched) != 2 {
		t.Fatalf("refetched %d", f.count(CounterFeedRefetched))
	}
	if _, err := f.cache.Validate(t.Context(), []Query{{Serial: snA, Operator: opNumber}}, PurposeIdentification); err != nil {
		t.Fatal(err)
	}
	probe := ReadinessProbe(f.cache, f.feed)
	if st, detail := probe(t.Context()); st != obs.StateUp {
		t.Fatalf("before: %s %s", st, detail)
	}
	f.fake.Down("validate")
	f.fake.SetUAS(snA, "suspended", "", "")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatalf("the feed answered: %v", err)
	}
	if _, ok := f.store.row(uasA); ok || f.projected(uasA) {
		t.Fatal("a stale answer was kept when its re-fetch failed")
	}
	if id := f.resolve(); id.Status != core.IdentUnknownOperator || id.Reason != core.ReasonRegistryUnavailable {
		t.Fatalf("resolved %+v", id)
	}
	if f.count(CounterRefetchFailed) != 1 || f.count(CounterFeedRefetched) != 2 {
		t.Fatalf("failed %d refetched %d", f.count(CounterRefetchFailed), f.count(CounterFeedRefetched))
	}
	st, detail := probe(t.Context())
	if st != obs.StateDegraded || !strings.Contains(detail, "refetch_failed=1") {
		t.Fatalf("readiness: %s %s", st, detail)
	}
	// The other key of the flight was not named and stands.
	if _, ok := f.store.row(opA); !ok {
		t.Fatal("an unchanged key went")
	}
	f.fake.Up()
	f.fake.SetUAS(snA, "suspended", "", "") // the next change re-fetches it
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if id := f.resolve(); id.Status != core.IdentSuspended {
		t.Fatalf("after: %+v", id)
	}
	if st, detail := probe(t.Context()); st != obs.StateUp || strings.Contains(detail, "refetch_failed") {
		t.Fatalf("recovered: %s %s", st, detail)
	}
	// A bound-key set that cannot be read fails the poll, which moves
	// nothing (the rows stand, the cursor stays).
	bs.err = errors.New("database down")
	f.fake.SetUAS(snA, "revoked", "", "")
	before := cursor(t, f.store).Since
	if err := f.feed.Poll(t.Context()); err == nil {
		t.Fatal("an unreadable bound-key set polled")
	}
	if _, ok := f.store.row(uasA); !ok || cursor(t, f.store).Since != before {
		t.Fatal("a failed poll moved something")
	}
}

// Plan §5 item 7 (E-10): more bound keys than one poll re-fetches. The
// keys within the bound are asked in batches of MaxQueries; the rest
// are deleted and counted deferred. Without the bound every key is
// re-fetched.
func TestRefetchIsBoundedPerPoll(t *testing.T) {
	for _, tc := range []struct {
		max, refetched, deferred, batches int
	}{
		{0, 250, 0, 3},     // the default (1000): 100, 100, 50
		{120, 120, 130, 2}, // 100, 20
	} {
		t.Run(fmt.Sprint(tc.max), func(t *testing.T) {
			f := newFixture(t)
			var keys []Key
			var qs []Query
			for i := range 250 {
				sn := fmt.Sprintf("TEST-SN-%03d", i)
				f.fake.SetUAS(sn, "active", "", "")
				keys, qs = append(keys, Key{EntityUAS, sn}), append(qs, Query{Serial: sn})
			}
			for start := 0; start < len(qs); start += MaxQueries {
				if _, err := f.cache.Validate(t.Context(), qs[start:min(start+MaxQueries, len(qs))], PurposeIdentification); err != nil {
					t.Fatal(err)
				}
			}
			withRefetch(f, keys...)
			f.feed = NewFeed(FeedConfig{Client: f.feed.cfg.Client, Store: f.store, Projector: f.feed.cfg.Projector, Bound: f.feed.cfg.Bound,
				Cache: f.cache, MaxRefetchPerPoll: tc.max, Counters: f.cache.Counters(), Now: f.clock.Now})
			batches := f.fake.Requests("validate_batch")
			if err := f.feed.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := f.fake.Requests("validate_batch") - batches; got != tc.batches {
				t.Fatalf("%d batches, want %d", got, tc.batches)
			}
			if f.count(CounterFeedRefetched) != uint64(tc.refetched) || f.count(CounterRefetchDeferred) != uint64(tc.deferred) ||
				f.count(CounterFeedInvalidated) != 250 {
				t.Fatalf("refetched %d deferred %d invalidated %d", f.count(CounterFeedRefetched), f.count(CounterRefetchDeferred),
					f.count(CounterFeedInvalidated))
			}
			n := 0
			for _, k := range keys {
				if _, ok := f.store.row(k); ok {
					n++
				}
			}
			if n != tc.refetched {
				t.Fatalf("%d rows stand, want %d", n, tc.refetched)
			}
		})
	}
}

// Plan §5 item 9: two api instances apply the same page. The first
// re-fetches the bound key; the second finds it fetched under a cursor
// at the change and neither asks again nor deletes it.
func TestTwoInstancesRefetchOnce(t *testing.T) {
	f := newFixture(t)
	bs, rec := withRefetch(f, uasA)
	f.fake.SetUAS(snA, "active", "", "")
	f.uas(t, snA) // cursor 0
	second := NewFeed(FeedConfig{Client: f.feed.cfg.Client, Store: staleCursor{Store: f.store, since: 0}, Projector: rec, Bound: bs,
		Cache: f.cache, Counters: f.cache.Counters(), Now: f.clock.Now})
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := second.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e, ok := f.store.row(uasA); !ok || e.FetchedSince != 1 {
		t.Fatalf("row %+v", e)
	}
	if f.fake.Requests("validate") != 2 || f.count(CounterFeedRefetched) != 1 || f.count(CounterFeedRetained) != 1 {
		t.Fatalf("validate %d refetched %d retained %d", f.fake.Requests("validate"), f.count(CounterFeedRefetched), f.count(CounterFeedRetained))
	}
	if ops := rec.of(uasA); slices.Contains(ops, "delete") {
		t.Fatalf("the re-fetched answer was deleted: %v", ops)
	}
}

// KeysOf derives the keys exactly as a lookup caches them (the bound
// keys pgstore reads are matched against the cached rows and the
// changes by these): an operator by its compare key, never its secret
// part; a serial normalised; a pilot trimmed; an empty part has none.
func TestKeysOfIsTheLookupsKeys(t *testing.T) {
	got := KeysOf(Query{Operator: " " + opSecret + " ", Serial: " " + snA + " ", Pilot: " " + pilotA + " "})
	want := []Key{{EntityOperator, opNumber}, {EntityUAS, snA}, {EntityPilot, pilotA}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	f := newFixture(t)
	f.fake.SetOperator(opNumber, "active", nil)
	f.fake.SetUAS(snA, "active", "", "")
	f.fake.SetPilot(pilotA, "active")
	if _, err := f.cache.Validate(t.Context(), []Query{{Operator: opSecret, Serial: snA, Pilot: pilotA}}, PurposeIdentification); err != nil {
		t.Fatal(err)
	}
	for _, k := range got {
		if _, ok := f.store.row(k); !ok {
			t.Errorf("no cached row under %v", k)
		}
	}
	if ks := KeysOf(Query{Serial: "  "}); len(ks) != 0 {
		t.Fatalf("an empty query has keys %v", ks)
	}
}
