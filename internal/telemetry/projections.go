package telemetry

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

// What the ingest reads of the control plane (D6): KV projections
// written by api, read here with their age, never blocking. Each is an
// interface so the tests drive the ingest without NATS.

// BindingsSource is the client_bindings projection: the serial fold keys
// a client may send telemetry for (06 T3). loaded is false while the
// bucket has not been read: nothing can be checked, so nothing passes.
type BindingsSource interface {
	Folds(clientID string) (folds []string, ageS float64, loaded bool)
}

// IntentFacts is what the ingest needs of an intent of intent_active
// (intent/state/v1).
type IntentFacts struct {
	IntentID            string    `json:"intent_id"`
	LocalState          string    `json:"local_state"`
	UASSerial           string    `json:"uas_serial"`
	OperatorReg         string    `json:"operator_reg"`
	AuthorisationNumber *string   `json:"authorisation_number"`
	InUSpaceAirspace    bool      `json:"in_uspace_airspace"`
	TimeStart           time.Time `json:"time_start"`
	TimeEnd             time.Time `json:"time_end"`
}

// FlyingStates are the local states of an intent a flight may fly: an
// activated intent, and one that deviated from it while flying (the
// flight goes on; conformance follows it, WP-10).
var FlyingStates = []string{"activated", "nonconforming", "contingent"}

func flying(state string) bool {
	for _, s := range FlyingStates {
		if s == state {
			return true
		}
	}
	return false
}

// IntentSource is the intent_active projection.
type IntentSource interface {
	Intent(id string) (f IntentFacts, found, loaded bool)
}

// RegistrySource is the registry_validity projection: the cached F8
// answer under a key. loaded is false while the bucket has not been read.
type RegistrySource interface {
	Entry(k registry.Key) (e registry.Entry, found, loaded bool)
}

// SourceGate is the source-control follower (internal/sources).
type SourceGate interface {
	Query(sourceType string, instanceID *string) coresources.Decision
}

// KVBindings reads client_bindings through a bus.Mirror: the sorted
// fold keys under the client id as a bus.KeyToken (internal/app/api's
// bindingsProjector).
type KVBindings struct{ M *bus.Mirror[[]string] }

// Folds implements BindingsSource.
func (b KVBindings) Folds(clientID string) ([]string, float64, bool) {
	v, _, age, loaded := b.M.Get(bus.KeyToken(clientID))
	return v, age, loaded
}

// KVIntents reads intent_active through a bus.Mirror: the intent/state/v1
// body under the intent id (internal/intent.BusProjector).
type KVIntents struct{ M *bus.Mirror[IntentFacts] }

// Intent implements IntentSource.
func (k KVIntents) Intent(id string) (IntentFacts, bool, bool) {
	if !bus.ValidKey(id) {
		_, _, loaded := k.M.Snapshot()
		return IntentFacts{}, false, loaded
	}
	v, found, _, loaded := k.M.Get(id)
	return v, found, loaded
}

// KVRegistry reads registry_validity through a bus.Mirror: each cached
// answer under registry.KVKey (internal/registry.BusProjector).
type KVRegistry struct{ M *bus.Mirror[registry.Entry] }

// Entry implements RegistrySource.
func (k KVRegistry) Entry(key registry.Key) (registry.Entry, bool, bool) {
	v, found, _, loaded := k.M.Get(registry.KVKey(key))
	return v, found, loaded
}

// DecodePolicy reads the policy bucket's value onto the defaults, so a
// version stored before a value existed reads that value's default (as
// internal/store does), and refuses one that does not validate (the
// follower then keeps the last one).
func DecodePolicy(data []byte) (policy.Record, error) {
	r := policy.Record{Values: policy.Defaults()}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return policy.Record{}, core.Fieldf("policy", "not a policy record: %s", clipErr(err))
	}
	if err := r.Values.Validate(); err != nil {
		return policy.Record{}, err
	}
	return r, nil
}

// lookupSource is the registry projection as registry.FromProjection
// reads it, cut to the entries of one fleet (registry.Keys): the hot
// path resolves one aircraft with two map reads, not a scan.
type lookupSource struct {
	src  RegistrySource
	keys []registry.Key
}

// Snapshot implements registry.ProjectionSource.
func (l lookupSource) Snapshot() ([]registry.Entry, bool) {
	out := make([]registry.Entry, 0, len(l.keys))
	loaded := true
	for _, k := range l.keys {
		e, found, ok := l.src.Entry(k)
		if !ok {
			loaded = false
			continue
		}
		if found {
			out = append(out, e)
		}
	}
	return out, loaded
}
