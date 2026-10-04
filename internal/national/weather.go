package national

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// WeatherAnswerer answers GET /v1/weather (internal/weather.Service):
// the answer in the shape of the WeatherAnswer schema.
type WeatherAnswerer interface {
	WeatherAnswer(ctx context.Context, box geodesy.BBox, at time.Time) (any, error)
}

// WeatherUnavailableError is a weather answer that cannot be given, with
// its reason (internal/weather.UnavailableError).
type WeatherUnavailableError interface {
	error
	WeatherUnavailable() (reason, detail string)
}

// SlugWeatherUnavailable is the problem of a weather answer that cannot
// be given; errors[] names the reason under the field weather_source.
const SlugWeatherUnavailable = "weather_unavailable"

// WeatherNotConfigured is the reason when no weather service is wired.
const WeatherNotConfigured = "not_configured"

func weatherUnavailable(w http.ResponseWriter, r *http.Request, reason, detail string) {
	httpx.NewProblem(http.StatusServiceUnavailable, SlugWeatherUnavailable, "", detail, core.Fieldf("weather_source", "%s", reason)).Write(w, r)
}

// GetWeather is GET /v1/weather: the products whose area meets the box
// at the instant (now when absent); no source configured, no station or
// no store is 503 weather_unavailable with the reason, never an empty
// answer (E-02).
func (s *Server) GetWeather(w http.ResponseWriter, r *http.Request, params gen.GetWeatherParams) {
	if s.Weather == nil {
		weatherUnavailable(w, r, WeatherNotConfigured, "no weather service is configured on this process")
		return
	}
	box, err := geo.ParseBBox(params.Bbox)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var at time.Time
	if params.At != nil {
		at = *params.At
	}
	a, err := s.Weather.WeatherAnswer(r.Context(), box, at)
	var u WeatherUnavailableError
	if errors.As(err, &u) {
		reason, detail := u.WeatherUnavailable()
		weatherUnavailable(w, r, reason, detail)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
