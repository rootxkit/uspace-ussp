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
	// ClientOf and ListOperator serve a portal session (brief WP-17).
	ClientOf(ctx context.Context, operatorID, id string) (string, error)
	ListOperator(ctx context.Context, operatorID string, f intent.ListFilter) ([]intent.Decision, error)
}

// actingClient is the client a request acts as: an operator token's
// own, or for a portal session the client the intent (id) was filed
// under, or, for a new intent (raw), the one its UAS serial is bound to.
// A viewer is refused when write. It writes the problem and returns
// false when the request may not go on.
func (s *Server) actingClient(w http.ResponseWriter, r *http.Request, id string, raw []byte, write bool) (string, *http.Request, bool) {
	if !portalSession(r) {
		return principal(r).Claims.Subject, r, true
	}
	m, ok := s.member(w, r, write)
	if !ok {
		return "", r, false
	}
	var client string
	var err error
	if raw != nil {
		var sn string
		if sn, err = intent.SerialOf(raw); err == nil {
			client, err = s.Portal.BoundClient(r.Context(), m.OperatorID, sn)
		}
	} else {
		client, err = s.Intents.ClientOf(r.Context(), m.OperatorID, id)
	}
	if err != nil {
		s.failIntent(w, r, err)
		return "", r, false
	}
	return client, r.WithContext(intent.WithPortalUser(r.Context(), m.Actor())), true
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
	client, r, ok := s.actingClient(w, r, "", raw, true)
	if !ok {
		return
	}
	d, created, err := s.Intents.Submit(r.Context(), client, raw)
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
	var ds []intent.Decision
	var err error
	if portalSession(r) {
		m, ok := s.member(w, r, false)
		if !ok {
			return
		}
		ds, err = s.Intents.ListOperator(r.Context(), m.OperatorID, f)
	} else {
		ds, err = s.Intents.List(r.Context(), principal(r).Claims.Subject, f)
	}
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
	client, r, ok := s.actingClient(w, r, intentID.String(), nil, false)
	if !ok {
		return
	}
	d, err := s.Intents.Get(r.Context(), client, intentID.String())
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
	client, r, ok := s.actingClient(w, r, intentID.String(), nil, true)
	if !ok {
		return
	}
	d, err := s.Intents.Change(r.Context(), client, intentID.String(), raw)
	if err != nil {
		s.failIntent(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
