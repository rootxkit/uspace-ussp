package geo

import (
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
)

// SchemaChanged is geo/changed/v1 (schemas/geo/changed/v1): a CIS
// version installed, on cis.v1.<dataset> and as the geo change push of
// the traffic stream.
const SchemaChanged = "geo/changed/v1"

// Producer is the envelope producer of api's geo messages.
const Producer = "ussp/api"

// MaxChangedBytes bounds a geo/changed/v1 message read (E-10): the
// feature id list is bounded by cis.MaxChangedFeatures.
const MaxChangedBytes = 256 << 10

// ChangedMessage is one geo/changed/v1 message.
type ChangedMessage struct {
	bus.Envelope
	Body cis.Change `json:"body"`
}

// ChangedMessageOf is c as published at now.
func ChangedMessageOf(c cis.Change, now time.Time) *ChangedMessage {
	if c.FeatureIDs == nil {
		c.FeatureIDs = []string{}
	}
	return &ChangedMessage{Envelope: bus.SystemEnvelope(SchemaChanged, Producer, now), Body: c}
}

// DecodeChanged reads one geo/changed/v1 message: the envelope, the
// schema, a known dataset and reason, a version, bounded feature ids.
// Anything else is an error naming the field; it never panics.
func DecodeChanged(data []byte) (ChangedMessage, error) {
	var m ChangedMessage
	if len(data) > MaxChangedBytes {
		return m, core.Fieldf("message", "longer than %d bytes", MaxChangedBytes)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return ChangedMessage{}, core.Fieldf("message", "not a geo/changed/v1 message")
	}
	b := &m.Body
	if _, ok := cis.ParseDataset(string(b.Dataset)); !ok {
		return ChangedMessage{}, core.Fieldf("body.dataset", "unknown dataset")
	}
	switch {
	case m.Schema != SchemaChanged:
		return ChangedMessage{}, core.Fieldf("schema", "%q where %q is expected", m.Schema, SchemaChanged)
	case b.Version < 1 || b.PreviousVersion < 0:
		return ChangedMessage{}, core.Fieldf("body.version", "not a version")
	case b.Reason != cis.ChangeInstalled && b.Reason != cis.ChangeWarm:
		return ChangedMessage{}, core.Fieldf("body.reason", "unknown reason")
	case len(b.FeatureIDs) > cis.MaxChangedFeatures:
		return ChangedMessage{}, core.Fieldf("body.feature_ids", "more than %d", cis.MaxChangedFeatures)
	}
	for _, id := range b.FeatureIDs {
		if id == "" || len(id) > 128 || !utf8.ValidString(id) {
			return ChangedMessage{}, core.Fieldf("body.feature_ids", "an id is empty, too long or not UTF-8")
		}
	}
	if err := m.Validate(); err != nil {
		return ChangedMessage{}, err
	}
	if b.FeatureIDs == nil {
		b.FeatureIDs = []string{}
	}
	return m, nil
}

// Counters of the change fan-out.
const (
	CounterChangesDropped = "geo_changes_dropped"
	CounterChangesUnread  = "geo_changes_unreadable"
)

// SubscriberQueue bounds the changes one subscriber holds (E-10); past
// it the oldest goes, counted: a later change says the same "refetch".
const SubscriberQueue = 16

// Changes fans the geo changes out to the traffic stream's connections
// (the small hook interface of brief WP-12: traffic-ws subscribes, and
// tells each subscriber to refetch /v1/geo). Safe for concurrent use;
// Publish never blocks.
type Changes struct {
	Counters *core.Counters

	mu   sync.Mutex
	next int
	subs map[int]chan cis.Change
}

func (c *Changes) counters() *core.Counters {
	if c.Counters == nil {
		c.Counters = &core.Counters{}
	}
	return c.Counters
}

// Subscribe returns a subscriber's changes and its cancel.
func (c *Changes) Subscribe() (<-chan cis.Change, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subs == nil {
		c.subs = map[int]chan cis.Change{}
	}
	id := c.next
	c.next++
	ch := make(chan cis.Change, SubscriberQueue)
	c.subs[id] = ch
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.subs, id)
	}
}

// Publish gives ch to every subscriber; a full subscriber loses its
// oldest change, counted.
func (c *Changes) Publish(ch cis.Change) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.subs {
		for {
			select {
			case s <- ch:
			default:
				select {
				case <-s:
					c.counters().Inc(CounterChangesDropped)
				default:
				}
				continue
			}
			break
		}
	}
}

// Take reads one cis.v1 message and publishes it; one that does not
// read is counted.
func (c *Changes) Take(data []byte) {
	m, err := DecodeChanged(data)
	if err != nil {
		c.mu.Lock()
		c.counters().Inc(CounterChangesUnread)
		c.mu.Unlock()
		return
	}
	c.Publish(m.Body)
}
