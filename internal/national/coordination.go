package national

import (
	"context"
	"net/http"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// CoordinationLister lists the Annex V notices the console must see,
// bounded and already in the contract's form (internal/coordination's
// Store, adapted by internal/app: PLAN §4 keeps national from importing
// the packages of its own layer).
type CoordinationLister interface {
	Open(ctx context.Context) (gen.CoordinationNotices, error)
}

// ListCoordinationNotices is GET /v1/admin/coordination: the pending,
// failed and escalated notices to the ANSP, oldest first. A list that
// cannot be read is 503, never an empty list.
func (s *Server) ListCoordinationNotices(w http.ResponseWriter, r *http.Request) {
	if s.Coordination == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "coordination_unavailable", "", "the coordination notices are not configured on this process").Write(w, r)
		return
	}
	out, err := s.Coordination.Open(r.Context())
	if err != nil {
		obsError(r, s, "coordination notices not listed", err)
		httpx.NewProblem(http.StatusServiceUnavailable, "coordination_unavailable", "", "the coordination notices cannot be read").Write(w, r)
		return
	}
	if out.Notices == nil {
		out.Notices = []gen.CoordinationNoticeItem{}
	}
	writeJSON(w, http.StatusOK, out)
}
