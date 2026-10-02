package api

import (
	"context"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
	"github.com/rootxkit/uspace-ussp/internal/registry/pgstore"
)

// DepRegistry is the readiness dependency of the registry validity
// cache and its change feed.
const DepRegistry = "registry"

// Registry is what the api process holds of the registry validity
// cache: the cache intent, accounts and GET /v1/registry/validate call,
// and its feed. The answers are projected to the KV bucket
// registry_validity the hot path reads.
type Registry struct {
	Cache *registry.Cache
	Feed  *registry.Feed
}

// startRegistry builds the cache and the feed from the configuration,
// registers their readiness and starts the feed on rt. A missing
// authority or token client is not a start failure: every uncached key
// is then unknown (registry_unavailable) and /readyz says why.
func startRegistry(ctx context.Context, rt *proc.Runtime, current func() policy.Values, tokens registry.TokenSource, kv registry.KVWriter) (*Registry, error) {
	cfg := rt.Config
	counters := &core.Counters{}
	proc.Publish(rt, "registry", counters)
	var client *registry.Client
	if cfg.AuthorityBaseURL != "" {
		cc := registry.ClientConfig{BaseURL: cfg.AuthorityBaseURL, Tokens: tokens}
		var err error
		if client, err = registry.NewClient(cc); err != nil {
			return nil, err
		}
	}
	var st *pgstore.Store
	if rt.Store != nil && rt.Store.Rel != nil {
		st = &pgstore.Store{S: rt.Store}
	}
	projection := registry.BusProjector{KV: kv}
	cc := registry.CacheConfig{
		Client: client, Projector: projection, Counters: counters, Logger: rt.Logger.With("component", "registry"),
		TTL: func() registry.TTL { return registry.TTLFromPolicy(current()) },
	}
	fc := registry.FeedConfig{Client: client, Projector: projection, Counters: counters, Logger: rt.Logger.With("component", "registry_feed")}
	if st != nil {
		cc.Store, cc.Audit, fc.Store = *st, *st, *st
	}
	out := &Registry{Cache: registry.NewCache(cc), Feed: registry.NewFeed(fc)}
	rt.Health.Register(DepRegistry, false, registry.ReadinessProbe(out.Cache, out.Feed))
	if client != nil && st != nil {
		rt.Go(ctx, out.Feed.Run)
	}
	return out, nil
}
