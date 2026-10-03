package dss

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/peeruss"
)

const (
	oursID  = "11111111-1111-4111-8111-111111111111"
	peerID  = "22222222-2222-4222-8222-222222222222"
	peer2ID = "33333333-3333-4333-8333-333333333333"
)

// The intent write protocol end to end against the fake DSS and a peer
// that filed first in the same airspace: our write reads the peer's
// details from its manager (stored as peer_intents, trust provider),
// carries its ovn in the key, is authorised only once the DSS took it,
// and the peer (a subscriber) is told within 5 s with our reference and
// details.
func TestWriterWritesAuthorisesAndNotifies(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	if _, err := g.peer.File(ctx, peeruss.Spec{ID: peerID, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}}); err != nil {
		t.Fatal(err)
	}
	g.in.put(pending(oursID, volume(41.7005, 44.8005)))
	start := time.Now()
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, f3548.Accepted)
	if r := g.in.get(oursID); r.LocalState != intent.StateAccepted {
		t.Fatalf("not authorised after the DSS write: %s", r.LocalState)
	}
	ref, _, ok := g.dss.OIR(oursID)
	if !ok || ref.Manager != ourManager || ref.State != f3548.Accepted || ref.UssBaseUrl != g.us.URL {
		t.Fatalf("the DSS holds %+v %v", ref, ok)
	}
	if p, _ := g.st.PeerIntent(ctx, peerID); p == nil || p.Manager != peerManager || p.OVN == "" {
		t.Fatalf("peer intent not stored: %+v", p)
	}
	if g.count(CounterPeerDetails) != 1 || g.count(CounterOIRCreated) != 1 {
		t.Fatalf("counters %v", g.counters.Snapshot())
	}
	if n := g.st.pending(store.OutboxPeerNotify); len(n) != 1 {
		t.Fatalf("notifications queued %d", len(n))
	}
	if _, err := g.w.NotifyOnce(ctx); err != nil {
		t.Fatal(err)
	}
	notes := g.peer.Notifications()
	if len(notes) != 1 {
		t.Fatalf("the peer got %d notifications", len(notes))
	}
	n := notes[0]
	if n.Body.OperationalIntentId != oursID || n.Body.OperationalIntent == nil || n.Body.OperationalIntent.Reference.Ovn == nil ||
		*n.Body.OperationalIntent.Reference.Ovn != *ref.Ovn || len(*n.Body.OperationalIntent.Details.Volumes) != 1 || len(n.Body.Subscriptions) == 0 {
		t.Fatalf("notification %+v", n.Body)
	}
	if lat := n.At.Sub(start); lat > time.Duration(f3548.UssOiChangeNotificationMaxSeconds)*time.Second {
		t.Fatalf("the subscriber was told after %s", lat)
	}
	if g.count(CounterNotified) != 1 || g.count(CounterNotifyLate) != 0 {
		t.Fatalf("counters %v", g.counters.Snapshot())
	}
	// A second mirror finds nothing to do (E-01 pair: the write above).
	calls := len(g.dss.Calls())
	if err := g.w.Mirror(ctx, oursID); err != nil || len(g.dss.Calls()) != calls {
		t.Fatalf("a mirrored intent written again: %v, %d calls", err, len(g.dss.Calls())-calls)
	}
}

