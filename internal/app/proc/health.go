package proc

import (
	"encoding/json"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// HealthHandlers answer the health paths of api/openapi.yaml with the
// contract's types. The api process serves them through the generated
// router (internal/national); every other process through HealthRoutes.
type HealthHandlers struct{ Health *obs.Health }

// GetHealthz answers 200 while the process runs.
func (h HealthHandlers) GetHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, gen.Health{Status: gen.Ok})
}

// GetReadyz runs every dependency check and answers 503 while a
// required dependency is down or unknown, 200 otherwise.
func (h HealthHandlers) GetReadyz(w http.ResponseWriter, r *http.Request) {
	rep := h.Health.Check(r.Context())
	status := http.StatusOK
	if !rep.Ready() {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, Readiness(rep))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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

// HealthRoutes adds GET /healthz and GET /readyz (the contract's health
// operations) and GET /metrics to mux, for a process that serves no
// other operation of api/openapi.yaml.
func HealthRoutes(mux *http.ServeMux, health *obs.Health, reg *prometheus.Registry) {
	h := HealthHandlers{Health: health}
	mux.HandleFunc("GET /healthz", h.GetHealthz)
	mux.HandleFunc("GET /readyz", h.GetReadyz)
	MetricsRoute(mux, reg)
}

// MetricsRoute adds GET /metrics to mux.
func MetricsRoute(mux *http.ServeMux, reg *prometheus.Registry) {
	mux.Handle("GET /metrics", obs.MetricsHandler(reg))
}
