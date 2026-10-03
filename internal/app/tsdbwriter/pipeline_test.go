package tsdbwriter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// fakeMsg is a delivered message.
type fakeMsg struct {
	data    []byte
	subject string
	seq     uint64
	src     *fakeSource
}

func (m *fakeMsg) Data() []byte    { return m.data }
func (m *fakeMsg) Subject() string { return m.subject }
func (m *fakeMsg) StreamSeq() (uint64, error) {
	if m.seq == 0 {
		return 0, errors.New("not a JetStream message")
	}
	return m.seq, nil
}
func (m *fakeMsg) Ack() error        { m.src.event("ack", m.seq); return nil }
func (m *fakeMsg) Nak() error        { m.src.event("nak", m.seq); return nil }
func (m *fakeMsg) InProgress() error { m.src.event("progress", m.seq); return nil }

// fakeSource is a stream: published messages by sequence, a first
// sequence (what the limits removed below it) and a consumer cursor.
type fakeSource struct {
	mu       sync.Mutex
	msgs     map[uint64]*fakeMsg
	next     uint64 // next sequence to deliver
	last     uint64 // last sequence published
	first    uint64 // the stream's first sequence
	floor    uint64
	events   []string
	acked    map[uint64]bool
	holesErr error
	fetchErr error
	fetched  int
}

func newSource() *fakeSource {
	return &fakeSource{msgs: map[uint64]*fakeMsg{}, next: 1, first: 1, acked: map[uint64]bool{}}
}

func (s *fakeSource) event(kind string, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, fmt.Sprintf("%s %d", kind, seq))
	if kind == "ack" {
		s.acked[seq] = true
		for s.acked[s.floor+1] {
			s.floor++
		}
	}
	if kind == "nak" && seq < s.next {
		s.next = seq
	}
}

func (s *fakeSource) publish(data []byte) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last++
	s.msgs[s.last] = &fakeMsg{data: data, subject: "trk.v1.c3:131:224.c5:1317:2248.trk-1", seq: s.last, src: s}
	return s.last
}

// remove drops the stream's head up to seq (limits, purge).
func (s *fakeSource) remove(upTo uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for q := s.first; q <= upTo; q++ {
		delete(s.msgs, q)
	}
	s.first = upTo + 1
}

func (s *fakeSource) Fetch(ctx context.Context, n int, wait time.Duration) ([]bus.Msg, error) {
	s.mu.Lock()
	if s.fetchErr != nil {
		err := s.fetchErr
		s.mu.Unlock()
		return nil, err
	}
	var out []bus.Msg
	for s.next <= s.last && len(out) < n {
		if m, ok := s.msgs[s.next]; ok && !s.acked[s.next] {
			out = append(out, m)
		}
		s.next++
	}
	s.fetched += len(out)
	s.mu.Unlock()
	if len(out) == 0 {
		sleep(ctx, min(wait, 5*time.Millisecond))
	}
	return out, nil
}

func (s *fakeSource) AckFloor(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.floor, nil
}

func (s *fakeSource) Holes(_ context.Context, jumps []bus.Jump) ([]bus.Hole, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holesErr != nil {
		return nil, s.holesErr
	}
	out := make([]bus.Hole, len(jumps))
	for i, j := range jumps {
		if end := min(j.Before, s.first); end > j.After+1 {
			out[i] = bus.Hole{FromSeq: j.After + 1, ToSeq: end - 1, Count: end - 1 - j.After}
		}
	}
	return out, nil
}

// fakeStore is the database: rows by table, unique by msg_id; gaps;
// positions; an error to return while down.
type fakeStore struct {
	mu        sync.Mutex
	rows      map[string]map[string]bool
	gaps      []store.Gap
	positions map[string]uint64
	down      error
	posErr    error
	writes    int
	refuse    func(store.WriteBatch) error
}

func newStore() *fakeStore {
	return &fakeStore{rows: map[string]map[string]bool{}, positions: map[string]uint64{}}
}

