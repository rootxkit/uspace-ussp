package national

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// Intents is the intent service (internal/intent.Service).
type Intents interface {
	Submit(ctx context.Context, clientID string, raw []byte) (intent.Decision, bool, error)
	Get(ctx context.Context, clientID, id string) (intent.Decision, error)
	List(ctx context.Context, clientID string, f intent.ListFilter) ([]intent.Decision, error)
	Change(ctx context.Context, clientID, id string, raw []byte) (intent.Decision, error)
}

// readIntentBody reads a JSON body of at most intent.MaxRequestBytes.
func readIntentBody(r *http.Request) ([]byte, error) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return nil, core.Fieldf("Content-Type", "must be application/json")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, intent.MaxRequestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > intent.MaxRequestBytes {
		return nil, &http.MaxBytesError{Limit: intent.MaxRequestBytes}
	}
	return raw, nil
}

// failIntent writes an intent refusal: an *intent.Error with its
// problems by field (each Annex IV problem names its item), else the
// common mapping.
func (s *Server) failIntent(w http.ResponseWriter, r *http.Request, err error) {
	var ie *intent.Error
	if errors.As(err, &ie) {
		httpx.NewProblem(ie.Status, ie.Slug, "", ie.Detail, ie.FieldErrors()...).Write(w, r)
		return
	}
	s.fail(w, r, err)
}

func (s *Server) intentsUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.Intents == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, "intents_unavailable", "", "the intent service is not configured on this process").Write(w, r)
		return true
	}
	return false
}

// CreateIntent is POST /v1/intents: 201 with the decision, or 200 with
// the decision of an earlier request under the same client_ref.
func (s *Server) CreateIntent(w http.ResponseWriter, r *http.Request) {
	if s.intentsUnavailable(w, r) {
		return
	}
	raw, err := readIntentBody(r)
	if err != nil {
		s.failIntent(w, r, err)
		return
	}
	d, created, err := s.Intents.Submit(r.Context(), principal(r).Claims.Subject, raw)
	if err != nil {
		s.failIntent(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/v1/intents/"+d.IntentID)
	}
	writeJSON(w, status, d)
}

// ListIntents is GET /v1/intents.
func (s *Server) ListIntents(w http.ResponseWriter, r *http.Request, params gen.ListIntentsParams) {
	if s.intentsUnavailable(w, r) {
		return
	}
	f := intent.ListFilter{From: params.From, To: params.To}
	if params.State != nil {
		f.State = *params.State
	}
	if params.Limit != nil {
		f.Limit = *params.Limit
	}
	ds, err := s.Intents.List(r.Context(), principal(r).Claims.Subject, f)
	if err != nil {
		s.failIntent(w, r, err)
		return
	}
	if ds == nil {
		ds = []intent.Decision{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"intents": ds})
}

// GetIntent is GET /v1/intents/{intent_id}.
func (s *Server) GetIntent(w http.ResponseWriter, r *http.Request, intentID gen.IntentID) {
	if s.intentsUnavailable(w, r) {
		return
	}
	d, err := s.Intents.Get(r.Context(), principal(r).Claims.Subject, intentID.String())
	if err != nil {
		s.failIntent(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ChangeIntent is PATCH /v1/intents/{intent_id}.
func (s *Server) ChangeIntent(w http.ResponseWriter, r *http.Request, intentID gen.IntentID) {
	if s.intentsUnavailable(w, r) {
		return
	}
	raw, err := readIntentBody(r)
	if err != nil {
		s.failIntent(w, r, err)
		return
	}
	d, err := s.Intents.Change(r.Context(), principal(r).Claims.Subject, intentID.String(), raw)
	if err != nil {
		s.failIntent(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
