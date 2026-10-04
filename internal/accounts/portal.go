package accounts

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// The bounds of the portal's client list (brief WP-17, E-10).
const (
	MaxClientsListed        = 200
	MaxSerialsListedPerItem = 200
)

// SlugSerialNotBound is the problem type of a portal intent whose UAS
// serial is bound to no client of the session's operator.
const SlugSerialNotBound = "serial_not_bound"

// Member is the portal user behind a session: its account, its
// operator and its role (brief WP-17).
type Member struct {
	AccountID  string
	OperatorID string
	Role       string
}

// Writes reports whether the role may file, change and acknowledge
// (operator_admin, remote_pilot); a viewer only reads.
func (m Member) Writes() bool {
	return m.Role == auth.RoleOperatorAdmin || m.Role == auth.RoleRemotePilot
}

// Actor is the member's audit and acknowledgement name,
// operator_user:<account id>.
func (m Member) Actor() string { return auth.ActorPortalUser + ":" + m.AccountID }

// portalUser returns the active portal user behind p.
func (s *Service) portalUser(ctx context.Context, q *relational.Queries, p auth.Principal) (relational.PortalUser, error) {
	if !p.Session || p.Claims.Realm != auth.RealmPortal {
		return relational.PortalUser{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "an operator portal session is required")
	}
	uid, err := store.UUID("sub", p.Claims.Subject)
	if err != nil {
		return relational.PortalUser{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "the session names no portal user")
	}
	u, err := q.PortalUserByID(ctx, uid)
	if store.IsNoRows(err) {
		return relational.PortalUser{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "the session names no portal user")
	}
	if err != nil {
		return relational.PortalUser{}, err
	}
	if u.Status != StatusActive {
		return relational.PortalUser{}, refuse(http.StatusForbidden, httpx.SlugForbidden, "the portal user is not active")
	}
	return u, nil
}

// PortalMember returns the active portal user behind p with its operator
// and role; any other caller is 403.
func (s *Service) PortalMember(ctx context.Context, p auth.Principal) (Member, error) {
	u, err := s.portalUser(ctx, s.Store.Queries(), p)
	if err != nil {
		return Member{}, err
	}
	return Member{AccountID: store.UUIDText(u.ID), OperatorID: store.UUIDText(u.OperatorID), Role: u.Role}, nil
}

// ClientInfo is one client of the portal's list: never a secret.
type ClientInfo struct {
	ClientID           string
	Scopes             []string
	Status             string
	CreatedAt          time.Time
	RotatedAt          *time.Time
	PreviousValidUntil *time.Time
	Serials            []BoundSerial
	SerialsTruncated   bool
}

// BoundSerial is a live binding of the list.
type BoundSerial struct {
	Serial  string
	BoundAt time.Time
}

// ListClients returns the clients of operatorID, oldest first, at most
// MaxClientsListed (truncated says when there are more), each with at
// most MaxSerialsListedPerItem live bindings, for a portal user of that
// operator (any role).
func (s *Service) ListClients(ctx context.Context, p auth.Principal, operatorID string) ([]ClientInfo, bool, error) {
	q := s.Store.Queries()
	u, err := s.portalUser(ctx, q, p)
	if err != nil {
		return nil, false, err
	}
	if store.UUIDText(u.OperatorID) != strings.ToLower(operatorID) {
		return nil, false, errNotYours
	}
	rows, err := q.ClientsOfOperator(ctx, relational.ClientsOfOperatorParams{OperatorID: u.OperatorID, MaxRows: MaxClientsListed + 1})
	if err != nil {
		return nil, false, err
	}
	truncated := len(rows) > MaxClientsListed
	if truncated {
		rows = rows[:MaxClientsListed]
	}
	out := make([]ClientInfo, len(rows))
	ids := make([]string, len(rows))
	for i, r := range rows {
		out[i] = ClientInfo{ClientID: r.ClientID, Scopes: r.Scopes, Status: r.Status, CreatedAt: r.CreatedAt.UTC(),
			RotatedAt: utcPtr(r.RotatedAt), PreviousValidUntil: utcPtr(r.PreviousValidUntil), Serials: []BoundSerial{}}
		ids[i] = r.ClientID
	}
	if len(ids) == 0 {
		return out, truncated, nil
	}
	bs, err := q.LiveBindingsOfClients(ctx, relational.LiveBindingsOfClientsParams{ClientIds: ids, PerClient: MaxSerialsListedPerItem + 1})
	if err != nil {
		return nil, false, err
	}
	for _, b := range bs {
		i := slices.Index(ids, b.ClientID)
		if i < 0 {
			continue
		}
		if len(out[i].Serials) == MaxSerialsListedPerItem {
			out[i].SerialsTruncated = true
			continue
		}
		out[i].Serials = append(out[i].Serials, BoundSerial{Serial: b.Serial, BoundAt: b.BoundAt.UTC()})
	}
	return out, truncated, nil
}

// BoundClient returns the client of operatorID that holds the live
// binding of sn (a portal intent is filed under it, brief WP-17); a
// serial bound to no client of the operator, or to another operator's,
// is 403 serial_not_bound.
func (s *Service) BoundClient(ctx context.Context, operatorID, sn string) (string, error) {
	notBound := refuse(http.StatusForbidden, SlugSerialNotBound, "the UAS serial is bound to no client of this operator")
	fold := serial.FoldKey(serial.Normalize(strings.TrimSpace(sn))) //nolint:misspell // uspace-core's name
	if fold == "" {
		return "", notBound
	}
	q := s.Store.Queries()
	b, err := q.LiveBindingByFold(ctx, fold)
	if store.IsNoRows(err) {
		return "", notBound
	}
	if err != nil {
		return "", err
	}
	c, err := q.ClientByID(ctx, b.ClientID)
	if store.IsNoRows(err) {
		return "", notBound
	}
	if err != nil {
		return "", err
	}
	if store.UUIDText(c.OperatorID) != strings.ToLower(operatorID) {
		return "", notBound
	}
	return c.ClientID, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