func (f *fakeStore) Write(_ context.Context, b store.WriteBatch) (store.Written, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return store.Written{}, f.down
	}
	if f.refuse != nil {
		if err := f.refuse(b); err != nil {
			return store.Written{}, err
		}
	}
	f.writes++
	var w store.Written
	for t, rows := range b.Rows {
		if f.rows[t.Name] == nil {
			f.rows[t.Name] = map[string]bool{}
		}
		for _, r := range rows {
			id := r[0].(string)
			if f.rows[t.Name][id] {
				w.Duplicates++
				continue
			}
			f.rows[t.Name][id] = true
			w.Inserted++
		}
	}
	for i := range b.Gaps {
		g := b.Gaps[i]
		if !slices.ContainsFunc(f.gaps, func(h store.Gap) bool { return h.DedupeKey == g.DedupeKey }) {
			f.gaps = append(f.gaps, g)
			w.Gaps++
		}
	}
	if b.Position > f.positions[b.Stream] {
		f.positions[b.Stream] = b.Position
	}
	return w, nil
}

func (f *fakeStore) Position(_ context.Context, stream string) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posErr != nil {
		return 0, false, f.posErr
	}
	p, ok := f.positions[stream]
	return p, ok, nil
}

func (f *fakeStore) count(table string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows[table])
}

func (f *fakeStore) gapList() []store.Gap {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.gaps)
}

type rig struct {
	src  *fakeSource
	st   *fakeStore
	p    *Pipeline
	logs *syncWriter
	stop func()
}

type syncWriter struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func testConfig() Config {
	c := DefaultConfig()
	c.BatchMaxWait, c.RetryMin, c.RetryMax, c.FetchWait = 20*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, 5*time.Millisecond
	return c
}

