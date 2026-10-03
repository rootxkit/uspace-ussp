package traffic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Counters of the persisted proximity state.
const (
	CounterStateSaved       = "traffic_state_saved"
	CounterStateSaveFailed  = "traffic_state_save_failed"
	CounterStateDeleted     = "traffic_state_deleted"
	CounterStateDelFailed   = "traffic_state_delete_failed"
	CounterStateUnreadable  = "traffic_state_unreadable"
	CounterStateRestored    = "traffic_alerts_restored"
	CounterStateOwnedByPeer = "traffic_state_owned_elsewhere"
	CounterStateNotLoaded   = "traffic_state_not_loaded"
)

// SavedAircraft is one aircraft of a saved pair: what the alert names
// and what the carried alert is judged by.
type SavedAircraft struct {
	ID                  string     `json:"id"`
	TrackID             string     `json:"track_id"`
	Trust               core.Trust `json:"trust"`
	Source              string     `json:"source"`
	Instance            string     `json:"source_instance"`
	FlightID            string     `json:"flight_id,omitempty"`
	IntentID            string     `json:"intent_id,omitempty"`
	AuthorisationNumber string     `json:"authorisation_number,omitempty"`
	Cell5               string     `json:"cell5,omitempty"`
}

// Saved is one active proximity alert in the proximity_state bucket
// (key: StateKey of the pair key): its raise time (which, with the pair
// and the flight, names its alert ids), the aircraft, the last numbers
// core judged it with, and the instance that owns it ("" once released
// at a stop). Releasing says the owner's anchor left its cells: the
// instance owning the anchor takes the alert over at once.
type Saved struct {
	Owner         string           `json:"owner"`
	SavedAt       time.Time        `json:"saved_at"`
	Key           string           `json:"key"`
	RaisedAt      time.Time        `json:"raised_at"`
	LastTrueAt    time.Time        `json:"last_true_at"`
	CapturedAt    time.Time        `json:"captured_at"`
	Severity      core.Severity    `json:"severity"`
	Aircraft      [2]SavedAircraft `json:"aircraft"`
	Detail        map[string]any   `json:"detail"`
	LoSStartS     float64          `json:"los_start_s"`
	PolicyVersion int64            `json:"policy_version"`
	Releasing     bool             `json:"releasing,omitempty"`
}

// StateKey is the bucket key of a pair key (keys hold ids of any shape;
// the bucket's keys are a fixed alphabet).
func StateKey(pairKey string) string {
	sum := sha256.Sum256([]byte(pairKey))
	return hex.EncodeToString(sum[:])
}

// StateStore is where the active proximity alerts persist (KVStates in
// production). Get and All answer from memory, never waiting on I/O (the
// engine's loop calls them); Put and Delete are called off the loop.
type StateStore interface {
	// Loaded reports whether the bucket has been read at least once.
	Loaded() bool
	// All are every saved alert, by bucket key.
	All() map[string]Saved
	// Get is the saved alert of a pair key.
	Get(pairKey string) (Saved, bool)
	Put(ctx context.Context, s Saved) error
	Delete(ctx context.Context, pairKey string) error
}

// KVStates is the proximity_state bucket as a StateStore: a watched
// mirror for the reads (bus.Mirror; Run it) and single-key writes.
type KVStates struct {
	M  *bus.Mirror[Saved]
	KV bus.KVStore
}

// DecodeSaved reads one saved alert; a value that does not read is an
// error (the mirror counts and skips it).
func DecodeSaved(_ string, raw []byte) (Saved, error) {
	var s Saved
	if len(raw) > bus.ProximityStateBytes {
		return s, core.Fieldf("proximity_state", "longer than %d bytes", bus.ProximityStateBytes)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Saved{}, core.Fieldf("proximity_state", "not a saved proximity alert")
	}
	if s.Key == "" || s.RaisedAt.IsZero() || s.Aircraft[0].ID == "" || s.Aircraft[1].ID == "" {
		return Saved{}, core.Fieldf("proximity_state", "incomplete")
	}
	return s, nil
}

// Loaded implements StateStore.
func (k KVStates) Loaded() bool {
	_, _, loaded := k.M.Snapshot()
	return loaded
}

// All implements StateStore.
func (k KVStates) All() map[string]Saved {
	vals, _, _ := k.M.Snapshot()
	return vals
}

// Get implements StateStore.
func (k KVStates) Get(pairKey string) (Saved, bool) {
	v, found, _, _ := k.M.Get(StateKey(pairKey))
	if found && v.Key != pairKey {
		return Saved{}, false
	}
	return v, found
}

// Put implements StateStore.
func (k KVStates) Put(ctx context.Context, s Saved) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return k.KV.Put(ctx, StateKey(s.Key), raw)
}

// Delete implements StateStore.
func (k KVStates) Delete(ctx context.Context, pairKey string) error {
	return k.KV.Delete(ctx, StateKey(pairKey))
}

// persistOp is the latest write of one pair: a save (s) or a delete.
type persistOp struct {
	del bool
	s   Saved
}

// persister writes the engine's saves and deletes off its loop (C-13):
// the latest operation per pair wins, one goroutine writes. Bounded by
// the pairs the engine holds.
type persister struct {
	store    StateStore
	counters *core.Counters
	logger   *slog.Logger

	mu      sync.Mutex
	pending map[string]persistOp
	kick    chan struct{}
}

func newPersister(store StateStore, counters *core.Counters, logger *slog.Logger) *persister {
	return &persister{store: store, counters: counters, logger: logger, pending: map[string]persistOp{}, kick: make(chan struct{}, 1)}
}

func (p *persister) put(key string, op persistOp) {
	p.mu.Lock()
	p.pending[key] = op
	p.mu.Unlock()
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *persister) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			p.drain(context.WithoutCancel(ctx), time.Now().Add(2*time.Second))
			return
		case <-p.kick:
			// A write in flight is never cut short by the stop (each has
			// its own timeout); the stop drains what is left.
			p.drain(context.WithoutCancel(ctx), time.Time{})
		}
	}
}

// drain writes every pending operation; with a deadline (a stop), only
// until it.
func (p *persister) drain(ctx context.Context, deadline time.Time) {
	for {
		p.mu.Lock()
		ops := p.pending
		p.pending = map[string]persistOp{}
		p.mu.Unlock()
		if len(ops) == 0 {
			return
		}
		for key := range ops {
			if !deadline.IsZero() && time.Now().After(deadline) {
				return
			}
			p.do(ctx, key, ops[key])
		}
	}
}

func (p *persister) do(ctx context.Context, key string, op persistOp) {
	wctx, cancel := context.WithTimeout(ctx, bus.DefaultKVTimeout)
	defer cancel()
	if op.del {
		if err := p.store.Delete(wctx, key); err != nil {
			p.counters.Inc(CounterStateDelFailed)
			p.logger.LogAttrs(ctx, slog.LevelWarn, "proximity state not deleted; its TTL removes it", slog.String("pair_id", PairID(key)), obs.Err(err))
			return
		}
		p.counters.Inc(CounterStateDeleted)
		return
	}
	if err := p.store.Put(wctx, op.s); err != nil {
		p.counters.Inc(CounterStateSaveFailed)
		p.logger.LogAttrs(ctx, slog.LevelWarn, "proximity state not saved: a restart would carry the last saved numbers or none",
			slog.String("pair_id", PairID(key)), obs.Err(err))
		return
	}
	p.counters.Inc(CounterStateSaved)
}
