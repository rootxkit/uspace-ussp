package bus

import (
	"crypto/rand"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Stamp is a time on the wire: RFC 3339 UTC with milliseconds and Z
// (spec 02 §1, envelope/v1 "timestamp"). It reads any RFC 3339 time and
// keeps it in UTC.
type Stamp struct{ time.Time }

// StampLayout is the wire form.
const StampLayout = "2006-01-02T15:04:05.000Z"

// MarshalJSON writes the wire form.
func (s Stamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.UTC().Format(StampLayout) + `"`), nil
}

// UnmarshalJSON reads an RFC 3339 time; anything else is an error.
func (s *Stamp) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return core.Fieldf("timestamp", "not a string")
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return core.Fieldf("timestamp", "%q is not RFC 3339", v)
	}
	s.Time = t.UTC()
	return nil
}

// Envelope is the common envelope of spec 04 §2 (uspace-lab
// schemas/common/envelope/v1). Every message on a subject that carries
// an 04 message embeds it beside its Body:
//
//	type Track struct {
//		bus.Envelope
//		Body TrackBody `json:"body"`
//	}
//
// Envelope has no MarshalJSON of its own, so the embedding struct's
// encoding flattens it.
type Envelope struct {
	Schema   string `json:"schema"`
	MsgID    string `json:"msg_id"`
	Producer string `json:"producer"`
	// TS is the source's own clock; null when the record carried none.
	TS         *Stamp          `json:"ts"`
	RxTS       Stamp           `json:"rx_ts"`
	CapturedAt Stamp           `json:"captured_at"`
	TimeSource core.TimeSource `json:"time_source"`
	Backlog    bool            `json:"backlog"`
}

// Head returns the envelope itself: a message embedding Envelope is an
// Enveloped through it.
func (e *Envelope) Head() *Envelope { return e }

// Enveloped is a message with the envelope (every struct embedding
// Envelope).
type Enveloped interface {
	Head() *Envelope
}

// NewEnvelope is the envelope of a message of schema produced by
// producer for a record with times t, under a fresh msg_id.
func NewEnvelope(schema, producer string, t core.Times) Envelope {
	e := Envelope{
		Schema: schema, MsgID: NewULID(time.Now()), Producer: producer,
		RxTS: Stamp{t.RxTS.UTC()}, CapturedAt: Stamp{t.CapturedAt.UTC()},
		TimeSource: t.Source, Backlog: t.Backlog,
	}
	if t.TS != nil {
		e.TS = &Stamp{t.TS.UTC()}
	}
	return e
}

// SystemEnvelope is the envelope of a message this system makes at now
// itself (an alert, a state change): ts, rx_ts and captured_at are now,
// time_source system, never backlog.
func SystemEnvelope(schema, producer string, now time.Time) Envelope {
	return NewEnvelope(schema, producer, core.Times{TS: &now, RxTS: now, CapturedAt: now, Source: core.TimeSystem})
}

// Times are the envelope's times as core reasons with them.
func (e *Envelope) Times() core.Times {
	t := core.Times{RxTS: e.RxTS.Time, CapturedAt: e.CapturedAt.Time, Source: e.TimeSource, Backlog: e.Backlog}
	if e.TS != nil {
		ts := e.TS.Time
		t.TS = &ts
	}
	return t
}

var (
	schemaName = regexp.MustCompile(`^[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*/v[1-9][0-9]*$`)
	ulidRe     = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)
	producerRe = regexp.MustCompile(`^(authority|cisp|ussp|ansp|lab)(/[a-z][a-z0-9]*(-[a-z][a-z0-9]*)*|-[0-9]+/[a-z][a-z0-9]*(-[a-z][a-z0-9]*)*-[0-9]+)$`)
)

var timeSources = map[core.TimeSource]bool{
	core.TimeSourceClock: true, core.TimeBroadcast: true, core.TimeReceiver: true, core.TimeProvider: true, core.TimeSystem: true,
}

// Validate checks the envelope against envelope/v1: the schema name,
// the msg_id ULID, the producer, the time source and both required
// times. Every refusal names its field.
func (e *Envelope) Validate() error {
	var errs []string
	if !schemaName.MatchString(e.Schema) {
		errs = append(errs, "schema")
	}
	if !ValidULID(e.MsgID) {
		errs = append(errs, "msg_id")
	}
	if !producerRe.MatchString(e.Producer) {
		errs = append(errs, "producer")
	}
	if !timeSources[e.TimeSource] {
		errs = append(errs, "time_source")
	}
	if e.RxTS.IsZero() {
		errs = append(errs, "rx_ts")
	}
	if e.CapturedAt.IsZero() {
		errs = append(errs, "captured_at")
	}
	if len(errs) > 0 {
		return core.Fieldf(errs[0], "envelope fields not valid: %s", strings.Join(errs, ", "))
	}
	return nil
}

// crockford is the ULID alphabet.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a ULID for at: 48 bits of milliseconds and 80 random
// bits, Crockford base32 (the envelope's msg_id, the dedupe key).
func NewULID(at time.Time) string {
	var b [16]byte
	ms := uint64(at.UnixMilli()) & (1<<48 - 1)
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	_, _ = rand.Read(b[6:])
	// 128 bits as 26 base32 digits, least significant first: the top
	// digit holds the top 3 bits, so it is 0-7 as the schema requires.
	var out [26]byte
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 | uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 | uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | (hi&31)<<59
		hi >>= 5
	}
	return string(out[:])
}

// ValidULID reports whether s is a ULID as envelope/v1 spells one.
func ValidULID(s string) bool { return ulidRe.MatchString(s) }

// ULIDTime is the millisecond time a ULID carries; false for a string
// that is not one.
func ULIDTime(s string) (time.Time, bool) {
	if !ValidULID(s) {
		return time.Time{}, false
	}
	var ms uint64
	for i := range 10 {
		ms = ms<<5 | uint64(strings.IndexByte(crockford, s[i]))
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}