func newRig(t *testing.T, cfg Config, prepare func(*fakeSource, *fakeStore), tweak ...func(*Pipeline)) *rig {
	t.Helper()
	r := &rig{src: newSource(), st: newStore(), logs: &syncWriter{b: &strings.Builder{}}}
	if prepare != nil {
		prepare(r.src, r.st)
	}
	r.p = &Pipeline{
		Stream: Stream{Name: "TRK", Subject: bus.SubjectTrkAll, Decode: DecodeTelemetry}, Source: r.src, Store: r.st,
		Config: cfg, Counters: &core.Counters{}, Logger: slog.New(slog.NewJSONHandler(r.logs, nil)),
	}
	for _, f := range tweak {
		f(r.p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.p.Run(ctx); close(done) }()
	r.stop = func() { cancel(); <-done }
	t.Cleanup(r.stop)
	return r
}

func (r *rig) logText() string {
	time.Sleep(10 * time.Millisecond)
	r.logs.mu.Lock()
	defer r.logs.mu.Unlock()
	return r.logs.b.String()
}

func (s *fakeSource) setFetchErr(err error) {
	s.mu.Lock()
	s.fetchErr = err
	s.mu.Unlock()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("not reached: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func publishTracks(t *testing.T, s *fakeSource, n int) []uint64 {
	t.Helper()
	seqs := make([]uint64, n)
	for i := range n {
		seqs[i] = s.publish(message(t, "track/telemetry/v1", time.Now(), trackBodyOf(flightID)))
	}
	return seqs
}

func (r *rig) counter(name string) uint64 { return r.p.Counters.Get(name) }

// Rows are written in batches, acknowledged after the commit, and the
// position follows; nothing is recorded as a gap (the twin of the gap
// tests below).
func TestPipelineWritesAndAcks(t *testing.T) {
	r := newRig(t, testConfig(), nil)
	publishTracks(t, r.src, 2500)
	eventually(t, "2500 rows", func() bool { return r.st.count("telemetry") == 2500 })
	eventually(t, "every ack", func() bool { f, _ := r.src.AckFloor(context.Background()); return f == 2500 })
	if r.counter(CounterRowsWritten) != 2500 || r.counter(CounterBatches) < 3 || len(r.st.gapList()) != 0 || r.counter(CounterDroppedRows) != 0 {
		t.Fatalf("counters %v gaps %v", r.p.Counters.Snapshot(), r.st.gapList())
	}
	if pos, _, _ := r.st.Position(context.Background(), "TRK"); pos != 2500 {
		t.Fatalf("position %d", pos)
	}
	if s := r.p.Snapshot(); s.State != StateOK || s.QueueRows != 0 || s.LastSeq != 2500 {
		t.Fatalf("%+v", s)
	}
}

// The same message republished (same msg_id) inside the window writes
// nothing and is a dedupe hit; a skipped track (no flight) is counted
// and acknowledged.
func TestPipelineDedupeWindowAndSkip(t *testing.T) {
	r := newRig(t, testConfig(), nil)
	data := message(t, "track/telemetry/v1", time.Now(), trackBodyOf(flightID))
	r.src.publish(data)
	r.src.publish(data)
	r.src.publish(message(t, "track/telemetry/v1", time.Now(), trackBodyOf(nil)))
	eventually(t, "all acked", func() bool { f, _ := r.src.AckFloor(context.Background()); return f == 3 })
	if r.st.count("telemetry") != 1 || r.counter(CounterDedupeHits) != 1 || r.counter(CounterSkippedPrefix+SkipNotOwnFlight) != 1 {
		t.Fatalf("rows %d counters %v", r.st.count("telemetry"), r.p.Counters.Snapshot())
	}
}

// Beyond the window the database's unique index is the dedupe: the
// store reports the duplicates and they are counted the same.
func TestPipelineDedupeByTheDatabase(t *testing.T) {
	cfg := testConfig()
	cfg.DedupeWindow = time.Nanosecond
	r := newRig(t, cfg, nil)
	data := message(t, "track/telemetry/v1", time.Now(), trackBodyOf(flightID))
	r.src.publish(data)
	eventually(t, "first", func() bool { return r.st.count("telemetry") == 1 })
	r.src.publish(data)
	eventually(t, "second acked", func() bool { f, _ := r.src.AckFloor(context.Background()); return f == 2 })
	if r.st.count("telemetry") != 1 || r.counter(CounterDedupeHits) != 1 || r.counter(CounterRowsWritten) != 1 {
		t.Fatalf("counters %v", r.p.Counters.Snapshot())
	}
}

// E-10: the in-memory window holds at most DedupeMax ids.
func TestDedupeWindowBound(t *testing.T) {
	p := &Pipeline{Config: Config{DedupeWindow: time.Hour, DedupeMax: 3}}
	p.init()
	now := time.Now()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if p.remember(id, now) {
			t.Fatalf("%s seen", id)
		}
	}
	if len(p.seen) > 4 || len(p.seenOrder) > 4 {
		t.Fatalf("window holds %d ids", len(p.seen))
	}
	if !p.remember("e", now) {
		t.Fatal("the newest id forgotten")
	}
	if p.remember("a", now) {
		t.Fatal("the oldest id kept beyond the bound")
	}
	// An id older than the window is forgotten.
	if p.remember("e", now.Add(2*time.Hour)) {
		t.Fatal("an expired id is a hit")
	}
}

// TimescaleDB down: nothing is acknowledged, the queue holds beyond its
// 10 s bound up to HoldMaxRows and then stops pulling (spills); when the
// database is back every row lands, none twice, and nothing was lost.
func TestPipelineHoldsWhileTheDatabaseIsDown(t *testing.T) {
	cfg := testConfig()
	cfg.HoldMaxRows, cfg.QueueMaxAge = 300, 10*time.Millisecond
	r := newRig(t, cfg, nil)
	publishTracks(t, r.src, 10)
	eventually(t, "first rows", func() bool { return r.st.count("telemetry") == 10 })
	r.st.mu.Lock()
	r.st.down = errors.New("connection refused")
	r.st.mu.Unlock()
	publishTracks(t, r.src, 1000)
	eventually(t, "held to the bound", func() bool { return r.p.Snapshot().QueueRows == 300 })
	eventually(t, "spilling", func() bool { return r.p.Snapshot().State == StateSpilling })
	time.Sleep(50 * time.Millisecond)
	if s := r.p.Snapshot(); s.QueueRows != 300 || r.counter(CounterSpills) == 0 || r.counter(CounterWriteFailed) == 0 {
		t.Fatalf("snapshot %+v counters %v", s, r.p.Counters.Snapshot())
	}
	if f, _ := r.src.AckFloor(context.Background()); f != 10 {
		t.Fatalf("acknowledged while down: floor %d", f)
	}
	r.st.mu.Lock()
	r.st.down = nil
	r.st.mu.Unlock()
	eventually(t, "every row", func() bool { return r.st.count("telemetry") == 1010 })
	eventually(t, "every ack", func() bool { f, _ := r.src.AckFloor(context.Background()); return f == 1010 })
	if r.counter(CounterDroppedRows) != 0 || len(r.st.gapList()) != 0 || r.counter(CounterDedupeHits) != 0 {
		t.Fatalf("counters %v", r.p.Counters.Snapshot())
	}
	if !strings.Contains(r.logText(), "writes resumed") {
		t.Fatal("recovery not logged")
	}
}

// While writes succeed the queue is bounded by its age: a slow database
// makes the writer spill at QueueMaxAge, not at HoldMaxRows.
func TestPipelineSpillsAtTheQueueAge(t *testing.T) {
	cfg := testConfig()
	cfg.QueueMaxAge, cfg.BatchMaxRows, cfg.BatchMaxWait = 20*time.Millisecond, 1, time.Millisecond
	var slow sync.Mutex
	r := newRig(t, cfg, func(_ *fakeSource, st *fakeStore) {
		st.refuse = func(store.WriteBatch) error { slow.Lock(); defer slow.Unlock(); return nil }
	})
	slow.Lock()
	publishTracks(t, r.src, 50)
	eventually(t, "spilling by age", func() bool { return r.counter(CounterSpills) == 1 })
	if s := r.p.Snapshot(); s.QueueRows >= cfg.HoldMaxRows {
		t.Fatalf("%+v", s)
	}
	slow.Unlock()
	eventually(t, "every row", func() bool { return r.st.count("telemetry") == 50 })
}

// A hole the stream's limits left is a stream_removed gap, committed
// with the message after it before that message is acknowledged,
// counted in dropped_rows and logged with the subject and time range.
func TestPipelineRecordsAHoleBeforeTheAck(t *testing.T) {
	r := newRig(t, testConfig(), nil)
	publishTracks(t, r.src, 5)
	eventually(t, "first five", func() bool { f, _ := r.src.AckFloor(context.Background()); return f == 5 })
	r.st.mu.Lock()
	r.st.down = errors.New("down") // the writer is behind
	r.st.mu.Unlock()
	r.src.setFetchErr(errors.New("nats down"))
	time.Sleep(20 * time.Millisecond)
	publishTracks(t, r.src, 10) // 6..15
	r.src.remove(12)            // 6..12 age out unwritten
	var firstAfter uint64 = 13
	r.st.mu.Lock()
	r.st.down = nil
	r.st.refuse = func(b store.WriteBatch) error {
		// The gap commits with or before the rows after it.
		if b.Position >= firstAfter && len(b.Gaps) == 0 && len(r.st.gaps) == 0 {
			return errors.New("rows after the hole without its gap")
		}
		return nil
	}
	r.st.mu.Unlock()
	r.src.setFetchErr(nil)
	eventually(t, "rest written", func() bool {
		f, _ := r.src.AckFloor(context.Background())
		return f == 5 && r.st.count("telemetry") == 8
	})
	gaps := r.st.gapList()
	if len(gaps) != 1 || gaps[0].Cause != store.CauseStreamRemoved || gaps[0].FromSeq != 6 || gaps[0].ToSeq != 12 || gaps[0].Count != 7 ||
		gaps[0].Subject != bus.SubjectTrkAll || gaps[0].AfterAt == nil || gaps[0].BeforeAt == nil || !gaps[0].BeforeAt.After(*gaps[0].AfterAt) {
		t.Fatalf("gaps %+v", gaps)
	}
	if r.counter(CounterDroppedRows) != 7 || r.counter(CounterGaps) != 1 {
		t.Fatalf("counters %v", r.p.Counters.Snapshot())
	}
	logs := r.logText()
	if !strings.Contains(logs, `"dropped_rows":7`) || !strings.Contains(logs, `"subject":"trk.v1.>"`) || !strings.Contains(logs, "after_captured_at") {
		t.Fatalf("log: %s", logs)
	}
}

// When the hole check cannot be made the messages are given back and
// nothing advances; once it can, they are written.
func TestPipelineHoleCheckFailure(t *testing.T) {
	r := newRig(t, testConfig(), func(src *fakeSource, _ *fakeStore) {
		src.holesErr = errors.New("stream info failed")
		src.floor = 2 // restart: 1..2 acknowledged before
		src.last = 2
		src.next = 3
	})
	publishTracks(t, r.src, 3) // 3..5, no jump
	eventually(t, "written", func() bool { return r.st.count("telemetry") == 3 })
	r.src.mu.Lock()
	r.src.last = 7 // 6, 7 never published: a jump at 8
	r.src.next = 8
	r.src.mu.Unlock()
	publishTracks(t, r.src, 1)
	eventually(t, "check failed", func() bool { return r.counter(CounterHoleCheckFailed) > 0 })
	if r.st.count("telemetry") != 3 {
		t.Fatal("written without the hole check")
	}
	r.src.mu.Lock()
	r.src.holesErr = nil
	r.src.mu.Unlock()
	eventually(t, "written after", func() bool { return r.st.count("telemetry") == 4 })
}

// A message the writer cannot read is a malformed gap and is
// acknowledged with it; the rows around it are written.
func TestPipelineMalformed(t *testing.T) {
	r := newRig(t, testConfig(), nil)
	publishTracks(t, r.src, 1)
	r.src.publish([]byte(`{"schema":"track/telemetry/v1"}`))
	publishTracks(t, r.src, 1)
	eventually(t, "acked", func() bool { f, _ := r.src.AckFloor(context.Background()); return f == 3 })
	gaps := r.st.gapList()
	if len(gaps) != 1 || gaps[0].Cause != store.CauseMalformed || gaps[0].FromSeq != 2 || r.st.count("telemetry") != 2 || r.counter(CounterMessagesMalformed) != 1 {
		t.Fatalf("gaps %+v counters %v", gaps, r.p.Counters.Snapshot())
	}
}

// A batch the database refuses as data is written message by message;
// the refused one becomes a rejected gap, the others are written.
func TestPipelineRejected(t *testing.T) {
	const badID = "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"
	r := newRig(t, testConfig(), func(_ *fakeSource, st *fakeStore) {
		st.refuse = func(b store.WriteBatch) error {
			for _, rows := range b.Rows {
				for _, row := range rows {
					if row[0] == "BAD" {
						return errCheckViolation
					}
				}
			}
			return nil
		}
	}, func(p *Pipeline) {
		p.DataError = func(err error) bool { return errors.Is(err, errCheckViolation) }
		p.Stream.Decode = func(data []byte) (Decoded, error) {
			d, err := DecodeTelemetry(data)
			if err == nil && d.MsgID == badID {
				d.Rows[0].Values[0] = "BAD"
			}
			return d, err
		}
	})
	publishTracks(t, r.src, 2)
	bad := message(t, "track/telemetry/v1", time.Now(), trackBodyOf(flightID))
	d, _ := DecodeTelemetry(bad)
	r.src.publish([]byte(strings.Replace(string(bad), d.MsgID, badID, 1)))
	eventually(t, "acked", func() bool { f, _ := r.src.AckFloor(context.Background()); return f == 3 })
	gaps := r.st.gapList()
	if r.st.count("telemetry") != 2 || len(gaps) != 1 || gaps[0].Cause != store.CauseRejected || gaps[0].Count != 1 || gaps[0].CountUnit != store.UnitRows {
		t.Fatalf("rows %d gaps %+v", r.st.count("telemetry"), gaps)
	}
	if r.counter(CounterRowsRejected) != 1 {
		t.Fatal(r.p.Counters.Snapshot())
	}
}

// At start, an ack floor beyond the written position is messages the
// stream removed while the writer was not reading: recorded before
// anything is pulled. A floor at the position records nothing (a quiet
// stream after a restart), nor does a new consumer on a database that
// never wrote. A consumer that acknowledged messages the database
// records no position for (restored from an older backup, re-created)
// lost them: a position_unknown gap from 1 to the floor (audit S3).
func TestPipelineStartPosition(t *testing.T) {
	for _, c := range []struct {
		name        string
		floor, pos  uint64
		known       bool
		wantGap     bool
		wantFrom    uint64
		wantRemoved uint64
		wantCause   string
	}{
		{"purged while down", 40, 25, true, true, 26, 15, store.CauseStreamRemoved},
		{"quiet stream", 25, 25, true, false, 0, 0, ""},
		{"written past the floor", 20, 25, true, false, 0, 0, ""},
		{"new consumer, never written", 0, 0, false, false, 0, 0, ""},
		{"acknowledged, no position written", 40, 0, false, true, 1, 40, store.CausePositionUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, testConfig(), func(src *fakeSource, st *fakeStore) {
				src.floor, src.last, src.next, src.first = c.floor, c.floor, c.floor+1, c.floor+1
				if c.known {
					st.positions["TRK"] = c.pos
				}
			})
			publishTracks(t, r.src, 1)
			eventually(t, "written", func() bool { return r.st.count("telemetry") == 1 })
			gaps := r.st.gapList()
			if c.wantGap != (len(gaps) == 1) || (c.wantGap && (gaps[0].FromSeq != c.wantFrom || gaps[0].ToSeq != c.floor ||
				uint64(gaps[0].Count) != c.wantRemoved || gaps[0].Cause != c.wantCause)) || r.counter(CounterDroppedRows) != c.wantRemoved {
				t.Fatalf("gaps %+v counters %v", gaps, r.p.Counters.Snapshot())
			}
		})
	}
}

