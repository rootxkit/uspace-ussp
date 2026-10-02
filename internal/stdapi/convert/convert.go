// Package convert is the one place where F3411 and F3548 messages cross
// between the wire and this USSP's code (docs/PLAN.md D3, brief WP-3).
// The generated servers and clients in internal/stdapi/f3411 and
// internal/stdapi/f3548 carry uspace-core's wire types; what arrives is
// read here through core's validating Unmarshal helpers (so the size
// bound, the presence, enumeration and range checks are core's, and a
// refusal is a *core.FieldError naming the member), and what leaves is
// written here and checked by the same helpers before it is sent, so this
// USSP never sends a message a peer running core would refuse.
//
// Nothing here judges: prefilter envelopes are core's
// Volume4DToZonesEnvelope, altitudes are core's Altitude.HAEM.
package convert

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
)

// RIDFlightFromWire reads one F3411 RIDFlight from untrusted bytes
// through core's UnmarshalRIDFlight.
func RIDFlightFromWire(b []byte) (*f3411.RIDFlight, error) {
	return f3411.UnmarshalRIDFlight(b)
}

// GetFlightsResponseFromWire reads a peer Service Provider's GET
// /uss/flights answer through core's UnmarshalGetFlightsResponse.
func GetFlightsResponseFromWire(b []byte) (*f3411.GetFlightsResponse, error) {
	return f3411.UnmarshalGetFlightsResponse(b)
}

// OperationalIntentFromWire reads one F3548 OperationalIntent from
// untrusted bytes through core's UnmarshalOperationalIntent.
func OperationalIntentFromWire(b []byte) (*f3548.OperationalIntent, error) {
	return f3548.UnmarshalOperationalIntent(b)
}

// OperationalIntentDetailsFromWire reads the body of GET
// /uss/v1/operational_intents/{entityid} (GetOperationalIntentDetailsResponse):
// its operational_intent member goes through UnmarshalOperationalIntent.
// A body over f3548.MaxMessageBytes, a body that is not an object or one
// without operational_intent is refused with a *core.FieldError.
func OperationalIntentDetailsFromWire(b []byte) (*f3548.GetOperationalIntentDetailsResponse, error) {
	if len(b) > f3548.MaxMessageBytes {
		return nil, core.Fieldf("response", "is %d bytes; at most %d", len(b), f3548.MaxMessageBytes)
	}
	var env struct {
		OperationalIntent json.RawMessage `json:"operational_intent"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, core.Fieldf("response", "not a JSON object")
	}
	if len(env.OperationalIntent) == 0 || string(env.OperationalIntent) == "null" {
		return nil, core.Fieldf("response.operational_intent", "required")
	}
	oi, err := f3548.UnmarshalOperationalIntent(env.OperationalIntent)
	if err != nil {
		return nil, prefix("response", err)
	}
	return &f3548.GetOperationalIntentDetailsResponse{OperationalIntent: *oi}, nil
}

// RIDFlightToWire writes f as JSON after core's UnmarshalRIDFlight has
// accepted the written bytes.
func RIDFlightToWire(f *f3411.RIDFlight) ([]byte, error) {
	if f == nil {
		return nil, core.Fieldf("flight", "nil")
	}
	return checked(f, func(b []byte) error { _, err := f3411.UnmarshalRIDFlight(b); return err })
}

// GetFlightsResponseToWire writes the answer to GET /uss/flights after
// core's UnmarshalGetFlightsResponse has accepted the written bytes.
func GetFlightsResponseToWire(r *f3411.GetFlightsResponse) ([]byte, error) {
	if r == nil {
		return nil, core.Fieldf("response", "nil")
	}
	return checked(r, func(b []byte) error { _, err := f3411.UnmarshalGetFlightsResponse(b); return err })
}

// OperationalIntentToWire writes oi as JSON after core's
// UnmarshalOperationalIntent has accepted the written bytes.
func OperationalIntentToWire(oi *f3548.OperationalIntent) ([]byte, error) {
	if oi == nil {
		return nil, core.Fieldf("operational_intent", "nil")
	}
	return checked(oi, func(b []byte) error { _, err := f3548.UnmarshalOperationalIntent(b); return err })
}

// OperationalIntentDetailsToWire writes the body of GET
// /uss/v1/operational_intents/{entityid} after
// OperationalIntentDetailsFromWire has accepted the written bytes.
func OperationalIntentDetailsToWire(r *f3548.GetOperationalIntentDetailsResponse) ([]byte, error) {
	if r == nil {
		return nil, core.Fieldf("response", "nil")
	}
	return checked(r, func(b []byte) error { _, err := OperationalIntentDetailsFromWire(b); return err })
}

// Envelope is a volume's prefilter box and time window; a zero Start or
// End is unbounded on that side (core's Volume4DToZonesEnvelope).
type Envelope struct {
	BBox  geodesy.BBox
	Start time.Time
	End   time.Time
}

// Volume4DEnvelope is core's f3548.Volume4DToZonesEnvelope of v.
func Volume4DEnvelope(v f3548.Volume4D) (Envelope, error) {
	box, start, end, err := f3548.Volume4DToZonesEnvelope(v)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{BBox: box, Start: start, End: end}, nil
}

// RIDVolume4DEnvelope is core's f3411.Volume4DToZonesEnvelope of v (an
// ISA's or a subscription's extents).
func RIDVolume4DEnvelope(v f3411.Volume4D) (Envelope, error) {
	box, start, end, err := f3411.Volume4DToZonesEnvelope(v)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{BBox: box, Start: start, End: end}, nil
}

// checked marshals v and returns the bytes only when check accepts them.
func checked(v any, check func([]byte) error) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, core.Fieldf("body", "not writable as JSON: %v", err)
	}
	if err := check(b); err != nil {
		return nil, err
	}
	return b, nil
}

// prefix puts root in front of a field error's path.
func prefix(root string, err error) error {
	var fe *core.FieldError
	if errors.As(err, &fe) {
		return &core.FieldError{Field: root + "." + fe.Field, Reason: fe.Reason}
	}
	return err
}
