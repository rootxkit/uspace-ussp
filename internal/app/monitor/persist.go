package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Counters of the persisted state.
const (
	CounterStateSaved        = "monitor_state_saved"
	CounterStateSaveFailed   = "monitor_state_save_failed"
	CounterStateLoadFailed   = "monitor_state_load_failed"
	CounterStateUnreadable   = "monitor_state_unreadable"
	CounterStateRestored     = "monitor_state_restored"
	CounterStateSeeded       = "monitor_state_seeded_from_intent"
	CounterStateNotOurs      = "monitor_state_owned_elsewhere"
	CounterStateDeleteFailed = "monitor_state_delete_failed"
	CounterReleased          = "monitor_flight_released"
	CounterYielded           = "monitor_flight_yielded"
)

// Timing of the persisted state.
const (
	// loadTimeout bounds the one read a worker makes when it takes over
	// a flight it did not track (a handover); the start reads every
	// state once before the track feed opens.
	loadTimeout = 500 * time.Millisecond
	// preloadTimeout bounds the read of every state at the start.
	preloadTimeout = 10 * time.Second
	// liveOwnerS is how recent another instance's save must be for its
	// ownership to be respected at a start (three heartbeats).
	liveOwnerS = 3 * DefaultConfHeartbeat
	// yieldCheckAfter is the silence after which a worker asks whether
	// another instance has taken its flight over (a flight that left
	// this instance's cells without a sample in its ring).
	yieldCheckAfter = 2 * time.Second
)

// ErrStateConflict is a save on a revision that is no longer the key's:
// another instance (or this one, earlier) wrote it meanwhile.
var ErrStateConflict = errors.New("conformance state written meanwhile")

// Saved is one flight's persisted conformance (the conformance_state
// bucket, key = flight id): the tracker's state machine, the nearby
// alerts it raised, the instance that owns it and the cell3 it is homed
// in. Owner is empty once released to whichever instance's cell the
// flight enters.
type Saved struct {
	Owner    string                   `json:"owner"`
	SavedAt  time.Time                `json:"saved_at"`
	Home     string                   `json:"home_cell3"`
	Instance string                   `json:"source_instance,omitempty"`
	LastAt   time.Time                `json:"last_at"`
	Tracker  conformance.TrackerState `json:"tracker"`
	Nearby   []conformance.SavedAlert `json:"nearby,omitempty"`
	// Zones are the flight's zone and identification alerts (brief
	// WP-12), carried by the instance that takes the flight over.
	Zones []geo.Alert `json:"zones,omitempty"`
}

// StoredState is a Saved with the revision it was read at.
type StoredState struct {
	Saved
	Rev uint64
}

// StateStore is where the trackers persist (KVStates in production).
type StateStore interface {
	// All are every saved state.
	All(ctx context.Context) ([]StoredState, error)
	// Load is one flight's state; false when there is none.
	Load(ctx context.Context, flightID string) (StoredState, bool, error)
	// Save writes s on the revision rev (0: the key must not exist) and
	// returns the new revision; ErrStateConflict when rev is not the
	// key's.
	Save(ctx context.Context, flightID string, s Saved, rev uint64) (uint64, error)
	// Delete removes the flight's state.
	Delete(ctx context.Context, flightID string) error
}

// KVStates is the conformance_state bucket (KV.Bucket) as a
// StateStore; each call is bounded by KV.Timeout, the start's read of
// every state by preloadTimeout.
type KVStates struct {
	KV bus.KVStore
}

func (k KVStates) kv(d time.Duration) bus.KVStore {
	s := k.KV
	if d > 0 {
		s.Timeout = d
	}
	return s
}

func decodeSaved(raw []byte) (Saved, error) {
	var s Saved
	if len(raw) > bus.ConformanceStateBytes {
		return s, core.Fieldf("conformance_state", "longer than %d bytes", bus.ConformanceStateBytes)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Saved{}, core.Fieldf("conformance_state", "%v", err)
	}
	return s, nil
}

// All implements StateStore; a value that does not read is skipped.
func (k KVStates) All(ctx context.Context) ([]StoredState, error) {
	es, err := k.kv(preloadTimeout).All(ctx, MaxTableFlights)
	out := make([]StoredState, 0, len(es))
	for i := range es {
		if s, derr := decodeSaved(es[i].Value); derr == nil {
			out = append(out, StoredState{Saved: s, Rev: es[i].Rev})
		}
	}
	return out, err
}

// Load implements StateStore.
func (k KVStates) Load(ctx context.Context, flightID string) (StoredState, bool, error) {
	e, found, err := k.kv(0).GetRev(ctx, flightID)
	if err != nil || !found {
		return StoredState{}, false, err
	}
	s, err := decodeSaved(e.Value)
	if err != nil {
		return StoredState{}, false, err
	}
	return StoredState{Saved: s, Rev: e.Rev}, true, nil
}

// Save implements StateStore.
func (k KVStates) Save(ctx context.Context, flightID string, s Saved, rev uint64) (uint64, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return 0, err
	}
	n, err := k.kv(0).PutRev(ctx, flightID, raw, rev)
	if errors.Is(err, bus.ErrRevisionConflict) {
		return 0, ErrStateConflict
	}
	return n, err
}

