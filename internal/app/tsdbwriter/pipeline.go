package tsdbwriter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Store writes a batch in one transaction and reads back how far a
// stream was written (store.TSWriter).
type Store interface {
	Write(ctx context.Context, b store.WriteBatch) (store.Written, error)
	// Position is the highest stream sequence written; false when none
	// is recorded.
	Position(ctx context.Context, stream string) (uint64, bool, error)
}

// Counter names (E-09), each on the status line and /metrics.
const (
	CounterRowsWritten       = "rows_written"
	CounterDedupeHits        = "dedupe_hits"
	CounterBatches           = "batches"
	CounterSpills            = "spills"
	CounterDroppedRows       = "dropped_rows"
	CounterGaps              = "gaps"
	CounterMessagesMalformed = "messages_malformed"
	CounterRowsRejected      = "rows_rejected"
	CounterRejectedUnrecord  = "rejected_unrecorded"
	CounterWriteFailed       = "write_failed"
	CounterFetchFailed       = "fetch_failed"
	CounterHoleCheckFailed   = "hole_check_failed"
	CounterAckFailed         = "ack_failed"
	CounterRedelivered       = "redelivered_while_queued"
	CounterPositionFailed    = "position_read_failed"
	CounterSkippedPrefix     = "skipped_"
)

// Pipeline states.
const (
	StateOK           = "ok"
	StateSpilling     = "spilling"
	StateWriteFailing = "write_failing"
)

// Config bounds one pipeline.
type Config struct {
	// BatchMaxRows and BatchMaxWait: a batch is written at this many rows
	// or when its oldest message has waited this long (1000, 1 s).
	BatchMaxRows int
	BatchMaxWait time.Duration
	// QueueMaxAge bounds the queue while writes succeed (10 s,
	// USSP_WRITER_QUEUE_S): at it the consumer stops pulling and the
	// stream holds the rest. HoldMaxRows bounds it always, also while
	// the database is down (50 000, USSP_WRITER_HOLD_ROWS; B-07).
	QueueMaxAge time.Duration
	HoldMaxRows int
	// FetchMax messages per pull, waiting at most FetchWait.
	FetchMax  int
	FetchWait time.Duration
	// WriteTimeout bounds one transaction.
	WriteTimeout time.Duration
	// RetryMin and RetryMax bound the doubling wait after a failure.
	RetryMin, RetryMax time.Duration
	// AckWait is the consumer's; a message held longer than half of it
	// is kept from redelivery with InProgress.
	AckWait time.Duration
	// DedupeWindow is how long a msg_id is remembered in memory (10 s);
	// DedupeMax bounds the remembered ids (E-10). The unique index is the
	// dedupe beyond the window.
	DedupeWindow time.Duration
	DedupeMax    int
}

// DefaultConfig is the brief's: batches of 1000 rows or 1 s, a 10 s
// queue, 50 000 rows held while the database is down.
func DefaultConfig() Config {
	return Config{
		BatchMaxRows: 1000, BatchMaxWait: time.Second, QueueMaxAge: 10 * time.Second, HoldMaxRows: 50_000,
		FetchMax: 500, FetchWait: 200 * time.Millisecond, WriteTimeout: 10 * time.Second,
		RetryMin: 250 * time.Millisecond, RetryMax: 5 * time.Second, AckWait: 30 * time.Second,
		DedupeWindow: 10 * time.Second, DedupeMax: 200_000,
	}
}

// item is one delivered message in the queue.
type item struct {
	msg     bus.Msg
	seq     uint64
	dec     Decoded
	n       int
	gaps    []store.Gap
	queued  time.Time
	touched time.Time
	// rejected is set once the database refused the item's rows and a
	// rejected gap replaced them.
	rejected bool
}

