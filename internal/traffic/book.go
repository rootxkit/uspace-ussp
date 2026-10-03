package traffic

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Counters of the alert book.
const (
	CounterBookTaken     = "book_alerts"
	CounterBookOverBound = "book_over_bound"
	CounterBookSubFull   = "book_subscriber_full"
	CounterBookOlder     = "book_older_than_held"
	CounterBookSubsBound = "book_subscribers_over_bound"
	// CounterBookMissedOverBound counts subscribers told to resync: more
	// clears waited for them than MaxMissedClears.
	CounterBookMissedOverBound = "book_missed_clears_over_bound"
	// MaxMissedClears bounds the clears kept for one full subscriber.
	MaxMissedClears       = 4 * DefaultSubscriberLen
	DefaultMaxBookAlerts  = 100_000
	DefaultSubscriberLen  = 256
	MaxBookSubscribers    = 10_000
	DefaultUnrefreshedAge = 5 * time.Second
)

// Entry is one alert as traffic-ws holds it: what it routes and shows
// by, and the message itself.
type Entry struct {
	AlertID  string
	Kind     string
	Severity core.Severity
	State    string
	FlightID string
	IntentID string
	Acked    bool
	// Escalated is set once api escalated the alert (escalated_at).
	Escalated bool
	Cell5     string
	// UpdatedAt is the alert's own updated_at; Seen when this process
	// last received it.
	UpdatedAt time.Time
	Seen      time.Time
	// Message is the whole alert/v1 message; Body its body.
	Message json.RawMessage
	Body    json.RawMessage
	// LoSStartS ranks active proximity alerts as the monitor does
	// (detail los_start_s); 0 for others.
	LoSStartS float64
}

// Book is every active alert traffic-ws heard on alrt.v1, by alert id,
// and the subscribers that receive each one as it comes. A cleared
// alert leaves the book once it is delivered; an active one is never
// removed for room (past the bound a new alert is refused and counted,
// C-18). Safe for concurrent use.
type Book struct {
	Counters *core.Counters
	Max      int

	mu     sync.Mutex
	alerts map[string]Entry
	subs   map[int]*subscriber
	next   int
	once   sync.Once
}

// subscriber is one subscription: its channel, and the clears it could
// not take (a clear leaves the book, so nothing would carry it again).
type subscriber struct {
	ch     chan Entry
	kick   chan struct{}
	missed map[string]Entry
	resync bool
}

// Subscription is one subscriber of the book: C carries every alert put
// from its start; Missed is signalled when a clear C could not take
// waits in TakeMissed.
type Subscription struct {
	C      <-chan Entry
	Missed <-chan struct{}
	b      *Book
	id     int
}

