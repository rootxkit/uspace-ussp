package national

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

type weatherSvc struct {
	a   any
	err error
	box geodesy.BBox
	at  time.Time
}

func (w *weatherSvc) WeatherAnswer(_ context.Context, box geodesy.BBox, at time.Time) (any, error) {
	w.box, w.at = box, at
	return w.a, w.err
}

type unavailable struct{ reason string }

func (u unavailable) Error() string                        { return "unavailable: " + u.reason }
func (u unavailable) WeatherUnavailable() (string, string) { return u.reason, "x" }

// GET /v1/weather: 200 with the answer and the instant passed through;
// 503 weather_unavailable naming the reason under weather_source when
// nothing is configured or the service cannot answer (E-02); 400 for a
// bad box; 500 for anything else.
func TestWeatherRoute(t *testing.T) {
	get := func(s *Server, p gen.GetWeatherParams) (int, map[string]any) {
		rec := httptest.NewRecorder()
		s.GetWeather(rec, httptest.NewRequest(http.MethodGet, "/v1/weather", nil), p)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	at := time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)
	svc := &weatherSvc{a: map[string]any{"at": at, "stale": true, "products": []any{}}}
	code, body := get(&Server{Weather: svc}, gen.GetWeatherParams{Bbox: "44.9,41.6,45.0,41.7", At: &at})
	if code != 200 || body["stale"] != true || svc.box.MinLon != 44.9 || !svc.at.Equal(at) {
		t.Fatalf("%d %v %+v", code, body, svc)
	}
	if code, _ := get(&Server{Weather: svc}, gen.GetWeatherParams{Bbox: "44.9,41.6,45.0,41.7"}); code != 200 || !svc.at.IsZero() {
		t.Fatalf("no at: %d %v", code, svc.at)
	}
	reason := func(body map[string]any) string {
		es, _ := body["errors"].([]any)
		if len(es) != 1 {
			return ""
		}
		e, _ := es[0].(map[string]any)
		if e["field"] != "weather_source" {
			return ""
		}
		r, _ := e["reason"].(string)
		return r
	}
	for name, c := range map[string]struct {
		s      *Server
		code   int
		slug   string
		reason string
	}{
		"not configured": {&Server{}, 503, "weather_unavailable", "not_configured"},
		"no source":      {&Server{Weather: &weatherSvc{err: fmt.Errorf("answer: %w", unavailable{"not_configured"})}}, 503, "weather_unavailable", "not_configured"},
		"no stations":    {&Server{Weather: &weatherSvc{err: unavailable{"no_stations"}}}, 503, "weather_unavailable", "no_stations"},
		"other":          {&Server{Weather: &weatherSvc{err: errors.New("boom")}}, 500, "internal", ""},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := get(c.s, gen.GetWeatherParams{Bbox: "44.9,41.6,45.0,41.7"})
			if code != c.code || body["type"] != "https://schemas.uspace.ge/problems/"+c.slug || reason(body) != c.reason {
				t.Fatalf("%d %v", code, body)
			}
		})
	}
	if code, body := get(&Server{Weather: svc}, gen.GetWeatherParams{Bbox: "1,2,3"}); code != 400 || body["type"] != "https://schemas.uspace.ge/problems/validation" {
		t.Fatalf("bad box: %d %v", code, body)
	}
}
