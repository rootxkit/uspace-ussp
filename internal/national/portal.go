package national

import (
	"context"
	"net/http"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
)

// The operator portal (brief WP-17). A portal session reaches the
// intents, geo, weather and alert operations as its operator: it reads
// whatever role it has, and files, changes and acknowledges as an
// operator_admin or a remote_pilot. An intent is written as the client
// it was filed under (a new one as the client its UAS serial is bound
// to), so every judgement, idempotency key and binding check is the one
// an operator client's request meets; the audit rows name the person.

// PortalMembers resolves a portal session to its operator
// (internal/accounts.Service).
type PortalMembers interface {
	PortalMember(ctx context.Context, p auth.Principal) (accounts.Member, error)
	BoundClient(ctx context.Context, operatorID, serial string) (string, error)
}

// SlugReadOnly is the problem type of a write by a viewer.
const SlugReadOnly = "portal_read_only"

// SlugPortalUnavailable is the problem type of a portal session on a
// process without the accounts service.
const SlugPortalUnavailable = "portal_unavailable"

// portalSession reports whether the request is a portal session's.
func portalSession(r *http.Request) bool {
	p := principal(r)
	return p.Session && p.Claims.Realm == auth.RealmPortal
}

// member resolves the request's portal session; write refuses a viewer.
// It writes the problem and returns false when the request may not go
// on.
func (s *Server) member(w http.ResponseWriter, r *http.Request, write bool) (accounts.Member, bool) {
	if s.Portal == nil {
		httpx.NewProblem(http.StatusServiceUnavailable, SlugPortalUnavailable, "", "portal sessions are not served by this process").Write(w, r)
		return accounts.Member{}, false
	}
	m, err := s.Portal.PortalMember(r.Context(), principal(r))
	if err != nil {
		s.fail(w, r, err)
		return accounts.Member{}, false
	}
	if write && !m.Writes() {
		httpx.NewProblem(http.StatusForbidden, SlugReadOnly, "", "a viewer reads the operator's intents and alerts; an operator_admin or a remote_pilot changes them").Write(w, r)
		return accounts.Member{}, false
	}
	return m, true
}

// ListClients is GET /v1/accounts/operators/{operator_id}/clients.
func (s *Server) ListClients(w http.ResponseWriter, r *http.Request, operatorID gen.OperatorID) {
	cs, truncated, err := s.Accounts.ListClients(r.Context(), principal(r), operatorID.String())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := gen.ClientList{Clients: make([]gen.ClientInfo, 0, len(cs)), Truncated: truncated}
	for i := range cs {
		c := &cs[i]
		info := gen.ClientInfo{ClientId: c.ClientID, Status: c.Status, CreatedAt: c.CreatedAt, RotatedAt: c.RotatedAt,
			PreviousValidUntil: c.PreviousValidUntil, Scopes: []gen.OperatorScope{}, Serials: []gen.BoundSerial{}, SerialsTruncated: c.SerialsTruncated}
		for _, sc := range c.Scopes {
			info.Scopes = append(info.Scopes, gen.OperatorScope(sc))
		}
		for _, b := range c.Serials {
			info.Serials = append(info.Serials, gen.BoundSerial{Serial: b.Serial, BoundAt: b.BoundAt})
		}
		out.Clients = append(out.Clients, info)
	}
	writeJSON(w, http.StatusOK, out)
}
