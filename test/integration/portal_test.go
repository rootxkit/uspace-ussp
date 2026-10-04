//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/alerts"
	alertstore "github.com/rootxkit/uspace-ussp/internal/alerts/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	flightstore "github.com/rootxkit/uspace-ussp/internal/flights/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// portalOperator is operatorClient with what the portal needs: the
// operator id, the admin's session and the client.
type portalOperator struct {
	number, serial, opID, session, clientID, token string
}

func (g *intentRig) portalOperator() portalOperator {
	g.t.Helper()
	s := g.stack
	s.registry.set(accounts.RegistryValid)
	u := unique()
	p := portalOperator{number: "GEO-TEST-" + u, serial: "TEST" + u}
	user, pass := "portal."+u, "portal-password-"+u
	r := s.call("POST", "/v1/accounts/operators", map[string]any{
		"registration_number": p.number, "display_name": "Portal operator " + u, "contact_email": "portal" + u + "@example.test",
		"admin_username": user, "admin_password": pass,
	}, nil)
	if r.status != 201 {
		g.t.Fatalf("register: %d %s", r.status, r.raw)
	}
	p.opID = r.str("id")
	p.session = s.login(auth.RealmPortal, user, pass, "").str("token")
	var secret string
	p.clientID, secret = s.client(p.opID, p.session, "ussp.intents", "ussp.traffic")
	if b := s.call("POST", "/v1/accounts/operators/"+p.opID+"/clients/"+p.clientID+"/serials", map[string]any{"serial": p.serial}, bearer(p.session)); b.status != 201 {
		g.t.Fatalf("bind: %d %s", b.status, b.raw)
	}
	p.token = s.token(p.clientID, secret).str("access_token")
	g.auth.SetOperator(p.number, "active", nil)
	g.auth.SetUAS(p.serial, "active", "", "")
	return p
}

// portalUser adds a portal user of the operator with role and signs it
// in (the portal has no user administration yet: a row, as WP-2's
// bootstrap writes one).
func (g *intentRig) portalUser(opID, role string) string {
	g.t.Helper()
	u := unique()
	user, pass := role+"."+u, "portal-password-"+u
	hash, err := g.stack.svc.Hasher.Hash(pass)
	if err != nil {
		g.t.Fatal(err)
	}
	op, err := store.UUID("operator_id", opID)
	if err != nil {
		g.t.Fatal(err)
	}
	if _, err := relOwner(g.t).Exec(context.Background(),
		"INSERT INTO portal_users (operator_id, username, password_hash, role, status) VALUES ($1, $2, $3, $4, 'active')",
		op, user, hash, role); err != nil {
		g.t.Fatal(err)
	}
	l := g.stack.login(auth.RealmPortal, user, pass, "")
	if l.status != 200 {
		g.t.Fatalf("login %s: %d %s", role, l.status, l.raw)
	}
	return l.str("token")
}

