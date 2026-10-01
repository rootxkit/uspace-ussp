package proc

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// healthServer implements the generated strict server for the health
// paths of api/openapi.yaml, so their bodies are the contract's types.
type healthServer struct{ health *obs.Health }

// GetHealthz answers 200 while the process runs.
func (s healthServer) GetHealthz(context.Context, gen.GetHealthzRequestObject) (gen.GetHealthzResponseObject, error) {
	return gen.GetHealthz200JSONResponse{Status: gen.Ok}, nil
}

// GetReadyz runs every dependency check and answers 503 while a
// required dependency is down or unknown, 200 otherwise.
func (s healthServer) GetReadyz(ctx context.Context, _ gen.GetReadyzRequestObject) (gen.GetReadyzResponseObject, error) {
	rep := s.health.Check(ctx)
	body := Readiness(rep)
	if !rep.Ready() {
		return gen.GetReadyz503JSONResponse(body), nil
	}
	return gen.GetReadyz200JSONResponse(body), nil
}

// Readiness converts a health report into the contract's body.
func Readiness(rep obs.Report) gen.Readiness {
	out := gen.Readiness{
		Status:       gen.ReadinessStatus(rep.Status),
		CheckedAt:    rep.CheckedAt,
		Dependencies: make(map[string]gen.Dependency, len(rep.Dependencies)),
		Degraded:     rep.Degraded,
	}
	for name, d := range rep.Dependencies {
		dep := gen.Dependency{State: gen.DependencyState(d.State), Required: d.Required, Since: d.Since, AgeS: d.AgeS}
		if d.Detail != "" {
			detail := d.Detail
			dep.Detail = &detail
		}
		out.Dependencies[name] = dep
	}
	return out
}

// HealthRoutes adds GET /healthz, GET /readyz and GET /metrics to mux.
func HealthRoutes(mux *http.ServeMux, health *obs.Health, reg *prometheus.Registry) {
	strict := gen.NewStrictHandlerWithOptions(healthServer{health: health}, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpx.WriteError,
		ResponseErrorHandlerFunc: httpx.WriteError,
	})
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{BaseRouter: mux})
	mux.Handle("GET /metrics", obs.MetricsHandler(reg))
}
