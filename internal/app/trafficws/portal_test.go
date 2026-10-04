package trafficws

import (
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

const (
	portalOperator = "7d1e3c5a-2b4f-4a6e-8c9d-0e1f2a3b4c5d"
	portalSub      = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	portalJTI      = "portal-session-1"
)

// portalRig is the rig with intentA filed by clientA for portalOperator
// and a live portal session of that operator (and one of another).
func portalRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	fa := flightA
	intents := &bus.Mirror[intent.StateBody]{}
	intents.Seed(map[string]intent.StateBody{intentA: {IntentID: intentA, UASSerial: serialA, FlightID: &fa,
		OperatorID: portalOperator, ClientID: clientA, Volumes: []f3548.Volume4D{circle(origin, 500)}}})
	r.hub.Intents = intents
	bindings := &bus.Mirror[[]string]{}
	bindings.Seed(map[string][]string{bus.KeyToken(clientA): {serial.FoldKey(serialA)}})
	r.hub.Bindings = bindings
	exp := time.Now().Add(time.Hour)
	v := r.srv.WS.Guard.Verifier.(verifier)
	v["portal"] = coreauth.Claims{Issuer: ownIss, Subject: portalSub, Scopes: []string{auth.SessionScope}, Realm: auth.RealmPortal,
		Roles: []string{auth.RoleViewer}, JTI: portalJTI, ExpiresAt: exp}
	v["portal-other"] = coreauth.Claims{Issuer: ownIss, Subject: portalSub, Scopes: []string{auth.SessionScope}, Realm: auth.RealmPortal,
		Roles: []string{auth.RoleViewer}, JTI: "portal-other", ExpiresAt: exp}
	v["portal-none"] = coreauth.Claims{Issuer: ownIss, Subject: portalSub, Scopes: []string{auth.SessionScope}, Realm: auth.RealmPortal,
		Roles: []string{auth.RoleViewer}, JTI: "portal-none", ExpiresAt: exp}
	r.sessions.Seed(map[string]auth.LiveSession{
		bus.KeyToken(staffJTI):       {Subject: staffSub, Realm: auth.RealmConsole, ExpiresAt: exp, IdleUntil: exp},
		bus.KeyToken(portalJTI):      {Subject: portalSub, Realm: auth.RealmPortal, ExpiresAt: exp, IdleUntil: exp, OperatorID: portalOperator},
		bus.KeyToken("portal-other"): {Subject: portalSub, Realm: auth.RealmPortal, ExpiresAt: exp, IdleUntil: exp, OperatorID: "0e0e0e0e-0e0e-4e0e-8e0e-0e0e0e0e0e0e"},
		bus.KeyToken("portal-none"):  {Subject: portalSub, Realm: auth.RealmPortal, ExpiresAt: exp, IdleUntil: exp},
	})
	r.srv.PortalOperator = func(jti string) (string, bool, bool) {
		ls, found, _, loaded := r.sessions.Get(bus.KeyToken(jti))
		return ls.OperatorID, found, loaded
	}
	return r
}

// A portal session of the intent's operator follows the intent's
// traffic and alerts on the cookie from an allowed origin, served as the
// intent's client (its own flight is own), the alert's delivery recorded
// under operator_user:<account>; another operator's session is 404, one
// without an operator 403, a bbox 400, and no portal resolver 403
// (E-01 pairs).
func TestPortalSessionFollowsItsOperatorsIntent(t *testing.T) {
	ss := schemas(t)
	r := portalRig(t)
	now := time.Now()
	r.track(flightA, core.TrustAuthenticated, telemetry.SourceOperatorWS, clientA, origin, now, true)
	r.alert(traffic.AlertRaised, false, now)

	c, _, err := r.dial("/v1/traffic?intent_id="+intentA, "portal", "https://console.test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	p := product(t, next(t, ss, c, 2*time.Second, func(f frame) bool { return f.Schema == traffic.SchemaProduct }))
	own := false
	for _, tr := range p.Tracks {
		own = own || (tr.TrackID == flightA && tr.Own)
	}
	if !own || len(p.Alerts) != 1 {
		t.Fatalf("own %v, alerts %d", own, len(p.Alerts))
	}

	a, _, err := r.dial("/v1/alerts?intent_id="+intentA, "portal", "https://console.test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseNow()
	next(t, ss, a, 2*time.Second, func(f frame) bool { return f.Schema == alerts.SchemaAlert })
	waitFor(t, func() bool { return r.pub.count("alrt/delivery") >= 1 })
	r.pub.mu.Lock()
	var delivered string
	for k, ms := range r.pub.msgs {
		if strings.HasPrefix(k, "alrt/delivery") {
			delivered = ms[0]
		}
	}
	r.pub.mu.Unlock()
	if !strings.Contains(delivered, `"client_id":"`+auth.ActorPortalUser+":"+portalSub+`"`) {
		t.Fatalf("delivery %s", delivered)
	}

	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/v1/traffic?intent_id=" + intentA, "portal-other", 404},
		{"/v1/alerts?intent_id=" + intentA, "portal-other", 404},
		{"/v1/traffic?intent_id=" + intentA, "portal-none", 403},
		{"/v1/traffic?bbox=44,41,45,42", "portal", 400},
	} {
		_, resp, err := r.dial(tc.path, tc.token, "https://console.test")
		if err == nil || resp == nil || resp.StatusCode != tc.status {
			t.Fatalf("%s with %s: %v %v", tc.path, tc.token, resp, err)
		}
	}
	r.srv.PortalOperator = nil
	if _, resp, err := r.dial("/v1/traffic?intent_id="+intentA, "portal", "https://console.test"); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("no resolver: %v %v", resp, err)
	}
}

// An intent projected before WP-17 (no operator and client) is not
// followed by a portal session, and an unread sessions_live is 503.
func TestPortalSessionNeedsTheProjectedOwner(t *testing.T) {
	r := portalRig(t)
	fa := flightA
	old := &bus.Mirror[intent.StateBody]{}
	old.Seed(map[string]intent.StateBody{intentA: {IntentID: intentA, UASSerial: serialA, FlightID: &fa}})
	r.hub.Intents = old
	if _, resp, err := r.dial("/v1/traffic?intent_id="+intentA, "portal", "https://console.test"); err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("old projection: %v %v", resp, err)
	}
	r.srv.PortalOperator = func(string) (string, bool, bool) { return "", false, false }
	if _, resp, err := r.dial("/v1/traffic?intent_id="+intentA, "portal", "https://console.test"); err == nil || resp == nil || resp.StatusCode != 503 {
		t.Fatalf("unread sessions: %v %v", resp, err)
	}
}