// S-M1 in the portal (brief WP-17): an operator_admin's session files an
// intent under the client its serial is bound to and the decision is the
// one an operator client gets; the audit row names the portal user; the
// client list shows the client and its serial; a viewer of the operator
// reads the intent and the list and is refused the writes; a serial
// bound to no client of the operator is 403 serial_not_bound; another
// operator's session gets 404; the admin ends the intent; the operator
// client's token still sees the same intent (E-01 pairs).
func TestIntegrationPortalSessionFilesAndReads(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	p := g.portalOperator()
	r := g.stack.call("POST", "/v1/intents", g.request(p.number, p.serial, "portal-1", g.box(0, 0, 0.01)), bearer(p.session))
	if r.status != 201 || r.str("decision") != "authorised" || r.str("authorisation_number") == "" {
		t.Fatalf("portal files: %d %s", r.status, r.raw)
	}
	id := r.str("intent_id")
	if n := count(t, relOwner(t), "SELECT count(*) FROM operational_intents WHERE id = $1 AND client_id = $2", id, p.clientID); n != 1 {
		t.Fatal("not filed under the bound client")
	}
	if n := count(t, relOwner(t), `SELECT count(*) FROM events WHERE entity_id = $1 AND event_type = $2
		AND actor_id = $3 AND payload->>'portal_user' LIKE 'operator_user:%'`, id, intent.EventSubmitted, p.clientID); n != 1 {
		t.Fatal("the audit row does not name the portal user")
	}
	if tok := g.stack.call("GET", "/v1/intents/"+id, nil, bearer(p.token)); tok.status != 200 || tok.raw != r.raw {
		t.Fatalf("the client's own read: %d %s", tok.status, tok.raw)
	}

	cl := g.stack.call("GET", "/v1/accounts/operators/"+p.opID+"/clients", nil, bearer(p.session))
	if cl.status != 200 || !strings.Contains(cl.raw, p.clientID) || !strings.Contains(cl.raw, p.serial) ||
		strings.Contains(cl.raw, "secret") || cl.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("clients: %d %s", cl.status, cl.raw)
	}

	viewer := g.portalUser(p.opID, auth.RoleViewer)
	if v := g.stack.call("GET", "/v1/intents/"+id, nil, bearer(viewer)); v.status != 200 || v.str("intent_id") != id {
		t.Fatalf("viewer reads: %d %s", v.status, v.raw)
	}
	if v := g.stack.call("GET", "/v1/intents", nil, bearer(viewer)); v.status != 200 || !strings.Contains(v.raw, id) {
		t.Fatalf("viewer lists: %d %s", v.status, v.raw)
	}
	if v := g.stack.call("GET", "/v1/accounts/operators/"+p.opID+"/clients", nil, bearer(viewer)); v.status != 200 {
		t.Fatalf("viewer clients: %d", v.status)
	}
	if v := g.stack.call("POST", "/v1/intents", g.request(p.number, p.serial, "portal-viewer", g.box(0.1, 0, 0.01)), bearer(viewer)); v.status != 403 || v.slug() != "portal_read_only" {
		t.Fatalf("viewer files: %d %s", v.status, v.raw)
	}
	if v := g.stack.call("PATCH", "/v1/intents/"+id, map[string]any{"action": "end"}, bearer(viewer)); v.status != 403 {
		t.Fatalf("viewer ends: %d %s", v.status, v.raw)
	}
	pilot := g.portalUser(p.opID, auth.RoleRemotePilot)
	if v := g.stack.call("POST", "/v1/intents", g.request(p.number, "TEST-NOT-BOUND"+unique(), "portal-unbound", g.box(0.2, 0, 0.01)), bearer(pilot)); v.status != 403 || v.slug() != accounts.SlugSerialNotBound {
		t.Fatalf("unbound serial: %d %s", v.status, v.raw)
	}

	other := g.portalOperator()
	if o := g.stack.call("GET", "/v1/intents/"+id, nil, bearer(other.session)); o.status != 404 {
		t.Fatalf("another operator's session: %d", o.status)
	}
	if o := g.stack.call("GET", "/v1/accounts/operators/"+p.opID+"/clients", nil, bearer(other.session)); o.status != 403 {
		t.Fatalf("another operator's clients: %d", o.status)
	}
	if o := g.stack.call("PATCH", "/v1/intents/"+id, map[string]any{"action": "end"}, bearer(other.session)); o.status != 404 {
		t.Fatalf("another operator ends: %d", o.status)
	}

	end := g.stack.call("PATCH", "/v1/intents/"+id, map[string]any{"action": "end"}, bearer(p.session))
	if end.status != 200 || end.str("state") != "ended" {
		t.Fatalf("admin ends: %d %s", end.status, end.raw)
	}
}

// The client list is bounded both ways (E-10): 201 clients answer 200
// and truncated; 201 live bindings on one client answer 200 of them and
// serials_truncated; an unbound serial is not listed.
func TestIntegrationPortalClientListBounds(t *testing.T) {
	g := newIntentRig(t)
	p := g.portalOperator()
	db := relOwner(t)
	ctx := context.Background()
	op, err := store.UUID("operator_id", p.opID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO oauth_clients (client_id, operator_id, secret_hash, scopes, status)
		SELECT 'op-bound-' || $2 || '-' || i, $1, 'x', ARRAY['ussp.geo'], 'active' FROM generate_series(1, 200) i`, op, unique()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO client_serial_bindings (client_id, serial, serial_fold, bound_at)
		SELECT $1, 'TESTB' || $2 || i, 'TESTB' || $2 || i, now() FROM generate_series(1, 201) i`, p.clientID, unique()); err != nil {
		t.Fatal(err)
	}
	if b := g.stack.call("DELETE", "/v1/accounts/operators/"+p.opID+"/clients/"+p.clientID+"/serials/"+p.serial, nil, bearer(p.session)); b.status != 204 {
		t.Fatalf("unbind: %d %s", b.status, b.raw)
	}
	r := g.stack.call("GET", "/v1/accounts/operators/"+p.opID+"/clients", nil, bearer(p.session))
	cs, _ := r.body["clients"].([]any)
	if r.status != 200 || r.body["truncated"] != true || len(cs) != accounts.MaxClientsListed {
		t.Fatalf("clients: %d truncated %v, %d listed", r.status, r.body["truncated"], len(cs))
	}
	first, _ := cs[0].(map[string]any)
	ss, _ := first["serials"].([]any)
	if first["client_id"] != p.clientID || first["serials_truncated"] != true || len(ss) != accounts.MaxSerialsListedPerItem ||
		strings.Contains(r.raw, `"`+p.serial+`"`) {
		t.Fatalf("first client %v, %d serials", first["client_id"], len(ss))
	}
}

