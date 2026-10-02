package sources

import (
	"context"
	"log/slog"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// Follower is the hot path's side of the switches (B-09): core's
// sources.Follower fed by the KV bucket source_control (watch, a re-read
// every 300 s) and the ctl.sources push. It holds no state until the
// first one arrives, and then everything is enabled (never fail closed
// by omission); a state from an older version is ignored, one from a new
// epoch (a restored database) is taken. Query answers from what it holds.
type Follower struct {
	*coresources.Follower
	kv *bus.Follower[coresources.State]
}

// Follow returns the follower of the switches on c; Run it.
func Follow(c *bus.Conn, logger *slog.Logger) *Follower {
	f := &Follower{Follower: coresources.NewFollower()}
	f.kv = &bus.Follower[coresources.State]{
		JS: c.JetStream(), Bucket: BucketSourceControl, Key: bus.KeySources, Decode: bus.DecodeSources,
		Apply: func(s coresources.State) { f.Apply(s) }, Core: c.Conn, Push: bus.CtlSources, Logger: logger, Counters: &core.Counters{},
	}
	return f
}

// Run follows until ctx ends.
func (f *Follower) Run(ctx context.Context) { f.kv.Run(ctx) }

// AgeS is the age of the state held since the bucket last confirmed it,
// and false while none is held (the status line says "unknown": SC-08
// step 8).
func (f *Follower) AgeS() (float64, bool) {
	_, age, ok := f.kv.Value()
	return age, ok
}

// FollowCounters are the KV follower's counters (applied, decode and
// read failures); Counters are core's (applied, ignored_older_version,
// new_epoch).
func (f *Follower) FollowCounters() *core.Counters { return f.kv.Counters }
