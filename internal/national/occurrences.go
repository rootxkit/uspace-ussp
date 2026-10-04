package national

import (
	"context"
	"errors"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/occurrence"
)

// Occurrences serves /v1/admin/occurrences (internal/occurrence).
type Occurrences struct {
	Service interface {
		Flag(ctx context.Context, staffID, alertID, kind, narrative string) (occurrence.Item, bool, error)
	}
	List interface {
		Open(ctx context.Context, n int) ([]occurrence.Item, bool, error)
	}
	// Delivery is why no report is sent ("" when reports are sent).
	Delivery string
}

func occurrencesUnavailable(w http.ResponseWriter, r *http.Request, detail string) {
	httpx.NewProblem(http.StatusServiceUnavailable, "occurrences_unavailable", "", detail).Write(w, r)
}

func reportItem(it *occurrence.Item) (gen.OccurrenceReportItem, error) {
	g := gen.OccurrenceReportItem{ReportRef: it.ReportRef, Kind: gen.OccurrenceReportItemKind(it.Kind), State: gen.OccurrenceReportItemState(it.State),
		Channel: gen.OccurrenceReportItemChannel(it.Channel), FlaggedBy: gen.OccurrenceReportItemFlaggedBy(it.FlaggedBy), BecameAwareAt: it.BecameAwareAt,
		DeadlineAt: it.DeadlineAt, TimeToDeadlineS: it.TimeToDeadlineS, Critical: it.Critical, Attempts: it.Attempts, LastError: it.LastError,
		SubmittedAt: it.SubmittedAt, AuthorityRef: it.AuthorityRef, FailedAt: it.FailedAt, NextAt: it.NextAt, FlightIds: make([]openapi_types.UUID, 0, len(it.FlightIDs))}
	for _, f := range it.FlightIDs {
		var u openapi_types.UUID
		if err := u.UnmarshalText([]byte(f)); err != nil {
			return g, err
		}
		g.FlightIds = append(g.FlightIds, u)
	}
	return g, nil
}

// ListOccurrences is GET /v1/admin/occurrences: the reports not
// delivered, the nearest deadline first; a list that cannot be read is
// 503, never an empty list.
func (s *Server) ListOccurrences(w http.ResponseWriter, r *http.Request) {
	if s.Occurrences == nil || s.Occurrences.List == nil {
		occurrencesUnavailable(w, r, "the occurrence reports are not configured on this process")
		return
	}
	items, truncated, err := s.Occurrences.List.Open(r.Context(), occurrence.MaxListed)
	if err != nil {
		occurrencesUnavailable(w, r, "the occurrence reports cannot be read")
		return
	}
	out := gen.OccurrenceReports{Reports: make([]gen.OccurrenceReportItem, 0, len(items)), Truncated: truncated}
	if s.Occurrences.Delivery != "" {
		d := s.Occurrences.Delivery
		out.Delivery = &d
	}
	for i := range items {
		g, err := reportItem(&items[i])
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out.Reports = append(out.Reports, g)
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
		if len(*body.Narrative) > occurrence.MaxNarrative {
			s.fail(w, r, core.Fieldf("narrative", "longer than %d characters", occurrence.MaxNarrative))
			return
		}
		narrative = *body.Narrative
	}
	it, created, err := s.Occurrences.Service.Flag(r.Context(), principal(r).Claims.Subject, body.AlertId.String(), kind, narrative)
	var fe *occurrence.FlagError
	switch {
	case errors.As(err, &fe) && fe.NotFound:
		httpx.NewProblem(http.StatusNotFound, "alert_not_found", "", fe.Reason).Write(w, r)
		return
	case errors.As(err, &fe):
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", fe.Reason).Write(w, r)
		return
	case err != nil:
		occurrencesUnavailable(w, r, "the report cannot be stored")
		return
	}
	g, err := reportItem(&it)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, g)
}
