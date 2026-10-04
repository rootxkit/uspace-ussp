package national

import (
	"net/http"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// Geo answers GET /v1/geo and GET /v1/geo/intents/{id} from the CIS
// cache (internal/geo.Service over internal/cis.Evaluator).
type Geo struct {
	Service *geo.Service
	// Intents gives the caller's own intent's volumes and windows
	// (internal/intent.Service).
	Intents geo.IntentWindows
}

func (s *Server) geoUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.Geo == nil || s.Geo.Service == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "cis_unavailable", "", "no CIS cache is configured on this process").Write(w, r)
		return true
	}
	return false
}

// GetGeo is GET /v1/geo: the box at an instant (now when absent).
func (s *Server) GetGeo(w http.ResponseWriter, r *http.Request, params gen.GetGeoParams) {
	if s.geoUnavailable(w, r) {
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
	a, err := s.Geo.Service.Box(box, at)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// GetGeoForIntent is GET /v1/geo/intents/{intent_id}: the operator's
// own intent's volumes and windows as the query.
func (s *Server) GetGeoForIntent(w http.ResponseWriter, r *http.Request, intentID gen.IntentID) {
	if s.geoUnavailable(w, r) {
		return
	}
	if s.Geo.Intents == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "intents_unavailable", "", "the intent service is not configured on this process").Write(w, r)
		return
	}
	client := principal(r).Claims.Subject
	if portalSession(r) {
		if s.Intents == nil {
			s.intentsUnavailable(w, r)
			return
		}
		var ok bool
		if client, r, ok = s.actingClient(w, r, intentID.String(), nil, false); !ok {
			return
		}
	}
	a, err := s.Geo.Service.Intent(r.Context(), s.Geo.Intents, client, intentID.String())
	if err != nil {
		s.failIntent(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
