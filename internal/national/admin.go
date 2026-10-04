package national

import (
	"context"
	"errors"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// Admin serves the console's /v1/admin/* operations of WP-18
// (internal/admin, adapted by internal/app into the contract's types).
// Its refusals are httpx.StatusError (404, 409, 503) or field errors
// (400); a policy or source switch the KV refuses is 503 with
// Retry-After (B-09).
type Admin interface {
	Flights(ctx context.Context) (gen.AdminFlights, error)
	Alerts(ctx context.Context, recent bool) (gen.AdminAlerts, error)
	Escalations(ctx context.Context) (gen.AdminAlerts, error)
	Escalate(ctx context.Context, staffID, alertID, reason string) (gen.AdminAlert, error)
	CloseAlert(ctx context.Context, staffID, alertID, reason string) (gen.AdminAlert, error)
	DSS(ctx context.Context) (gen.AdminDSS, error)
	Inputs(ctx context.Context) (gen.AdminInputs, error)
	Policy(ctx context.Context) (gen.AdminPolicy, error)
	PutPolicy(ctx context.Context, staffID string, in gen.PolicyPut) (gen.PolicyVersion, error)
	Sources(ctx context.Context) (gen.SourceSwitches, error)
	Switch(ctx context.Context, staffID string, in gen.SourceSwitchRequest) (gen.SourceSwitches, error)
	Cases(ctx context.Context) (gen.EmergencyCases, error)
	Case(ctx context.Context, flightID string) (gen.EmergencyCase, error)
	Act(ctx context.Context, staffID, flightID string, in gen.EmergencyAction) (gen.EmergencyCase, bool, error)
	RecordDays(ctx context.Context) (gen.RecordDays, error)
	Events(ctx context.Context, entityType, entityID string, limit int) (gen.AdminEvents, error)
}

// ProjectionRetryAfter is the wait a 503 of a refused KV projection
// asks for.
const ProjectionRetryAfter = 5 * time.Second

func adminUnavailable(w http.ResponseWriter, r *http.Request) {
	httpx.NewProblem(http.StatusServiceUnavailable, "admin_unavailable", "", "the console endpoints are not configured on this process").Write(w, r)
}

// admin answers v or the problem of err; a 503 carries Retry-After.
func (s *Server) admin(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		var se httpx.StatusError
		if errors.As(err, &se) && se.HTTPStatus() == http.StatusServiceUnavailable {
			httpx.RetryAfter(w, ProjectionRetryAfter)
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, v)
}

func staff(r *http.Request) string { return principal(r).Claims.Subject }

// ListAdminFlights is GET /v1/admin/flights.
func (s *Server) ListAdminFlights(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.Flights(r.Context())
	s.admin(w, r, http.StatusOK, v, err)
}

// ListAdminAlerts is GET /v1/admin/alerts.
func (s *Server) ListAdminAlerts(w http.ResponseWriter, r *http.Request, params gen.ListAdminAlertsParams) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	recent := false
	if params.View != nil {
		switch *params.View {
		case "active":
		case "recent":
			recent = true
		default:
			s.fail(w, r, core.Fieldf("view", "must be active or recent"))
			return
		}
	}
	v, err := s.Admin.Alerts(r.Context(), recent)
	s.admin(w, r, http.StatusOK, v, err)
}

// ListEscalations is GET /v1/admin/escalations.
func (s *Server) ListEscalations(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.Escalations(r.Context())
	s.admin(w, r, http.StatusOK, v, err)
}

// EscalateAlert is POST /v1/admin/alerts/{alert_id}/escalate.
func (s *Server) EscalateAlert(w http.ResponseWriter, r *http.Request, alertID gen.AlertID) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	var in gen.AlertAction
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Admin.Escalate(r.Context(), staff(r), alertID.String(), in.Reason)
	s.admin(w, r, http.StatusOK, v, err)
}

// CloseAlert is POST /v1/admin/alerts/{alert_id}/close.
func (s *Server) CloseAlert(w http.ResponseWriter, r *http.Request, alertID gen.AlertID) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	var in gen.AlertAction
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Admin.CloseAlert(r.Context(), staff(r), alertID.String(), in.Reason)
	s.admin(w, r, http.StatusOK, v, err)
}

// GetAdminDSS is GET /v1/admin/dss.
func (s *Server) GetAdminDSS(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.DSS(r.Context())
	s.admin(w, r, http.StatusOK, v, err)
}

// GetAdminInputs is GET /v1/admin/inputs.
func (s *Server) GetAdminInputs(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.Inputs(r.Context())
	s.admin(w, r, http.StatusOK, v, err)
}

// GetAdminPolicy is GET /v1/admin/policy.
func (s *Server) GetAdminPolicy(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.Policy(r.Context())
	s.admin(w, r, http.StatusOK, v, err)
}

// PutAdminPolicy is PUT /v1/admin/policy.
func (s *Server) PutAdminPolicy(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	var in gen.PolicyPut
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Admin.PutPolicy(r.Context(), staff(r), in)
	s.admin(w, r, http.StatusCreated, v, err)
}

// ListSourceSwitches is GET /v1/admin/sources.
func (s *Server) ListSourceSwitches(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.Sources(r.Context())
	s.admin(w, r, http.StatusOK, v, err)
}

// SwitchSource is POST /v1/admin/sources.
func (s *Server) SwitchSource(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	var in gen.SourceSwitchRequest
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.Admin.Switch(r.Context(), staff(r), in)
	s.admin(w, r, http.StatusOK, v, err)
}

// ListEmergencyCases is GET /v1/admin/emergency.
func (s *Server) ListEmergencyCases(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.Cases(r.Context())
	s.admin(w, r, http.StatusOK, v, err)
}

// GetEmergencyCase is GET /v1/admin/emergency/{flight_id}.
func (s *Server) GetEmergencyCase(w http.ResponseWriter, r *http.Request, flightID openapi_types.UUID) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.Case(r.Context(), flightID.String())
	s.admin(w, r, http.StatusOK, v, err)
}

// ActOnEmergency is POST /v1/admin/emergency/{flight_id}: 201 for a case
// opened, 200 for a note or the close.
func (s *Server) ActOnEmergency(w http.ResponseWriter, r *http.Request, flightID openapi_types.UUID) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	var in gen.EmergencyAction
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, opened, err := s.Admin.Act(r.Context(), staff(r), flightID.String(), in)
	status := http.StatusOK
	if opened {
		status = http.StatusCreated
	}
	s.admin(w, r, status, v, err)
}

// ListRecordDays is GET /v1/admin/records/days.
func (s *Server) ListRecordDays(w http.ResponseWriter, r *http.Request) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	v, err := s.Admin.RecordDays(r.Context())
	s.admin(w, r, http.StatusOK, v, err)
}

// ListAdminEvents is GET /v1/admin/events.
func (s *Server) ListAdminEvents(w http.ResponseWriter, r *http.Request, params gen.ListAdminEventsParams) {
	if s.Admin == nil {
		adminUnavailable(w, r)
		return
	}
	limit := 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	v, err := s.Admin.Events(r.Context(), string(params.EntityType), params.EntityId, limit)
	s.admin(w, r, http.StatusOK, v, err)
}
