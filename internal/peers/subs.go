package peers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// MaxSavedSubscriptions bounds the subscriptions read back at a start
// (E-10): four times MaxAreas, the areas of several runs.
const MaxSavedSubscriptions = 4 * MaxAreas

// SavedSubscription is one of our DSS RID subscriptions as it is kept
// across a restart (rid_dp_subscriptions): the base URL it was put
// under, the area it covers, its version at the DSS and its end.
type SavedSubscription struct {
	USSBaseURL string    `json:"uss_base_url"`
	AreaID     string    `json:"area_id"`
	MinLat     float64   `json:"min_lat"`
	MinLon     float64   `json:"min_lon"`
	MaxLat     float64   `json:"max_lat"`
	MaxLon     float64   `json:"max_lon"`
	Version    string    `json:"version"`
	End        time.Time `json:"end"`
}

func savedOf(base string, st *subState) SavedSubscription {
	b := st.area.Box
	return SavedSubscription{USSBaseURL: normBase(base), AreaID: st.area.ID, MinLat: b.MinLat, MinLon: b.MinLon, MaxLat: b.MaxLat, MaxLon: b.MaxLon,
		Version: st.version, End: st.end.UTC()}
}

func (s SavedSubscription) state() *subState {
	return &subState{area: Area{ID: s.AreaID, Box: geodesy.BBox{MinLat: s.MinLat, MinLon: s.MinLon, MaxLat: s.MaxLat, MaxLon: s.MaxLon}},
		version: s.Version, end: s.End}
}

// SubscriptionStore keeps our DSS RID subscriptions across a restart,
// so that one whose area was dropped while the process was down is
// still deleted at the DSS (it would otherwise stay until its 24 h end,
// with the peers notifying it).
type SubscriptionStore interface {
	// Load is every subscription saved, by subscription id.
	Load(ctx context.Context) (map[string]SavedSubscription, error)
	// Put saves one subscription after the DSS answered its put.
	Put(ctx context.Context, id string, s SavedSubscription) error
	// Delete forgets one after the DSS deleted it (or no longer has it).
	Delete(ctx context.Context, id string) error
}

// KVSubscriptions is the SubscriptionStore on a KV bucket
// (rid_dp_subscriptions), one key per subscription id.
type KVSubscriptions struct {
	KV bus.KVStore
}

// Load reads every saved subscription; a value that does not read is
// left out (its subscription ends at the DSS within 24 h).
func (s KVSubscriptions) Load(ctx context.Context) (map[string]SavedSubscription, error) {
	es, err := s.KV.All(ctx, MaxSavedSubscriptions)
	if err != nil {
		return nil, err
	}
	out := make(map[string]SavedSubscription, len(es))
	for _, e := range es {
		var v SavedSubscription
		if json.Unmarshal(e.Value, &v) != nil || v.AreaID == "" || v.Version == "" {
			continue
		}
		out[e.Key] = v
	}
	return out, nil
}

// Put writes one subscription.
func (s KVSubscriptions) Put(ctx context.Context, id string, v SavedSubscription) error {
	if !bus.ValidKey(id) {
		return core.Fieldf("subscription_id", "not a KV key")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > bus.RIDSubscriptionBytes {
		return core.Fieldf("subscription", "over %d bytes", bus.RIDSubscriptionBytes)
	}
	return s.KV.Put(ctx, id, b)
}

// Delete removes one subscription.
func (s KVSubscriptions) Delete(ctx context.Context, id string) error {
	if !bus.ValidKey(id) {
		return core.Fieldf("subscription_id", "not a KV key")
	}
	return s.KV.Delete(ctx, id)
}
