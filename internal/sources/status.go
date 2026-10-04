package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// SchemaStatus is source/status/v1 (schemas/source/status/v1): an
// adapter's status, every 2 s (spec 04 §3.6).
const SchemaStatus = "source/status/v1"

// The states of source/status/v1. Down is "unavailable since T": the
// source was expected and is not heard (02 F4, LESSONS B-04).
const (
	StateLive     = "live"
	StateStale    = "stale"
	StateDisabled = "disabled"
	StateDown     = "down"
	StateUnknown  = "unknown"
)

// AllInstances is the subject token of the status of an adapter type as
// a whole (its body's source_instance is null): src.v1.<type>._all. An
// instance id is a slug or a token that never starts with "_", so it
// never meets it.
const AllInstances = "_all"

// StatusBody is the body of source/status/v1 as the WP-14 adapters
// publish it: the schema's members, and Detail, a sentence a person
// reads (the body admits more members).
type StatusBody struct {
	Source         string            `json:"source"`
	SourceInstance *string           `json:"source_instance"`
	State          string            `json:"state"`
	Since          bus.Stamp         `json:"since"`
	AgeS           *float64          `json:"age_s"`
	DisabledBy     *string           `json:"disabled_by"`
	Counters       map[string]uint64 `json:"counters"`
	Detail         string            `json:"detail,omitempty"`
}

// Status is one source/status/v1 message.
type Status struct {
	bus.Envelope
	Body StatusBody `json:"body"`
}

// Publisher is the bus (bus.Publisher).
type Publisher interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// Disabled sets b's state to disabled with how, when d says the source
// is switched off; it reports whether it did.
func (b *StatusBody) Disabled(d coresources.Decision) bool {
	if d.Enabled {
		return false
	}
	why := string(coresources.WhyInstance)
	if d.WhyDisabled != nil {
		why = string(*d.WhyDisabled)
	}
	b.State, b.DisabledBy = StateDisabled, &why
	return true
}

// PublishStatus publishes b on src.v1.<source>.<token>, token being
// AllInstances for the status of the type as a whole and the instance
// as a subject token (KeyToken when it is not one) otherwise. The
// counters accepted and refused are always present.
func PublishStatus(ctx context.Context, pub Publisher, producer string, now time.Time, b StatusBody) error {
	if b.Counters == nil {
		b.Counters = map[string]uint64{}
	}
	for _, k := range []string{"accepted", "refused"} {
		if _, ok := b.Counters[k]; !ok {
			b.Counters[k] = 0
		}
	}
	tok := AllInstances
	if b.SourceInstance != nil {
		tok = InstanceToken(*b.SourceInstance)
	}
	subject, err := bus.Src(b.Source, tok)
	if err != nil {
		return err
	}
	return pub.Publish(ctx, subject, &Status{Envelope: bus.SystemEnvelope(SchemaStatus, producer, now), Body: b})
}

// InstanceToken is an instance id as a subject token: itself when it is
// a slug of letters, digits and dashes (a client or receiver id), else
// "_h" and the first 32 hex digits of its SHA-256 (a peer's base URL),
// which no slug and not AllInstances can be.
func InstanceToken(instance string) string {
	ok := instance != "" && len(instance) <= 64 && instance[0] != '_'
	for _, r := range instance {
		if !slugRune(r) {
			ok = false
			break
		}
	}
	if ok {
		return instance
	}
	sum := sha256.Sum256([]byte(instance))
	return "_h" + hex.EncodeToString(sum[:16])
}

// slugRune reports whether r may be in an instance slug.
func slugRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
}
