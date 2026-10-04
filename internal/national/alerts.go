package national

import (
	"context"
	"errors"
	"net/http"

	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// AlertAcker records acknowledgements (internal/alerts.Service).
type AlertAcker interface {
	Ack(ctx context.Context, alertID, clientID string) (alerts.AckResult, error)
	AckForOperator(ctx context.Context, alertID, operatorID, actor string) (alerts.AckResult, error)
}

// SlugAlertNotFound is the problem type of an alert that is not the
// caller's operator's (or does not exist): 404, never 403.
const SlugAlertNotFound = "alert_not_found"

// AckAlert is POST /v1/alerts/{alert_id}/ack: the operator's
// acknowledgement of an alert of one of its flights.
func (s *Server) AckAlert(w http.ResponseWriter, r *http.Request, alertID gen.AlertID) {
	if s.Alerts == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "alerts_unavailable", "", "the alerts record is not configured on this process").Write(w, r)
		return
	}
	var res alerts.AckResult
	var err error
	if portalSession(r) {
		m, ok := s.member(w, r, true)
		if !ok {
			return
		}
		res, err = s.Alerts.AckForOperator(r.Context(), alertID.String(), m.OperatorID, m.Actor())
	} else {
		res, err = s.Alerts.Ack(r.Context(), alertID.String(), principal(r).Claims.Subject)
	}
	if errors.Is(err, alerts.ErrNotFound) {
		httpx.NewProblem(http.StatusNotFound, SlugAlertNotFound, "", "no alert of one of this operator's flights has this id").Write(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.AlertAck{AlertId: alertID, AckedAt: res.AckedAt, AckedBy: res.AckedBy})
}