// Delete implements StateStore.
func (k KVStates) Delete(ctx context.Context, flightID string) error {
	return k.kv(0).Delete(ctx, flightID)
}

// opKind is what the persister does with a flight's state.
type opKind int

const (
	// opSave writes the state; another instance's newer ownership wins
	// and this one yields the flight.
	opSave opKind = iota
	// opClaim writes the state of a flight taken over in this
	// instance's cell: it wins over any other owner.
	opClaim
	// opRelease writes the state with no owner (the flight left this
	// instance's cells); it never overwrites another owner's state.
	opRelease
	// opDelete removes the state (the flight ended).
	opDelete
	// opCheck reads the state and yields the flight when another
	// instance owns it.
	opCheck
)

type persistOp struct {
	kind opKind
	s    Saved
}

// persister writes the workers' states off their loops (C-13): the
// latest operation per flight wins, written by one goroutine with the
// revision it last wrote (a compare-and-set, so two instances never
// overwrite each other unseen). Bounded by the flights tracked.
type persister struct {
	store    StateStore
	me       string
	counters *core.Counters
	logger   *slog.Logger
	yield    func(flightID string)

	mu      sync.Mutex
	pending map[string]persistOp
	revs    map[string]uint64
	kick    chan struct{}
}

func newPersister(store StateStore, me string, counters *core.Counters, logger *slog.Logger, yield func(string)) *persister {
	return &persister{store: store, me: me, counters: counters, logger: logger, yield: yield,
		pending: map[string]persistOp{}, revs: map[string]uint64{}, kick: make(chan struct{}, 1)}
}

func (p *persister) put(id string, op persistOp) {
	p.mu.Lock()
	if cur, ok := p.pending[id]; ok && op.kind == opCheck && cur.kind != opCheck {
		p.mu.Unlock()
		return // a write already pending answers the check
	}
	if cur, ok := p.pending[id]; ok && cur.kind == opClaim && op.kind == opSave {
		op.kind = opClaim // the claim is not written yet: this save is it
	}
	p.pending[id] = op
	p.mu.Unlock()
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// adopt records the revision a flight's state was read at.
func (p *persister) adopt(id string, rev uint64) {
	p.mu.Lock()
	p.revs[id] = rev
	p.mu.Unlock()
}

func (p *persister) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			p.drain(context.WithoutCancel(ctx), time.Now().Add(2*time.Second))
			return
		case <-p.kick:
			p.drain(ctx, time.Time{})
		}
	}
}

// drain does every pending operation; with a deadline (a stop), only
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
		for id := range ops {
			if !deadline.IsZero() && time.Now().After(deadline) {
				return
			}
			p.do(ctx, id, ops[id])
		}
	}
}

func (p *persister) rev(id string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.revs[id]
}

func (p *persister) setRev(id string, rev uint64, keep bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if keep {
		p.revs[id] = rev
	} else {
		delete(p.revs, id)
	}
}

func (p *persister) do(ctx context.Context, id string, op persistOp) {
	if op.kind == opDelete {
		p.setRev(id, 0, false)
		if err := p.store.Delete(ctx, id); err != nil {
			p.counters.Inc(CounterStateDeleteFailed)
			p.logger.LogAttrs(ctx, slog.LevelWarn, "conformance state not deleted; its TTL removes it", obs.FlightID(id), obs.Err(err))
		}
		return
	}
	if op.kind == opCheck {
		cur, found, err := p.store.Load(ctx, id)
		if err != nil {
			p.counters.Inc(CounterStateLoadFailed)
			return
		}
		if found && cur.Owner != "" && cur.Owner != p.me {
			p.setRev(id, 0, false)
			p.yield(id)
		}
		return
	}
	s := op.s
	s.SavedAt = time.Now().UTC()
	if op.kind != opRelease {
		s.Owner = p.me
	}
	rev, err := p.store.Save(ctx, id, s, p.rev(id))
	if errors.Is(err, ErrStateConflict) {
		cur, found, lerr := p.store.Load(ctx, id)
		if lerr != nil {
			p.counters.Inc(CounterStateLoadFailed)
			err = lerr
		} else {
			other := found && cur.Owner != "" && cur.Owner != p.me
			switch {
			case other && op.kind == opRelease:
				p.setRev(id, 0, false)
				return // taken over already: nothing to release
			case other && op.kind == opSave:
				p.setRev(id, 0, false)
				p.counters.Inc(CounterStateNotOurs)
				p.yield(id)
				return
			}
			var at uint64
			if found {
				at = cur.Rev
			}
			rev, err = p.store.Save(ctx, id, s, at)
		}
	}
	if err != nil {
		p.setRev(id, 0, false)
		p.counters.Inc(CounterStateSaveFailed)
		p.logger.LogAttrs(ctx, slog.LevelWarn, "conformance state not saved: a restart or handover would start the flight from its intent's state",
			obs.FlightID(id), obs.Err(err))
		return
	}
	p.counters.Inc(CounterStateSaved)
	p.setRev(id, rev, op.kind != opRelease)
}