// Pipeline is one stream: a durable consumer over the whole stream, a
// bounded queue of whole messages and a batching writer. A message is
// acknowledged only after the transaction holding its rows, any gap it
// carries and the stream position commits (B-05), in delivery order.
//
// Holes are records, never silence (B-13): a step in the delivered
// sequences whose messages the stream no longer holds is a
// stream_removed gap, committed with the message after the hole before
// that message is acknowledged, counted in dropped_rows and logged with
// the stream's subject and the time range around it. The writer itself
// never drops a message it was delivered: at its bound it stops pulling
// and the stream holds the rest; what the stream's limits remove before
// the writer reads it is the gap. At start a consumer whose ack floor is
// beyond the position written (writer_positions) lost messages to a
// purge or the stream's limits while the writer was down: the same gap.
type Pipeline struct {
	Stream   Stream
	Source   bus.Source
	Store    Store
	Config   Config
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
	// Observer, when set, receives every committed batch.
	Observer Observer
	// DataError tells a refusal of the data from a failure to write
	// (default store.IsDataError).
	DataError func(error) bool

	mu       sync.Mutex
	queue    []*item
	rows     int
	lastSeq  uint64
	lastAt   *time.Time
	spilling bool
	failing  bool
	failedAt time.Time
	posKnown bool
	floor    uint64
	// lastKeepAlive is when keepAliveDue last ran keepAlive.
	lastKeepAlive time.Time
	started       bool
	wake          chan struct{}
	once          sync.Once

	// seen is the in-memory dedupe window (the pull loop's only).
	seen      map[string]time.Time
	seenOrder []seenEntry
	// lastLog rate-limits the repeated warnings (one per key per
	// logEvery).
	logMu   sync.Mutex
	lastLog map[string]time.Time
}

// logEvery is the shortest interval between two repeated warnings.
const logEvery = 10 * time.Second

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Pipeline) init() {
	p.once.Do(func() {
		p.wake = make(chan struct{}, 1)
		p.seen = map[string]time.Time{}
	})
}

func (p *Pipeline) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Snapshot is one pipeline's queue now.
type Snapshot struct {
	Stream        string  `json:"stream"`
	State         string  `json:"state"`
	QueueRows     int     `json:"queue_rows"`
	QueueMessages int     `json:"queue_messages"`
	QueueAgeS     float64 `json:"queue_age_s"`
	LastSeq       uint64  `json:"last_seq"`
}

// Snapshot reports the pipeline now.
func (p *Pipeline) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Snapshot{Stream: p.Stream.Name, State: StateOK, QueueRows: p.rows, QueueMessages: len(p.queue), LastSeq: p.lastSeq}
	switch {
	case p.spilling:
		s.State = StateSpilling
	case p.failing:
		s.State = StateWriteFailing
	}
	if len(p.queue) > 0 {
		s.QueueAgeS = p.now().Sub(p.queue[0].queued).Seconds()
	}
	return s
}

// fullLocked: the queue is at HoldMaxRows rows or messages, or, while
// writes succeed, its oldest message has waited QueueMaxAge.
func (p *Pipeline) fullLocked(now time.Time) bool {
	if p.rows >= p.Config.HoldMaxRows || len(p.queue) >= p.Config.HoldMaxRows {
		return true
	}
	return !p.failing && len(p.queue) > 0 && now.Sub(p.queue[0].queued) >= p.Config.QueueMaxAge
}

