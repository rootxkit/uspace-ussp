package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// BackfillBatch bounds the intent_active entries one BackfillOwners pass
// rewrites (E-10); the rest wait for the next pass.
const BackfillBatch = SweepBatch

// Counters of the owner backfill (with the service's intent_ prefix).
const (
	// CounterOwnersBackfilled counts the intent_active entries rewritten
	// with their operator and client.
	CounterOwnersBackfilled = "owners_backfilled"
	// CounterOwnersBackfillLeft counts the ownerless entries a pass left
	// for the next one (more than its batch).
	CounterOwnersBackfillLeft = "owners_backfill_left"
)

// ActiveKV is intent_active as the owner backfill reads and writes it
// (bus.Projector).
type ActiveKV interface {
	Keys(ctx context.Context, bucket string) ([]string, error)
	Get(ctx context.Context, bucket, key string) ([]byte, bool, error)
	PutJSON(ctx context.Context, bucket, key string, v any) error
}

// owners is the part of an intent_active value the backfill reads.
type owners struct {
	OperatorID string `json:"operator_id"`
	ClientID   string `json:"client_id"`
}

// BackfillResult is what one BackfillOwners pass did.
type BackfillResult struct {
	// Rewritten entries now carry their operator and client.
	Rewritten int
	// Left are the ownerless entries past the batch, for the next pass.
	Left int
}

// BackfillOwners rewrites, at most limit of them, the intent_active
// entries projected before intent/state/v1 carried operator_id and
// client_id (brief WP-17), which traffic-ws refuses to a portal session
// (Hub.OwnedBy) until the intent's next version is projected. Each is
// rewritten from its intent's newest version under the projection's row
// lock, and only when that version is projected (an unprojected one is
// Republish's, with its intent.v1 message): the value written is the one
// the projection of that version writes now, and nothing is published.
// Idempotent: an entry that carries both is left alone, so a second pass
// rewrites nothing; an entry whose intent is not active is left to the
// projection that deletes it.
func (s *Service) BackfillOwners(ctx context.Context, kv ActiveKV, limit int) (BackfillResult, error) {
	var res BackfillResult
	keys, err := kv.Keys(ctx, bus.BucketIntentActive)
	if err != nil {
		return res, fmt.Errorf("intent_active keys: %w", err)
	}
	slices.Sort(keys)
	for _, id := range keys {
		raw, found, err := kv.Get(ctx, bus.BucketIntentActive, id)
		if err != nil {
			return res, fmt.Errorf("intent_active %s: %w", id, err)
		}
		var o owners
		if !found || json.Unmarshal(raw, &o) != nil || (o.OperatorID != "" && o.ClientID != "") {
			continue
		}
		if res.Rewritten >= limit {
			res.Left++
			continue
		}
		wrote := false
		if _, err := s.Store.Reproject(ctx, id, func(ctx context.Context, r *Record) error {
			if !slices.Contains(ActiveStates, r.LocalState) || r.OperatorID == "" || r.ClientID == "" {
				return nil
			}
			wrote = true
			return kv.PutJSON(ctx, bus.BucketIntentActive, id, StateOf(r))
		}); err != nil {
			return res, fmt.Errorf("intent %s: %w", id, err)
		}
		if wrote {
			res.Rewritten++
		}
	}
	if s.Counters != nil {
		s.Counters.Add("intent_"+CounterOwnersBackfilled, uint64(res.Rewritten))
		s.Counters.Add("intent_"+CounterOwnersBackfillLeft, uint64(res.Left))
	}
	return res, nil
}

// RunOwnerBackfill runs BackfillOwners at start, BackfillBatch entries a
// pass, every interval until a pass leaves nothing behind (a pass that
// fails, NATS or the database down, is tried again), then stops: every
// later projection carries the operator and client itself.
func (s *Service) RunOwnerBackfill(ctx context.Context, kv ActiveKV, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	total := 0
	for {
		res, err := s.BackfillOwners(ctx, kv, BackfillBatch)
		total += res.Rewritten
		switch {
		case err != nil:
			obs.Error(ctx, s.logger(), "intent_active owner backfill failed; it is tried again", err,
				slog.Int("rewritten", res.Rewritten))
		case res.Left == 0:
			s.logger().Info("intent_active owner backfill done", slog.Int("rewritten", total))
			return
		default:
			s.logger().Info("intent_active owner backfill continues", slog.Int("rewritten", res.Rewritten), slog.Int("left", res.Left))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