// A conflict PeerCheck finds refuses the intent: nothing is written to
// the DSS (the pair of the write above).
func TestWriterWritesNothingForARejectedIntent(t *testing.T) {
	g := newRig(t)
	g.in.check = func(*intent.Record) intent.PeerCheckResult {
		return intent.PeerCheckResult{Outcome: intent.PeerCheckRejected}
	}
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(context.Background(), oursID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := g.dss.OIR(oursID); ok {
		t.Fatal("a rejected intent was written to the DSS")
	}
	if r := g.in.get(oursID); r.LocalState != intent.StateRejected {
		t.Fatalf("state %s", r.LocalState)
	}
}

// DSS down: the intent waits pending_dss with dss_unavailable and the
// item is retried; DSS back: it is written and authorised (E-01 both).
func TestWriterHoldsWhileTheDSSIsDownAndWritesWhenBack(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	g.dss.Down(true)
	err := g.w.Mirror(ctx, oursID)
	if !errors.Is(err, errWaiting) || g.in.lastHold() != intent.ReasonDSSUnavailable {
		t.Fatalf("down: %v, hold %q", err, g.in.lastHold())
	}
	if r := g.c.Reach(); !r.Known || r.Up {
		t.Fatalf("reach %+v", r)
	}
	if ok, why := (&Gate{Client: g.c}).Available(ctx); ok || !strings.Contains(why, "does not answer") {
		t.Fatalf("gate %v %q", ok, why)
	}
	g.dss.Down(false)
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, f3548.Accepted)
	if ok, why := (&Gate{Client: g.c}).Available(ctx); !ok {
		t.Fatalf("gate after the DSS is back: %q", why)
	}
}

// The authority holds this USSP Down: no new write, the intent waits
// with uss_availability_down; Normal: written (E-01 both).
func TestWriterHoldsWhileOurAvailabilityIsDown(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	a := &Availability{Client: g.c, Store: g.st, USSID: ourManager, Counters: g.counters}
	g.w.Availability = a
	g.dss.SetAvailability(ourManager, f3548.Down)
	if err := a.Poll(ctx); err != nil || !a.Down() {
		t.Fatalf("poll %v down %v", err, a.Down())
	}
	if ok, why := (&Gate{Client: g.c, Availability: a}).Available(ctx); ok || !strings.HasPrefix(why, intent.ReasonUSSAvailabilityDown) {
		t.Fatalf("gate %v %q", ok, why)
	}
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errWaiting) || g.in.lastHold() != intent.ReasonUSSAvailabilityDown {
		t.Fatalf("%v %q", err, g.in.lastHold())
	}
	if _, _, ok := g.dss.OIR(oursID); ok {
		t.Fatal("written while Down")
	}
	g.dss.SetAvailability(ourManager, f3548.Normal)
	if err := a.Poll(ctx); err != nil || a.Down() {
		t.Fatalf("poll %v", err)
	}
	if st, _ := g.st.State(ctx); st.Availability != string(f3548.Normal) {
		t.Fatalf("dss_state %+v", st)
	}
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, f3548.Accepted)
}

// A peer that files between our survey and our write: the DSS answers
// 409 naming it, its details are fetched, the intent judged again and
// written once more with its ovn.
func TestWriterRefetchesWhatA409Names(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	filed := false
	g.in.check = func(*intent.Record) intent.PeerCheckResult {
		if !filed {
			filed = true
			if _, err := g.peer.File(ctx, peeruss.Spec{ID: peerID, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}}); err != nil {
				t.Error(err)
			}
		}
		return intent.PeerCheckResult{Outcome: intent.PeerCheckOK}
	}
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, f3548.Accepted)
	if g.count(CounterKeyConflict) != 1 || g.count(CounterKeyConflictResolved) != 1 || g.dss.Conflicts() != 1 {
		t.Fatalf("counters %v, 409s %d", g.counters.Snapshot(), g.dss.Conflicts())
	}
}

// A 409 again after the refetch: the intent waits with dss_key_conflict.
func TestWriterHoldsAfterTwo409s(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	n := 0
	g.in.check = func(*intent.Record) intent.PeerCheckResult {
		n++
		id := []string{peerID, peer2ID}[min(n-1, 1)]
		if n <= 2 {
			if _, err := g.peer.File(ctx, peeruss.Spec{ID: id, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}}); err != nil {
				t.Error(err)
			}
		}
		return intent.PeerCheckResult{Outcome: intent.PeerCheckOK}
	}
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errWaiting) || g.in.lastHold() != intent.ReasonDSSKeyConflict {
		t.Fatalf("%v %q", err, g.in.lastHold())
	}
	if g.in.get(oursID).LocalState != intent.StatePendingDSS {
		t.Fatal("authorised without a DSS write")
	}
}

