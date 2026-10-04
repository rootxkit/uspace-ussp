package national

import (
	"context"
	"errors"
	"net/http"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// StatusNotices serves /v1/admin/status (internal/status, adapted by
// internal/app). A kind the notices do not admit is a *RefusedError.
type StatusNotices interface {
	Request(ctx context.Context, staffID, kind string) (gen.StatusNotice, bool, error)
	List(ctx context.Context) ([]gen.StatusNotice, error)
}

func statusUnavailable(w http.ResponseWriter, r *http.Request, detail string) {
	httpx.NewProblem(http.StatusServiceUnavailable, "status_unavailable", "", detail).Write(w, r)
}

// ListStatusNotices is GET /v1/admin/status.
func (s *Server) ListStatusNotices(w http.ResponseWriter, r *http.Request) {
	if s.Status == nil {
		statusUnavailable(w, r, "the operating-status notices are not configured on this process")
		return
	}
	ns, err := s.Status.List(r.Context())
	if err != nil {
		obsError(r, s, "operating-status notices not listed", err)
		statusUnavailable(w, r, "the operating-status notices cannot be read")
		return
	}
	out := gen.StatusNotices{Notices: ns}
	if out.Notices == nil {
		out.Notices = []gen.StatusNotice{}
	}
	if s.CertificateID != "" {
		id := s.CertificateID
		out.CertificateId = &id
	}
	writeJSON(w, http.StatusOK, out)
}

// RequestStatusNotice is POST /v1/admin/status: 201 for a new notice,
// 200 for the one already stored, 400 for one the notices do not admit.
func (s *Server) RequestStatusNotice(w http.ResponseWriter, r *http.Request) {
	if s.Status == nil {
		statusUnavailable(w, r, "the operating-status notices are not configured on this process")
		return
	}
	var body gen.StatusRequest
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	n, created, err := s.Status.Request(r.Context(), principal(r).Claims.Subject, string(body.Kind))
	var re *RefusedError
	switch {
	case errors.As(err, &re):
		httpx.NewProblem(http.StatusBadRequest, "status_not_admitted", "", re.Reason).Write(w, r)
		return
	case err != nil:
		obsError(r, s, "operating-status notice not stored", err)
		statusUnavailable(w, r, "the notice cannot be stored")
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, n)
}
