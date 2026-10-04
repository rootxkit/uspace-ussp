package national

import (
	"context"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/rootxkit/uspace-ussp/internal/coordination"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// CoordinationLister lists the Annex V notices the console must see
// (internal/coordination's Store).
type CoordinationLister interface {
	Open(ctx context.Context, n int) ([]coordination.Item, bool, error)
}

// ListCoordinationNotices is GET /v1/admin/coordination: the pending,
// failed and escalated notices to the ANSP, oldest first. A list that
// cannot be read is 503, never an empty list.
func (s *Server) ListCoordinationNotices(w http.ResponseWriter, r *http.Request) {
	if s.Coordination == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "coordination_unavailable", "", "the coordination notices are not configured on this process").Write(w, r)
		return
	}
	items, truncated, err := s.Coordination.Open(r.Context(), coordination.MaxListed)
	if err != nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "coordination_unavailable", "", "the coordination notices cannot be read").Write(w, r)
		return
	}
	out := gen.CoordinationNotices{Notices: make([]gen.CoordinationNoticeItem, 0, len(items)), Truncated: truncated}
	for i := range items {
		it := &items[i]
		g := gen.CoordinationNoticeItem{Id: it.ID, NoticeRef: it.NoticeRef, Kind: gen.CoordinationNoticeItemKind(it.Kind),
			State: gen.CoordinationNoticeItemState(it.State), CreatedAt: it.CreatedAt, AgeS: it.AgeS, Attempts: it.Attempts,
			LastError: it.LastError, AckId: it.AckID, ReceivedAt: it.ReceivedAt, EscalatedAt: it.EscalatedAt, FailedAt: it.FailedAt, NextAt: it.NextAt}
		if err := g.IntentId.UnmarshalText([]byte(it.IntentID)); err != nil {
			s.fail(w, r, err)
			return
		}
		if it.FlightID != nil {
			var f openapi_types.UUID
			if err := f.UnmarshalText([]byte(*it.FlightID)); err != nil {
				s.fail(w, r, err)
				return
			}
			g.FlightId = &f
		}
		out.Notices = append(out.Notices, g)
	}
	writeJSON(w, http.StatusOK, out)
}
