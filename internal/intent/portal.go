package intent

import (
	"context"
	"net/http"
	"strings"
)

// The operator portal (brief WP-17) acts on an operator's intents
// through the client each was filed under; these are its lookups, and
// the portal user it names in the audit rows.

type portalUserKey struct{}

// WithPortalUser marks ctx as a request of a portal user, whose actor
// name (operator_user:<account id>) the audit rows of the intent's
// versions carry beside the client.
func WithPortalUser(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, portalUserKey{}, actor)
}

// PortalUserFrom is the portal user of WithPortalUser, "" when none.
func PortalUserFrom(ctx context.Context) string {
	s, _ := ctx.Value(portalUserKey{}).(string)
	return s
}

// ClientOf returns the client an intent of operatorID was filed under;
// another operator's intent, or none, is 404 not_found.
func (s *Service) ClientOf(ctx context.Context, operatorID, id string) (string, error) {
	if !validUUID(id) {
		return "", refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	r, err := s.Store.Get(ctx, id)
	if err != nil {
		return "", &UnavailableError{Dependency: "database", Detail: "the intent could not be read"}
	}
	if r == nil || !strings.EqualFold(r.OperatorID, operatorID) || r.ClientID == "" {
		return "", refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	return r.ClientID, nil
}

// SerialOf is the UAS serial of a request body (annex_iv item 1), for
// the portal to find the client the serial is bound to; the body's
// problems are the ones Submit answers.
func SerialOf(raw []byte) (string, error) {
	req, err := Decode(raw)
	if err != nil {
		return "", err
	}
	return req.UASSerial, nil
}

// ListOperator is GET /v1/intents for a portal session: the operator's
// intents, newest first, at most MaxList.
func (s *Service) ListOperator(ctx context.Context, operatorID string, f ListFilter) ([]Decision, error) {
	if err := checkFilter(&f); err != nil {
		return nil, err
	}
	return s.list(ctx, operatorID, f)
}
