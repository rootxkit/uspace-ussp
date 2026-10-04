//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/peers"
)

// The peer Display Provider's subscriptions on the real bucket
// (rid_dp_subscriptions, WP-14 review): a run saves the subscription of
// each area; the next start, with one area dropped, reads them back and
// deletes the dropped one at the DSS and in the bucket, keeping the
// other. The bucket's bounds (E-10): a value over RIDSubscriptionBytes
// is refused, and at most MaxSavedSubscriptions are read back.
func TestIntegrationPeerSubscriptionDroppedAcrossARestartIsDeleted(t *testing.T) {
	ctx := context.Background()
	nc := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	js := nc.JetStream()
	if _, err := bus.Ensure(ctx, js, bus.DefaultTopology()); err != nil {
		t.Fatal(err)
	}
	store := peers.KVSubscriptions{KV: bus.KVStore{JS: js, Bucket: bus.BucketRIDSubscriptions}}
	dss := fakedss.New()
	t.Cleanup(dss.Close)
	us := "https://ussp-" + unique() + ".test"
	a1 := peers.Area{ID: "TSA-1", Box: boxAround(41.70, 44.80)}
	a2 := peers.Area{ID: "TSA-2", Box: boxAround(41.80, 44.90)}
	run := func(areas ...peers.Area) *peers.DP {
		d := &peers.DP{DSSBaseURL: dss.URL(), USSBaseURL: us, Tokens: staticToken{}, Subscriptions: store,
			Areas: func() ([]peers.Area, bool) { return areas, true }}
		d.Discover(ctx)
		return d
	}
	run(a1, a2)
	kept, dropped := peers.SubscriptionID(us, a1.ID), peers.SubscriptionID(us, a2.ID)
	t.Cleanup(func() { _ = store.Delete(context.Background(), kept); _ = store.Delete(context.Background(), dropped) })
	saved, err := store.Load(ctx)
	if err != nil || saved[kept].AreaID != a1.ID || saved[dropped].AreaID != a2.ID || saved[dropped].Version == "" || saved[dropped].USSBaseURL != us {
		t.Fatalf("saved %v %v", saved, err)
	}
	d := run(a1)
	subs := dss.Subscriptions()
	if _, ok := subs[dropped]; ok || len(subs) != 1 || d.Counters.Get(peers.CounterSubscriptionDel) != 1 {
		t.Fatalf("after the restart: DSS %v, counters %v", subs, d.Counters.Snapshot())
	}
	if saved, err = store.Load(ctx); err != nil || saved[kept].AreaID != a1.ID {
		t.Fatalf("saved after the restart %v %v", saved, err)
	}
	if _, ok := saved[dropped]; ok {
		t.Fatalf("saved after the restart %v %v", saved, err)
	}
	if err := store.Put(ctx, dropped, peers.SavedSubscription{AreaID: strings.Repeat("x", bus.RIDSubscriptionBytes), Version: "v"}); err == nil {
		t.Fatal("a subscription over its bound was saved")
	}
	for i := 0; i <= peers.MaxSavedSubscriptions; i++ {
		id := fmt.Sprintf("bound-%s-%04d", unique(), i)
		if err := store.Put(ctx, id, peers.SavedSubscription{AreaID: "A", Version: "v", End: time.Now()}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
	}
	if saved, err = store.Load(ctx); err != nil || len(saved) != peers.MaxSavedSubscriptions {
		t.Fatalf("read back %d (%v), the bound is %d", len(saved), err, peers.MaxSavedSubscriptions)
	}
}

// boxAround is a small box around lat, lon.
func boxAround(lat, lon float64) geodesy.BBox {
	return geodesy.BBox{MinLat: lat - 0.01, MinLon: lon - 0.01, MaxLat: lat + 0.01, MaxLon: lon + 0.01}
}

// staticToken hands out one token for every audience (the fake DSS
// does not check it).
type staticToken struct{}

func (staticToken) Token(context.Context, string, ...string) (string, error) { return "tok", nil }
