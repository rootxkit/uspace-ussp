package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

func cursor(t *testing.T, st Store) FeedCursor {
	t.Helper()
	c, err := st.Cursor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A poll applies at most MaxPages pages (E-10: the bound is exceeded and
// the rest waits for the next poll), then a poll with nothing new is a
// 304 that still records the answer.
func TestFeedPagesAreBoundedPerPoll(t *testing.T) {
	f := newFixture(t)
	for i := range 5 {
		f.fake.SetUAS(fmt.Sprintf("TEST-%d", i), "active", "", "")
	}
	f.feed.cfg.PageLimit, f.feed.cfg.MaxPages = 2, 2
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := cursor(t, f.store); c.Since != 4 || f.fake.Requests("changes") != 2 {
		t.Fatalf("after one poll: cursor %d, %d requests", c.Since, f.fake.Requests("changes"))
	}
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := cursor(t, f.store); c.Since != 5 {
		t.Fatalf("after two polls: cursor %d", c.Since)
	}
	f.clock.Advance(30 * time.Second)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Second)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := cursor(t, f.store)
	if c.Since != 5 || f.count(CounterFeedNotModified) != 1 || c.AgeS == nil || *c.AgeS != 0 {
		t.Fatalf("304: cursor %+v, not modified %d", c, f.count(CounterFeedNotModified))
	}
}

// A serial looked up in another case is invalidated by a change to the
// serial as registered (the fold key); an operator by its compare key.
func TestFeedInvalidatesByFoldKey(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	f.fake.SetOperator(opNumber, "active", nil)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cache.Validate(t.Context(), []Query{{Serial: "test-sn-a", Operator: "geotestop0001"}, {Serial: snA}}, PurposeIdentification); err != nil {
		t.Fatal(err)
	}
	f.fake.SetUAS(snA, "revoked", "", "")
	f.fake.SetOperator(opNumber, "suspended", nil)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, k := range []Key{{EntityUAS, "test-sn-a"}, {EntityUAS, snA}, {EntityOperator, opNumber}} {
		if _, ok := f.store.row(k); ok {
			t.Errorf("%v survived its change", k)
		}
	}
	if f.count(CounterFeedInvalidated) != 3 {
		t.Fatalf("invalidated %d", f.count(CounterFeedInvalidated))
	}
}

// A change to another key leaves an answer alone (E-01 pair of the
// invalidation).
func TestFeedLeavesOtherKeysAlone(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	if err := f.feed.Poll(t.Context()); err != nil { // its own registration
		t.Fatal(err)
	}
	f.uas(t, snA)
	f.fake.SetUAS("TEST-SN-B", "active", "", "")
	f.fake.SetPilot(pilotA, "active")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); !ok {
		t.Fatal("an unchanged key was invalidated")
	}
}

// A refused page, a store or projection that fails, or a down authority
// leaves the cursor and the rows where they were, counted.
func TestFeedFailuresMoveNothing(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.uas(t, snA)
	f.fake.SetUAS(snA, "suspended", "", "")

	f.proj.Fail = errors.New("bucket down")
	if err := f.feed.Poll(t.Context()); err == nil {
		t.Fatal("a refused projection was accepted")
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); !ok || cursor(t, f.store).Since != 1 {
		t.Fatal("the invalidation was applied without its projection")
	}
	f.proj.Fail = nil

	f.store.failRead = errDown
	if err := f.feed.Poll(t.Context()); err == nil {
		t.Fatal("an unreadable cursor polled")
	}
	f.store.failRead = nil

	f.fake.Down()
	if err := f.feed.Poll(t.Context()); err == nil {
		t.Fatal("a down authority polled")
	}
	f.fake.Up()
	if f.count(CounterFeedFailed) != 3 || cursor(t, f.store).Since != 1 {
		t.Fatalf("failed %d, cursor %d", f.count(CounterFeedFailed), cursor(t, f.store).Since)
	}
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); ok || cursor(t, f.store).Since != 2 {
		t.Fatal("the change was not applied once everything was back")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"changes":[],"next_since":0}`))
	}))
	defer srv.Close()
	client, _ := NewClient(ClientConfig{BaseURL: srv.URL, Tokens: authority.Tokens{}})
	bad := NewFeed(FeedConfig{Client: client, Store: f.store, Counters: f.cache.Counters()})
	if err := bad.Poll(t.Context()); err == nil || f.count(CounterFeedRefused) != 1 {
		t.Fatalf("a page going backwards: %v, refused %d", err, f.count(CounterFeedRefused))
	}
	if cursor(t, f.store).Since != 2 {
		t.Fatal("the cursor moved backwards")
	}
}

func TestFeedWithoutAuthorityOrStore(t *testing.T) {
	if err := NewFeed(FeedConfig{}).Poll(t.Context()); err == nil {
		t.Fatal("no authority polled")
	}
	client, _ := NewClient(ClientConfig{BaseURL: "https://authority.test", Tokens: authority.Tokens{}})
	if err := NewFeed(FeedConfig{Client: client}).Poll(t.Context()); err == nil {
		t.Fatal("no store polled")
	}
}

// Run polls at once, then every period, and stops with its context.
func TestFeedRunPollsAndStops(t *testing.T) {
	fake := authority.New()
	defer fake.Close()
	client, _ := NewClient(ClientConfig{BaseURL: fake.URL(), Tokens: authority.Tokens{}})
	feed := NewFeed(FeedConfig{Client: client, Store: newMemStore(newClock()), Interval: 10 * time.Millisecond, Counters: &core.Counters{}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { feed.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for fake.Requests("changes") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if fake.Requests("changes") < 2 {
		t.Fatal("Run did not poll twice")
	}
	fake.Down()
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	feed.Run(ctx) // one failed poll, logged, then the cancelled context
}

// projected says whether the projection holds k.
func (f *fixture) projected(k Key) bool {
	es, _ := f.proj.Snapshot()
	for i := range es {
		if es[i].Key == k {
			return true
		}
	}
	return false
}

// A projected answer the table lacks (its put reached the KV and the
// commit failed after it) is removed when the feed invalidates its fold
// key: the hot path never keeps resolving a suspended registration from
// it until the bucket TTL (audit S1). A projected answer of another key
// stays (E-01 pair).
func TestFeedDeletesAProjectedAnswerWithoutARow(t *testing.T) {
	f := newFixture(t)
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	now := f.clock.Now()
	orphan := Entry{Key: Key{EntityUAS, "test-sn-a"}, KeyFold: entryKeyFold(EntityUAS, "test-sn-a"), Status: StatusValid, FetchedAt: now}
	other := Entry{Key: Key{EntityUAS, "TEST-SN-B"}, KeyFold: entryKeyFold(EntityUAS, "TEST-SN-B"), Status: StatusValid, FetchedAt: now}
	if err := f.proj.ProjectRegistry(t.Context(), []Entry{orphan, other}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.row(orphan.Key); ok {
		t.Fatal("the fixture has a row for the orphan")
	}
	f.fake.SetUAS(snA, "revoked", "", "")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.projected(orphan.Key) {
		t.Fatal("a projected answer without a row outlived its invalidation")
	}
	if !f.projected(other.Key) {
		t.Fatal("a projected answer of another key was removed")
	}
}