// A peer whose manager does not answer and whose intent is not held:
// the intent waits with peer_intent_unavailable, the peer's intents are
// marked; with its stored copy of the version the DSS names, the copy's
// ovn is used and the write made (E-01 both).
func TestWriterPeerDown(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	ref, err := g.peer.File(ctx, peeruss.Spec{ID: peerID, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}})
	if err != nil {
		t.Fatal(err)
	}
	g.peer.Down(true)
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errWaiting) || g.in.lastHold() != intent.ReasonPeerUnavailable {
		t.Fatalf("%v %q", err, g.in.lastHold())
	}
	oi, _ := g.peer.Intent(peerID)
	rec, err := peerRecordOf(&oi, g.peer.URL())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.UpsertPeerIntent(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, f3548.Accepted)
	p, _ := g.st.PeerIntent(ctx, peerID)
	if !p.PeerUnavailable || p.Version != int64(ref.Version) || g.count(CounterPeerStoredUsed) != 1 || g.count(CounterPeerUnavailable) != 1 {
		t.Fatalf("peer %+v counters %v", p, g.counters.Snapshot())
	}
	// The peer answers again: the mark is cleared at the next read.
	g.peer.Down(false)
	if _, ok := g.w.peerOVN(ctx, f3548.OperationalIntentReference{Id: peerID, Manager: peerManager, UssBaseUrl: g.peer.URL(), Version: ref.Version}, nil); !ok {
		t.Fatal("peer read refused")
	}
	if p, _ := g.st.PeerIntent(ctx, peerID); p.PeerUnavailable {
		t.Fatal("peer_unavailable kept after an answer")
	}
}

// Activation, nonconformance, contingency and the end are mirrored: the
// DSS state follows the intent's, and the end deletes the reference and
// tells the subscribers with no operational intent.
func TestWriterMirrorsEveryState(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	if _, err := g.peer.File(ctx, peeruss.Spec{ID: peerID, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}}); err != nil {
		t.Fatal(err)
	}
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		local string
		want  f3548.OperationalIntentState
	}{
		{intent.StateActivated, f3548.Activated},
		{intent.StateNonconforming, f3548.Nonconforming},
		{intent.StateActivated, f3548.Activated},
		{intent.StateContingent, f3548.Contingent},
	} {
		w := string(s.want)
		g.in.setState(oursID, s.local, &w)
		if err := g.w.Mirror(ctx, oursID); err != nil {
			t.Fatalf("%s: %v", s.local, err)
		}
		mustState(t, g.in, oursID, s.want)
		if ref, _, _ := g.dss.OIR(oursID); ref.State != s.want {
			t.Fatalf("the DSS holds %s, want %s", ref.State, s.want)
		}
	}
	h := g.in.heldOf(oursID)
	d := Details(g.in.get(oursID), h)
	if d.Details.OffNominalVolumes == nil || len(*d.Details.OffNominalVolumes) != 1 || len(*d.Details.Volumes) != 0 {
		t.Fatalf("contingent details %+v", d.Details)
	}
	g.in.setState(oursID, intent.StateEnded, nil)
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, "")
	if _, _, ok := g.dss.OIR(oursID); ok {
		t.Fatal("ended intent still in the DSS")
	}
	if g.count(CounterOIRDeleted) != 1 || g.count(CounterOIRUpdated) != 4 {
		t.Fatalf("counters %v", g.counters.Snapshot())
	}
	var last PeerNotify
	notes := g.st.pending(store.OutboxPeerNotify)
	if len(notes) != 6 {
		t.Fatalf("%d notifications queued, want one a write", len(notes))
	}
	_ = json.Unmarshal(notes[len(notes)-1].Payload, &last)
	if last.Body.OperationalIntentId != oursID || last.Body.OperationalIntent != nil {
		t.Fatalf("delete notification %+v", last.Body)
	}
}

