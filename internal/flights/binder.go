package flights

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Schema is flight/event/v1 (schemas/flight/event/v1).
const Schema = "flight/event/v1"

// Producer is the envelope producer of the flight facts.
const Producer = "ussp/telemetry-ingest"

// The four facts.
const (
	EventStarted          = "started"
	EventTelemetryLost    = "telemetry_lost"
	EventTelemetryResumed = "telemetry_resumed"
	EventEnded            = "ended"
)

// End reasons (the flights.end_reason values).
const (
	EndLanded        = "landed"
	EndTelemetryLost = "telemetry_lost"
	EndIntentEnded   = "intent_ended"
	EndOperator      = "operator_ended"
)

// Body is flight/event/v1.
type Body struct {
	FlightID            string    `json:"flight_id"`
	Event               string    `json:"event"`
	At                  bus.Stamp `json:"at"`
	StartedAt           bus.Stamp `json:"started_at"`
	ClientID            string    `json:"client_id"`
	UASSerial           string    `json:"uas_serial"`
	IntentID            *string   `json:"intent_id"`
	AuthorisationNumber *string   `json:"authorisation_number"`
	OperatorReg         *string   `json:"operator_reg"`
	EndReason           *string   `json:"end_reason"`
}

// Event is one flight/event/v1 message.
type Event struct {
	bus.Envelope
	Body Body `json:"body"`
}

// Subject is flight.v1.<event>.<flight_id>.
func (e *Event) Subject() (string, error) { return bus.Flight(e.Body.Event, e.Body.FlightID) }

// Counters of the binder.
const (
	CounterStarted   = "flights_started"
	CounterLost      = "flights_telemetry_lost"
	CounterResumed   = "flights_telemetry_resumed"
	CounterEnded     = "flights_ended"
	CounterOverBound = "flights_over_bound"
	// CounterIntentBound counts flights without an intent that a sample
	// bound to one.
	CounterIntentBound = "flights_intent_bound"
)

// MaxFlights bounds the flights a binder holds (E-10); it is the
// ingest's aircraft bound, one flight per aircraft.
const MaxFlights = 10_000

type flight struct {
	id                         string
	key, clientID, serial      string
	intentID, authNo, opReg    *string
	startedAt, lastLive, lastS time.Time
	lost                       bool
}

// Binder holds the running flight of each aircraft (keyed by the
// ingest's aircraft key) and emits its facts. Safe for concurrent use.
type Binder struct {
	// Emit receives every fact (the ingest's event queue).
	Emit func(e *Event)
	// Policy gives telemetry_lost_s and flight_end_after_s.
	Policy func() policy.Values
	// IntentActive says whether an intent is still in intent_active;
	// known is false while that projection has not been read (nothing is
	// ended on its account then).
	IntentActive func(intentID string) (active, known bool)
	Counters     *core.Counters
	Logger       *slog.Logger
	Now          func() time.Time
	// Max bounds the flights held (MaxFlights).
	Max int

	mu      sync.Mutex
	flights map[string]*flight
}

func (b *Binder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Binder) counters() *core.Counters {
	if b.Counters == nil {
		b.Counters = &core.Counters{}
	}
	return b.Counters
}

func (b *Binder) logger() *slog.Logger {
	if b.Logger == nil {
		return obs.Discard()
	}
	return b.Logger
}

func (b *Binder) policy() policy.Values {
	if b.Policy != nil {
		return b.Policy()
	}
	return policy.Defaults()
}

// newID is a version 4 UUID.
func newID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

func same(a, c *string) bool {
	if a == nil || c == nil {
		return a == nil && c == nil
	}
	return *a == *c
}

