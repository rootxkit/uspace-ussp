//nolint:misspell // serial.Normalize is uspace-core's API name (Go spelling)
package registry

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"
)

// Purpose is why a lookup is made; F8 records it on every call.
type Purpose string

// The two purposes of F8 (spec 02 F8).
const (
	PurposeAuthorisation  Purpose = "authorisation"
	PurposeIdentification Purpose = "identification"
)

// Valid reports whether p is one of F8's purposes.
func (p Purpose) Valid() bool { return p == PurposeAuthorisation || p == PurposeIdentification }

// Status is an F8 validity status: status only, never a name.
type Status string

// The four F8 statuses. An expired registration is revoked with its
// valid_until in the past (the authority's F8).
const (
	StatusValid     Status = "valid"
	StatusSuspended Status = "suspended"
	StatusRevoked   Status = "revoked"
	StatusUnknown   Status = "unknown"
)

// Valid reports whether s is one of the four.
func (s Status) Valid() bool {
	switch s {
	case StatusValid, StatusSuspended, StatusRevoked, StatusUnknown:
		return true
	}
	return false
}

// Reasons an answer is unknown without the registry having said so. An
// answer the registry gave has no reason.
const (
	// ReasonRegistryUnavailable: the authority could not be asked or did
	// not answer, and no answer within its TTL is cached.
	ReasonRegistryUnavailable = "registry_unavailable"
	// ReasonAnswerRefused: the authority answered something this USSP
	// refuses (a field it did not ask for, an answer to another key).
	ReasonAnswerRefused = "registry_answer_refused"
)

// Counters of the cache and the feed (CLAUDE.md engineering rules: a
// stable snake_case name for everything refused, dropped or degraded).
const (
	CounterCacheHit         = "registry_cache_hit"          // an entity answered from the cache within its TTL
	CounterCacheMiss        = "registry_cache_miss"         // an entity not cached, or cached beyond its TTL
	CounterFetched          = "registry_fetched"            // an entity the authority answered
	CounterUnavailable      = "registry_unavailable"        // an entity answered unknown because the authority could not be asked
	CounterPIIRefused       = "registry_pii_refused"        // an answer refused for a field F8 does not define, or an echoed secret part
	CounterAnswerRefused    = "registry_answer_refused"     // an answer refused for not answering what was asked
	CounterCacheReadFailed  = "registry_cache_read_failed"  // the table could not be read: every key asked of the authority
	CounterCacheWriteFailed = "registry_cache_write_failed" // an answer not cached (table or projection refused it)
	CounterWriteSkipped     = "registry_write_skipped"      // an answer not cached because the feed invalidated its key meanwhile
	CounterFeedPolled       = "registry_feed_polled"        // a change page applied
	CounterFeedNotModified  = "registry_feed_not_modified"  // the feed answered 304
	CounterFeedInvalidated  = "registry_feed_invalidated"   // a cached entry deleted by a change
	CounterFeedFailed       = "registry_feed_failed"        // a poll that did not complete
	CounterFeedRefused      = "registry_feed_refused"       // a change page refused (out of order, malformed)
)

// EntityType is what a key names.
type EntityType string

// The three entities of F8.
const (
	EntityOperator EntityType = "operator"
	EntityUAS      EntityType = "uas"
	EntityPilot    EntityType = "pilot"
)

// Bounds of a query: the authority's own (api/clients/authority.yaml).
const (
	MaxOperatorLen = 64
	MaxSerialLen   = 64
	MaxPilotLen    = 32
	// MaxQueries bounds one Validate call and one batch to the authority.
	MaxQueries = 100
	// MaxCompetencies bounds a pilot's competency set as cached (E-10;
	// the migration's check says the same).
	MaxCompetencies = 64
	// MaxFieldLen bounds a class label, a band and a competency name.
	MaxFieldLen = 64
)

// Query names up to one operator, one UAS and one pilot; at least one.
type Query struct {
	Operator string
	Serial   string
	Pilot    string
}

// Key is one cached entity: its type and the key it is stored under
// (an operator's regnum.CompareKey, a serial as uspace-core's serial
// package normalises it, a pilot's id).
type Key struct {
	Entity EntityType `json:"entity_type"`
	Key    string     `json:"key"`
}

// Competency is one competency of a pilot's answer.
type Competency struct {
	Competency string    `json:"competency"`
	ValidUntil time.Time `json:"valid_until"`
}

// Entry is one cached F8 answer: a row of registry_validity and the
// value of the KV projection registry_validity under its Key. It holds
// statuses only (CLAUDE.md rule 8).
type Entry struct {
	Key
	// KeyFold is what the change feed invalidates: the key itself, or
	// for a serial its serial.FoldKey.
	KeyFold string `json:"key_fold"`
	Status  Status `json:"status"`
	// ValidUntil is the registration's end of validity as F8 answers it
	// (an operator's); nil where F8 gives none.
	ValidUntil   *time.Time   `json:"valid_until,omitempty"`
	ClassLabel   string       `json:"class_label,omitempty"`
	MTOMBand     string       `json:"mtom_band,omitempty"`
	Competencies []Competency `json:"competencies,omitempty"`
	// FetchedAt is when the authority answered, on the database clock.
	FetchedAt time.Time `json:"fetched_at"`
}

