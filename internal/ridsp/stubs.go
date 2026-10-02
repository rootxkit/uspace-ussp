package ridsp

import (
	"context"

	"github.com/rootxkit/uspace-ussp/internal/stdapi"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
)

// PostIdentificationServiceArea is not served yet.
func (s *Server) PostIdentificationServiceArea(context.Context, stdf3411.PostIdentificationServiceAreaRequestObject) (stdf3411.PostIdentificationServiceAreaResponseObject, error) {
	return nil, stdapi.ErrNotImplemented
}