// A reference the DSS no longer holds: an end records it gone (404); an
// activated intent whose reference was lost is written again, Accepted
// first, then Activated.
func TestWriterRecoversLostReferences(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	h := g.in.heldOf(oursID)
	if _, err := g.c.DeleteOperationalIntent(ctx, oursID, h.OVN); err != nil {
		t.Fatal(err)
	}
	a := string(f3548.Activated)
	g.in.setState(oursID, intent.StateActivated, &a)
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, f3548.Activated)
	if ref, _, ok := g.dss.OIR(oursID); !ok || ref.State != f3548.Activated {
		t.Fatalf("not written again: %+v", ref)
	}
	h = g.in.heldOf(oursID)
	if _, err := g.c.DeleteOperationalIntent(ctx, oursID, h.OVN); err != nil {
		t.Fatal(err)
	}
	g.in.setState(oursID, intent.StateEnded, nil)
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, "")
}

// A create whose answer was lost: the DSS holds the reference and
// refuses a second create; the writer reads it, adopts its ovn and
// updates it, and only then authorises.
func TestWriterAdoptsACreateWhoseAnswerWasLost(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	r := pending(oursID, volume(41.7, 44.8))
	g.in.put(r)
	yes := true
	if _, err := g.c.PutOperationalIntent(ctx, oursID, "", f3548.PutOperationalIntentReferenceParameters{Extents: r.Request.Volumes,
		State: f3548.Accepted, UssBaseUrl: g.us.URL, NewSubscription: &f3548.ImplicitSubscriptionParameters{UssBaseUrl: g.us.URL, NotifyForConstraints: &yes}}); err != nil {
		t.Fatal(err)
	}
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errWaiting) || g.count(CounterOIRAdopted) != 1 {
		t.Fatalf("%v %v", err, g.counters.Snapshot())
	}
	if g.in.get(oursID).LocalState != intent.StatePendingDSS {
		t.Fatal("authorised on an adopted reference before writing it")
	}
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, f3548.Accepted)
	if g.in.get(oursID).LocalState != intent.StateAccepted {
		t.Fatal("not authorised")
	}
}

// A peer this intent displaces (a higher priority) is told inline within
// 900 ms, measured, and its outbox item is done; a peer too slow for it
// is counted peer_notify_late and told by the outbox.
func TestWriterTellsADisplacedPeerWithinOneSecond(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "in time", true: "late"}[slow], func(t *testing.T) {
			g := newRig(t)
			ctx := context.Background()
			if _, err := g.peer.File(ctx, peeruss.Spec{ID: peerID, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}}); err != nil {
				t.Fatal(err)
			}
			g.in.check = func(*intent.Record) intent.PeerCheckResult {
				return intent.PeerCheckResult{Outcome: intent.PeerCheckOK, Displaced: []string{peerID}}
			}
			r := pending(oursID, volume(41.7, 44.8))
			r.Priority = 100
			g.in.put(r)
			start := time.Now()
			if slow {
				g.peer.Slow(1500 * time.Millisecond)
			}
			if err := g.w.Mirror(ctx, oursID); err != nil {
				t.Fatal(err)
			}
			if len(g.st.auditsOf("dss_peer_displaced")) != 1 || g.count(CounterDisplaced) != 1 {
				t.Fatalf("not audited: %v", g.counters.Snapshot())
			}
			if slow {
				if g.count(CounterNotifyLate) != 1 || len(g.st.pending(store.OutboxPeerNotify)) != 1 {
					t.Fatalf("late: %v, %d queued", g.counters.Snapshot(), len(g.st.pending(store.OutboxPeerNotify)))
				}
				return
			}
			notes := g.peer.Notifications()
			if len(notes) != 1 {
				t.Fatalf("the displaced peer got %d notifications", len(notes))
			}
			lat := notes[0].At.Sub(start)
			t.Logf("displaced peer told %s after the write began", lat)
			if lat > time.Duration(f3548.ConflictingOIMaxUSSNotificationTimeSeconds)*time.Second {
				t.Fatalf("told after %s", lat)
			}
			if g.count(CounterDisplacedInline) != 1 || len(g.st.pending(store.OutboxPeerNotify)) != 0 {
				t.Fatalf("inline: %v, %d queued", g.counters.Snapshot(), len(g.st.pending(store.OutboxPeerNotify)))
			}
		})
	}
}

