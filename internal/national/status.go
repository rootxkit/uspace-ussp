package national

import (
	"context"
	"errors"
	"net/http"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/status"
)

// StatusNotices serves /v1/admin/status (internal/status).
type StatusNotices interface {
	Request(ctx context.Context, staffID, kind string) (status.Notice, bool, error)
	List(ctx context.Context) ([]status.Notice, error)
}

func statusUnavailable(w http.ResponseWriter, r *http.Request, detail string) {
	httpx.NewProblem(http.StatusServiceUnavailable, "status_unavailable", "", detail).Write(w, r)
}

func statusNotice(n *status.Notice) gen.StatusNotice {
	return gen.StatusNotice{Kind: gen.StatusNoticeKind(n.Kind), At: n.At, CertificateId: n.CertificateID, Reference: n.Reference,
		RequestedBy: n.RequestedBy, State: gen.StatusNoticeState(n.State), Attempts: n.Attempts, NextAt: n.NextAt, LastError: n.LastError,
		SubmittedAt: n.SubmittedAt, AuthorityRef: n.AuthorityRef, FailedAt: n.FailedAt, CreatedAt: n.CreatedAt}
}

// ListStatusNotices is GET /v1/admin/status.
func (s *Server) ListStatusNotices(w http.ResponseWriter, r *http.Request) {
	if s.Status == nil {
		statusUnavailable(w, r, "the operating-status notices are not configured on this process")
		return
	}
	ns, err := s.Status.List(r.Context())
	if err != nil {
		statusUnavailable(w, r, "the operating-status notices cannot be read")
		return
	}
	out := gen.StatusNotices{Notices: make([]gen.StatusNotice, 0, len(ns))}
	if s.CertificateID != "" {
		id := s.CertificateID
		out.CertificateId = &id
	}
	for i := range ns {
		out.Notices = append(out.Notices, statusNotice(&ns[i]))
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
	var re *status.RequestError
	switch {
	case errors.As(err, &re):
		httpx.NewProblem(http.StatusBadRequest, "status_not_admitted", "", re.Reason).Write(w, r)
		return
	case err != nil:
		statusUnavailable(w, r, "the notice cannot be stored")
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, statusNotice(&n))
}