// The alert race (brief WP-17): the browser sees an alert on traffic-ws
// as soon as the monitor raises it, but its flight's row is written by
// api from the flight fact, which may come later. The alert is recorded
// at once under the intent it names (the flight left NULL, and the
// message still deferred for it), so the portal's acknowledgement finds
// it and answers; another operator's does not (E-01). Once the flight
// is recorded the next delivery fills the flight in, on a cleared row
// too, and keeps the acknowledgement.
func TestIntegrationPortalAcksBeforeTheFlightIsRecorded(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	p := g.portalOperator()
	r := g.stack.call("POST", "/v1/intents", g.request(p.number, p.serial, "ack-race", g.box(0.3, 0, 0.01)), bearer(p.session))
	if r.status != 201 {
		t.Fatalf("file: %d %s", r.status, r.raw)
	}
	intentID, flightID, alertID := r.str("intent_id"), newUUID(), newUUID()
	ctx := context.Background()
	db := relOwner(t)
	ast := alertstore.Store{S: appStore(t)}
	svc := &alerts.Service{Store: ast, Logger: quiet()}
	now := time.Now().UTC().Truncate(time.Millisecond)
	msg := func(state string) alerts.Record {
		b := alerts.Body{AlertID: alertID, Kind: "proximity", Severity: core.SeverityCritical, State: state, FlightID: flightID,
			IntentID: &intentID, CapturedAt: now, RaisedAt: now, UpdatedAt: now, PolicyVersion: 1, Detail: json.RawMessage(`{}`)}
		if state == alerts.StateCleared {
			reason := "resolved"
			b.ClearReason, b.UpdatedAt = &reason, now.Add(time.Second)
		}
		return alerts.Record{Body: b}
	}

	recorded, err := ast.RecordAlert(ctx, msg(alerts.StateRaised))
	if err != nil || recorded {
		t.Fatalf("raised before the flight: recorded %v, %v (the message must stay deferred for its flight)", recorded, err)
	}
	if n := count(t, db, "SELECT count(*) FROM alerts WHERE id = $1 AND flight_id IS NULL AND intent_id = $2", alertID, intentID); n != 1 {
		t.Fatal("the alert is not recorded under its intent while its flight is not")
	}
	actor := auth.ActorPortalUser + ":" + newUUID()
	other := g.portalOperator()
	if _, err := svc.AckForOperator(ctx, alertID, other.opID, actor); !errors.Is(err, alerts.ErrNotFound) {
		t.Fatalf("another operator's acknowledgement: %v", err)
	}
	res, err := svc.AckForOperator(ctx, alertID, p.opID, actor)
	if err != nil || res.AckedBy != actor {
		t.Fatalf("acknowledged before the flight is recorded: %+v %v", res, err)
	}

	if _, err := ast.RecordAlert(ctx, msg(alerts.StateCleared)); err != nil {
		t.Fatal(err)
	}
	if err := (flightstore.Store{S: appStore(t)}).Record(ctx, flights.Body{FlightID: flightID, Event: flights.EventStarted,
		At: bus.Stamp{Time: now}, StartedAt: bus.Stamp{Time: now}, ClientID: p.clientID, UASSerial: p.serial, IntentID: &intentID}); err != nil {
		t.Fatal(err)
	}
	recorded, err = ast.RecordAlert(ctx, msg(alerts.StateCleared))
	if err != nil || !recorded {
		t.Fatalf("after the flight: recorded %v, %v", recorded, err)
	}
	if n := count(t, db, `SELECT count(*) FROM alerts WHERE id = $1 AND flight_id = $2 AND cleared_at IS NOT NULL
		AND acked_by = $3`, alertID, flightID, actor); n != 1 {
		t.Fatal("the flight was not filled in, or the clear or the acknowledgement was lost")
	}
}
