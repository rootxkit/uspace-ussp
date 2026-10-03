package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// cisPublishTimeout bounds the publish of one installed version on
// cis.v1 (it runs on the dataset's worker).
const cisPublishTimeout = 5 * time.Second

// startGeo wires geo-awareness (brief WP-12): GET /v1/geo* from the CIS
// cache, the publish of every installed version on cis.v1.<dataset>
// (after its projection and store: the monitor reloads its zones and
// traffic-ws tells its subscribers to refetch), and the standing
// re-check of the active intents on every change and every sweep. It
// returns the national handlers.
func startGeo(ctx context.Context, rt *proc.Runtime, cisState *CIS, intents *intent.Service) *national.Geo {
	counters := &core.Counters{}
	proc.Publish(rt, "geo", counters)
	pub := bus.NewPublisher(rt.Bus, counters)
	logger := rt.Logger.With("component", "geo")
	cisState.OnChange(func(ctx context.Context, c cis.Change) {
		subject, err := bus.CIS(string(c.Dataset))
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, cisPublishTimeout)
			err = pub.Publish(pctx, subject, geo.ChangedMessageOf(c, time.Now()))
			cancel()
		}
		if err != nil {
			// The monitor reloads from cis_current on its own watch and
			// the sweep re-checks the intents: a lost publish costs the
			// refetch hint only. Counted and logged.
			counters.Inc("geo_change_publish_failed")
			logger.LogAttrs(ctx, slog.LevelWarn, "CIS change not published on cis.v1", slog.String("dataset", string(c.Dataset)),
				slog.Int64("version", c.Version), obs.Err(err))
		}
	})
	re := geo.NewRechecker(intents, cisState.Evaluator, counters, logger)
	cisState.OnChange(re.Changed)
	rt.Go(ctx, re.Run)
	return &national.Geo{Service: &geo.Service{CIS: cisState.Evaluator, Counters: counters}, Intents: intents}
}