// A subscriber that does not take a notification: retried from the
// outbox, and after MaxNotifyAttempts dropped and counted; one that
// takes it late is counted peer_notify_late.
func TestNotifyRetriesDropsAndCountsLate(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.peer.Down(true)
	n := PeerNotify{IntentID: oursID, URL: g.peer.URL(), QueuedAt: time.Now(),
		Body: f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: oursID, Subscriptions: []f3548.SubscriptionState{{SubscriptionId: peerID}}}}
	g.st.enqueue(store.OutboxPeerNotify, oursID+"/a", 1, n)
	for i := range MaxNotifyAttempts {
		if _, err := g.w.NotifyOnce(ctx); err != nil {
			t.Fatal(err)
		}
		g.st.advance(2 * notifyRetry)
		if i < MaxNotifyAttempts-1 && len(g.st.pending(store.OutboxPeerNotify)) != 1 {
			t.Fatalf("attempt %d: dropped early", i+1)
		}
	}
	if len(g.st.pending(store.OutboxPeerNotify)) != 0 || g.count(CounterNotifyDropped) != 1 || g.count(CounterNotifyRetried) != MaxNotifyAttempts-1 {
		t.Fatalf("%v", g.counters.Snapshot())
	}
	g.peer.Down(false)
	n.QueuedAt = time.Now().Add(-10 * time.Second)
	g.st.enqueue(store.OutboxPeerNotify, oursID+"/b", 1, n)
	if _, err := g.w.NotifyOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if g.count(CounterNotified) != 1 || g.count(CounterNotifyLate) != 1 {
		t.Fatalf("%v", g.counters.Snapshot())
	}
	// An unreadable item is dropped at once.
	g.st.mu.Lock()
	g.st.seq++
	g.st.outbox = append(g.st.outbox, store.OutboxItem{ID: g.st.seq, Kind: store.OutboxPeerNotify, EntityID: "x", Payload: []byte("{"), NextAt: g.st.now})
	g.st.mu.Unlock()
	if _, err := g.w.NotifyOnce(ctx); err != nil || len(g.st.pending(store.OutboxPeerNotify)) != 0 || g.count(CounterNotifyDropped) != 2 {
		t.Fatalf("%v %v", err, g.counters.Snapshot())
	}
}

// The outbox loop: an item whose intent mirrors is done; one that waits
// is retried with a backoff; an unreadable one is dropped and counted.
func TestWriterOnce(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	g.st.enqueue(store.OutboxOIRPut, oursID, 1, intent.DSSWorkload{IntentID: oursID, Version: 1})
	g.st.enqueue(store.OutboxOIRPut, "bad", 1, "not a workload")
	g.dss.Down(true)
	if n, err := g.w.Once(ctx); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if p := g.st.pending(store.OutboxOIRPut); len(p) != 1 || p[0].LastError == nil || g.count(CounterOutboxUnreadable) != 1 {
		t.Fatalf("%+v %v", p, g.counters.Snapshot())
	}
	g.dss.Down(false)
	g.st.advance(time.Minute)
	if _, err := g.w.Once(ctx); err != nil || len(g.st.pending(store.OutboxOIRPut)) != 0 {
		t.Fatalf("%v %d", err, len(g.st.pending(store.OutboxOIRPut)))
	}
	mustState(t, g.in, oursID, f3548.Accepted)
	g.st.failClaim = errBoom
	if _, err := g.w.Once(ctx); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if _, err := g.w.NotifyOnce(ctx); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
}

