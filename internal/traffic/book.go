package traffic

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Counters of the alert book.
const (
	CounterBookTaken      = "book_alerts"
	CounterBookOverBound  = "book_over_bound"
	CounterBookSubFull    = "book_subscriber_full"
	CounterBookOlder      = "book_older_than_held"
	CounterBookSubsBound  = "book_subscribers_over_bound"
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
	subs   map[int]chan Entry
	next   int
	once   sync.Once
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
// changes nothing. Every subscriber gets it (a full subscriber misses
// it, counted: the next product or repeat carries the alert again).
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
	subs := make([]chan Entry, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	b.counters().Inc(CounterBookTaken)
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			b.counters().Inc(CounterBookSubFull)
		}
	}
}

func (b *Book) max() int {
	if b.Max > 0 {
		return b.Max
	}
	return DefaultMaxBookAlerts
}

// Subscribe returns a channel of every alert put from now on and its
// cancel; nil when the bound of subscribers is reached.
func (b *Book) Subscribe() (<-chan Entry, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[int]chan Entry{}
	}
	if len(b.subs) >= MaxBookSubscribers {
		b.counters().Inc(CounterBookSubsBound)
		return nil, func() {}
	}
	id := b.next
	b.next++
	ch := make(chan Entry, DefaultSubscriberLen)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
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
