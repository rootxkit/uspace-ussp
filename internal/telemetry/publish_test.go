package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// handoffOf is a handoff of a fresh track whose settlement goes to got.
func handoffOf(t *testing.T, seq int64, got chan<- bool) *handoff {
	t.Helper()
	at := t0.Add(time.Duration(seq) * time.Second)
	tr := &Track{Envelope: bus.NewEnvelope(SchemaTrack, Producer, core.Times{TS: &at, RxTS: at, CapturedAt: at, Source: core.TimeSourceClock}),
		Body: TrackBody{TrackID: flightIDs[0], SourceInstance: clientA, Position: tbilisi, Seq: seq}}
	subject, err := trackSubject(&tr.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &handoff{subject: subject, cell3: "c3:131:224", track: tr, done: func(ok bool) { got <- ok }}
}

// 05 §5, B-05 pairs: a sample published from memory is acknowledged; one
// that finds memory full goes to the work queue and is acknowledged once
// written there; one neither can take is not acknowledged, counted.
func TestOutboxMemorySpillAndFull(t *testing.T) {
	p := &pub{}
	o := NewOutbox(p, 1, 1, nil, nil)
	got := make(chan bool, 10)
	if !o.Offer(handoffOf(t, 1, got)) { // memory
		t.Fatal("memory refused")
	}
	if !o.Offer(handoffOf(t, 2, got)) { // memory full: towards the work queue
		t.Fatal("spill refused")
	}
	if o.Offer(handoffOf(t, 3, got)) { // both full
		t.Fatal("a third taken by queues of one")
	}
	if ok := <-got; ok {
		t.Fatal("the refused one acknowledged")
	}
	if mem, spill := o.Depth(); mem != 1 || spill != 1 {
		t.Fatalf("depth %d %d", mem, spill)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { o.Run(ctx) })
	wg.Go(func() { o.RunSpill(ctx) })
	for range 2 {
		if !<-got {
			t.Fatal("not acknowledged")
		}
	}
	cancel()
	wg.Wait()
	if len(p.kind(bus.KindTrk)) != 1 || len(p.kind(bus.KindIngest)) != 1 {
		t.Fatalf("trk %d ingest %d", len(p.kind(bus.KindTrk)), len(p.kind(bus.KindIngest)))
	}
	c := o.Counters
	if c.Get(CounterPublished) != 1 || c.Get(CounterSpilled) != 1 || c.Get(CounterSpilledFull) != 2 || c.Get(CounterDroppedQueueFull) != 1 {
		t.Fatalf("counters %v", c.Snapshot())
	}
}

// A sample that waited longer than ingest_queue_s in memory, or whose
// core publish failed, goes to the work queue (never published late as
// live); a work queue that refuses leaves it unacknowledged.
func TestOutboxAgedAndFailedGoToTheWorkQueue(t *testing.T) {
	p := &pub{}
	clk := &clock{at: t0}
	o := NewOutbox(p, 4, 4, nil, nil)
	o.Now = clk.now
	o.QueueFor = func() time.Duration { return 10 * time.Second }
	got := make(chan bool, 10)
	o.Offer(handoffOf(t, 1, got))
	clk.add(11 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.publish(ctx, <-o.q)
	if o.Counters.Get(CounterSpilledAged) != 1 {
		t.Fatal("aged not spilled")
	}
	o.writeQueue(ctx, <-o.spill)
	if !<-got || len(p.kind(bus.KindIngest)) != 1 {
		t.Fatal("aged not queued")
	}
	p.setFail(bus.KindTrk, true)
	o.Offer(handoffOf(t, 2, got))
	o.publish(ctx, <-o.q)
	if o.Counters.Get(CounterSpilledFailed) != 1 || len(o.spill) != 1 {
		t.Fatal("failed publish not spilled")
	}
	p.setFail(bus.KindIngest, true)
	o.writeQueue(ctx, <-o.spill)
	if <-got || o.Counters.Get(CounterSpillFailed) != 1 {
		t.Fatal("a refused work-queue write acknowledged")
	}
	// The cell must be one: an invalid one is a failed write.
	h := handoffOf(t, 3, got)
	h.cell3 = "nope"
	o.writeQueue(ctx, h)
	if <-got {
		t.Fatal("acknowledged without a subject")
	}
}

// Stopping refuses what memory and the spill still hold (the clients
// send it again), never acknowledges it.
func TestOutboxStopRefusesWhatIsLeft(t *testing.T) {
	o := NewOutbox(&pub{}, 4, 4, nil, nil)
	got := make(chan bool, 10)
	o.Offer(handoffOf(t, 1, got))
	o.toSpill(handoffOf(t, 2, got))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.Run(ctx)
	o.RunSpill(ctx)
	for range 2 {
		if <-got {
			t.Fatal("acknowledged on stop")
		}
	}
}

// Without memory every sample goes through the work queue.
func TestOutboxWithoutMemory(t *testing.T) {
	o := NewOutbox(&pub{}, -1, 4, nil, nil)
	got := make(chan bool, 1)
	if !o.Offer(handoffOf(t, 1, got)) || len(o.spill) != 1 {
		t.Fatal("not spilled")
	}
}

// The event queue retries a failed publish until it is stored (the
// msg_id dedupes a retry), and counts what it cannot take.
func TestEventsRetryAndBound(t *testing.T) {
	p := &pub{}
	p.setFail(bus.KindIdent, true)
	e := NewEvents(p, 1, nil, nil)
	e.Retry = time.Millisecond
	m := &IdentChange{Envelope: bus.SystemEnvelope(SchemaIdentChange, Producer, t0), Body: IdentBody{TrackID: "t", SourceInstance: clientA}}
	e.Publish("ident.v1.t", m)
	e.Publish("ident.v1.t", m) // over the bound
	if e.Counters.Get(CounterEventsDropped) != 1 || e.Len() != 1 {
		t.Fatalf("bound: %v", e.Counters.Snapshot())
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { e.Run(ctx) })
	waitFor(t, func() bool { return e.Counters.Get(CounterEventsRetried) >= 2 })
	p.setFail(bus.KindIdent, false)
	waitFor(t, func() bool { return e.Counters.Get(CounterEventsPublished) == 1 })
	cancel()
	wg.Wait()
}

// queueMsg is a work-queue message of a test.
type queueMsg struct {
	data   []byte
	seq    uint64
	acked  bool
	naked  bool
	ackErr error
}

func (m *queueMsg) Data() []byte               { return m.data }
func (m *queueMsg) Subject() string            { return "ingest.v1.c3:131:224" }
func (m *queueMsg) StreamSeq() (uint64, error) { return m.seq, nil }
func (m *queueMsg) Ack() error                 { m.acked = true; return m.ackErr }
func (m *queueMsg) Nak() error                 { m.naked = true; return nil }
func (m *queueMsg) InProgress() error          { return nil }

// queue is a QueueSource of a test.
type queue struct {
	mu        sync.Mutex
	msgs      [][]bus.Msg
	from, to  uint64
	failFetch bool
}

func (q *queue) Fetch(context.Context, int, time.Duration) ([]bus.Msg, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failFetch {
		return nil, errors.New("down")
	}
	if len(q.msgs) == 0 {
		return nil, nil
	}
	m := q.msgs[0]
	q.msgs = q.msgs[1:]
	return m, nil
}

func (q *queue) NeverDelivered(context.Context) (uint64, uint64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.from, q.to, nil
}

// gaps is a GapReporter of a test.
type gaps struct {
	mu  sync.Mutex
	all []Gap
}

func (g *gaps) ReportGap(_ context.Context, gap Gap) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.all = append(g.all, gap)
}

func queued(t *testing.T, client string, seq int64, rx time.Time) *queueMsg {
	t.Helper()
	at := rx.Add(-100 * time.Millisecond)
	tr := &Track{Envelope: bus.NewEnvelope(SchemaTrack, Producer, core.Times{TS: &at, RxTS: rx, CapturedAt: at, Source: core.TimeSourceClock}),
		Body: TrackBody{TrackID: flightIDs[0], SourceInstance: client, Position: tbilisi, Seq: seq}}
	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	return &queueMsg{data: data, seq: uint64(seq)}
}

// 05 §5 pairs: the drain replays a queued track as backlog with its own
// captured_at and msg_id, then acknowledges it; one older than the bound
// is shed with a gap record before it leaves the queue, oldest first,
// and one that does not read is a corrupt gap. Nothing is dropped
// without a record.
func TestDrainReplaysShedsAndRecords(t *testing.T) {
	p := &pub{}
	g := &gaps{}
	clk := &clock{at: t0.Add(time.Hour)}
	d := &Drain{Pub: p, Gaps: g, Counters: &core.Counters{}, Now: clk.now, MaxAge: func() time.Duration { return 9 * time.Minute }}
	old1 := queued(t, clientA, 1, t0)
	old2 := queued(t, clientA, 2, t0.Add(time.Second))
	oldB := queued(t, clientB, 3, t0.Add(2*time.Second))
	fresh := queued(t, clientA, 4, clk.now().Add(-time.Minute))
	corrupt := &queueMsg{data: []byte(`{"schema":"x"}`), seq: 5}
	if !d.Take(context.Background(), []bus.Msg{old1, old2, oldB, fresh, corrupt}) {
		t.Fatal("take failed")
	}
	if len(g.all) != 3 {
		t.Fatalf("gaps %+v", g.all)
	}
	if g.all[0].Cause != GapQueueAge || g.all[0].SourceInstance != clientA || g.all[0].Dropped != 2 ||
		!g.all[0].GapStarted.Equal(t0.Add(-100*time.Millisecond)) || !g.all[0].GapEnded.Equal(t0.Add(900*time.Millisecond)) {
		t.Fatalf("gap a %+v", g.all[0])
	}
	if g.all[1].SourceInstance != clientB || g.all[2].Cause != GapCorrupt || g.all[2].FromSeq != 5 {
		t.Fatalf("gaps %+v", g.all)
	}
	for _, m := range []*queueMsg{old1, old2, oldB, fresh, corrupt} {
		if !m.acked {
			t.Fatalf("seq %d not acknowledged", m.seq)
		}
	}
	trs := p.tracks(t)
	if len(trs) != 1 || !trs[0].Backlog || trs[0].Body.Seq != 4 {
		t.Fatalf("replayed %+v", trs)
	}
	var orig Track
	_ = json.Unmarshal(fresh.data, &orig)
	if trs[0].MsgID != orig.MsgID || !trs[0].CapturedAt.Equal(orig.CapturedAt.Time) {
		t.Fatal("the replay changed the msg_id or captured_at")
	}
	if d.Counters.Get(CounterDrained) != 1 || d.Counters.Get(CounterDrainShedAge) != 3 || d.Counters.Get(CounterDrainCorrupt) != 1 {
		t.Fatalf("counters %v", d.Counters.Snapshot())
	}
}

// A replay that cannot be published leaves it and the rest of the fetch
// queued (nak), acknowledged nothing.
func TestDrainPublishFailureLeavesItQueued(t *testing.T) {
	p := &pub{}
	p.setFail(bus.KindTrk, true)
	d := &Drain{Pub: p, Gaps: &gaps{}, Counters: &core.Counters{}, Now: func() time.Time { return t0 }}
	a, b := queued(t, clientA, 1, t0), queued(t, clientA, 2, t0)
	if d.Take(context.Background(), []bus.Msg{a, b}) {
		t.Fatal("reported success")
	}
	if a.acked || b.acked || !a.naked || !b.naked {
		t.Fatal("acknowledged or not left queued")
	}
}

// The unread loss of the queue is recorded once, and nothing when the
// queue lost nothing (E-01 pair).
func TestDrainRecordsUnreadLossOnce(t *testing.T) {
	g := &gaps{}
	q := &queue{from: 1, to: 0}
	d := &Drain{Source: q, Gaps: g, Counters: &core.Counters{}}
	d.CheckUnread(context.Background())
	if len(g.all) != 0 {
		t.Fatal("a gap for nothing")
	}
	q.from, q.to = 11, 14
	d.CheckUnread(context.Background())
	d.CheckUnread(context.Background())
	if len(g.all) != 1 || g.all[0].Dropped != 4 || g.all[0].Cause != GapAgedOutUnread || d.Counters.Get(CounterDrainAgedOut) != 4 {
		t.Fatalf("gaps %+v", g.all)
	}
	q.from, q.to = 11, 20
	d.CheckUnread(context.Background())
	if len(g.all) != 2 || g.all[1].FromSeq != 15 || g.all[1].Dropped != 6 {
		t.Fatalf("second %+v", g.all)
	}
}

// Run fetches, replays and stops with its context; a fetch error waits.
func TestDrainRun(t *testing.T) {
	p := &pub{}
	q := &queue{from: 1, to: 0, msgs: [][]bus.Msg{{queued(t, clientA, 1, time.Now())}}}
	d := &Drain{Source: q, Pub: p, Gaps: &gaps{}, Counters: &core.Counters{}, Wait: time.Millisecond, CheckEvery: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.Run(ctx) })
	waitFor(t, func() bool { return len(p.tracks(t)) == 1 })
	q.mu.Lock()
	q.failFetch = true
	q.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	cancel()
	wg.Wait()
}