// Run pulls and writes until ctx ends; what is still queued then is
// unacknowledged and is delivered again to the next writer.
func (p *Pipeline) Run(ctx context.Context) {
	p.init()
	var wg sync.WaitGroup
	wg.Go(func() { p.pull(ctx) })
	wg.Go(func() { p.write(ctx) })
	wg.Wait()
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (p *Pipeline) warn(msg string, attrs ...slog.Attr) {
	p.Logger.LogAttrs(context.Background(), slog.LevelWarn, msg, append([]slog.Attr{slog.String("stream", p.Stream.Name)}, attrs...)...)
}

// warnLimited is warn at most once per logEvery for one message: a
// dependency that stays down is said, not shouted.
func (p *Pipeline) warnLimited(msg string, attrs ...slog.Attr) {
	now := time.Now()
	p.logMu.Lock()
	if p.lastLog == nil {
		p.lastLog = map[string]time.Time{}
	}
	if now.Sub(p.lastLog[msg]) < logEvery {
		p.logMu.Unlock()
		return
	}
	p.lastLog[msg] = now
	p.logMu.Unlock()
	p.warn(msg, attrs...)
}

// start reads the consumer's ack floor: deliveries are measured from it.
func (p *Pipeline) start(ctx context.Context) bool {
	for ctx.Err() == nil {
		floor, err := p.Source.AckFloor(ctx)
		if err == nil {
			p.mu.Lock()
			p.lastSeq, p.floor, p.started = floor, floor, true
			p.mu.Unlock()
			return true
		}
		p.Counters.Inc(CounterFetchFailed)
		p.warnLimited("consumer unavailable; messages wait in the stream", slog.String("error", err.Error()))
		sleep(ctx, p.Config.RetryMax)
	}
	return false
}

func (p *Pipeline) pull(ctx context.Context) {
	if !p.start(ctx) {
		return
	}
	// Nothing is pulled before the written position has been compared
	// with the floor (checkPosition): a purge while the writer was down
	// is recorded first.
	for ctx.Err() == nil {
		p.mu.Lock()
		known := p.posKnown
		p.mu.Unlock()
		if known {
			break
		}
		sleep(ctx, p.Config.RetryMin)
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		p.mu.Lock()
		full := p.fullLocked(p.now())
		// Never fetch past the bound: one message is at most one row.
		room := p.Config.HoldMaxRows - max(p.rows, len(p.queue))
		p.mu.Unlock()
		p.setSpilling(full)
		if full {
			select {
			case <-ctx.Done():
			case <-tick.C:
			}
			continue
		}
		msgs, err := p.Source.Fetch(ctx, min(p.Config.FetchMax, room), p.Config.FetchWait)
		if err != nil {
			if ctx.Err() == nil {
				p.Counters.Inc(CounterFetchFailed)
				p.warnLimited("fetch failed; messages wait in the stream", slog.String("error", err.Error()))
				sleep(ctx, p.Config.RetryMin)
			}
			continue
		}
		if len(msgs) > 0 {
			p.take(ctx, msgs)
		}
	}
}

func (p *Pipeline) setSpilling(on bool) {
	p.mu.Lock()
	was := p.spilling
	p.spilling = on
	rows, msgs, failing := p.rows, len(p.queue), p.failing
	p.mu.Unlock()
	switch {
	case on && !was:
		p.Counters.Inc(CounterSpills)
		p.warn("writer queue at its bound: not pulling, the stream holds the rest", slog.Int("queue_rows", rows),
			slog.Int("queue_messages", msgs), slog.Bool("database_failing", failing))
	case !on && was:
		p.Logger.Info("writer queue below its bound: pulling again", slog.String("stream", p.Stream.Name), slog.Int("queue_rows", rows))
	}
}

// remember checks id against the dedupe window and remembers it; true
// when it was seen within the window. The window holds at most DedupeMax
// ids, the oldest leaving first (E-10).
func (p *Pipeline) remember(id string, now time.Time) bool {
	for len(p.seenOrder) > 0 {
		h := p.seenOrder[0]
		if now.Sub(h.at) < p.Config.DedupeWindow && len(p.seenOrder) < p.Config.DedupeMax {
			break
		}
		p.seenOrder = p.seenOrder[1:]
		if p.seen[h.id].Equal(h.at) {
			delete(p.seen, h.id)
		}
	}
	if at, ok := p.seen[id]; ok && now.Sub(at) < p.Config.DedupeWindow {
		return true
	}
	p.seen[id] = now
	p.seenOrder = append(p.seenOrder, seenEntry{id: id, at: now})
	return false
}

type seenEntry struct {
	id string
	at time.Time
}

// take queues fetched messages in delivery order. A message already
// queued (redelivered while held) replaces its earlier delivery. A step
// in the sequences is checked against the stream; what it no longer
// holds is a stream_removed gap carried by the message after the hole.
// When the check fails the messages are given back (Nak).
func (p *Pipeline) take(ctx context.Context, msgs []bus.Msg) {
	now := p.now()
	type fresh struct {
		m    bus.Msg
		seq  uint64
		jump int
	}
	p.mu.Lock()
	last := p.lastSeq
	queued := make(map[uint64]*item, len(p.queue))
	for _, it := range p.queue {
		queued[it.seq] = it
	}
	p.mu.Unlock()
	var news []fresh
	var jumps []bus.Jump
	for _, m := range msgs {
		seq, err := m.StreamSeq()
		if err != nil || seq == 0 {
			news = append(news, fresh{m: m, jump: -1})
			continue
		}
		if it, ok := queued[seq]; ok {
			p.Counters.Inc(CounterRedelivered)
			p.mu.Lock()
			it.msg, it.touched = m, now
			p.mu.Unlock()
			continue
		}
		if seq <= last {
			// Delivered again after an earlier delivery was written and
			// acknowledged late, or before a restart: the unique index
			// dedupes it.
			news = append(news, fresh{m: m, seq: seq, jump: -1})
			continue
		}
		f := fresh{m: m, seq: seq, jump: -1}
		if seq > last+1 {
			f.jump = len(jumps)
			jumps = append(jumps, bus.Jump{After: last, Before: seq})
		}
		last = seq
		news = append(news, f)
	}
	var holes []bus.Hole
	if len(jumps) > 0 {
		var err error
		holes, err = p.Source.Holes(ctx, jumps)
		if err != nil || len(holes) != len(jumps) {
			p.Counters.Inc(CounterHoleCheckFailed)
			p.warn("could not check the stream for a hole; the messages are given back", slog.Any("error", err))
			for _, f := range news {
				if err := f.m.Nak(); err != nil {
					p.Counters.Inc(CounterAckFailed)
				}
			}
			sleep(ctx, p.Config.RetryMin)
			return
		}
	}
	p.mu.Lock()
	lastAt := p.lastAt
	p.mu.Unlock()
	items := make([]*item, 0, len(news))
	added := 0
	for _, f := range news {
		it := p.decode(f.m, f.seq, now)
		if f.jump >= 0 {
			if h := holes[f.jump]; h.Count > 0 {
				it.gaps = append(it.gaps, p.removedGap(h, lastAt, &it.dec.CapturedAt))
			}
		}
		if !it.dec.CapturedAt.IsZero() {
			at := it.dec.CapturedAt
			lastAt = &at
		}
		items = append(items, it)
		added += it.n
	}
	p.mu.Lock()
	p.queue = append(p.queue, items...)
	p.rows += added
	p.lastSeq = max(p.lastSeq, last)
	p.lastAt = lastAt
	p.mu.Unlock()
	p.signal()
}

// removedGap is the record of a hole: messages the stream removed
// before the writer wrote them. One message is one row on every stream
// the writer reads, so the count is rows; they are counted in
// dropped_rows and logged with the subject and the time range.
func (p *Pipeline) removedGap(h bus.Hole, after, before *time.Time) store.Gap {
	g := store.Gap{
		DedupeKey: fmt.Sprintf("%s:%s:%d-%d", store.CauseStreamRemoved, p.Stream.Name, h.FromSeq, h.ToSeq),
		Stream:    p.Stream.Name, Subject: p.Stream.Subject, FromSeq: h.FromSeq, ToSeq: h.ToSeq, Cause: store.CauseStreamRemoved,
		Count: int64(h.Count), CountUnit: store.UnitRows, AfterAt: utcPtr(after), BeforeAt: utcPtr(before),
		Detail: "the stream no longer held these messages when the writer reached them: aged out, over a stream limit, purged or deleted",
	}
	p.Counters.Add(CounterDroppedRows, h.Count)
	attrs := []slog.Attr{slog.String("subject", g.Subject), slog.Uint64("from_seq", g.FromSeq), slog.Uint64("to_seq", g.ToSeq),
		slog.Uint64("dropped_rows", h.Count)}
	if g.AfterAt != nil {
		attrs = append(attrs, slog.Time("after_captured_at", *g.AfterAt))
	}
	if g.BeforeAt != nil {
		attrs = append(attrs, slog.Time("before_captured_at", *g.BeforeAt))
	}
	p.Logger.LogAttrs(context.Background(), slog.LevelError, "rows lost before they were written: the stream removed them; recorded in writer_gaps",
		append([]slog.Attr{slog.String("stream", p.Stream.Name)}, attrs...)...)
	return g
}

// unknownPositionGap is the record of messages 1..floor that the
// consumer acknowledged while the database holds no position for the
// stream: counted in dropped_rows and logged at error.
func (p *Pipeline) unknownPositionGap(floor uint64) store.Gap {
	p.Counters.Add(CounterDroppedRows, floor)
	p.Logger.LogAttrs(context.Background(), slog.LevelError,
		"the consumer acknowledged messages the database records no position for; recorded in writer_gaps",
		slog.String("stream", p.Stream.Name), slog.Uint64("ack_floor", floor), slog.Uint64("dropped_rows", floor))
	return store.Gap{
		DedupeKey: fmt.Sprintf("%s:%s:1-%d", store.CausePositionUnknown, p.Stream.Name, floor),
		Stream:    p.Stream.Name, Subject: p.Stream.Subject, FromSeq: 1, ToSeq: floor, Cause: store.CausePositionUnknown,
		Count: int64(floor), CountUnit: store.UnitRows,
		Detail: fmt.Sprintf("the consumer acknowledged up to %d but the database records no position: a database restored or re-created under a running consumer", floor),
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// decode makes one message an item: its rows, nothing (a skip or a
// duplicate inside the window, counted), or a malformed gap.
func (p *Pipeline) decode(m bus.Msg, seq uint64, now time.Time) *item {
	it := &item{msg: m, seq: seq, queued: now, touched: now}
	var err error
	if seq == 0 {
		err = errNoSequence
	} else {
		it.dec, err = p.Stream.Decode(m.Data())
	}
	switch {
	case err != nil:
		p.Counters.Inc(CounterMessagesMalformed)
		it.dec = Decoded{}
		it.gaps = []store.Gap{{
			DedupeKey: fmt.Sprintf("%s:%s:%d", store.CauseMalformed, p.Stream.Name, seq), Stream: p.Stream.Name, Subject: m.Subject(),
			FromSeq: seq, ToSeq: seq, Cause: store.CauseMalformed, Count: 1, CountUnit: store.UnitMessages, Detail: truncate(err.Error()),
		}}
		if seq == 0 {
			it.gaps[0].DedupeKey = fmt.Sprintf("%s:%s:unsequenced:%d", store.CauseMalformed, p.Stream.Name, now.UnixNano())
		}
		p.warn("message not readable; recorded in writer_gaps", slog.String("subject", m.Subject()), slog.Uint64("stream_seq", seq),
			slog.String("error", err.Error()))
	case p.remember(it.dec.MsgID, now):
		p.Counters.Inc(CounterDedupeHits)
		it.dec.Rows = nil
	case it.dec.Skip != "":
		p.Counters.Inc(CounterSkippedPrefix + it.dec.Skip)
	}
	it.n = len(it.dec.Rows)
	return it
}

var errNoSequence = errors.New("message without a stream sequence")

func truncate(s string) string {
	const maxDetail = 500
	if len(s) > maxDetail {
		return s[:maxDetail]
	}
	return s
}

// checkPosition compares, once before the first write, the ack floor
// the consumer started from with the position written. The floor moves
// beyond what the writer wrote only when the stream removed messages
// under it (a purge, its limits) while the writer was down: recorded as
// a stream_removed gap with the position raised to the floor, in one
// transaction. With no position at all and a floor above zero the
// database lost what was acknowledged: a position_unknown gap from 1 to
// the floor. Until it is done nothing is written or pulled.
func (p *Pipeline) checkPosition(ctx context.Context) bool {
	p.mu.Lock()
	started, floor := p.started, p.floor
	p.mu.Unlock()
	if !started {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, p.Config.WriteTimeout)
	pos, known, err := p.Store.Position(rctx, p.Stream.Name)
	cancel()
	if err != nil {
		p.Counters.Inc(CounterPositionFailed)
		p.markFailing()
		p.warnLimited("written position unreadable; nothing is written until it is", slog.String("error", err.Error()))
		return false
	}
	if known && floor > pos {
		g := p.removedGap(bus.Hole{FromSeq: pos + 1, ToSeq: floor, Count: floor - pos}, nil, nil)
		g.Detail = "the consumer's acknowledgement floor is beyond the position written: the stream removed these messages (a purge or its limits) while the writer was not reading"
		wctx, cancel := context.WithTimeout(ctx, p.Config.WriteTimeout)
		w, err := p.Store.Write(wctx, store.WriteBatch{Stream: p.Stream.Name, Gaps: []store.Gap{g}, Position: floor})
		cancel()
		if err != nil {
			p.failed(err)
			return false
		}
		p.Counters.Add(CounterGaps, uint64(w.Gaps))
	}
	if !known && floor > 0 {
		// The consumer acknowledged messages the database records no
		// position for (restored from an older backup, re-created): those
		// rows are in no database. A hole, not a clean start (B-13).
		g := p.unknownPositionGap(floor)
		wctx, cancel := context.WithTimeout(ctx, p.Config.WriteTimeout)
		w, err := p.Store.Write(wctx, store.WriteBatch{Stream: p.Stream.Name, Gaps: []store.Gap{g}, Position: floor})
		cancel()
		if err != nil {
			p.failed(err)
			return false
		}
		p.Counters.Add(CounterGaps, uint64(w.Gaps))
	}
	p.mu.Lock()
	p.posKnown = true
	p.failing = false
	p.mu.Unlock()
	return true
}

func (p *Pipeline) markFailing() {
	p.mu.Lock()
	if !p.failing {
		p.failing, p.failedAt = true, p.now()
	}
	p.mu.Unlock()
}

func (p *Pipeline) ready(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return false
	}
	return p.rows >= p.Config.BatchMaxRows || now.Sub(p.queue[0].queued) >= p.Config.BatchMaxWait
}

// next is the batch at the head of the queue: whole messages up to
// BatchMaxRows rows, at least one. The items stay queued until written.
func (p *Pipeline) next() []*item {
	p.mu.Lock()
	defer p.mu.Unlock()
	var batch []*item
	rows := 0
	for _, it := range p.queue {
		if len(batch) > 0 && rows+it.n > p.Config.BatchMaxRows {
			break
		}
		batch = append(batch, it)
		rows += it.n
	}
	return batch
}

func (p *Pipeline) write(ctx context.Context) {
	tick := time.NewTicker(max(p.Config.BatchMaxWait/5, 10*time.Millisecond))
	defer tick.Stop()
	retry := p.Config.RetryMin
	checked := false
	for ctx.Err() == nil {
		if !checked {
			if !p.checkPosition(ctx) {
				sleep(ctx, retry)
				retry = min(2*retry, p.Config.RetryMax)
				continue
			}
			checked, retry = true, p.Config.RetryMin
		}
		// Every held message is kept alive while it waits, whether the
		// writes fail or succeed slowly: a queue deeper than AckWait of
		// writes would otherwise be redelivered from its tail, and every
		// fetch would bring messages already held (audit S4).
		p.keepAliveDue()
		if !p.ready(p.now()) {
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
			case <-tick.C:
			}
			continue
		}
		batch := p.next()
		start := p.now()
		err := p.writeBatch(ctx, batch)
		if err == nil {
			p.observe(batch, p.now().Sub(start))
			retry = p.Config.RetryMin
			p.done(batch)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		p.failed(err)
		p.keepAlive()
		sleep(ctx, retry)
		retry = min(2*retry, p.Config.RetryMax)
	}
}

// Observer receives each committed batch's size and latency (the
// writer's Prometheus histograms).
type Observer interface {
	ObserveBatch(stream string, rows int, latency time.Duration)
}

func (p *Pipeline) observe(batch []*item, d time.Duration) {
	if p.Observer == nil {
		return
	}
	n := 0
	for _, it := range batch {
		n += it.n
	}
	p.Observer.ObserveBatch(p.Stream.Name, n, d)
}

// batchOf is the transaction of a batch: rows by table, gaps, and the
// highest sequence as the position.
func (p *Pipeline) batchOf(items []*item) store.WriteBatch {
	b := store.WriteBatch{Stream: p.Stream.Name, Rows: map[*store.CopyTable][][]any{}}
	for _, it := range items {
		for _, r := range it.dec.Rows {
			b.Rows[r.Table] = append(b.Rows[r.Table], r.Values)
		}
		b.Gaps = append(b.Gaps, it.gaps...)
		b.Position = max(b.Position, it.seq)
	}
	return b
}

func (p *Pipeline) store(ctx context.Context, items []*item) (store.Written, error) {
	wctx, cancel := context.WithTimeout(ctx, p.Config.WriteTimeout)
	defer cancel()
	return p.Store.Write(wctx, p.batchOf(items))
}

// writeBatch writes batch in one transaction. When the database refuses
// the data itself, each message is written alone, and a message refused
// alone is replaced by a rejected gap carrying its count: one bad row
// never holds the rest back and is never dropped unrecorded. When the
// gap record is refused too, the batch is a failed write: nothing is
// acknowledged and it is retried with the backoff (audit S8).
func (p *Pipeline) writeBatch(ctx context.Context, batch []*item) error {
	isData := p.DataError
	if isData == nil {
		isData = store.IsDataError
	}
	w, err := p.store(ctx, batch)
	if err == nil {
		p.count(w)
		return nil
	}
	if !isData(err) {
		return err
	}
	for _, it := range batch {
		w, err := p.store(ctx, []*item{it})
		if err == nil {
			p.count(w)
			continue
		}
		if !isData(err) {
			return err
		}
		if err := p.reject(ctx, it, err); err != nil {
			return err
		}
	}
	return nil
}

// reject replaces the item's rows by a rejected gap (once) and writes
// it. A gap the database refuses too is returned: the message is then
// neither acknowledged nor dropped, and stays at the head of the queue.
func (p *Pipeline) reject(ctx context.Context, it *item, cause error) error {
	if !it.rejected {
		it.rejected = true
		p.Counters.Add(CounterRowsRejected, uint64(it.n))
		p.Logger.LogAttrs(ctx, slog.LevelError, "the database refused a message's rows; recorded in writer_gaps",
			slog.String("stream", p.Stream.Name), slog.String("subject", it.msg.Subject()), slog.Uint64("stream_seq", it.seq),
			slog.String("error", cause.Error()))
		count, unit := int64(it.n), store.UnitRows
		if count == 0 {
			count, unit = 1, store.UnitMessages
		}
		it.dec.Rows = nil
		it.gaps = append(it.gaps, store.Gap{
			DedupeKey: fmt.Sprintf("%s:%s:%d", store.CauseRejected, p.Stream.Name, it.seq), Stream: p.Stream.Name, Subject: it.msg.Subject(),
			FromSeq: it.seq, ToSeq: it.seq, Cause: store.CauseRejected, Count: count, CountUnit: unit, Detail: truncate(cause.Error()),
		})
	}
	w, err := p.store(ctx, []*item{it})
	if err != nil {
		p.Counters.Inc(CounterRejectedUnrecord)
		p.warnLimited("a rejected message's gap record was refused too; it is not acknowledged and is retried",
			slog.Uint64("stream_seq", it.seq), slog.String("error", err.Error()))
		return fmt.Errorf("the rejected gap of stream sequence %d was refused: %w", it.seq, err)
	}
	p.count(w)
	return nil
}

func (p *Pipeline) count(w store.Written) {
	p.Counters.Add(CounterRowsWritten, uint64(max(w.Inserted, 0)))
	p.Counters.Add(CounterDedupeHits, uint64(max(w.Duplicates, 0)))
	p.Counters.Add(CounterGaps, uint64(max(w.Gaps, 0)))
}

// done acknowledges a committed batch in delivery order and removes it.
func (p *Pipeline) done(batch []*item) {
	p.Counters.Inc(CounterBatches)
	msgs := make([]bus.Msg, len(batch))
	n := 0
	p.mu.Lock()
	for i, it := range batch {
		msgs[i] = it.msg
		n += it.n
	}
	p.mu.Unlock()
	for _, m := range msgs {
		if err := m.Ack(); err != nil {
			p.Counters.Inc(CounterAckFailed) // stored; a redelivery writes nothing twice
		}
	}
	p.mu.Lock()
	p.queue = p.queue[len(batch):]
	p.rows = max(p.rows-n, 0)
	recovered, failedFor := p.failing, p.now().Sub(p.failedAt)
	p.failing = false
	queued := p.rows
	p.mu.Unlock()
	if recovered {
		p.Logger.Info("writes resumed", slog.String("stream", p.Stream.Name), slog.Float64("failed_for_s", failedFor.Seconds()),
			slog.Int("queue_rows", queued))
	}
}

func (p *Pipeline) failed(err error) {
	p.Counters.Inc(CounterWriteFailed)
	p.markFailing()
	p.mu.Lock()
	rows := p.rows
	p.mu.Unlock()
	p.warnLimited("write failed; the batch is retried and nothing is acknowledged", slog.Int("queue_rows", rows),
		slog.String("error", err.Error()))
}

// keepAliveDue runs keepAlive at most every AckWait/4: a message is
// then kept in progress between AckWait/2 and 3/4 AckWait after it was
// last touched, before the stream delivers it again.
func (p *Pipeline) keepAliveDue() {
	now := p.now()
	p.mu.Lock()
	due := now.Sub(p.lastKeepAlive) >= p.Config.AckWait/4
	if due {
		p.lastKeepAlive = now
	}
	p.mu.Unlock()
	if due {
		p.keepAlive()
	}
}

// keepAlive keeps held messages from redelivery while they wait.
func (p *Pipeline) keepAlive() {
	now := p.now()
	var due []bus.Msg
	p.mu.Lock()
	for _, it := range p.queue {
		if now.Sub(it.touched) >= p.Config.AckWait/2 {
			due = append(due, it.msg)
			it.touched = now
		}
	}
	p.mu.Unlock()
	for _, m := range due {
		if err := m.InProgress(); err != nil {
			p.Counters.Inc(CounterAckFailed)
		}
	}
}
