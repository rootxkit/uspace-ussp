package registry

import (
	"context"
	"testing"
	"time"
)

// staleCursor is a Store whose Cursor reports since as the
// feed's cursor: a second api instance that read the cursor before the
// first one moved it, so it applies a page the first already applied.
type staleCursor struct {
	Store
	since int64
}

func (s staleCursor) Cursor(ctx context.Context) (FeedCursor, error) {
	c, err := s.Store.Cursor(ctx)
	c.Since, c.ETag = s.since, ""
	return c, err
}

// Plan §5 item 1: a change the feed applied before the fetch leaves the
// answer standing when the next poll comes; nothing is asked again and
// nothing is invalidated.
func TestChangeBeforeTheFetchKeepsTheAnswer(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a := f.uas(t, snA); a.Status != StatusValid {
		t.Fatalf("first %+v", a)
	}
	if e, ok := f.store.row(Key{EntityUAS, snA}); !ok || e.FetchedSince != 1 {
		t.Fatalf("fetched under cursor 1: %+v", e)
	}
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); !ok || !f.projected(Key{EntityUAS, snA}) {
		t.Fatal("an answer fetched after its change was removed")
	}
	if f.fake.Requests("validate") != 1 || f.count(CounterFeedInvalidated) != 0 {
		t.Fatalf("validate %d, invalidated %d", f.fake.Requests("validate"), f.count(CounterFeedInvalidated))
	}
}

// Plan §5 item 9 (the guard): a second instance that applies the page
// naming the registration after the answer was fetched under it does
// not delete the answer, in the table or the projection; it is counted
// retained. A change after the fetch, applied the same way, still
// deletes it (E-01 pair).
func TestAPageAppliedTwiceKeepsTheAnswerFetchedAfterIt(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.uas(t, snA) // fetched under cursor 1
	second := NewFeed(FeedConfig{Client: f.feed.cfg.Client, Store: staleCursor{Store: f.store, since: 0}, Projector: f.proj,
		Counters: f.cache.Counters(), Now: f.clock.Now})
	if err := second.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); !ok || !f.projected(Key{EntityUAS, snA}) {
		t.Fatal("a page applied twice deleted the answer fetched after it")
	}
	if f.count(CounterFeedRetained) != 1 || f.count(CounterFeedInvalidated) != 0 {
		t.Fatalf("retained %d invalidated %d", f.count(CounterFeedRetained), f.count(CounterFeedInvalidated))
	}
	f.fake.SetUAS(snA, "suspended", "", "") // seq 2, after the fetch
	second.cfg.Store = staleCursor{Store: f.store, since: 1}
	if err := second.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); ok || f.projected(Key{EntityUAS, snA}) {
		t.Fatal("a change after the fetch left the answer standing")
	}
}

// Plan §5 item 6: the authority's clock decides nothing. With the
// changes' at an hour ahead of this USSP's clock, and an hour behind,
// the outcomes of items 1 and 3 are the same as with the clocks equal.
func TestClockSkewDoesNotDecideRetention(t *testing.T) {
	for _, skew := range []time.Duration{-time.Hour, 0, time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			// Item 1: the change before the fetch keeps the answer.
			f := newFixture(t)
			f.fake.SetClock(func() time.Time { return f.clock.Now().Add(skew) })
			f.fake.SetUAS(snA, "active", "", "")
			if err := f.feed.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.uas(t, snA)
			if err := f.feed.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, ok := f.store.row(Key{EntityUAS, snA}); !ok {
				t.Fatal("item 1: the answer was removed")
			}
			// Item 3: the change unseen at the fetch removes it.
			g := newFixture(t)
			g.fake.SetClock(func() time.Time { return g.clock.Now().Add(skew) })
			g.fake.SetUAS(snA, "active", "", "")
			g.uas(t, snA)
			if err := g.feed.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, ok := g.store.row(Key{EntityUAS, snA}); ok || g.projected(Key{EntityUAS, snA}) {
				t.Fatal("item 3: the answer survived a change it did not see")
			}
			if at := g.fake.Changes()[0].At; !at.Equal(g.clock.Now().Add(skew)) {
				t.Fatalf("the fake's at is %v, not skewed by %v", at, skew)
			}
		})
	}
}