// The status publisher records a gap at once on the client's subject,
// logged and counted, and the queue's own gaps on its instance.
func TestStatusReportsGaps(t *testing.T) {
	r := newRig(t, rigOpts{})
	st := &Status{Ingest: r.in, Pub: r.pub, Counters: &core.Counters{}, Now: r.clk.now}
	st.ReportGap(context.Background(), Gap{Cause: GapQueueAge, SourceInstance: clientA, GapStarted: t0, GapEnded: t0, Dropped: 3})
	st.ReportGap(context.Background(), Gap{Cause: GapAgedOutUnread, SourceInstance: GapInstance, Dropped: 2, FromSeq: 1, ToSeq: 2})
	src := r.pub.kind(bus.KindSrc)
	if len(src) != 2 || src[0].subject != "src.v1.operator_ws."+clientA || src[1].subject != "src.v1.operator_ws."+GapInstance {
		t.Fatalf("published %+v", src)
	}
	if !strings.Contains(string(src[0].data), `"cause":"ingest_queue_age"`) || st.Counters.Get(CounterGapDropped) != 5 {
		t.Fatalf("%s %v", src[0].data, st.Counters.Snapshot())
	}
	// A client id that is no subject token is counted, not published.
	st.ReportGap(context.Background(), Gap{Cause: GapQueueAge, SourceInstance: "a.b", Dropped: 1})
	if st.Counters.Get(CounterStatusFailed) != 1 {
		t.Fatal("not counted")
	}
}
