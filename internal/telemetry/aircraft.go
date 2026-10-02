package telemetry

import (
	"math"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/rid"
	"github.com/rootxkit/uspace-core/serial"
)

// MaxAircraft bounds the aircraft one process follows (E-10): beyond it
// a sample of a new aircraft is refused (refused_capacity, counted) and
// nothing held is evicted. 10 000 is ten times the design point of 1000
// drones (spec 05 §7).
const MaxAircraft = 10_000

// maxDedupe bounds the (serial, epoch, seq) keys remembered per aircraft
// (E-10): the live and backlog rates of the policy over its window fit
// well inside; beyond it the oldest is forgotten and counted.
const maxDedupe = 4096

// aircraftKey identifies the samples of one aircraft of one client: the
// client and the serial's fold key (G-05: "sn-1" and "SN-1" are one
// aircraft).
type aircraftKey struct{ client, fold string }

func keyOf(clientID, sn string) aircraftKey {
	return aircraftKey{client: clientID, fold: serial.FoldKey(sn)}
}

// replayKey is one sample of an aircraft as the client counts it: its
// epoch (the client's run of its seq counter; empty when it sends none)
// and its seq.
type replayKey struct {
	epoch string
	seq   int64
}

func replayKeyOf(f *Frame) replayKey { return replayKey{epoch: f.Epoch, seq: f.Seq} }

// seenEntry is a sample taken: when (wall clock) and its own time. A
// replay is the same sample sent again, so its ts is the same; the same
// key with another ts is a client that reused the seq (a restart
// without an epoch), not a replay.
type seenEntry struct {
	at, ts time.Time
}

// String is the key as the flights binder and the logs name it.
func (k aircraftKey) String() string { return k.client + "|" + k.fold }

// bucket is a token bucket on the wall clock.
type bucket struct {
	tokens float64
	last   time.Time
}

// take takes one token at now with rate per second and burst; false
// when the bucket is empty.
func (b *bucket) take(now time.Time, rate, burst float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else if d := now.Sub(b.last).Seconds(); d > 0 {
		b.tokens = math.Min(burst, b.tokens+d*rate)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// stream is the state of one direction of an aircraft's samples: live
// or backlog. They never share it (T-04: a replayed history moves
// neither the live clock nor the live altitude hold).
type stream struct {
	held     time.Time // the newest own time taken (T-03, T-13)
	hasHeld  bool
	rate     bucket
	altitude *rid.AltitudeSelector
	// lastPos and lastAt are the position and placement of the last
	// sample taken (the teleport check, 06 T3).
	lastPos core.LatLon
	lastAt  time.Time
	hasLast bool
}

// aircraft is everything the ingest holds about one aircraft of one
// client. mu serialises its samples: they are ordered, placed and handed
// to the publisher under it, so its tracks leave in placement order.
type aircraft struct {
	mu sync.Mutex

	key    aircraftKey
	serial string // as the client last sent it

	// owner is the session (WebSocket connection) that streams it now
	// and ownerOrder the order that session was opened in; 0 is none
	// (B-14: a later session replaces an earlier one, and the earlier
	// one's teardown clears only what it still owns).
	owner      uint64
	ownerOrder uint64

	anchor  *anchor
	live    stream
	backlog stream

	// seenMu guards the replay window alone: the outbox settles a
	// sample under it (never under mu, which Take holds while it hands
	// samples to the outbox).
	seenMu sync.Mutex
	// seen is the (serial, epoch, seq) replay window: the key -> when it
	// was taken and its ts; pending are those taken and not yet handed
	// to the bus.
	seen      map[replayKey]seenEntry
	seenOrder []replayKey
	pending   map[replayKey]bool

	ident      *core.Identification // the last identification published
	identTrack string               // for this track

	lastRx time.Time // the last sample taken, wall clock
}

func newAircraft(key aircraftKey, sn string) *aircraft {
	return &aircraft{key: key, serial: sn, seen: map[replayKey]seenEntry{}, pending: map[replayKey]bool{}}
}

// stream is the live or the backlog stream.
func (a *aircraft) stream(backlog bool) *stream {
	if backlog {
		return &a.backlog
	}
	return &a.live
}

// duplicate reports whether the sample k with own time ts was taken
// within window of now, and whether it is still on its way to the bus;
// reused is the same key taken with another ts (not a replay). It
// forgets what is older than the window.
func (a *aircraft) duplicate(k replayKey, ts, now time.Time, window time.Duration) (dup, pending, reused bool) {
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	a.prune(now, window)
	e, ok := a.seen[k]
	if ok && !e.ts.Equal(ts) {
		return false, false, true
	}
	return ok, ok && a.pending[k], false
}

// prune forgets what is older than the window, never a sample still
// pending. Called with seenMu held.
func (a *aircraft) prune(now time.Time, window time.Duration) {
	n := 0
	for n < len(a.seenOrder) {
		k := a.seenOrder[n]
		e, ok := a.seen[k]
		if ok && (now.Sub(e.at) <= window || a.pending[k]) {
			break
		}
		if ok {
			delete(a.seen, k)
		}
		n++
	}
	a.seenOrder = a.seenOrder[n:]
}

// remember records k with own time ts as taken at now and pending;
// when the window is full its oldest entry is forgotten to make room
// (counted).
func (a *aircraft) remember(k replayKey, ts, now time.Time, counters *core.Counters) {
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	if len(a.seenOrder) >= maxDedupe {
		old := a.seenOrder[0]
		a.seenOrder = a.seenOrder[1:]
		if !a.pending[old] {
			delete(a.seen, old)
		}
		counters.Inc(CounterDedupeEvicted)
	}
	a.seen[k] = seenEntry{at: now, ts: ts}
	a.seenOrder = append(a.seenOrder, k)
	a.pending[k] = true
}

// landed settles a pending key: handed, it stays remembered (a replay
// publishes nothing twice, B-05); not handed, it is forgotten, so the
// client's next copy is taken.
func (a *aircraft) landed(k replayKey, handed bool) {
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	delete(a.pending, k)
	if !handed {
		delete(a.seen, k)
	}
}

// inflight reports whether a sample of the aircraft is still on its way.
func (a *aircraft) inflight() bool {
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	return len(a.pending) > 0
}

// fleet holds the aircraft of the process, at most MaxAircraft.
type fleet struct {
	mu  sync.Mutex
	all map[aircraftKey]*aircraft
	max int
}

func newFleet(maxN int) *fleet {
	if maxN <= 0 {
		maxN = MaxAircraft
	}
	return &fleet{all: map[aircraftKey]*aircraft{}, max: maxN}
}

// get returns the aircraft of key, creating it; nil when the fleet is
// full.
func (f *fleet) get(key aircraftKey, sn string) *aircraft {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.all[key]; ok {
		return a
	}
	if len(f.all) >= f.max {
		return nil
	}
	a := newAircraft(key, sn)
	f.all[key] = a
	return a
}

// list is every aircraft held, for the tick.
func (f *fleet) list() []*aircraft {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*aircraft, 0, len(f.all))
	for _, a := range f.all {
		out = append(out, a)
	}
	return out
}

// forget removes a, if it is still the one held under its key.
func (f *fleet) forget(a *aircraft) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.all[a.key] == a {
		delete(f.all, a.key)
	}
}

func (f *fleet) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.all)
}