// Run works the outbox and the notifications until its context ends.
func TestWriterRun(t *testing.T) {
	g := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := g.peer.File(ctx, peeruss.Spec{ID: peerID, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}}); err != nil {
		t.Fatal(err)
	}
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	g.st.enqueue(store.OutboxOIRPut, oursID, 1, intent.DSSWorkload{IntentID: oursID, Version: 1})
	done := make(chan struct{})
	go func() { g.w.Run(ctx); close(done) }()
	within(t, 5*time.Second, func() bool { return len(g.peer.Notifications()) == 1 })
	cancel()
	<-done
	if g.w.WriteNow(context.Background(), oursID) != nil {
		t.Fatal("WriteNow of a mirrored intent")
	}
}

// The backoff doubles to its cap.
func TestBackoff(t *testing.T) {
	w := &Writer{MaxBackoff: 4 * time.Second}
	for _, c := range []struct {
		n    int32
		want time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {9, 4 * time.Second}} {
		if got := w.backoff(c.n); got != c.want {
			t.Errorf("%d: %s", c.n, got)
		}
	}
	if (&Writer{}).backoff(30) != DefaultMaxBackoff {
		t.Error("default cap")
	}
}

// The area of interest holds every extent; extents it cannot bound are
// refused, never an empty area.
func TestAreaOfInterest(t *testing.T) {
	a, b := volume(41.7, 44.8), volume(41.8, 44.9)
	aoi, err := areaOfInterest([]f3548.Volume4D{a, b})
	if err != nil {
		t.Fatal(err)
	}
	vs := aoi.Volume.OutlinePolygon.Vertices
	if len(vs) != 4 || vs[0].Lat > 41.7 || vs[2].Lat < 41.8 || aoi.Volume.AltitudeLower.Value != 100 || aoi.Volume.AltitudeUpper.Value != 200 {
		t.Fatalf("%+v", aoi)
	}
	if _, err := areaOfInterest(nil); err == nil {
		t.Fatal("no extents")
	}
	c := a
	c.TimeEnd = nil
	if _, err := areaOfInterest([]f3548.Volume4D{c}); err == nil {
		t.Fatal("an extent without its end")
	}
	d := a
	bad := f3548.Altitude{Reference: "SFC", Units: f3548.AltitudeUnitsM}
	d.Volume.AltitudeLower = &bad
	if _, err := areaOfInterest([]f3548.Volume4D{d}); err == nil {
		t.Fatal("a non-W84 altitude")
	}
}

// A constraint the extents meet: its ovn is read from its manager and
// stored, and carried in the key (the implicit subscription tells of
// constraints, so the DSS asks for it); a manager that does not answer
// leaves the intent waiting unless a copy of the version named is held.
func TestWriterConstraintsInTheKey(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	const cid = "55555555-5555-4555-8555-555555555555"
	cref, err := g.peer.Constrain(ctx, cid, []f3548.Volume4D{volume(41.7, 44.8)}, "")
	if err != nil {
		t.Fatal(err)
	}
	g.peer.Down(true)
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errWaiting) || g.in.lastHold() != intent.ReasonPeerUnavailable || g.count(CounterConstraintFailed) != 1 {
		t.Fatalf("%v %q", err, g.in.lastHold())
	}
	g.peer.Down(false)
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, oursID, f3548.Accepted)
	c, _ := g.st.Constraint(ctx, cid)
	if c == nil || c.OVN != *cref.Ovn || g.count(CounterConstraintDetails) != 1 {
		t.Fatalf("constraint %+v", c)
	}
	// The stored copy serves when the manager is down again.
	g.peer.Down(true)
	if ovn, ok := g.w.constraintOVN(ctx, cref, nil); !ok || ovn != *cref.Ovn {
		t.Fatalf("stored copy not used: %q %v", ovn, ok)
	}
	// A manager that names another manager is refused.
	g.peer.Down(false)
	other := cref
	other.Manager = "someone-else"
	if _, ok := g.w.constraintOVN(ctx, other, nil); ok {
		t.Fatal("a constraint from another manager accepted")
	}
	pref := f3548.OperationalIntentReference{Id: cid, Manager: "someone-else", UssBaseUrl: g.peer.URL()}
	if _, ok := g.w.peerOVN(ctx, pref, nil); ok {
		t.Fatal("an intent from another manager accepted")
	}
}

