package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"

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

// ErrWSRefused is returned by AcceptWS when the upgrade was refused:
// answered with its problem before any upgrade, or, for a browser's
// session, accepted and closed with CloseRelogin.
var ErrWSRefused = errors.New("websocket upgrade refused")

// WSAuth authenticates WebSocket upgrades for the stream processes
// (M22): a browser on a same-origin upgrade with the uspace_session
// cookie and an Origin on AllowedOrigins (USSP_WS_ALLOWED_ORIGINS), or
// a machine client with a bearer token and no browser Origin. No ticket.
//
// Every upgrade is judged before anything is upgraded (conformance C8),
// and before the operation's parameters are read when the route is
// registered through Require (C6). A refusal is answered with its
// status and problem body and never with 101: no credential or a
// refused token is 401, an Origin that is not allowed or a cookie
// without an Origin 403, a scope or realm the operation does not admit
// 403, keys or a session store that cannot be read now 503. The one
// exception is the browser of M22: an upgrade from an allowed Origin
// that carries the session cookie, whose session is refused, or no
// credential at all (the cookie of an ended session has expired), is
// accepted and closed at once with 4401 (1013 when it cannot be checked
// now), because a browser cannot read the status of a failed handshake
// and must be told to sign in again. An upgrade with no credential and
// no Origin is not a browser's and gets the 401. The CSRF double submit
// does not apply: the Origin allow-list is what keeps another site from
// opening the socket with the cookie.
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

// judge decides an upgrade before anything is upgraded. browser is true
// when a refusal is to be told as a close (M22): an allowed Origin with
// the session cookie or with no credential at all.
func (a *WSAuth) judge(r *http.Request, access httpx.Access) (p Principal, ref *refusal, browser bool) {
	origin := r.Header.Get("Origin")
	token, viaCookie, malformed := credential(r)
	originRefused := func(detail string) *refusal {
		return &refusal{status: http.StatusForbidden, slug: httpx.SlugForbidden, counter: CounterWSOriginRefused,
			detail: detail, field: &core.FieldError{Field: "Origin", Reason: "not an allowed origin"},
			actor: ActorAnonymous, actorID: "unknown"}
	}
	switch {
	case origin != "" && !a.originAllowed(origin):
		// A browser of another site, with or without the cookie.
		return Principal{}, originRefused("the upgrade's Origin is not allowed"), false
	case viaCookie && origin == "":
		// The cookie is a browser's credential and a browser always
		// sends its Origin on an upgrade.
		return Principal{}, originRefused("an upgrade with the session cookie must come from an allowed Origin"), false
	}
	// A WebSocket upgrade is a GET, so the guard asks no CSRF token.
	p, ref = a.Guard.authenticate(r, access)
	// Past the switch an Origin is an allowed one. A browser whose
	// session cookie expired sends none, and must still be told to sign
	// in again; a bearer is a machine client's and is answered.
	noCredential := token == "" && !malformed
	return p, ref, viaCookie || (origin != "" && noCredential)
}

// refuse answers a refused upgrade: the problem before any upgrade, or,
// for a browser, the upgrade closed with 4401 (1013 when the
// session cannot be checked now).
func (a *WSAuth) refuse(w http.ResponseWriter, r *http.Request, ref *refusal, browser bool) {
	g := a.Guard
	g.count(CounterWSRefused)
	if !browser {
		g.refuse(w, r, ref)
		return
	}
	g.record(r, ref)
	// The origin was judged; Accept must not judge it again.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	if ref.status == http.StatusServiceUnavailable {
		// The credential could not be judged now (the session store or
		// its projection is not available): try again, not sign in again.
		_ = conn.Close(websocket.StatusTryAgainLater, "the session cannot be checked now; retry later")
		return
	}
	_ = conn.Close(CloseRelogin, "sign in again")
}

// judged is the caller Require admitted for one access entry.
type judged struct {
	access string
	p      Principal
}

type judgedKey struct{}

// Require is the guard of the WebSocket entries of a GuardedMux
// (httpx.GuardedMux.WebSockets): it judges the upgrade against a before
// the generated router reads the operation's parameters, so a request
// without credential is 401 whatever else is wrong with it (C6),
// counts an admitted caller as Guard.Require does, and hands it to
// AcceptWS.
func (a *WSAuth) Require(access httpx.Access) func(http.Handler) http.Handler {
	key := access.String()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ref, browser := a.judge(r, access)
			if ref != nil {
				a.refuse(w, r, ref, browser)
				return
			}
			a.Guard.count(CounterAccepted)
			ctx := context.WithValue(WithPrincipal(r.Context(), p), judgedKey{}, judged{access: key, p: p})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AcceptWS authenticates the upgrade against access, unless Require
// already admitted the caller for the same entry, and accepts it. On
// success the connection and the caller are returned. A refusal is
// answered as Require answers it (the problem before any upgrade, or a
// browser's session closed with 4401), counted and audited as by the
// guard, and ErrWSRefused returned with a nil connection. An
// authenticated caller Admit turns away gets Admit's answer and no
// socket (ErrWSNotAdmitted).
func (a *WSAuth) AcceptWS(w http.ResponseWriter, r *http.Request, access httpx.Access) (*websocket.Conn, Principal, error) {
	var p Principal
	if j, ok := r.Context().Value(judgedKey{}).(judged); ok && j.access == access.String() {
		p = j.p
	} else {
		var ref *refusal
		var browser bool
		p, ref, browser = a.judge(r, access)
		if ref != nil {
			a.refuse(w, r, ref, browser)
			return nil, Principal{}, ErrWSRefused
		}
	}
	if a.Admit != nil && !a.Admit(w, r, p) {
		return nil, Principal{}, ErrWSNotAdmitted
	}
	// The origin was judged above; Accept must not judge it again.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, Principal{}, err
	}
	a.Guard.count(CounterWSAccepted)
	return conn, p, nil
}

// Guarded is Require as an httpx.Guard for GuardedMux.WebSockets; nil
// on a nil WSAuth, which the mux then refuses (fail closed).
func (a *WSAuth) Guarded() httpx.Guard {
	if a == nil {
		return nil
	}
	return a.Require
}
