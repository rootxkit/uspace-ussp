package auth

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/coder/websocket"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// CloseRelogin is the WebSocket close code that means "the session is
// not valid: sign in again" (M22; uspace-ui reconnects through the
// login on it).
const CloseRelogin websocket.StatusCode = 4401

// Counter names of the WebSocket upgrade.
const (
	CounterWSOriginRefused = "ws_origin_refused"
	CounterWSRefused       = "ws_auth_refused"
	CounterWSAccepted      = "ws_auth_accepted"
)

// ErrWSRefused is returned by AcceptWS when the socket was closed with
// CloseRelogin.
var ErrWSRefused = errors.New("websocket upgrade refused: closed with 4401")

// WSAuth authenticates WebSocket upgrades for the stream processes
// (M22): a browser on a same-origin upgrade with the uspace_session
// cookie and an Origin on AllowedOrigins (USSP_WS_ALLOWED_ORIGINS), or
// a machine client with a bearer token and no browser Origin. No ticket.
// A refused upgrade is accepted and closed at once with 4401, because a
// browser cannot read the status of a failed handshake and must be told
// to sign in again. The CSRF double submit does not apply: the Origin
// allow-list is what keeps another site from opening the socket with
// the cookie.
type WSAuth struct {
	Guard *Guard
	// AllowedOrigins are full origins ("https://ussp.example"), compared
	// exactly, case-insensitively.
	AllowedOrigins []string
	// Admit, when set, is asked about an authenticated caller before the
	// upgrade. It returns false after answering the request itself, and
	// then nothing is upgraded: a source switched off is refused with 503
	// and Retry-After, which a client retries (B-10), never with a close
	// it may treat as final.
	Admit func(w http.ResponseWriter, r *http.Request, p Principal) bool
}

// ErrWSNotAdmitted is returned by AcceptWS when Admit answered the
// request instead of the upgrade.
var ErrWSNotAdmitted = errors.New("websocket upgrade not admitted: answered without an upgrade")

func (a *WSAuth) originAllowed(origin string) bool {
	return origin != "" && slices.ContainsFunc(a.AllowedOrigins, func(o string) bool { return strings.EqualFold(o, origin) })
}

// AcceptWS authenticates the upgrade against access and accepts it.
// On success the connection and the caller are returned. On refusal the
// socket is closed with CloseRelogin, the refusal counted and audited
// as by the guard, and ErrWSRefused returned with a nil connection. An
// authenticated caller Admit turns away gets Admit's answer and no
// socket (ErrWSNotAdmitted).
func (a *WSAuth) AcceptWS(w http.ResponseWriter, r *http.Request, access httpx.Access) (*websocket.Conn, Principal, error) {
	g := a.Guard
	origin := r.Header.Get("Origin")
	var p Principal
	var ref *refusal
	_, viaCookie, _ := credential(r)
	switch {
	case viaCookie && !a.originAllowed(origin):
		ref = &refusal{status: http.StatusForbidden, counter: CounterWSOriginRefused, actor: ActorAnonymous, actorID: "unknown"}
	case !viaCookie && origin != "" && !a.originAllowed(origin):
		// A browser that sends no cookie is still a browser: its Origin
		// must be allowed as well.
		ref = &refusal{status: http.StatusForbidden, counter: CounterWSOriginRefused, actor: ActorAnonymous, actorID: "unknown"}
	default:
		// A WebSocket upgrade is a GET, so the guard asks no CSRF token.
		p, ref = g.authenticate(r, access)
	}
	if ref == nil && a.Admit != nil && !a.Admit(w, r, p) {
		return nil, Principal{}, ErrWSNotAdmitted
	}
	// The origin was judged above; Accept must not judge it again.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, Principal{}, err
	}
	if ref != nil {
		g.count(CounterWSRefused)
		g.record(r, ref)
		_ = conn.Close(CloseRelogin, "sign in again")
		return nil, Principal{}, ErrWSRefused
	}
	g.count(CounterWSAccepted)
	return conn, p, nil
}