func clone(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// Bind returns the flight the sample of the aircraft key belongs to (see
// the package documentation); capturedAt is the sample's placement and
// live false for a backlog sample, which never resumes a lost flight.
func (b *Binder) Bind(key, clientID, uasSerial string, intentID, authorisationNumber, operatorReg *string, capturedAt time.Time, live bool) string {
	now := b.now()
	var events []*Event
	b.mu.Lock()
	if b.flights == nil {
		b.flights = map[string]*flight{}
	}
	f := b.flights[key]
	switch {
	case f != nil && f.intentID == nil && intentID != nil:
		// A flight without an intent, now flown under one (the
		// intent_active projection caught up, or the intent was
		// activated in flight): the same flight, bound to the intent.
		// Its later facts carry the intent; the recorder fills the row.
		f.intentID, f.authNo, f.opReg = clone(intentID), clone(authorisationNumber), clone(operatorReg)
		b.counters().Inc(CounterIntentBound)
	case f != nil && !same(f.intentID, intentID):
		events = append(events, b.endLocked(f, EndIntentEnded, now))
		f = nil
	}
	if f == nil {
		maxN := b.Max
		if maxN <= 0 {
			maxN = MaxFlights
		}
		if len(b.flights) >= maxN {
			// E-10: the longest silent flight ends to make room, counted.
			var oldest *flight
			for _, o := range b.flights {
				if oldest == nil || o.lastS.Before(oldest.lastS) {
					oldest = o
				}
			}
			b.counters().Inc(CounterOverBound)
			events = append(events, b.endLocked(oldest, EndTelemetryLost, now))
		}
		f = &flight{id: newID(), key: key, clientID: clientID, serial: uasSerial, intentID: clone(intentID),
			authNo: clone(authorisationNumber), opReg: clone(operatorReg), startedAt: capturedAt}
		b.flights[key] = f
		b.counters().Inc(CounterStarted)
		events = append(events, b.event(f, EventStarted, now, nil))
	}
	f.lastS = now
	if live {
		if capturedAt.After(f.lastLive) {
			f.lastLive = capturedAt
		}
		if f.lost {
			f.lost = false
			b.counters().Inc(CounterResumed)
			events = append(events, b.event(f, EventTelemetryResumed, now, nil))
		}
	}
	id := f.id
	b.mu.Unlock()
	b.emit(events)
	return id
}

// End ends the flight of key with reason at at; nothing when it has
// none.
func (b *Binder) End(key, reason string, at time.Time) {
	b.mu.Lock()
	f := b.flights[key]
	var events []*Event
	if f != nil {
		events = append(events, b.endLocked(f, reason, at))
	}
	b.mu.Unlock()
	b.emit(events)
}

// endLocked removes f and returns its ended fact. Called with mu held.
func (b *Binder) endLocked(f *flight, reason string, at time.Time) *Event {
	delete(b.flights, f.key)
	b.counters().Inc(CounterEnded)
	r := reason
	return b.event(f, EventEnded, at, &r)
}

// Tick marks the silent flights telemetry_lost, ends the ones silent for
// flight_end_after_s and those whose intent left intent_active; it
// returns how many it ended.
func (b *Binder) Tick() int {
	now := b.now()
	pol := b.policy()
	lostAfter := time.Duration(pol.TelemetryLostS * float64(time.Second))
	endAfter := time.Duration(pol.FlightEndAfterS * float64(time.Second))
	var events []*Event
	ended := 0
	b.mu.Lock()
	for _, f := range b.flights {
		if f.intentID != nil && b.IntentActive != nil {
			if active, known := b.IntentActive(*f.intentID); known && !active {
				events = append(events, b.endLocked(f, EndIntentEnded, now))
				ended++
				continue
			}
		}
		if now.Sub(f.lastS) >= endAfter {
			events = append(events, b.endLocked(f, EndTelemetryLost, now))
			ended++
			continue
		}
		// T-06: liveness counts from capture time, capped at now; a
		// flight that never had a live sample counts from its last one.
		last := f.lastLive
		if last.IsZero() {
			last = f.lastS
		}
		if last.After(now) {
			last = now
		}
		if !f.lost && now.Sub(last) >= lostAfter {
			f.lost = true
			b.counters().Inc(CounterLost)
			events = append(events, b.event(f, EventTelemetryLost, now, nil))
		}
	}
	b.mu.Unlock()
	b.emit(events)
	return ended
}

// Len is the flights held.
func (b *Binder) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.flights)
}

func (b *Binder) event(f *flight, kind string, at time.Time, reason *string) *Event {
	return &Event{
		Envelope: bus.SystemEnvelope(Schema, Producer, b.now()),
		Body: Body{
			FlightID: f.id, Event: kind, At: bus.Stamp{Time: at.UTC()}, StartedAt: bus.Stamp{Time: f.startedAt.UTC()},
			ClientID: f.clientID, UASSerial: f.serial, IntentID: clone(f.intentID), AuthorisationNumber: clone(f.authNo),
			OperatorReg: clone(f.opReg), EndReason: reason,
		},
	}
}

func (b *Binder) emit(events []*Event) {
	for _, e := range events {
		b.logger().LogAttrs(context.Background(), slog.LevelInfo, "flight "+e.Body.Event,
			slog.String("flight_id", e.Body.FlightID), slog.String("client_id", e.Body.ClientID))
		if b.Emit != nil {
			b.Emit(e)
		}
	}
}