// Our own other intent the extents meet: its ovn is the DSS's answer to
// us, with no details read from anyone.
func TestWriterOwnIntentsInTheKey(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	g.in.put(pending(peer2ID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, peer2ID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g.in, peer2ID, f3548.Accepted)
	if g.count(CounterPeerDetails) != 0 || g.dss.Conflicts() != 0 {
		t.Fatalf("%v, %d conflicts", g.counters.Snapshot(), g.dss.Conflicts())
	}
}

// The failures that are not the DSS's: the intent store, the lock, a
// DSS refusal of the write (dss_refused), and a mirror that does not
// converge.
func TestWriterFailures(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	g.in.failHeld = errBoom
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	g.in.failHeld = nil
	g.st.failLock = errBoom
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	g.st.failLock = nil
	// An extent the DSS refuses (it ends in the past): dss_refused.
	past := volume(41.7, 44.8)
	past.TimeStart.Value, past.TimeEnd.Value = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	g.in.put(pending(peer2ID, past))
	if err := g.w.Mirror(ctx, peer2ID); !errors.Is(err, errWaiting) || g.in.lastHold() != intent.ReasonDSSRefused {
		t.Fatalf("%v %q", err, g.in.lastHold())
	}
	// Not judged: the hold carries PeerCheck's reason.
	g.in.check = func(*intent.Record) intent.PeerCheckResult {
		return intent.PeerCheckResult{Outcome: intent.PeerCheckNotJudged, Reason: intent.ReasonDeconflictNotJudged, Detail: "no geoid"}
	}
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errWaiting) || g.in.lastHold() != intent.ReasonDeconflictNotJudged {
		t.Fatalf("%v %q", err, g.in.lastHold())
	}
	// No intent: nothing to do.
	if err := g.w.Mirror(ctx, "66666666-6666-4666-8666-666666666666"); err != nil {
		t.Fatal(err)
	}
	// An intent the DSS must hold Activated that is never written
	// converges in at most maxMirrorSteps; a decision naming a state that
	// is not F3548's writes nothing.
	g.in.check = nil
	bogus := "Flying"
	r := pending(peerID, volume(41.71, 44.81))
	r.LocalState, r.Decision.DSSState = intent.StateActivated, &bogus
	g.in.put(r)
	if err := g.w.Mirror(ctx, peerID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := g.dss.OIR(peerID); ok {
		t.Fatal("a state that is not F3548's was written")
	}
	if g.w.logger() == nil || g.w.now().IsZero() {
		t.Fatal("defaults")
	}
}

// lockWatch hands out the rig's tokens and counts the ones asked for a
// peer while a mirror holds its transaction.
type lockWatch struct {
	TokenSource
	st       *memStore
	peerBase string
	mu       sync.Mutex
	inLock   int
	outside  int
}

func (l *lockWatch) Token(ctx context.Context, base string, scopes ...string) (string, error) {
	if base == l.peerBase {
		l.mu.Lock()
		if l.st.inLock.Load() > 0 {
			l.inLock++
		} else {
			l.outside++
		}
		l.mu.Unlock()
	}
	return l.TokenSource.Token(ctx, base, scopes...)
}

// A peer whose manager takes every request and never answers: the mirror
// of an intent that meets its intents is bounded by MirrorTimeout, asks
// the dead manager once (not once per intent of it), never while it
// holds its transaction, and the write of another intent, queued after
// it in the same batch, is made (E-01: the peer that answers is the
// first test of this file).
func TestWriterADeadPeerDoesNotStallOtherWrites(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	for _, id := range []string{peerID, peer2ID, "44444444-4444-4444-8444-444444444444"} {
		if _, err := g.peer.File(ctx, peeruss.Spec{ID: id, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}}); err != nil {
			t.Fatal(err)
		}
	}
	watch := &lockWatch{TokenSource: g.c.Tokens, st: g.st, peerBase: g.peer.URL()}
	g.c.Tokens = watch
	g.c.CallTimeout = time.Second
	g.w.MirrorTimeout = 2 * time.Second
	g.peer.Slow(time.Hour)
	const otherID = "55555555-5555-4555-8555-555555555555"
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	g.in.put(pending(otherID, volume(42.5, 45.5)))
	g.st.enqueue(store.OutboxOIRPut, oursID, 1, intent.DSSWorkload{IntentID: oursID, Version: 1})
	g.st.enqueue(store.OutboxOIRPut, otherID, 1, intent.DSSWorkload{IntentID: otherID, Version: 1})
	start := time.Now()
	done := make(chan error, 1)
	go func() { _, err := g.w.Once(ctx); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("one dead peer held the DSS writes for more than 4 s (mirror bound %s)", g.w.MirrorTimeout)
	}
	if took := time.Since(start); took > g.w.MirrorTimeout+time.Second {
		t.Fatalf("the batch took %s", took)
	}
	mustState(t, g.in, otherID, f3548.Accepted)
	mustState(t, g.in, oursID, "")
	if r := g.in.get(oursID); r.LocalState != intent.StatePendingDSS {
		t.Fatalf("the intent meeting the dead peer is %s", r.LocalState)
	}
	watch.mu.Lock()
	inLock, outside := watch.inLock, watch.outside
	watch.mu.Unlock()
	if inLock != 0 {
		t.Fatalf("%d peer requests made while the mirror held its transaction", inLock)
	}
	if attempts := g.c.attempts(); outside > attempts {
		t.Fatalf("the dead manager was asked %d times; one call is %d attempts", outside, attempts)
	}
}

