package national

import (
	"context"
	"errors"
	"net/http"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// Occurrences serves /v1/admin/occurrences (internal/occurrence,
// adapted by internal/app). A flag the service does not admit is a
// *RefusedError.
type Occurrences struct {
	Service interface {
		Flag(ctx context.Context, staffID, alertID, kind, narrative string) (gen.OccurrenceReportItem, bool, error)
	}
	// List answers the open reports, bounded, and whether more exist.
	List interface {
		Open(ctx context.Context) ([]gen.OccurrenceReportItem, bool, error)
	}
	// Delivery is why no report is sent ("" when reports are sent).
	Delivery string
	// MaxNarrative bounds a supervisor's narrative (E-10); a longer one
	// is 400. Zero admits no narrative: the bound is never absent.
	MaxNarrative int
}

func occurrencesUnavailable(w http.ResponseWriter, r *http.Request, detail string) {
	httpx.NewProblem(http.StatusServiceUnavailable, "occurrences_unavailable", "", detail).Write(w, r)
}

// ListOccurrences is GET /v1/admin/occurrences: the reports not
// delivered, the nearest deadline first; a list that cannot be read is
// 503, never an empty list.
func (s *Server) ListOccurrences(w http.ResponseWriter, r *http.Request) {
	if s.Occurrences == nil || s.Occurrences.List == nil {
		occurrencesUnavailable(w, r, "the occurrence reports are not configured on this process")
		return
	}
	items, truncated, err := s.Occurrences.List.Open(r.Context())
	if err != nil {
		obsError(r, s, "occurrence reports not listed", err)
		occurrencesUnavailable(w, r, "the occurrence reports cannot be read")
		return
	}
	out := gen.OccurrenceReports{Reports: items, Truncated: truncated}
	if out.Reports == nil {
		out.Reports = []gen.OccurrenceReportItem{}
	}
	if s.Occurrences.Delivery != "" {
		d := s.Occurrences.Delivery
		out.Delivery = &d
	}
	writeJSON(w, http.StatusOK, out)
}

// FlagOccurrence is POST /v1/admin/occurrences: a supervisor's report of
// an alert, 201 when queued, 200 for an event reported before.
func (s *Server) FlagOccurrence(w http.ResponseWriter, r *http.Request) {
	if s.Occurrences == nil || s.Occurrences.Service == nil {
		occurrencesUnavailable(w, r, "the occurrence reports are not configured on this process")
		return
	}
	var body gen.OccurrenceFlag
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	kind, narrative := "", ""
	if body.Kind != nil {
		kind = string(*body.Kind)
	}
	if body.Narrative != nil {
		if len(*body.Narrative) > s.Occurrences.MaxNarrative {
			s.fail(w, r, core.Fieldf("narrative", "longer than %d characters", s.Occurrences.MaxNarrative))
			return
		}
		narrative = *body.Narrative
	}
	it, created, err := s.Occurrences.Service.Flag(r.Context(), principal(r).Claims.Subject, body.AlertId.String(), kind, narrative)
	var fe *RefusedError
	switch {
	case errors.As(err, &fe) && fe.NotFound:
		httpx.NewProblem(http.StatusNotFound, "alert_not_found", "", fe.Reason).Write(w, r)
		return
	case errors.As(err, &fe):
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", fe.Reason).Write(w, r)
		return
	case err != nil:
		obsError(r, s, "occurrence report not stored", err)
		occurrencesUnavailable(w, r, "the report cannot be stored")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, it)
}
