//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/alerts"
	alertstore "github.com/rootxkit/uspace-ussp/internal/alerts/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

// H-2, the safety scenario of the ANSP's degraded direct delivery: the
// CISP never receives the restriction (its restrictions version stays
// at 3, above the restriction's ansp_version 2, which the receiver used
// to read as a CIS version and skip). The ANSP posts the activation to
// /v1/cis/notifications, signed with its own key; this USSP pulls the
// ANSP's signed restriction/direct/v1 from the pull_url (no credential),
// verifies it with the ANSP's publisher key, stores it and applies it:
// the activated intent under it gets restriction_activated within one
// tick. The ANSP lifts it the same way (restriction_ended, ansp_version
// 3): the clearing pass clears the alert resolved. Absence pairs:
// nothing is raised before the delivery, nor by a body under another
// key, and the clearing pass leaves the alert open while the
// restriction is in force.
func TestIntegrationDegradedDirectRestrictionRaisesAndClears(t *testing.T) {
	g := newIntentRig(t)
	pol := policy.Record{Version: 1, Values: policy.Defaults()}
	pol.Values.ActivationLeadS = 7200
	g.pol.Store(&pol)
	conn := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	pub := bus.NewPublisher(conn, g.counters)
	g.svc.Projector = intent.BusProjector{KV: g.kv, Pub: pub, Notices: geo.NoticeBus{Pub: pub, Counters: g.counters}}
	re := geo.NewRechecker(g.svc, g.cis.eval, g.counters, quiet())
	hook := cis.ChangeHook(re.Changed)
	g.cis.hook.Store(&hook)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go re.Run(ctx)
	g.publishAll(nil, nil, nil)
	g.publish(cis.Restrictions)
	g.publish(cis.Restrictions)
	if v := g.cis.version(cis.Restrictions); v != 3 {
		t.Fatalf("the CISP's restrictions version is %d", v)
	}

	number, serial, token := g.operatorWith("ussp.intents")
	c := g.file(token, g.request(number, serial, "d-"+unique(), g.box(0.001, 0.001, 0.004)))
	if c.status != 201 || c.str("state") != "accepted" {
		t.Fatalf("file: %d %s", c.status, c.raw)
	}
	id := c.str("intent_id")
	if r := g.stack.call("PATCH", "/v1/intents/"+id, map[string]any{"action": "activate"}, bearer(token)); r.status != 200 || r.str("state") != "activated" {
		t.Fatalf("activate: %d %s", r.status, r.raw)
	}
	log := &noticeLog{}
	stop, err := conn.Listen("alrt.v1.restriction_activated.>", func(_ string, data []byte) {
		var m geo.NoticeMessage
		if json.Unmarshal(data, &m) == nil {
			log.mu.Lock()
			log.msgs, log.at = append(log.msgs, m), append(log.at, time.Now())
			log.mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	rec := &alerts.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(conn.JetStream(), bus.DefaultTopology(), bus.StreamALRT, bus.PullSpec{
			Durable: "it-direct-" + unique(), FilterSubject: "alrt.v1.restriction_activated.>", MaxAckPending: 256,
		})},
		Store: alertstore.Store{S: appStore(t)}, Logger: quiet(),
	}
	go rec.Run(ctx)

	now := time.Now().UTC().Truncate(time.Millisecond)
	u := strings.ToUpper(unique())
	ident := "DAR" + u[len(u)-4:]
	restrictionID := "01K6P0" + u[len(u)-12:]
	feature := geoFeature(ident, "PROHIBITED", g.box(-0.002, -0.002, 0.01), 0, 3000, period(now, now.Add(3*time.Hour)), nil)
	pullURL := g.cis.ansp.URL() + "/v1/restrictions/" + restrictionID + "/direct"
	publisher := g.cis.fake.Publishers[cisp.PublisherANSP]
	serve := func(version int64, state string, signedBy *signer.Signer) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"schema": cis.DirectSchema, "id": restrictionID, "ansp_ref": "ansp-01:" + restrictionID, "ansp_version": version,
			"identifier": ident, "uspace_airspace_id": "GEOTU01", "state": state, "starts_at": now.Format(time.RFC3339Nano),
			"ends_at": now.Add(3 * time.Hour).Format(time.RFC3339Nano), "changed_at": time.Now().UTC().Format(time.RFC3339Nano),
			"feature": feature,
		})
		if err != nil {
			t.Fatal(err)
		}
		sig, err := signedBy.SignDetached(body, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		g.cis.ansp.ServeDirect(restrictionID, body, sig)
	}
	notify := func(version int64, reason string) {
		t.Helper()
		removed := []string{}
		if reason == "restriction_ended" {
			removed = []string{ident}
		}
		code, err := g.cis.ansp.Notify(ctx, g.cis.callback, restrictionID, map[string]any{
			"schema": "cis/change/v1", "msg_id": signer.NewID(), "producer": "ansp/api", "dataset": "restrictions", "version": version,
			"etag":        `"ansp-01:` + restrictionID + `:` + strconv.FormatInt(version, 10) + `"`,
			"feature_ids": []string{ident}, "removed_ids": removed, "reason": reason,
			"at": time.Now().UTC().Format(time.RFC3339Nano), "pull_url": pullURL,
		})
		if err != nil || code != http.StatusNoContent {
			t.Fatalf("notify %s: %d %v", reason, code, err)
		}
	}
	// The absence: a body under a key that is not the ANSP's publisher
	// key is pulled and refused; nothing is raised.
	serve(2, "active", g.cis.ansp.Signer)
	refused := g.cis.counters.Get(cis.CounterDirectRefused)
	notify(2, "restriction_activated")
	within(t, 10*time.Second, func() bool { return g.cis.counters.Get(cis.CounterDirectRefused) > refused })
	if n := len(log.of(id)); n != 0 || g.cis.eval.RestrictionLift(ident) != cis.LiftAbsent {
		t.Fatalf("a body the ANSP did not sign raised %d notices", n)
	}
	// The presence: the ANSP's signed body.
	serve(2, "active", publisher)
	delivered := time.Now()
	notify(2, "restriction_activated")
	within(t, 10*time.Second, func() bool { return len(log.of(id)) == 1 })
	log.mu.Lock()
	raisedAfter := log.at[0].Sub(delivered)
	log.mu.Unlock()
	t.Logf("direct restriction %s delivered to restriction_activated on the bus: %v", ident, raisedAfter)
	if raisedAfter > 2*time.Second {
		t.Fatalf("restriction_activated %v after the direct delivery, beyond one tick", raisedAfter)
	}
	raised := log.of(id)[0]
	if raised.Body.State != geo.StateRaised || raised.Body.Severity != core.SeverityCritical {
		t.Fatalf("raised %+v", raised.Body)
	}
	if v := g.cis.version(cis.Restrictions); v != 3 || g.cis.eval.RestrictionLift(ident) != cis.LiftInForce {
		t.Fatalf("CISP version %d, lift %v", v, g.cis.eval.RestrictionLift(ident))
	}
	if n, cred := g.cis.ansp.DirectPulls(); n < 2 || cred {
		t.Fatalf("pulls %d, a credential sent %v", n, cred)
	}
	if n := g.cis.dbCount("SELECT count(*) FROM cis_direct_restrictions WHERE identifier = $1 AND ansp_version = 2 AND state = 'active'", ident); n != 1 {
		t.Fatalf("stored %d", n)
	}
	db := relOwner(t)
	within(t, 10*time.Second, func() bool {
		return count(t, db, "SELECT count(*) FROM alerts WHERE id = $1::uuid AND cleared_at IS NULL", raised.Body.AlertID) == 1
	})
	svc := &alerts.Service{Store: alertstore.Store{S: appStore(t)}, Bus: pub, Restrictions: g.cis.cache, Logger: quiet()}
	if _, err := svc.ClearLiftedNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(log.of(id)); n != 1 {
		t.Fatalf("the clearing pass touched an alert whose restriction is in force: %d messages", n)
	}
	// The ANSP lifts it, still without the CISP.
	serve(3, "ended", publisher)
	notify(3, "restriction_ended")
	within(t, 10*time.Second, func() bool { return g.cis.eval.RestrictionLift(ident) == cis.LiftEnded })
	if n, err := svc.ClearLiftedNotices(ctx); err != nil || n < 1 {
		t.Fatalf("cleared on the direct end: %d %v", n, err)
	}
	within(t, 10*time.Second, func() bool {
		return count(t, db, "SELECT count(*) FROM alerts WHERE id = $1::uuid AND cleared_at IS NOT NULL AND clear_reason = 'resolved'",
			raised.Body.AlertID) == 1
	})
	ms := log.of(id)
	if last := ms[len(ms)-1]; last.Body.AlertID != raised.Body.AlertID || last.Body.State != "cleared" {
		t.Fatalf("clear on the bus %+v", last.Body)
	}
}