// A restart while the authority holds this USSP Down: until the Down
// recorded in dss_state is loaded no DSS write is made, after it the
// intent waits uss_availability_down; with Normal recorded the write is
// made once loaded (E-01 both). WaitLoaded returns once Load succeeds
// and gives up with its context.
func TestWriterWaitsForTheRecordedAvailability(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	g.st.state = StateRecord{Availability: string(f3548.Down)}
	a := &Availability{Client: g.c, Store: g.st, USSID: ourManager, Counters: g.counters}
	g.w.Availability = a
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errWaiting) {
		t.Fatalf("written before the recorded availability was loaded: %v", err)
	}
	if _, _, ok := g.dss.OIR(oursID); ok {
		t.Fatal("written to the DSS before the recorded availability was loaded")
	}
	if err := a.WaitLoaded(ctx, time.Millisecond); err != nil || !a.Down() {
		t.Fatalf("load %v down %v", err, a.Down())
	}
	if err := g.w.Mirror(ctx, oursID); !errors.Is(err, errWaiting) || g.in.lastHold() != intent.ReasonUSSAvailabilityDown {
		t.Fatalf("%v %q", err, g.in.lastHold())
	}
	if _, _, ok := g.dss.OIR(oursID); ok {
		t.Fatal("written while the recorded availability is Down")
	}

	g2 := newRig(t)
	g2.st.state = StateRecord{Availability: string(f3548.Normal)}
	a2 := &Availability{Client: g2.c, Store: g2.st, USSID: ourManager}
	g2.w.Availability = a2
	g2.in.put(pending(oursID, volume(41.7, 44.8)))
	g2.st.failState = errBoom
	lctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := a2.WaitLoaded(lctx, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || a2.Loaded() {
		t.Fatalf("a load that keeps failing: %v loaded %v", err, a2.Loaded())
	}
	g2.st.mu.Lock()
	g2.st.failState = nil
	g2.st.mu.Unlock()
	if err := a2.WaitLoaded(ctx, time.Millisecond); err != nil || !a2.Loaded() || a2.Down() {
		t.Fatalf("load %v", err)
	}
	if err := g2.w.Mirror(ctx, oursID); err != nil {
		t.Fatal(err)
	}
	mustState(t, g2.in, oursID, f3548.Accepted)
}