// The position cannot be read: nothing is pulled or written until it
// can (a purge while down is never missed).
func TestPipelinePositionUnreadable(t *testing.T) {
	r := newRig(t, testConfig(), func(_ *fakeSource, st *fakeStore) { st.posErr = errors.New("db down") })
	publishTracks(t, r.src, 3)
	eventually(t, "counted", func() bool { return r.counter(CounterPositionFailed) > 1 })
	r.src.mu.Lock()
	fetched := r.src.fetched
	r.src.mu.Unlock()
	if fetched != 0 || r.p.Snapshot().State != StateWriteFailing {
		t.Fatalf("pulled %d before the position was read; %+v", r.src.fetched, r.p.Snapshot())
	}
	r.st.mu.Lock()
	r.st.posErr = nil
	r.st.mu.Unlock()
	eventually(t, "written", func() bool { return r.st.count("telemetry") == 3 })
}

// Held messages are kept from redelivery while the database is down;
// a message redelivered while held replaces its earlier delivery.
func TestPipelineKeepAliveAndRedelivery(t *testing.T) {
	cfg := testConfig()
	cfg.AckWait = 10 * time.Millisecond
	r := newRig(t, cfg, func(_ *fakeSource, st *fakeStore) { st.down = errors.New("down") })
	publishTracks(t, r.src, 2)
	eventually(t, "progress sent", func() bool {
		r.src.mu.Lock()
		defer r.src.mu.Unlock()
		return slices.Contains(r.src.events, "progress 1")
	})
	// The stream delivers message 1 again (its ack wait ran out).
	r.src.mu.Lock()
	r.src.next = 1
	r.src.mu.Unlock()
	eventually(t, "redelivered", func() bool { return r.counter(CounterRedelivered) >= 1 })
	if r.p.Snapshot().QueueMessages != 2 {
		t.Fatalf("%v %+v", r.p.Counters.Snapshot(), r.p.Snapshot())
	}
	r.st.mu.Lock()
	r.st.down = nil
	r.st.mu.Unlock()
	eventually(t, "written", func() bool { return r.st.count("telemetry") == 2 })
}

