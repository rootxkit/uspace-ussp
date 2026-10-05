package dss

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/peeruss"
)

// recordedSub is the sub the token service puts in our tokens, which the
// DSS records as the manager of what we write; ourManager, the client id
// the configuration derives, may differ from it in case and format (a
// lab issuer listing ussp-dev01-01 for the code DEV01).
const recordedSub = "ussp-USSP-TEST-01"

// Our identity is the one the DSS records, not the configured client
// id: our own references go into the key without a peer read and are
// never stored as a peer's, a create whose answer was lost is adopted,
// the availability polled is ours, and a notification about our own
// intent is ignored. E-01: the same rig with equal names is every other
// writer and server test.
func TestOurIdentityIsTheOneTheDSSRecords(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.c.Tokens = &tokens{sub: recordedSub}
	if g.w.Manager != ourManager || g.srv.Manager != ourManager || ourManager == recordedSub {
		t.Fatal("the rig must configure a manager other than the recorded sub")
	}
	if m, err := g.c.Manager(ctx); err != nil || m != recordedSub {
		t.Fatalf("manager %q %v", m, err)
	}

	// Our first intent, then a second one meeting it: the first's ovn is
	// ours, from the DSS's answer, and no peer read is made.
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	if ref, _, ok := g.dss.OIR(oursID); !ok || ref.Manager != recordedSub {
		t.Fatalf("the DSS holds %+v %v", ref, ok)
	}
	g.in.put(pending(peer2ID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, peer2ID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, peer2ID, f3548.Accepted)
	if g.count(CounterPeerDetails) != 0 || g.dss.Conflicts() != 0 {
		t.Fatalf("our own intent read as a peer's: %v, %d conflicts", g.counters.Snapshot(), g.dss.Conflicts())
	}
	if p, _ := g.st.PeerIntent(ctx, oursID); p != nil {
		t.Fatalf("our own intent stored as a peer's: %+v", p)
	}

	// A create whose answer was lost is adopted.
	const lostID = "66666666-6666-4666-8666-666666666666"
	r := pending(lostID, volume(41.75, 44.85))
	g.in.put(r)
	yes := true
	if _, err := g.c.PutOperationalIntent(ctx, lostID, "", f3548.PutOperationalIntentReferenceParameters{Extents: r.Request.Volumes,
		State: f3548.Accepted, UssBaseUrl: g.us.URL, NewSubscription: &f3548.ImplicitSubscriptionParameters{UssBaseUrl: g.us.URL, NotifyForConstraints: &yes}}); err != nil {
		t.Fatal(err)
	}
	if err := g.w.Mirror(ctx, lostID); !errors.Is(err, errWaiting) || g.count(CounterOIRAdopted) != 1 {
		t.Fatalf("not adopted: %v %v", err, g.counters.Snapshot())
	}
	if err := g.w.Mirror(ctx, lostID); err != nil {
		t.Fatal(err)
	}
	if g.in.get(lostID).LocalState != intent.StateAccepted {
		t.Fatal("the adopted reference not authorised")
	}

	// The availability the authority set for us.
	g.dss.SetAvailability(recordedSub, f3548.Down)
	a := &Availability{Client: g.c, Store: g.st, USSID: ourManager}
	if err := a.Poll(ctx); err != nil || !a.Down() {
		t.Fatalf("availability of another id polled: %v down %v", err, a.Down())
	}

	// A notification naming us as the manager (from another base URL) is
	// about our own intent: ignored, nothing stored.
	if _, err := g.peer.File(ctx, peeruss.Spec{ID: peerID, Volumes: []f3548.Volume4D{volume(41.9, 44.9)}}); err != nil {
		t.Fatal(err)
	}
	oi, _ := g.peer.Intent(peerID)
	const echoID = "77777777-7777-4777-8777-777777777777"
	oi.Reference.Manager, oi.Reference.Id = recordedSub, echoID
	body := f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: echoID, OperationalIntent: &oi, Subscriptions: []f3548.SubscriptionState{}}
	if st, _ := g.call(http.MethodPost, "/uss/v1/operational_intents", recordedSub, body); st != http.StatusNoContent || g.count(CounterPeerOwnIgnored) != 1 {
		t.Fatalf("%d %v", st, g.counters.Snapshot())
	}
	if p, _ := g.st.PeerIntent(ctx, echoID); p != nil {
		t.Fatalf("our own intent stored as a peer's: %+v", p)
	}
}