// Negative reports whether the entry caches the registry not holding
// the key (unknown), which lives for the negative TTL.
func (e Entry) Negative() bool { return e.Status == StatusUnknown }

// Answer is what Validate says of one entity.
type Answer struct {
	// Key is the key as answered: an operator number's public part, the
	// serial, the pilot id.
	Key    string
	Status Status
	// Reason is empty when the registry gave the status, else why it is
	// unknown (ReasonRegistryUnavailable, ReasonAnswerRefused).
	Reason       string
	ValidUntil   *time.Time
	ClassLabel   string
	MTOMBand     string
	Competencies []Competency
	// CacheAgeS is the age of the answer in seconds: 0 when the
	// authority was just asked, nil when there is no answer.
	CacheAgeS *float64
}

// Result is the answer to one Query: one part per key asked.
type Result struct {
	Operator *Answer
	UAS      *Answer
	Pilot    *Answer
}

// TTL is the cache's lifetime of an answer (policy).
type TTL struct {
	Positive time.Duration
	Negative time.Duration
}

// For is the lifetime of e.
func (t TTL) For(e Entry) time.Duration {
	if e.Negative() {
		return t.Negative
	}
	return t.Positive
}

// operatorKey is the cache key, the fold key and the public part of an
// operator number (the secret part never leaves this function, G-04).
func operatorKey(raw string) (key, public string) {
	public, key = regnum.Public(raw)
	return key, public
}

// serialKey is the cache key and the fold key of a serial.
func serialKey(raw string) (key, fold string) {
	key = serial.Normalize(raw)
	return key, serial.FoldKey(key)
}

// pilotKey is the cache key of a pilot id.
func pilotKey(raw string) string { return strings.TrimSpace(raw) }

// entryKeyFold is the fold key an entity's key is invalidated under.
func entryKeyFold(e EntityType, key string) string {
	if e == EntityUAS {
		return serial.FoldKey(key)
	}
	return key
}

// normalised is a Query with its keys as cached, and the public parts
// asked of the authority.
type normalised struct {
	operatorKey, operatorPublic string
	serial, serialFold          string
	pilot                       string
}

func (n normalised) keys() []Key {
	var out []Key
	if n.operatorKey != "" {
		out = append(out, Key{Entity: EntityOperator, Key: n.operatorKey})
	}
	if n.serial != "" {
		out = append(out, Key{Entity: EntityUAS, Key: n.serial})
	}
	if n.pilot != "" {
		out = append(out, Key{Entity: EntityPilot, Key: n.pilot})
	}
	return out
}

// checkQueries refuses a lookup F8 does not define, naming each field.
func checkQueries(qs []Query, p Purpose) ([]normalised, error) {
	var errs []error
	if !p.Valid() {
		errs = append(errs, core.Fieldf("purpose", "required: authorisation or identification"))
	}
	switch {
	case len(qs) == 0:
		errs = append(errs, core.Fieldf("items", "name at least one operator, serial or pilot"))
	case len(qs) > MaxQueries:
		errs = append(errs, core.Fieldf("items", "at most %d per request", MaxQueries))
		qs = nil
	}
	out := make([]normalised, 0, len(qs))
	for i, q := range qs {
		prefix := ""
		if len(qs) > 1 {
			prefix = fmt.Sprintf("items[%d].", i)
		}
		var n normalised
		for _, f := range []struct {
			name, value string
			max         int
		}{{"operator", q.Operator, MaxOperatorLen}, {"serial", q.Serial, MaxSerialLen}, {"pilot", q.Pilot, MaxPilotLen}} {
			v := strings.TrimSpace(f.value)
			switch {
			case len(v) > f.max:
				errs = append(errs, core.Fieldf(prefix+f.name, "longer than %d bytes", f.max))
				continue
			case strings.IndexFunc(v, unicode.IsControl) >= 0:
				errs = append(errs, core.Fieldf(prefix+f.name, "contains a control character"))
				continue
			}
			switch {
			case v == "":
			case f.name == "operator":
				n.operatorKey, n.operatorPublic = operatorKey(v)
			case f.name == "serial":
				n.serial, n.serialFold = serialKey(v)
			default:
				n.pilot = pilotKey(v)
			}
		}
		if n.operatorKey == "" && n.serial == "" && n.pilot == "" {
			field := "query"
			if prefix != "" {
				field = strings.TrimSuffix(prefix, ".")
			}
			errs = append(errs, core.Fieldf(field, "name an operator, a serial or a pilot"))
		}
		out = append(out, n)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}