// TakeMissed returns the clears the subscription missed, once, and
// whether more were missed than kept (MaxMissedClears): the subscriber
// must then resync from the book (a new snapshot).
func (s *Subscription) TakeMissed() ([]Entry, bool) {
	if s.b == nil {
		return nil, false
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	sub := s.b.subs[s.id]
	if sub == nil {
		return nil, false
	}
	out := make([]Entry, 0, len(sub.missed))
	for _, id := range slices.Sorted(maps.Keys(sub.missed)) {
		out = append(out, sub.missed[id])
	}
	resync := sub.resync
	sub.missed, sub.resync = map[string]Entry{}, false
	return out, resync
}

// Cancel ends the subscription.
func (s *Subscription) Cancel() {
	if s.b == nil {
		return
	}
	s.b.mu.Lock()
	delete(s.b.subs, s.id)
	s.b.mu.Unlock()
}

func (b *Book) counters() *core.Counters {
	b.once.Do(func() {
		if b.Counters == nil {
			b.Counters = &core.Counters{}
		}
	})
	return b.Counters
}

// Put takes one alert message; an older update than the one held
// changes nothing. Every subscriber gets it. A full subscriber misses
// it, counted: an active alert comes again with its next republish, and
// a clear is kept for the subscriber (TakeMissed).
func (b *Book) Put(e Entry) {
	b.mu.Lock()
	if b.alerts == nil {
		b.alerts = map[string]Entry{}
	}
	cur, held := b.alerts[e.AlertID]
	// A fact api adds (an acknowledgement, an escalation) comes on a
	// republish of the record, which can be older than the monitor's last
	// update: it is taken all the same, and never undone by a later one.
	newFact := held && ((e.Acked && !cur.Acked) || (e.Escalated && !cur.Escalated))
	if held {
		e.Acked = e.Acked || cur.Acked
		e.Escalated = e.Escalated || cur.Escalated
	}
	switch {
	case held && e.UpdatedAt.Before(cur.UpdatedAt) && !newFact:
		b.mu.Unlock()
		b.counters().Inc(CounterBookOlder)
		return
	case !held && e.State != "cleared" && len(b.alerts) >= b.max():
		b.mu.Unlock()
		b.counters().Inc(CounterBookOverBound)
		return
	}
	if e.State == "cleared" {
		delete(b.alerts, e.AlertID)
	} else {
		b.alerts[e.AlertID] = e
	}
	b.counters().Inc(CounterBookTaken)
	// Under the lock: a missed clear is kept before the next Put, and
	// the sends never block.
	for _, sub := range b.subs {
		select {
		case sub.ch <- e:
			continue
		default:
			b.counters().Inc(CounterBookSubFull)
		}
		if e.State != AlertCleared {
			continue
		}
		if _, kept := sub.missed[e.AlertID]; !kept && len(sub.missed) >= MaxMissedClears {
			if !sub.resync {
				b.counters().Inc(CounterBookMissedOverBound)
			}
			sub.resync = true
		} else {
			sub.missed[e.AlertID] = e
		}
		select {
		case sub.kick <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
}

func (b *Book) max() int {
	if b.Max > 0 {
		return b.Max
	}
	return DefaultMaxBookAlerts
}

// Subscribe returns a subscription to every alert put from now on;
// past the bound of subscribers one that never receives (nil channels).
func (b *Book) Subscribe() *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[int]*subscriber{}
	}
	if len(b.subs) >= MaxBookSubscribers {
		b.counters().Inc(CounterBookSubsBound)
		return &Subscription{}
	}
	id := b.next
	b.next++
	sub := &subscriber{ch: make(chan Entry, DefaultSubscriberLen), kick: make(chan struct{}, 1), missed: map[string]Entry{}}
	b.subs[id] = sub
	return &Subscription{C: sub.ch, Missed: sub.kick, b: b, id: id}
}

// Active are the active alerts match selects, critical first, then the
// proximity alerts by the time to the loss of separation (the monitor's
// order), then by id.
func (b *Book) Active(match func(*Entry) bool) []Entry {
	b.mu.Lock()
	out := make([]Entry, 0, 8)
	for id := range b.alerts {
		e := b.alerts[id]
		if match == nil || match(&e) {
			out = append(out, e)
		}
	}
	b.mu.Unlock()
	slices.SortFunc(out, func(x, y Entry) int {
		if d := sevRank(x.Severity) - sevRank(y.Severity); d != 0 {
			return d
		}
		if x.LoSStartS != y.LoSStartS {
			if x.LoSStartS < y.LoSStartS {
				return -1
			}
			return 1
		}
		return strings.Compare(x.AlertID, y.AlertID)
	})
	return out
}

// Unrefreshed is the oldest Seen of an active alert not received for
// longer than after (the monitor republishes every active alert every
// second, C-08): zero when every one is fresh.
func (b *Book) Unrefreshed(now time.Time, after time.Duration) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	var oldest time.Time
	for id := range b.alerts {
		if seen := b.alerts[id].Seen; now.Sub(seen) > after && (oldest.IsZero() || seen.Before(oldest)) {
			oldest = seen
		}
	}
	return oldest
}

func sevRank(s core.Severity) int {
	switch s {
	case core.SeverityCritical:
		return 0
	case core.SeverityWarning:
		return 1
	case core.SeverityInfo:
		return 2
	}
	return 3
}