// A fetch failure is counted and retried; a message without a stream
// sequence is malformed.
func TestPipelineFetchFailureAndUnsequenced(t *testing.T) {
	r := newRig(t, testConfig(), func(src *fakeSource, _ *fakeStore) { src.fetchErr = errors.New("nats: timeout") })
	eventually(t, "counted", func() bool { return r.counter(CounterFetchFailed) > 0 })
	r.src.mu.Lock()
	r.src.fetchErr = nil
	r.src.last++
	r.src.msgs[r.src.last] = &fakeMsg{data: []byte("x"), subject: "trk.v1.x", src: r.src} // no stream sequence
	r.src.mu.Unlock()
	eventually(t, "malformed recorded", func() bool { return len(r.st.gapList()) == 1 })
	if g := r.st.gapList()[0]; g.Cause != store.CauseMalformed || !strings.Contains(g.DedupeKey, "unsequenced") {
		t.Fatalf("%+v", g)
	}
}

func TestWriterLogsNothingOnASuccessfulRun(t *testing.T) {
	r := newRig(t, testConfig(), nil)
	publishTracks(t, r.src, 3)
	eventually(t, "written", func() bool { return r.st.count("telemetry") == 3 })
	if l := r.logText(); strings.Contains(l, "WARN") || strings.Contains(l, "ERROR") {
		t.Fatalf("warnings on a clean run: %s", l)
	}
}

var errCheckViolation = errors.New("check violation")
