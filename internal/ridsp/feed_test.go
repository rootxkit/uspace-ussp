package ridsp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// fakeSource fails its first fails opens, then delivers msgs from the
// moment asked.
type fakeSource struct {
	mu      sync.Mutex
	fails   int
	opens   int
	from    time.Time
	msgs    [][]byte
	stopped bool
}

func (s *fakeSource) Open(_ context.Context, from time.Time, handle func([]byte)) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	if s.opens <= s.fails {
		return nil, errors.New("stream TRK not found")
	}
	s.from = from
	for _, m := range s.msgs {
		handle(m)
	}
	return func() { s.mu.Lock(); s.stopped = true; s.mu.Unlock() }, nil
}

// E-02 both ways: a source that does not open is retried and /readyz
// says trk down since T; once it opens the window fills from a minute
// back and trk is up; the source is stopped with the context.
func TestTrackFeed(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	src := &fakeSource{fails: 2, msgs: [][]byte{trackMsg(t, body(1, origin), t0)}}
	f := &TrackFeed{Source: src, Window: w, Retry: 5 * time.Millisecond, Now: c.now}
	if st, _ := f.Probe()(context.Background()); st != obs.StateUnknown {
		t.Fatalf("before: %s", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for w.Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the window was not fed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st, _ := f.Probe()(context.Background()); st != obs.StateUp {
		t.Fatalf("open: %s", st)
	}
	src.mu.Lock()
	if src.opens != 3 || !src.from.Equal(t0.Add(-Horizon)) {
		t.Errorf("opens %d from %v", src.opens, src.from)
	}
	src.mu.Unlock()
	cancel()
	<-done
	if !src.stopped {
		t.Error("the source was not stopped")
	}

	down := &TrackFeed{Source: &fakeSource{fails: 1 << 30}, Window: w, Retry: time.Millisecond, Now: c.now}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	down.Run(ctx)
	if st, d := down.Probe()(context.Background()); st != obs.StateDown || !strings.Contains(d, "down since") || !strings.Contains(d, "TRK not found") {
		t.Fatalf("down: %s %q", st, d)
	}
}

type fakeKV struct {
	m   map[string][]byte
	err error
}

func (k *fakeKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	if k.err != nil {
		return nil, false, k.err
	}
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *fakeKV) Put(_ context.Context, key string, v []byte) error {
	if k.err != nil {
		return k.err
	}
	k.m[key] = v
	return nil
}

// The KV store round-trips a notification, says false for one it does
// not hold, and returns the bucket's errors (a value that does not read,
// a failing read or write).
func TestKVNotifications(t *testing.T) {
	kv := &fakeKV{m: map[string][]byte{}}
	s := KVNotifications{KV: kv}
	ctx := context.Background()
	if _, found, err := s.Get(ctx, isaID); found || err != nil {
		t.Fatalf("empty: %v %v", found, err)
	}
	n := ISANotification{ISAID: isaID, Sender: "peer", ReceivedAt: t0, Subscriptions: notification("v1").Subscriptions}
	if err := s.Put(ctx, n); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Get(ctx, isaID)
	if err != nil || !found || got.Sender != "peer" || !got.ReceivedAt.Equal(t0) {
		t.Fatalf("%+v %v %v", got, found, err)
	}
	kv.m[isaID] = []byte("{")
	if _, _, err := s.Get(ctx, isaID); err == nil {
		t.Error("a value that does not read")
	}
	kv.err = errors.New("timeout")
	if _, _, err := s.Get(ctx, isaID); err == nil {
		t.Error("a failing read")
	}
	if err := s.Put(ctx, n); err == nil {
		t.Error("a failing write")
	}
}
