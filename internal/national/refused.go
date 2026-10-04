package national

import (
	"net/http"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// RefusedError is a request a console service does not admit, with the
// reason it gives: 404 when NotFound, 400 otherwise. internal/app maps
// the services' own errors onto it.
type RefusedError struct {
	Reason   string
	NotFound bool
}

func (e *RefusedError) Error() string { return e.Reason }

func obsError(r *http.Request, s *Server, msg string, err error) {
	obs.Error(r.Context(), s.logger(), msg, err)
}
