package api

import (
	"context"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/weather"
	weatherstore "github.com/rootxkit/uspace-ussp/internal/weather/pgstore"
)

// DepWeather is the readiness dependency of the weather source (an
// optional service: never required).
const DepWeather = "weather"

// startWeather runs the optional weather information service (WP-16,
// Art. 12): the source of USSP_WEATHER_SOURCE polled every
// weather_refresh_s into weather_products, its state on /readyz. A
// malformed USSP_WEATHER_SOURCE refuses the start; an unset one runs the
// service without a source, which answers 503 weather_unavailable with
// reason not_configured and makes every decision say weather_unavailable.
func startWeather(ctx context.Context, rt *proc.Runtime, pol *policy.Service) (*weather.Service, error) {
	src, err := weather.NewSource(rt.Config.WeatherSource, nil)
	if err != nil {
		return nil, err
	}
	counters := &core.Counters{}
	proc.Publish(rt, "weather", counters)
	svc := &weather.Service{Source: src, Counters: counters, Logger: rt.Logger.With("component", "weather"),
		Policy: func() policy.Record {
			if r, ok := pol.Current(); ok {
				return r
			}
			return policy.Record{Values: policy.Defaults()}
		}}
	if rt.Store != nil && rt.Store.Rel != nil {
		svc.Store = weatherstore.Store{S: rt.Store}
	}
	rt.Health.Register(DepWeather, false, svc.Probe())
	rt.Go(ctx, svc.Run)
	return svc, nil
}

// weatherCheck is the decision's consultation of the weather service
// (intent.Weather).
type weatherCheck struct{ s *weather.Service }

// Check implements intent.Weather.
func (w weatherCheck) Check(ctx context.Context, boxes []geodesy.BBox, from, to time.Time) intent.WeatherCheck {
	c := w.s.Check(ctx, boxes, from, to)
	return intent.WeatherCheck{Ref: c.Ref(), Unavailable: c.Unavailable, Stale: c.Stale, Advisories: c.Advisories}
}
